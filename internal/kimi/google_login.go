package kimi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Google OAuth loopback (same flow as Kimi Desktop). Client credentials must come from env —
// never commit secrets.
//
//	KIMI_GOOGLE_CLIENT_ID
//	KIMI_GOOGLE_CLIENT_SECRET
const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

func googleOAuthCreds() (clientID, clientSecret string, err error) {
	clientID = strings.TrimSpace(os.Getenv("KIMI_GOOGLE_CLIENT_ID"))
	clientSecret = strings.TrimSpace(os.Getenv("KIMI_GOOGLE_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("defina KIMI_GOOGLE_CLIENT_ID e KIMI_GOOGLE_CLIENT_SECRET no ambiente (não embutidos no binário público)")
	}
	return clientID, clientSecret, nil
}

var loopbackPorts = []int{61120, 61121, 61122, 61123, 61124}

// GoogleLoginSession is the result of browser Google login → Kimi tokens.
type GoogleLoginSession struct {
	Session
	IDToken string `json:"-"`
}

func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// LoginWithGoogleBrowser opens the USER's default browser (same as Kimi Desktop),
// receives OAuth callback on 127.0.0.1, exchanges for Google id_token, then
// POST https://www.kimi.com/api/auth/login/google with {code: id_token}.
func LoginWithGoogleBrowser(timeout time.Duration) (*GoogleLoginSession, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	clientID, clientSecret, err := googleOAuthCreds()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<html><body style="font-family:sans-serif;padding:40px"><h2>Login cancelado</h2><p>%s</p><p>Pode fechar esta aba.</p></body></html>`, e)
			errCh <- fmt.Errorf("google oauth: %s", e)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", 400)
			errCh <- fmt.Errorf("missing authorization code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;padding:40px;background:#111;color:#eee">
<h2>Login Google OK</h2><p>Pode fechar esta aba e voltar ao Grok Desktop.</p>
<script>setTimeout(function(){window.close()},800)</script>
</body></html>`)
		select {
		case codeCh <- code:
		default:
		}
	})

	var ln net.Listener
	var port int
	for _, p := range loopbackPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln = l
			port = p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("portas loopback ocupadas (%v) — feche apps que usem 61120–61124", loopbackPorts)
	}
	defer ln.Close()

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"select_account"},
	}.Encode()

	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("não abriu o navegador: %w", err)
	}

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout Google OAuth (%s) — tente de novo", timeout)
	}

	idToken, err := exchangeGoogleCode(code, verifier, redirectURI, clientID, clientSecret)
	if err != nil {
		return nil, err
	}

	sess, err := exchangeGoogleIDTokenForKimi(idToken)
	if err != nil {
		return nil, err
	}
	return &GoogleLoginSession{Session: *sess, IDToken: idToken}, nil
}

func exchangeGoogleCode(code, verifier, redirectURI, clientID, clientSecret string) (string, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequest(http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("google token exchange HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", err
	}
	idToken, _ := data["id_token"].(string)
	if idToken == "" {
		return "", fmt.Errorf("google response missing id_token")
	}
	return idToken, nil
}

// exchangeGoogleIDTokenForKimi — SPA: POST /api/auth/login/google {code: <google id_token>}
func exchangeGoogleIDTokenForKimi(idToken string) (*Session, error) {
	body, _ := json.Marshal(map[string]any{"code": idToken})
	req, err := http.NewRequest(http.MethodPost, DefaultKimiURL+"/api/auth/login/google", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", DefaultKimiURL)
	req.Header.Set("Referer", DefaultKimiURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.1.0")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("kimi google login HTTP %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("kimi login json: %w body=%s", err, truncate(string(b), 200))
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		access, _ = data["accessToken"].(string)
	}
	// nested
	if access == "" {
		if d, ok := data["data"].(map[string]any); ok {
			access, _ = d["access_token"].(string)
		}
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh, _ = data["refreshToken"].(string)
	}
	if access == "" {
		return nil, fmt.Errorf("kimi login missing access_token: %s", truncate(string(b), 300))
	}
	p, _ := DecodeJWT(access)
	s := &Session{
		AccessToken:  access,
		RefreshToken: refresh,
		Source:       "google_browser",
		CapturedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if p != nil {
		s.UserID = p.Sub
		s.DeviceID = DeviceIDString(p.DeviceID)
		s.SSID = p.SSID
		s.Exp = p.Exp
	}
	if s.UserID == "" {
		if u, ok := data["user"].(map[string]any); ok {
			if id, ok := u["id"].(string); ok {
				s.UserID = id
			}
		}
	}
	return s, nil
}
