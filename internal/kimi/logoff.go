package kimi

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LogoffURL is the consumer account deletion endpoint (Settings → Delete Account).
// DELETE with Bearer access JWT (web session). Confirmation phrase is UI-only.
const LogoffURL = DefaultKimiURL + "/api/user/logoff"

// HasWebSession reports whether the account can call consumer APIs (logoff, etc.).
// sk-kimi keys alone are Work gateway credentials and cannot delete the user account.
func HasWebSession(accessToken, refreshToken string) bool {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	if refreshToken != "" {
		return true
	}
	if accessToken == "" || strings.HasPrefix(accessToken, "sk-kimi-") {
		return false
	}
	return strings.Count(accessToken, ".") == 2
}

// EnsureAccessToken returns a usable access JWT, refreshing when needed.
func EnsureAccessToken(accessToken, refreshToken string) (access, refresh string, err error) {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	if strings.HasPrefix(accessToken, "sk-kimi-") {
		accessToken = ""
	}
	needRefresh := accessToken == "" || refreshToken != ""
	if accessToken != "" {
		if p, perr := DecodeJWT(accessToken); perr == nil && p != nil && p.Exp > 0 {
			// refresh if expired or under 2 minutes left
			if time.Until(time.Unix(p.Exp, 0)) > 2*time.Minute {
				return accessToken, refreshToken, nil
			}
			needRefresh = true
		}
	}
	if !needRefresh {
		if accessToken != "" {
			return accessToken, refreshToken, nil
		}
		return "", refreshToken, fmt.Errorf("no web access_token or refresh_token")
	}
	if refreshToken == "" {
		if accessToken != "" {
			return accessToken, "", nil
		}
		return "", "", fmt.Errorf("access_token expired and no refresh_token")
	}
	s, err := RefreshAccessToken(refreshToken)
	if err != nil {
		// last chance: try existing access if still present
		if accessToken != "" {
			return accessToken, refreshToken, nil
		}
		return "", refreshToken, err
	}
	access = s.AccessToken
	refresh = s.RefreshToken
	if refresh == "" {
		refresh = refreshToken
	}
	return access, refresh, nil
}

// LogoffAccount permanently deletes the Kimi user account (same as site Delete Account).
// Uses consumer web session JWT, not sk-kimi.
func LogoffAccount(accessToken string) error {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	if accessToken == "" || strings.HasPrefix(accessToken, "sk-kimi-") {
		return fmt.Errorf("web access_token JWT required for account logoff")
	}
	req, err := http.NewRequest(http.MethodDelete, LogoffURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("Origin", DefaultKimiURL)
	req.Header.Set("Referer", DefaultKimiURL+"/settings")
	req.Header.Set("x-msh-platform", "web")
	req.Header.Set("x-msh-version", "2.0.0")
	req.Header.Set("X-Language", "en-US")

	if p, err := DecodeJWT(accessToken); err == nil && p != nil {
		if did := DeviceIDString(p.DeviceID); did != "" && did != "<nil>" {
			req.Header.Set("x-msh-device-id", did)
		}
		if p.SSID != "" {
			req.Header.Set("x-msh-session-id", p.SSID)
		}
		if p.Sub != "" {
			req.Header.Set("X-Traffic-Id", p.Sub)
		}
	}

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 2xx and 404 (already gone) count as success for cleanup.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("logoff HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
}

// LogoffWithSession refreshes the web JWT if needed, then deletes the account.
// Returns the access token used (for diagnostics) and error.
func LogoffWithSession(accessToken, refreshToken string) (usedAccess string, err error) {
	if !HasWebSession(accessToken, refreshToken) {
		return "", fmt.Errorf("account has no web session (only sk-kimi?) — cannot logoff")
	}
	access, _, err := EnsureAccessToken(accessToken, refreshToken)
	if err != nil {
		return "", err
	}
	if err := LogoffAccount(access); err != nil {
		return access, err
	}
	return access, nil
}
