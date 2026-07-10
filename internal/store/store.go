package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	AppName              = "GrokDesktop"
	DefaultUpstream      = "https://cli-chat-proxy.grok.com/v1"
	DefaultClientVersion = "0.2.93"
	DefaultModel         = "grok-4.5"
	DefaultEffort        = "high"
	DefaultClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	DefaultIssuer        = "https://auth.x.ai"
	DefaultScopes        = "openid profile email offline_access api:access grok-cli:access conversations:read conversations:write"
)

type Account struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	Email        string    `json:"email,omitempty"`
	TeamID       string    `json:"team_id,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientID     string    `json:"client_id"`
	Issuer       string    `json:"issuer"`
	Scope        string    `json:"scope,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Exhausted    bool      `json:"exhausted,omitempty"`
	ExhaustedAt  time.Time `json:"exhausted_at,omitempty"`
	// ChatDenied: upstream 403 "Access to the chat endpoint is denied" / permissions
	ChatDenied       bool      `json:"chat_denied,omitempty"`
	ChatDeniedAt     time.Time `json:"chat_denied_at,omitempty"`
	ChatDeniedReason string    `json:"chat_denied_reason,omitempty"`
}

func (a *Account) Expired() bool {
	if a.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().UTC().After(a.ExpiresAt)
}

func (a *Account) ExpiresSoon(skew time.Duration) bool {
	if a.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().UTC().After(a.ExpiresAt.Add(-skew))
}

func (a *Account) IsExhausted() bool {
	if !a.Exhausted {
		return false
	}
	if a.ExhaustedAt.IsZero() {
		return true
	}
	return time.Since(a.ExhaustedAt) < 24*time.Hour
}

func (a *Account) ExhaustedStatus() string {
	if !a.Exhausted {
		return ""
	}
	if a.ExhaustedAt.IsZero() {
		return "exausta"
	}
	remaining := 24*time.Hour - time.Since(a.ExhaustedAt)
	if remaining <= 0 {
		return "recuperada"
	}
	return "exausta"
}

// ExhaustedRemaining returns time left in the 24h rate-limit window, or 0.
func (a *Account) ExhaustedRemaining() time.Duration {
	if !a.Exhausted {
		return 0
	}
	if a.ExhaustedAt.IsZero() {
		return 24 * time.Hour // unknown start → full window as upper bound
	}
	rem := 24*time.Hour - time.Since(a.ExhaustedAt)
	if rem < 0 {
		return 0
	}
	return rem
}

func (a *Account) IsChatDenied() bool {
	return a != nil && a.ChatDenied
}

type Settings struct {
	ActiveAccountID string `json:"active_account_id"`
	DefaultModel    string `json:"default_model"`
	ReasoningEffort string `json:"reasoning_effort"`
	APIMode         string `json:"api_mode"`
	UpstreamBase    string `json:"upstream_base"`
	ClientVersion   string `json:"client_version"`
	ProxyListen     string `json:"proxy_listen"`
	ProxyEnabled    bool   `json:"proxy_enabled"`
	ProxyAPIKey     string `json:"proxy_api_key,omitempty"`
	StoreResponses  bool   `json:"store_responses"`
	ThemeAccent     string `json:"theme_accent,omitempty"`

	// Auto-register (device OAuth + Python bot)
	// No pool size cap — only batch/loop concurrency.
	AutoRegisterEnabled   bool `json:"auto_register_enabled"`
	// Target minimum non-exhausted accounts for background loop (keep-alive).
	AutoRegisterMinActive int `json:"auto_register_min_active,omitempty"`
	// Max accounts to create in one batch/loop wave (simultaneous creation cap), NOT pool size.
	AutoRegisterMaxActive int      `json:"auto_register_max_active,omitempty"`
	PythonPath            string   `json:"python_path,omitempty"`
	BotDir                string   `json:"bot_dir,omitempty"`
	EmailProviders        []string `json:"email_providers,omitempty"`
	DuckMailURL           string   `json:"duckmail_url,omitempty"`
	DuckMailKey           string   `json:"duckmail_key,omitempty"`
}

