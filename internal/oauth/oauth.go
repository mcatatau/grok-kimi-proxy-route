package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

type DeviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type Client struct {
	HTTP       *http.Client
	Issuer     string
	ClientID   string
	Scopes     string
	CLIVersion string
}

func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
		Issuer:     store.DefaultIssuer,
		ClientID:   store.DefaultClientID,
		Scopes:     store.DefaultScopes,
		CLIVersion: store.DefaultClientVersion,
	}
}

func (c *Client) headers() http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("User-Agent", "grok-desktop/"+c.CLIVersion)
	h.Set("x-grok-client-version", c.CLIVersion)
	h.Set("x-grok-client-surface", "grok-desktop")
	return h
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header = c.headers()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (c *Client) StartDevice(ctx context.Context) (*DeviceStart, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("scope", c.Scopes)
	ep := strings.TrimRight(c.Issuer, "/") + "/oauth2/device/code"
	b, code, err := c.postForm(ctx, ep, form)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("device code HTTP %d: %s", code, string(b))
	}
	var out DeviceStart
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("invalid device response: %s", string(b))
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out, nil
}

// PollDevice blocks until authorized, denied, expired, or ctx done.
func (c *Client) PollDevice(ctx context.Context, deviceCode string, intervalSec int) (*TokenResponse, error) {
	if intervalSec <= 0 {
		intervalSec = 5
	}
	ep := strings.TrimRight(c.Issuer, "/") + "/oauth2/token"
	pendingLogs := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(intervalSec) * time.Second):
		}
		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", deviceCode)
		form.Set("client_id", c.ClientID)
		b, code, err := c.postForm(ctx, ep, form)
		if err != nil {
			log.Printf("PollDevice: HTTP error: %v", err)
			return nil, err
		}
		var tok TokenResponse
		_ = json.Unmarshal(b, &tok)
		if tok.Error != "" {
			switch tok.Error {
			case "authorization_pending":
				pendingLogs++
				// spam control: first, then every 6th (~30s at 5s interval)
				if pendingLogs == 1 || pendingLogs%6 == 0 {
					log.Printf("PollDevice: still pending (n=%d) %s", pendingLogs, tok.ErrorDesc)
				}
				continue
			case "slow_down":
				intervalSec += 5
				log.Printf("PollDevice: slow_down → interval=%ds", intervalSec)
				continue
			default:
				log.Printf("PollDevice: error=%s desc=%s", tok.Error, tok.ErrorDesc)
				return nil, fmt.Errorf("%s: %s", tok.Error, tok.ErrorDesc)
			}
		}
		if code >= 400 {
			// try parse soft errors
			if strings.Contains(string(b), "authorization_pending") {
				pendingLogs++
				if pendingLogs == 1 || pendingLogs%6 == 0 {
					log.Printf("PollDevice: still pending HTTP (n=%d)", pendingLogs)
				}
				continue
			}
			return nil, fmt.Errorf("token HTTP %d: %s", code, string(b))
		}
		if tok.AccessToken == "" {
			continue
		}
		if tok.ExpiresIn <= 0 {
			tok.ExpiresIn = 21600
		}
		log.Printf("PollDevice: success token_len=%d", len(tok.AccessToken))
		return &tok, nil
	}
}

func (c *Client) Refresh(ctx context.Context, refreshToken, clientID, issuer string) (*TokenResponse, error) {
	if clientID == "" {
		clientID = c.ClientID
	}
	if issuer == "" {
		issuer = c.Issuer
	}
	ep := strings.TrimRight(issuer, "/") + "/oauth2/token"
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	b, code, err := c.postForm(ctx, ep, form)
	if err != nil {
		return nil, err
	}
	var tok TokenResponse
	_ = json.Unmarshal(b, &tok)
	if tok.Error != "" {
		return nil, fmt.Errorf("%s: %s", tok.Error, tok.ErrorDesc)
	}
	if code >= 400 || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh HTTP %d: %s", code, string(b))
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 21600
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return &tok, nil
}

func (c *Client) UserInfo(ctx context.Context, accessToken, issuer string) (email, userID string) {
	if issuer == "" {
		issuer = c.Issuer
	}
	ep := strings.TrimRight(issuer, "/") + "/oauth2/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	if e, ok := m["email"].(string); ok {
		email = e
	}
	if s, ok := m["sub"].(string); ok {
		userID = s
	}
	return email, userID
}

func ClaimsFromAccess(access string) (teamID, userID string) {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", ""
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", ""
	}
	if v, ok := m["team_id"].(string); ok {
		teamID = v
	}
	if v, ok := m["sub"].(string); ok {
		userID = v
	}
	return teamID, userID
}

func AccountFromToken(tok *TokenResponse, clientID, issuer string) store.Account {
	if clientID == "" {
		clientID = store.DefaultClientID
	}
	if issuer == "" {
		issuer = store.DefaultIssuer
	}
	teamID, userID := ClaimsFromAccess(tok.AccessToken)
	id := userID
	if id == "" {
		id = fmt.Sprintf("acc_%d", time.Now().UnixNano())
	}
	return store.Account{
		ID:           id,
		Label:        "Grok account",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second),
		ClientID:     clientID,
		Issuer:       issuer,
		Scope:        tok.Scope,
		TeamID:       teamID,
		UserID:       userID,
	}
}
