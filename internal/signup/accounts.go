package signup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultAccountsBase = "https://accounts.x.ai"
	ServicePathPrefix   = "/auth_mgmt.AuthManagement/"
	TurnstileSiteKey    = "0x4AAAAAAAhr9JGVDZbrZOo0"
	CastlePK            = "pk_p8GGWvD3TmFJZRsX3BQcqAv9aFVispNz"
	UserAgent           = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// TokenProviders abstract the anti-bot bits so we can swap isolate/solver later.
type CastleProvider interface {
	// CreateRequestToken returns a Castle request token for the current session.
	CreateRequestToken(ctx context.Context) (string, error)
}

type TurnstileProvider interface {
	// Solve returns a turnstile token for the configured sitekey/page.
	Solve(ctx context.Context, sitekey, pageURL string) (string, error)
}

type AccountsClient struct {
	Base   string
	HTTP   *http.Client
	Jar    http.CookieJar
	Castle CastleProvider
	// optional: used only on credentials step
	Turnstile TurnstileProvider
}

func NewAccountsClient(castle CastleProvider) (*AccountsClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &AccountsClient{
		Base:   DefaultAccountsBase,
		Jar:    jar,
		Castle: castle,
		HTTP: &http.Client{
			Timeout: 45 * time.Second,
			Jar:     jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}, nil
}

func (c *AccountsClient) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultAccountsBase
}

// Bootstrap hits the sign-up page to collect CF / anon cookies when possible.
func (c *AccountsClient) Bootstrap(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/sign-up?redirect=grok-com&return_to=%2F", nil)
	if err != nil {
		return err
	}
	c.setBrowserHeaders(req, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bootstrap HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *AccountsClient) setBrowserHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", c.base())
	if referer != "" {
		req.Header.Set("Referer", referer)
	} else {
		req.Header.Set("Referer", c.base()+"/sign-up?redirect=grok-com&return_to=%2F")
	}
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

func (c *AccountsClient) postGRPC(ctx context.Context, method string, msg []byte) (status int, body []byte, err error) {
	frame := GRPCWebFrame(msg)
	u := c.base() + ServicePathPrefix + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(frame))
	if err != nil {
		return 0, nil, err
	}
	c.setBrowserHeaders(req, c.base()+"/sign-up?redirect=grok-com&return_to=%2F")
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-User-Agent", "connect-es/2.1.1")
	req.Header.Set("Accept", "*/*")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// CreateEmailValidationCode sends step-1 of email signup.
func (c *AccountsClient) CreateEmailValidationCode(ctx context.Context, email string) error {
	if c.Castle == nil {
		return fmt.Errorf("castle provider required")
	}
	tok, err := c.Castle.CreateRequestToken(ctx)
	if err != nil {
		return fmt.Errorf("castle token: %w", err)
	}
	if tok == "" {
		return fmt.Errorf("empty castle token")
	}
	msg := CreateEmailValidationCodeRequest(email, tok)
	code, body, err := c.postGRPC(ctx, "CreateEmailValidationCode", msg)
	if err != nil {
		return err
	}
	if code == 403 && bytes.Contains(body, []byte("Cloudflare")) {
		return fmt.Errorf("cloudflare blocked CreateEmailValidationCode (need better TLS/cookies/IP)")
	}
	if code >= 400 {
		return fmt.Errorf("CreateEmailValidationCode HTTP %d: %s", code, truncate(string(body), 300))
	}
	// gRPC may still encode application errors inside 200
	_, trailer, _ := ParseGRPCWebResponse(body)
	if strings.Contains(trailer, "grpc-status:3") || strings.Contains(trailer, "grpc-status:7") || strings.Contains(trailer, "grpc-status:13") {
		return fmt.Errorf("grpc error trailer: %s", truncate(trailer, 400))
	}
	if strings.Contains(string(body), "disposable") {
		return fmt.Errorf("server rejected disposable email")
	}
	return nil
}

// VerifyEmailValidationCode submits the OTP from darkemail.
func (c *AccountsClient) VerifyEmailValidationCode(ctx context.Context, email, otp string) error {
	msg := VerifyEmailValidationCodeRequest(email, otp)
	code, body, err := c.postGRPC(ctx, "VerifyEmailValidationCode", msg)
	if err != nil {
		return err
	}
	if code >= 400 {
		return fmt.Errorf("VerifyEmailValidationCode HTTP %d: %s", code, truncate(string(body), 300))
	}
	_, trailer, _ := ParseGRPCWebResponse(body)
	if trailer != "" && strings.Contains(trailer, "grpc-status:") && !strings.Contains(trailer, "grpc-status:0") {
		return fmt.Errorf("verify grpc trailer: %s", truncate(trailer, 400))
	}
	return nil
}

// CookieHeader returns cookie header for debugging / isolate handoff.
func (c *AccountsClient) CookieHeader() string {
	u, _ := url.Parse(c.base())
	if c.Jar == nil || u == nil {
		return ""
	}
	var parts []string
	for _, ck := range c.Jar.Cookies(u) {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