type UsageTotals struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Requests         int64   `json:"requests"`
	CostUSD          float64 `json:"cost_usd"`
	// Latency aggregates (ms)
	LatencySumMs   int64 `json:"latency_sum_ms"`
	TTFTSumMs      int64 `json:"ttft_sum_ms"`
	LatencySamples int64 `json:"latency_samples"`
}

// RequestSample is one completed turn for charts / history.
type RequestSample struct {
	ID               string  `json:"id"`
	At               string  `json:"at"` // RFC3339
	AccountID        string  `json:"account_id"`
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	LatencyMs        int64   `json:"latency_ms"`
	TTFTMs           int64   `json:"ttft_ms"`
	Estimated        bool    `json:"estimated"`
}

// Layout under AppData:
//
//	%LOCALAPPDATA%/GrokDesktop/
//	  settings.json
//	  usage.json
//	  accounts/
//	    <account-id>.json
//	  logs/   (reserved)
type Store struct {
	mu       sync.RWMutex
	root     string
	settings Settings
	usage    map[string]UsageTotals
	// accounts loaded from accounts/*.json
	accounts map[string]Account
	// recent request samples (persisted, capped)
	history []RequestSample
}

func DefaultDataDir() (string, error) {
	// Windows: %LOCALAPPDATA%\GrokDesktop
	// macOS:   ~/Library/Application Support/GrokDesktop
	// Linux:   ~/.local/share/GrokDesktop
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, AppName), nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", AppName), nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}

func Open(root string) (*Store, error) {
	if root == "" {
		dir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		root = dir
	}
	for _, sub := range []string{"", "accounts", "logs"} {
		p := root
		if sub != "" {
			p = filepath.Join(root, sub)
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{
		root:     root,
		accounts: map[string]Account{},
		usage:    map[string]UsageTotals{},
		settings: defaultSettings(),
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	// One-time migrations
	_ = s.migrateFromLegacy()
	return s, nil
}

func defaultSettings() Settings {
	return Settings{
		DefaultModel:          DefaultModel,
		ReasoningEffort:       DefaultEffort,
		APIMode:               "responses",
		UpstreamBase:          DefaultUpstream,
		ClientVersion:         DefaultClientVersion,
		ProxyListen:           "127.0.0.1:8787",
		ProxyEnabled:          true,
		StoreResponses:        true,
		AutoRegisterEnabled:   false,
		AutoRegisterMinActive: 2,
		AutoRegisterMaxActive: 5,
	}
}

func (s *Store) Root() string { return s.root }

func (s *Store) settingsPath() string { return filepath.Join(s.root, "settings.json") }
func (s *Store) usagePath() string    { return filepath.Join(s.root, "usage.json") }
func (s *Store) historyPath() string  { return filepath.Join(s.root, "history.json") }
func (s *Store) accountsDir() string  { return filepath.Join(s.root, "accounts") }
func (s *Store) accountPath(id string) string {
	safe := id
	for _, c := range []string{`\`, `/`, `:`, `*`, `?`, `"`, `<`, `>`, `|`} {
		safe = strings.ReplaceAll(safe, c, "_")
	}
	return filepath.Join(s.accountsDir(), safe+".json")
}

func (s *Store) loadAll() error {
	// settings
	if b, err := os.ReadFile(s.settingsPath()); err == nil {
		var st Settings
		if json.Unmarshal(b, &st) == nil {
			s.settings = mergeSettings(defaultSettings(), st)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// usage
	if b, err := os.ReadFile(s.usagePath()); err == nil {
		var u map[string]UsageTotals
		if json.Unmarshal(b, &u) == nil && u != nil {
			s.usage = u
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// history
	if b, err := os.ReadFile(s.historyPath()); err == nil {
		var h []RequestSample
		if json.Unmarshal(b, &h) == nil {
			s.history = h
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// accounts
	entries, err := os.ReadDir(s.accountsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.accountsDir(), e.Name()))
		if err != nil {
			continue
		}
		var a Account
		if json.Unmarshal(b, &a) != nil || a.ID == "" || a.AccessToken == "" {
			continue
		}
		s.accounts[a.ID] = a
	}

	// ensure active account valid
	if s.settings.ActiveAccountID != "" {
		if _, ok := s.accounts[s.settings.ActiveAccountID]; !ok {
			s.settings.ActiveAccountID = ""
		}
	}
	if s.settings.ActiveAccountID == "" {
		for id := range s.accounts {
			s.settings.ActiveAccountID = id
			break
		}
	}
	return nil
}

func mergeSettings(base, over Settings) Settings {
	if over.DefaultModel != "" {
		base.DefaultModel = over.DefaultModel
	}
	if over.ReasoningEffort != "" {
		base.ReasoningEffort = over.ReasoningEffort
	}
	if over.APIMode != "" {
		base.APIMode = over.APIMode
	}
	if over.UpstreamBase != "" {
		base.UpstreamBase = over.UpstreamBase
	}
	if over.ClientVersion != "" {
		base.ClientVersion = over.ClientVersion
	}
	if over.ProxyListen != "" {
		base.ProxyListen = over.ProxyListen
	}
	base.ProxyEnabled = over.ProxyEnabled
	if over.ProxyAPIKey != "" {
		base.ProxyAPIKey = over.ProxyAPIKey
	}
	base.StoreResponses = over.StoreResponses
	if over.ActiveAccountID != "" {
		base.ActiveAccountID = over.ActiveAccountID
	}
	if over.ThemeAccent != "" {
		base.ThemeAccent = over.ThemeAccent
	}
	base.AutoRegisterEnabled = over.AutoRegisterEnabled
	if over.AutoRegisterMinActive > 0 {
		base.AutoRegisterMinActive = over.AutoRegisterMinActive
	}
	if over.AutoRegisterMaxActive > 0 {
		base.AutoRegisterMaxActive = over.AutoRegisterMaxActive
	}
	if over.PythonPath != "" {
		base.PythonPath = over.PythonPath
	}
	if over.BotDir != "" {
		base.BotDir = over.BotDir
	}
	if len(over.EmailProviders) > 0 {
		base.EmailProviders = append([]string{}, over.EmailProviders...)
	}
	if over.DuckMailURL != "" {
		base.DuckMailURL = over.DuckMailURL
	}
	if over.DuckMailKey != "" {
		base.DuckMailKey = over.DuckMailKey
	}
	return base
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) saveSettingsLocked() error {
	return writeJSON(s.settingsPath(), s.settings)
}

func (s *Store) saveUsageLocked() error {
	return writeJSON(s.usagePath(), s.usage)
}

func (s *Store) saveHistoryLocked() error {
	return writeJSON(s.historyPath(), s.history)
}

func (s *Store) saveAccountLocked(a Account) error {
	return writeJSON(s.accountPath(a.ID), a)
}

func (s *Store) Path() string { return s.root }

// migrateFromLegacy imports:
//  1. %USERPROFILE%\.grok-openai-proxy\desktop\state.json (old desktop monolith)
//  2. %USERPROFILE%\.grok-openai-proxy\auth.json (python proxy oauth)
func (s *Store) migrateFromLegacy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accounts) > 0 {
		return nil // already has data
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// 1) old desktop state
	oldState := filepath.Join(home, ".grok-openai-proxy", "desktop", "state.json")
	if b, err := os.ReadFile(oldState); err == nil {
		var d struct {
			Accounts []Account             `json:"accounts"`
			Settings Settings              `json:"settings"`
			Usage    map[string]UsageTotals `json:"usage"`
		}
		if json.Unmarshal(b, &d) == nil {
			for _, a := range d.Accounts {
				if a.ID == "" || a.AccessToken == "" {
					continue
				}
				s.accounts[a.ID] = a
				_ = s.saveAccountLocked(a)
			}
			if len(d.Accounts) > 0 {
				s.settings = mergeSettings(s.settings, d.Settings)
				if d.Usage != nil {
					s.usage = d.Usage
					_ = s.saveUsageLocked()
				}
				_ = s.saveSettingsLocked()
				return nil
			}
		}
	}

	// 2) python proxy auth.json (flat single account)
	authPath := filepath.Join(home, ".grok-openai-proxy", "auth.json")
	if b, err := os.ReadFile(authPath); err == nil {
		var flat map[string]any
		if json.Unmarshal(b, &flat) == nil {
			// our python format: access_token, refresh_token, ...
			if at, _ := flat["access_token"].(string); at != "" {
				a := Account{
					ID:           "migrated",
					Label:        "Imported",
					AccessToken:  at,
					RefreshToken: str(flat["refresh_token"]),
					ClientID:     or(str(flat["client_id"]), DefaultClientID),
					Issuer:       or(str(flat["issuer"]), DefaultIssuer),
					Scope:        str(flat["scope"]),
					Email:        str(flat["email"]),
					UserID:       str(flat["user_id"]),
					TeamID:       str(flat["team_id"]),
					CreatedAt:    time.Now().UTC(),
					UpdatedAt:    time.Now().UTC(),
				}
				if a.Email != "" {
					a.Label = a.Email
					if a.UserID != "" {
						a.ID = a.UserID
					}
				}
				if exp := str(flat["expires_at"]); exp != "" {
					if t, err := time.Parse(time.RFC3339Nano, exp); err == nil {
						a.ExpiresAt = t.UTC()
					} else if t, err := time.Parse(time.RFC3339, exp); err == nil {
						a.ExpiresAt = t.UTC()
					}
				}
				s.accounts[a.ID] = a
				s.settings.ActiveAccountID = a.ID
				_ = s.saveAccountLocked(a)
				_ = s.saveSettingsLocked()
			}
		}
	}
	return nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Store) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.settings)
	return s.saveSettingsLocked()
}

func (s *Store) ListAccounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out
}

func (s *Store) PublicAccounts() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.accounts))
	for _, a := range s.accounts {
		u := s.usage[a.ID]
		tokenExpired := a.Expired()
		hasRefresh := a.RefreshToken != ""
		// needs_login: access dead AND cannot refresh (SSO-style / no RT)
		needsLogin := tokenExpired && !hasRefresh
		exh := a.IsExhausted()
		remSec := int64(0)
		until := ""
		if exh {
			rem := a.ExhaustedRemaining()
			remSec = int64(rem.Seconds())
			if a.ExhaustedAt.IsZero() {
				until = time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
			} else {
				until = a.ExhaustedAt.Add(24 * time.Hour).UTC().Format(time.RFC3339)
			}
		}
		out = append(out, map[string]any{
			"id":         a.ID,
			"label":      a.Label,
			"email":      a.Email,
			"team_id":    a.TeamID,
			"expires_at": a.ExpiresAt,
			"expired":    tokenExpired, // access token clock (may still refresh)
			"has_refresh": hasRefresh,
			"needs_login": needsLogin,
			"active":     a.ID == s.settings.ActiveAccountID,
			"exhausted":  exh,
			"exhausted_status": a.ExhaustedStatus(),
			"exhausted_at": a.ExhaustedAt,
			"exhausted_remaining_sec": remSec,
			"exhausted_until": until,
			"chat_denied": a.IsChatDenied(),
			"chat_denied_reason": a.ChatDeniedReason,
			"usage": map[string]any{
				"prompt_tokens":     u.PromptTokens,
				"completion_tokens": u.CompletionTokens,
				"reasoning_tokens":  u.ReasoningTokens,
				"total_tokens":      u.TotalTokens,
				"requests":          u.Requests,
				"cost_usd":          u.CostUSD,
			},
		})
	}
	// stable sort: active first, then label
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			ai, _ := out[i]["active"].(bool)
			aj, _ := out[j]["active"].(bool)
			if aj && !ai {
				out[i], out[j] = out[j], out[i]
				continue
			}
			if ai == aj {
				li, _ := out[i]["label"].(string)
				lj, _ := out[j]["label"].(string)
				if lj < li {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
	}
	return out
}

func (s *Store) GetAccount(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, false
	}
	cp := a
	return &cp, true
}

func (s *Store) ActiveAccount() (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.settings.ActiveAccountID
	if id != "" {
		if a, ok := s.accounts[id]; ok {
			cp := a
			return &cp, true
		}
	}
	for _, a := range s.accounts {
		cp := a
		return &cp, true
	}
	return nil, false
}

func (s *Store) UpsertAccount(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if a.ID == "" {
		return errors.New("account id required")
	}
	if a.CreatedAt.IsZero() {
		if old, ok := s.accounts[a.ID]; ok {
			a.CreatedAt = old.CreatedAt
		} else {
			a.CreatedAt = now
		}
	}
	a.UpdatedAt = now
	s.accounts[a.ID] = a
	if s.settings.ActiveAccountID == "" {
		s.settings.ActiveAccountID = a.ID
		_ = s.saveSettingsLocked()
	}
	return s.saveAccountLocked(a)
}

func (s *Store) RemoveAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, id)
	_ = os.Remove(s.accountPath(id))
	if s.settings.ActiveAccountID == id {
		s.settings.ActiveAccountID = ""
		for k := range s.accounts {
			s.settings.ActiveAccountID = k
			break
		}
		_ = s.saveSettingsLocked()
	}
	delete(s.usage, id)
	_ = s.saveUsageLocked()
	return nil
}

func (s *Store) SetActiveAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	s.settings.ActiveAccountID = id
	return s.saveSettingsLocked()
}

func (s *Store) MarkAccountExhausted(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	a.Exhausted = true
	a.ExhaustedAt = time.Now().UTC()
	a.UpdatedAt = time.Now().UTC()
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

func (s *Store) MarkAccountChatDenied(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	a.ChatDenied = true
	a.ChatDeniedAt = time.Now().UTC()
	if reason != "" {
		if len(reason) > 400 {
			reason = reason[:400]
		}
		a.ChatDeniedReason = reason
	} else {
		a.ChatDeniedReason = "Access to the chat endpoint is denied"
	}
	a.UpdatedAt = time.Now().UTC()
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

// ResetAccountExhausted clears rate-limit exhaustion AND chat-denied flags.
func (s *Store) ResetAccountExhausted(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	a.Exhausted = false
	a.ExhaustedAt = time.Time{}
	a.ChatDenied = false
	a.ChatDeniedAt = time.Time{}
	a.ChatDeniedReason = ""
	a.UpdatedAt = time.Now().UTC()
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

func (s *Store) RecoverExhaustedAccounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, a := range s.accounts {
		if a.Exhausted && !a.IsExhausted() {
			a.Exhausted = false
			a.ExhaustedAt = time.Time{}
			a.UpdatedAt = time.Now().UTC()
			s.accounts[id] = a
			_ = s.saveAccountLocked(a)
		}
	}
}

func (s *Store) RecordRequest(sample RequestSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage == nil {
		s.usage = map[string]UsageTotals{}
	}
	add := func(key string) {
		u := s.usage[key]
		u.PromptTokens += sample.PromptTokens
		u.CompletionTokens += sample.CompletionTokens
		u.ReasoningTokens += sample.ReasoningTokens
		u.CachedTokens += sample.CachedTokens
		u.TotalTokens += sample.TotalTokens
		if u.TotalTokens == 0 {
			u.TotalTokens = sample.PromptTokens + sample.CompletionTokens
		}
		u.Requests++
		u.CostUSD += sample.CostUSD
		if sample.LatencyMs > 0 {
			u.LatencySumMs += sample.LatencyMs
			u.TTFTSumMs += sample.TTFTMs
			u.LatencySamples++
		}
		s.usage[key] = u
	}
	if sample.AccountID != "" {
		add(sample.AccountID)
	}
	add("_global")

	// newest first, cap 200
	s.history = append([]RequestSample{sample}, s.history...)
	const maxHist = 200
	if len(s.history) > maxHist {
		s.history = s.history[:maxHist]
	}
	if err := s.saveUsageLocked(); err != nil {
		return err
	}
	return s.saveHistoryLocked()
}

// Deprecated wrapper
func (s *Store) AddUsage(accountID string, prompt, completion, reasoning int64) error {
	return s.RecordRequest(RequestSample{
		AccountID:        accountID,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		ReasoningTokens:  reasoning,
		TotalTokens:      prompt + completion,
		At:               time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Store) UsageSnapshot() map[string]UsageTotals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]UsageTotals{}
	for k, v := range s.usage {
		out[k] = v
	}
	return out
}

func (s *Store) History(limit int) []RequestSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.history) {
		limit = len(s.history)
	}
	out := make([]RequestSample, limit)
	copy(out, s.history[:limit])
	return out
}
