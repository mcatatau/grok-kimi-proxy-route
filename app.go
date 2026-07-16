package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"grok-desktop/internal/kimi"
	"grok-desktop/internal/mcpconfig"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/pricing"
	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/signup"
	"grok-desktop/internal/skills"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

type App struct {
	ctx context.Context

	store    *store.Store
	oauth    *oauth.Client
	upstream *upstream.Client
	proxy    *proxyhttp.Server
	skills   *skills.Store
	mcp      *mcpconfig.Store

	mu            sync.Mutex
	deviceCancel  context.CancelFunc
	deviceState   *deviceLoginState
	reqCancel     context.CancelFunc
	activeReq     *ActiveRequest
	signupRunning bool
	signupCancel  context.CancelFunc
	autoCreate    bool

	// Per-account refresh serialization (refresh-token rotation is not concurrent-safe).
	refreshGates sync.Map // accountID → *sync.Mutex

	// Throttle CLI auth sync so Codex multi-request storms don't hammer disk every turn.
	lastCLISync   time.Time
	lastCLISyncMu sync.Mutex
}

type deviceLoginState struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
	StartedAt       string `json:"started_at"`
}

type ActiveRequest struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Label     string `json:"label"`
	Model     string `json:"model"`
	StartedAt string `json:"started_at"`
	Phase     string `json:"phase"` // thinking | content | idle
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	st, err := store.Open("")
	if err != nil {
		runtime.LogErrorf(ctx, "store open: %v", err)
		return
	}
	a.store = st
	a.oauth = oauth.New()
	// Align OAuth client surface version with the installed Grok CLI default.
	if v := st.Settings().ClientVersion; v != "" {
		a.oauth.CLIVersion = v
	}
	a.upstream = upstream.New()
	a.proxy = proxyhttp.New(st, a.upstream, a.ensureCreds)
	a.proxy.SetForceRefresh(a.forceRefreshAccount)
	a.proxy.SetQuotaHandler(func(accountID, reason string) bool {
		a.markAccountExhausted(accountID, reason)
		after, ok := a.store.ActiveAccount()
		return ok && after != nil && after.ID != accountID && after.Usable()
	})
	a.proxy.SetAuthFailHandler(func(accountID, reason string) bool {
		a.markAccountAuthDenied(accountID, reason)
		after, ok := a.store.ActiveAccount()
		return ok && after != nil && after.ID != accountID && after.Usable()
	})
	if sk, err := skills.Open(filepath.Join(st.Root(), "skills")); err == nil {
		a.skills = sk
	} else {
		runtime.LogErrorf(ctx, "skills: %v", err)
	}
	if mc, err := mcpconfig.Open(filepath.Join(st.Root(), "mcp_servers.json")); err == nil {
		a.mcp = mc
	} else {
		runtime.LogErrorf(ctx, "mcpconfig: %v", err)
	}

	settings := st.Settings()
	if settings.ProxyEnabled {
		listen := settings.ProxyListen
		if listen == "" {
			listen = "127.0.0.1:8787"
		}
		if err := a.proxy.Start(listen); err != nil {
			// Port busy (e.g. old Python proxy) — try fallback
			fallback := "127.0.0.1:8788"
			runtime.LogErrorf(ctx, "proxy start on %s failed: %v; trying %s", listen, err, fallback)
			if err2 := a.proxy.Start(fallback); err2 != nil {
				runtime.LogErrorf(ctx, "proxy fallback failed: %v", err2)
			} else {
				_ = a.store.UpdateSettings(func(s *store.Settings) { s.ProxyListen = fallback })
				runtime.LogInfof(ctx, "OpenAI proxy listening on http://%s", a.proxy.Addr())
			}
		} else {
			runtime.LogInfof(ctx, "OpenAI proxy listening on http://%s", a.proxy.Addr())
		}
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.proxy != nil {
		_ = a.proxy.Stop(context.Background())
	}
	a.mu.Lock()
	if a.deviceCancel != nil {
		a.deviceCancel()
	}
	if a.reqCancel != nil {
		a.reqCancel()
	}
	a.mu.Unlock()
}

// ---------- Public API for frontend ----------

func (a *App) GetBootstrap() map[string]any {
	if a.store == nil {
		return map[string]any{"error": "store not ready"}
	}
	s := a.store.Settings()
	active := map[string]any{}
	if s.IsOllie() {
		active = map[string]any{
			"id": "ollie", "email": "keyless@olliechat", "label": "OllieChat", "provider": store.ProviderOllie, "auth_mode": store.AuthModeAPIKey,
		}
	} else if s.IsGemini() {
		active = map[string]any{
			"id": "gemini-adc", "email": s.EffectiveGeminiProject(), "label": "Gemini ADC", "provider": store.ProviderGemini, "auth_mode": store.AuthModeAPIKey,
		}
	} else if acc, ok := a.store.ActiveAccount(); ok && acc != nil {
		active = map[string]any{
			"id": acc.ID, "email": acc.Email, "label": acc.Label, "expires_at": acc.ExpiresAt,
			"provider": acc.NormalizedProvider(), "auth_mode": s.ProviderAuthMode(),
		}
	}
	proxyAddr := ""
	if a.proxy != nil {
		proxyAddr = a.proxy.Addr()
	}
	dataDir := ""
	if a.store != nil {
		dataDir = a.store.Root()
	}
	skillsList := []skills.Skill{}
	if a.skills != nil {
		if list, err := a.skills.List(); err == nil {
			skillsList = list
		}
	}
	mcpList := []map[string]any{}
	if a.mcp != nil {
		mcpList = a.mcp.List(true)
	}
	return map[string]any{
		"settings":       s,
		"accounts":       a.store.PublicAccountsForProvider(s.NormalizedProvider()),
		"active":         active,
		"usage":          a.store.UsageSnapshot(),
		"proxy_addr":     proxyAddr,
		"proxy_base":     fmt.Sprintf("http://%s/v1", proxyAddr),
		"data_dir":       dataDir,
		"active_request": a.GetActiveRequest(),
		"skills":         skillsList,
		"mcp_servers":    mcpList,
		"providers":      store.ProviderCatalog(),
		"auth_mode":      s.ProviderAuthMode(),
		"provider":       s.NormalizedProvider(),
		"endpoints": []string{
			"/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/search",
		},
	}
}

func (a *App) GetSettings() store.Settings {
	if a.store == nil {
		return store.Settings{}
	}
	return a.store.Settings()
}

func (a *App) UpdateSettings(patch map[string]any) (store.Settings, error) {
	err := a.store.UpdateSettings(func(s *store.Settings) {
		if v, ok := patch["provider"].(string); ok && v != "" {
			// Full switch: resets model + upstream for that provider (no leftover ids).
			s.ApplyProviderDefaults(v)
		}
		if v, ok := patch["default_model"].(string); ok && v != "" {
			s.DefaultModel = v
		}
		if v, ok := patch["gemini_project"].(string); ok && v != "" {
			s.GeminiProject = strings.TrimSpace(v)
		}
		if v, ok := patch["gemini_location"].(string); ok && v != "" {
			s.GeminiLocation = strings.TrimSpace(v)
		}
		if v, ok := patch["reasoning_effort"].(string); ok && v != "" {
			s.ReasoningEffort = v
		}
		if v, ok := patch["api_mode"].(string); ok && v != "" {
			s.APIMode = v
		}
		if v, ok := patch["upstream_base"].(string); ok && v != "" {
			s.UpstreamBase = v
		}
		if v, ok := patch["client_version"].(string); ok && v != "" {
			s.ClientVersion = v
		}
		if v, ok := patch["proxy_listen"].(string); ok && v != "" {
			s.ProxyListen = v
		}
		if v, ok := patch["proxy_enabled"].(bool); ok {
			s.ProxyEnabled = v
		}
		if v, ok := patch["proxy_api_key"].(string); ok {
			s.ProxyAPIKey = v
		}
		if v, ok := patch["store_responses"].(bool); ok {
			s.StoreResponses = v
		}
		if v, ok := patch["force_default_model"].(bool); ok {
			s.ForceDefaultModel = &v
		}
		// Keep provider/upstream/model coherent after any patch.
		if s.IsOllie() && (s.UpstreamBase == "" || strings.Contains(s.UpstreamBase, "cli-chat-proxy")) {
			s.UpstreamBase = store.OllieUpstream
		}
		if s.IsXAI() && strings.Contains(strings.ToLower(s.UpstreamBase), "olliechat") {
			s.UpstreamBase = store.DefaultUpstream
		}
		s.SanitizeModelForProvider()
	})
	if err != nil {
		return store.Settings{}, err
	}
	// Only restart the HTTP listener when enable/listen address actually change.
	// Restarting on every model/provider tweak dropped in-flight Codex streams
	// and could leave the UI looking "dead".
	s := a.store.Settings()
	a.reconcileProxy(s)
	return s, nil
}

func (a *App) reconcileProxy(s store.Settings) {
	if a.proxy == nil {
		return
	}
	wantListen := s.ProxyListen
	if wantListen == "" {
		wantListen = "127.0.0.1:8787"
	}
	cur := a.proxy.Addr()
	if !s.ProxyEnabled {
		if cur != "" {
			_ = a.proxy.Stop(context.Background())
		}
		return
	}
	// Already listening on the desired host:port — keep connections.
	if cur != "" && (cur == wantListen || "http://"+cur == wantListen) {
		return
	}
	// Addr() may be host:port without scheme; also accept resolved form.
	if cur != "" {
		// If only model/provider changed, Start() is a no-op while srv != nil.
		// Only bounce when the listen target differs.
		if normalizeListen(cur) == normalizeListen(wantListen) {
			return
		}
		_ = a.proxy.Stop(context.Background())
	}
	if err := a.proxy.Start(wantListen); err != nil {
		fallback := "127.0.0.1:8788"
		if err2 := a.proxy.Start(fallback); err2 == nil {
			_ = a.store.UpdateSettings(func(st *store.Settings) { st.ProxyListen = fallback })
		}
	}
}

func normalizeListen(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	return s
}

func (a *App) ListAccounts() []map[string]any {
	if a.store == nil {
		return nil
	}
	return a.store.PublicAccountsForProvider(a.store.Settings().NormalizedProvider())
}

// ListAccountsForProvider returns accounts for a specific provider (UI modal).
func (a *App) ListAccountsForProvider(provider string) []map[string]any {
	if a.store == nil {
		return nil
	}
	return a.store.PublicAccountsForProvider(provider)
}

// ListProviders returns the multi-route provider catalog (auth vs api_key).
func (a *App) ListProviders() []map[string]any {
	return store.ProviderCatalog()
}

func (a *App) SetActiveAccount(id string) error {
	return a.store.SetActiveAccount(id)
}

func (a *App) RemoveAccount(id string) error {
	return a.store.RemoveAccount(id)
}

func (a *App) RenameAccount(id, label string) error {
	acc, ok := a.store.GetAccount(id)
	if !ok {
		return fmt.Errorf("account not found")
	}
	acc.Label = label
	return a.store.UpsertAccount(*acc)
}

// StartKimiBrowserLogin mirrors Kimi Desktop Google login:
// open USER's default browser → Google OAuth loopback → POST /api/auth/login/google → mint sk-kimi.
// Does NOT use Playwright and does NOT read Kimi Desktop APPDATA.
func (a *App) StartKimiBrowserLogin() (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	a.safeEmit("kimi:login", map[string]any{
		"phase":   "browser",
		"message": "Abrindo o navegador do sistema (Google)…",
	})
	gl, err := kimi.LoginWithGoogleBrowser(5 * time.Minute)
	if err != nil {
		return nil, err
	}
	access := gl.AccessToken
	refresh := gl.RefreshToken
	if p, _ := kimi.DecodeJWT(access); p != nil && p.Exp > 0 && time.Until(time.Unix(p.Exp, 0)) < 2*time.Minute && refresh != "" {
		if renewed, rerr := kimi.RefreshAccessToken(refresh); rerr == nil && renewed != nil {
			access = renewed.AccessToken
			if renewed.RefreshToken != "" {
				refresh = renewed.RefreshToken
			}
		}
	}
	payload, _ := kimi.DecodeJWT(access)
	return a.addKimiSession(access, refresh, payload, "google_browser")
}

// AddKimiFromJWT mints/reuses sk-kimi from an explicit web access_token JWT (optional fallback).
func (a *App) AddKimiFromJWT(accessToken string) (map[string]any, error) {
	accessToken = strings.TrimSpace(accessToken)
	accessToken = strings.TrimPrefix(accessToken, "Bearer ")
	payload, err := kimi.DecodeJWT(accessToken)
	if err != nil {
		return nil, fmt.Errorf("jwt inválido: %w", err)
	}
	return a.addKimiSession(accessToken, "", payload, "paste_jwt")
}

// AddKimiAPIKey registers a pasted sk-kimi key (optional fallback).
func (a *App) AddKimiAPIKey(apiKey, label string) (map[string]any, error) {
	apiKey = strings.TrimSpace(apiKey)
	if !strings.HasPrefix(apiKey, "sk-kimi-") {
		return nil, fmt.Errorf("api key deve começar com sk-kimi-")
	}
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	id := "kimi-" + apiKey[len(apiKey)-8:]
	if label == "" {
		label = "Kimi Work"
	}
	acc := store.Account{
		ID:        id,
		Provider:  store.ProviderKimiWork,
		Label:     label,
		Email:     label,
		APIKey:    apiKey,
		Source:    "paste_key",
		ClientID:  "kimi-work",
		Issuer:    "https://agent-gw.kimi.com",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	_ = a.store.UpdateSettings(func(s *store.Settings) {
		if s.NormalizedProvider() != store.ProviderKimiWork {
			s.ApplyProviderDefaults(store.ProviderKimiWork)
		}
	})
	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, err
	}
	_ = a.store.SetActiveAccount(acc.ID)
	a.safeEmit("account:added", map[string]any{"id": acc.ID, "provider": store.ProviderKimiWork})
	return map[string]any{
		"id": acc.ID, "label": acc.Label, "provider": store.ProviderKimiWork, "source": acc.Source,
	}, nil
}

// kimiProjectRoot finds repo root that contains scripts/kimi-browser-login.mjs
func (a *App) kimiProjectRoot() (string, error) {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			dir,
			filepath.Join(dir, ".."),
			filepath.Join(dir, "..", ".."),
			filepath.Dir(filepath.Dir(dir)), // build/bin -> repo
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	candidates = append(candidates, `D:\grok proxy open ai compatible`)
	for _, c := range candidates {
		script := filepath.Join(c, "scripts", "kimi-browser-login.mjs")
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			return c, nil
		}
		// build/bin layout
		script = filepath.Join(c, "..", "..", "scripts", "kimi-browser-login.mjs")
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(filepath.Join(c, "..", ".."))
			return abs, nil
		}
	}
	return "", fmt.Errorf("scripts/kimi-browser-login.mjs não encontrado — rode a partir do repo GrokDesktop")
}

func (a *App) addKimiSession(accessToken, refreshToken string, payload *kimi.JWTPayload, source string) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	minted, err := kimi.MintWorkAPIKey(accessToken, "grok-desktop-kimi")
	if err != nil {
		return nil, err
	}
	userID := minted.UserID
	if userID == "" && payload != nil {
		userID = payload.Sub
	}
	id := "kimi-" + userID
	if userID == "" {
		id = "kimi-" + minted.APIKey[len(minted.APIKey)-8:]
	}
	label := "Kimi Work"
	if userID != "" {
		label = "Kimi · " + userID
		if len(userID) > 12 {
			label = "Kimi · " + userID[:8] + "…"
		}
	}
	// Preserve custom label / created on re-login same user
	if prev, ok := a.store.GetAccount(id); ok && prev != nil {
		if prev.Label != "" && prev.Label != prev.Email {
			label = prev.Label
		}
	}
	acc := store.Account{
		ID:           id,
		Provider:     store.ProviderKimiWork,
		Label:        label,
		Email:        userID,
		UserID:       userID,
		APIKey:       minted.APIKey,
		DeviceID:     minted.DeviceID,
		Source:       source,
		ClientID:     "kimi-work",
		Issuer:       "https://www.kimi.com",
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    minted.ExpiresAt,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_ = a.store.UpdateSettings(func(s *store.Settings) {
		if s.NormalizedProvider() != store.ProviderKimiWork {
			s.ApplyProviderDefaults(store.ProviderKimiWork)
		}
	})
	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, err
	}
	_ = a.store.SetActiveAccount(acc.ID)
	a.safeEmit("account:added", map[string]any{"id": acc.ID, "provider": store.ProviderKimiWork, "user_id": userID, "source": source})
	hint := minted.APIKey
	if len(hint) > 14 {
		hint = hint[:10] + "…" + hint[len(hint)-4:]
	}
	return map[string]any{
		"id": acc.ID, "label": acc.Label, "user_id": userID, "provider": store.ProviderKimiWork,
		"api_key_hint": hint, "source": source, "has_refresh": refreshToken != "",
	}, nil
}

// StartDeviceLogin begins OAuth device flow. Frontend shows URL + code.
func (a *App) StartDeviceLogin() (*deviceLoginState, error) {
	a.mu.Lock()
	if a.deviceCancel != nil {
		a.deviceCancel()
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
	a.deviceCancel = cancel
	a.mu.Unlock()

	start, err := a.oauth.StartDevice(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	url := start.VerificationURIComplete
	if url == "" {
		url = start.VerificationURI
	}
	st := &deviceLoginState{
		DeviceCode:      start.DeviceCode,
		UserCode:        start.UserCode,
		VerificationURL: url,
		Interval:        start.Interval,
		ExpiresIn:       start.ExpiresIn,
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	a.mu.Lock()
	a.deviceState = st
	a.mu.Unlock()

	// background poll
	go func() {
		tok, err := a.oauth.PollDevice(ctx, start.DeviceCode, start.Interval)
		if err != nil {
			if ctx.Err() == nil {
				runtime.EventsEmit(a.ctx, "auth:error", err.Error())
			}
			return
		}
		acc := oauth.AccountFromToken(tok, a.oauth.ClientID, a.oauth.Issuer)
		email, uid := a.oauth.UserInfo(context.Background(), tok.AccessToken, a.oauth.Issuer)
		if email != "" {
			acc.Email = email
		}
		if uid != "" {
			acc.UserID = uid
			acc.ID = uid
		}
		// Re-login same xAI user → refresh tokens; keep custom label if any.
		if prev, ok := a.store.GetAccount(acc.ID); ok && prev != nil {
			lowLabel := strings.ToLower(prev.Label)
			if prev.Label != "" && prev.Label != prev.Email && prev.Label != "Grok account" &&
				!strings.Contains(lowLabel, "esgotada") && !strings.Contains(lowLabel, "auth-denied") {
				acc.Label = prev.Label
			}
			acc.CreatedAt = prev.CreatedAt
			// Fresh OAuth after re-auth clears quota / auth-denied marks.
			acc.ExhaustedAt = time.Time{}
			acc.ExhaustReason = ""
			acc.AuthDeniedAt = time.Time{}
			acc.AuthDeniedReason = ""
		}
		if acc.Label == "" || acc.Label == "Grok account" {
			if acc.Email != "" {
				acc.Label = acc.Email
			} else if len(acc.ID) >= 8 {
				acc.Label = "Conta " + acc.ID[:8]
			} else {
				acc.Label = "Conta"
			}
		}
		acc.Provider = store.ProviderXAI
		acc.Source = "oauth"
		if err := a.store.UpsertAccount(acc); err != nil {
			runtime.EventsEmit(a.ctx, "auth:error", err.Error())
			return
		}
		// New account (or re-auth) becomes the active one for the next request.
		_ = a.store.SetActiveAccount(acc.ID)
		runtime.EventsEmit(a.ctx, "auth:success", map[string]any{
			"id":       acc.ID,
			"email":    acc.Email,
			"label":    acc.Label,
			"provider": store.ProviderXAI,
			"accounts": a.store.PublicAccountsForProvider(store.ProviderXAI),
			"count":    len(a.store.ListAccountsForProvider(store.ProviderXAI)),
		})
		a.mu.Lock()
		a.deviceState = nil
		a.mu.Unlock()
	}()

	return st, nil
}

func (a *App) CancelDeviceLogin() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deviceCancel != nil {
		a.deviceCancel()
		a.deviceCancel = nil
	}
	a.deviceState = nil
}

func (a *App) GetDeviceLoginState() *deviceLoginState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deviceState
}

func (a *App) OpenExternal(url string) {
	runtime.BrowserOpenURL(a.ctx, url)
}

func (a *App) ListModels() ([]upstream.ModelInfo, error) {
	settings := a.store.Settings()
	if settings.IsKimiWork() {
		out := make([]upstream.ModelInfo, 0)
		for _, m := range kimi.StaticModels() {
			out = append(out, upstream.ModelInfo{
				ID: m["id"], Name: m["name"], Description: "Kimi Work · coding", APIMode: m["api_mode"],
			})
		}
		return out, nil
	}
	token, _, settings, err := a.ensureCreds(a.ctx)
	if err != nil {
		return nil, err
	}
	return a.upstream.ListModels(a.ctx, token, settings)
}

func (a *App) GetUsage() map[string]store.UsageTotals {
	if a.store == nil {
		return map[string]store.UsageTotals{}
	}
	return a.store.UsageSnapshot()
}

// GetStats returns usage totals, latency history, pricing and OpenAI-compatible config.
func (a *App) GetStats() map[string]any {
	if a.store == nil {
		return map[string]any{"error": "store not ready"}
	}
	usage := a.store.UsageSnapshot()
	hist := a.store.History(60)
	g := usage["_global"]
	avgLat, avgTTFT := int64(0), int64(0)
	if g.LatencySamples > 0 {
		avgLat = g.LatencySumMs / g.LatencySamples
		avgTTFT = g.TTFTSumMs / g.LatencySamples
	}

	// sparkline series (oldest → newest for chart L→R)
	latSeries := make([]int64, 0, len(hist))
	ttftSeries := make([]int64, 0, len(hist))
	costSeries := make([]float64, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		latSeries = append(latSeries, h.LatencyMs)
		ttftSeries = append(ttftSeries, h.TTFTMs)
		costSeries = append(costSeries, h.CostUSD)
	}

	proxyAddr := ""
	if a.proxy != nil {
		proxyAddr = a.proxy.Addr()
	}
	if proxyAddr == "" {
		proxyAddr = a.store.Settings().ProxyListen
	}
	baseURL := fmt.Sprintf("http://%s/v1", proxyAddr)
	settings := a.store.Settings()
	apiKey := settings.ProxyAPIKey
	if apiKey == "" {
		if settings.IsOllie() {
			apiKey = "ollie"
		} else {
			apiKey = "grok-desktop" // placeholder for clients that require a key
		}
	}
	// Clients should pin model=default; proxy routes to settings.DefaultModel.
	model := "default"
	resolved := settings.ResolveModel(model)
	providerName := "Grok Desktop"
	if settings.IsOllie() {
		providerName = "Grok Desktop · OllieChat"
	}

	// Open Code / Continue / Cursor style snippets
	openCodeJSON := fmt.Sprintf(`{
  "provider": {
    "grok-desktop": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "%s",
      "options": {
        "baseURL": "%s",
        "apiKey": "%s"
      },
      "models": {
        "default": {
          "name": "proxy global → %s"
        }
      }
    }
  }
}`, providerName, baseURL, apiKey, resolved)

	openaiEnv := fmt.Sprintf(`OPENAI_BASE_URL=%s
OPENAI_API_KEY=%s
OPENAI_MODEL=default`, baseURL, apiKey)

	curlExample := fmt.Sprintf(`curl %s/chat/completions \
  -H "Authorization: Bearer %s" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"default\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}"`,
		baseURL, apiKey)

	rate := pricing.RateFor(resolved)

	return map[string]any{
		"usage":            usage,
		"global":           g,
		"history":          hist,
		"latency_series":   latSeries,
		"ttft_series":      ttftSeries,
		"cost_series":      costSeries,
		"avg_latency_ms":   avgLat,
		"avg_ttft_ms":      avgTTFT,
		"pricing":          pricing.AllRates(),
		"active_rate":      rate,
		"proxy": map[string]any{
			"base_url":         baseURL,
			"api_key":          apiKey,
			"model":            model,
			"resolved_model":   resolved,
			"force_default":    settings.ForceModel(),
			"openai_env":       openaiEnv,
			"opencode":         openCodeJSON,
			"curl":             curlExample,
		},
		"data_dir": a.store.Root(),
	}
}

func (a *App) GetActiveRequest() *ActiveRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeReq == nil {
		return nil
	}
	cp := *a.activeReq
	return &cp
}

func (a *App) CancelChat() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reqCancel != nil {
		a.reqCancel()
		a.reqCancel = nil
	}
	a.activeReq = nil
	runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
}

// SendChat streams a completion. Events: chat:event
func (a *App) SendChat(req upstream.ChatRequest) error {
	token, acc, settings, err := a.ensureCreds(a.ctx)
	if err != nil {
		return err
	}
	if req.Model == "" {
		req.Model = settings.DefaultModel
	}
	if req.ReasoningEffort == "" {
		req.ReasoningEffort = settings.ReasoningEffort
	}
	if req.APIMode == "" {
		req.APIMode = settings.APIMode
	}
	// Kimi Work agent-gw has no /responses — always chat/completions.
	if settings.IsKimiWork() {
		req.APIMode = "chat"
		req.Model = kimi.ResolveKimiModel(req.Model)
		if req.Model == "" || req.Model == "default" {
			req.Model = store.KimiWorkDefaultModel
		}
	}

	a.mu.Lock()
	if a.reqCancel != nil {
		a.reqCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.reqCancel = cancel
	reqID := uuid.NewString()
	label := acc.Label
	if label == "" {
		label = acc.Email
	}
	a.activeReq = &ActiveRequest{
		ID:        reqID,
		AccountID: acc.ID,
		Email:     acc.Email,
		Label:     label,
		Model:     req.Model,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Phase:     "thinking",
	}
	a.mu.Unlock()
	runtime.EventsEmit(a.ctx, "request:active", a.GetActiveRequest())

	go func() {
		defer func() {
			a.mu.Lock()
			a.reqCancel = nil
			a.activeReq = nil
			a.mu.Unlock()
			runtime.EventsEmit(a.ctx, "request:active", nil)
		}()

		emit := func(ev upstream.StreamEvent) {
			switch ev.Type {
			case "thinking":
				a.setPhase("thinking")
			case "content":
				a.setPhase("content")
			case "tool_call", "search_query", "search_results":
				a.setPhase("searching")
			}
			if ev.Account == "" {
				ev.Account = label
			}
			if ev.Email == "" {
				ev.Email = acc.Email
			}
			// Fan-out search UI events (native xAI web_search / x_search)
			switch ev.Type {
			case "search_query":
				payload := map[string]any{
					"query": ev.Text, "provider": "xAI", "tool_call_id": ev.ID,
				}
				if ev.Payload != nil {
					for k, v := range ev.Payload {
						payload[k] = v
					}
				}
				runtime.EventsEmit(a.ctx, "search:start", payload)
			case "search_results":
				payload := map[string]any{"query": ev.Text, "provider": "xAI", "tool_call_id": ev.ID}
				if ev.Payload != nil {
					for k, v := range ev.Payload {
						payload[k] = v
					}
				}
				runtime.EventsEmit(a.ctx, "search:results", payload)
				runtime.EventsEmit(a.ctx, "search:done", map[string]any{"query": ev.Text, "tool_call_id": ev.ID})
			case "tool_call":
				runtime.EventsEmit(a.ctx, "tool:call", map[string]any{
					"id": ev.ID, "name": ev.Text, "payload": ev.Payload,
				})
			case "tool_done":
				runtime.EventsEmit(a.ctx, "tool:done", map[string]any{
					"id": ev.ID, "name": ev.Text, "payload": ev.Payload,
				})
			case "tool_error":
				runtime.EventsEmit(a.ctx, "search:error", map[string]any{
					"error": ev.Error, "tool_call_id": ev.ID,
				})
			}
			runtime.EventsEmit(a.ctx, "chat:event", ev)
			if ev.Type == "usage" && ev.Usage != nil {
				model := ev.Model
				if model == "" {
					model = req.Model
				}
				cost := pricing.CostUSD(model, ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.ReasoningTokens, ev.Usage.CachedTokens)
				total := ev.Usage.TotalTokens
				if total == 0 {
					total = ev.Usage.PromptTokens + ev.Usage.CompletionTokens
				}
				sample := store.RequestSample{
					ID:               ev.ID,
					At:               time.Now().UTC().Format(time.RFC3339),
					AccountID:        acc.ID,
					Model:            model,
					PromptTokens:     ev.Usage.PromptTokens,
					CompletionTokens: ev.Usage.CompletionTokens,
					ReasoningTokens:  ev.Usage.ReasoningTokens,
					CachedTokens:     ev.Usage.CachedTokens,
					TotalTokens:      total,
					CostUSD:          cost,
					LatencyMs:        ev.LatencyMs,
					TTFTMs:           ev.TTFTMs,
					Estimated:        ev.Estimated,
				}
				_ = a.store.RecordRequest(sample)
				runtime.EventsEmit(a.ctx, "usage:update", a.store.UsageSnapshot())
				runtime.EventsEmit(a.ctx, "stats:sample", sample)
			}
		}

		if req.ReasoningEffort == "" {
			req.ReasoningEffort = settings.ReasoningEffort
		}
		// xAI: Responses + native search.
		// Kimi Work / Ollie / Gemini: OpenAI chat/completions only (agent-gw has no /responses).
		if settings.IsOllie() || settings.IsGemini() || settings.IsKimiWork() {
			req.APIMode = "chat"
		} else {
			req.APIMode = "responses"
		}
		// Kimi wire model id
		if settings.IsKimiWork() {
			req.Model = kimi.ResolveKimiModel(req.Model)
			if req.Model == "" {
				req.Model = store.KimiWorkDefaultModel
			}
		}

		// Inject skills + MCP catalog into conversation context.
		req.Messages = a.injectAgentContext(req.Messages)

		err := a.upstream.StreamChat(ctx, token, settings, label, acc.Email, req, emit)
		if err != nil && ctx.Err() == nil {
			// Gemini uses ADC (no multi-account rotate).
			if settings.IsGemini() {
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: err.Error()})
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
				return
			}
			// Auth failure: force-refresh once, then rotate.
			if isAuthDeniedErr(err) && acc != nil {
				if tokR, accR, settingsR, errR := a.forceRefreshAccount(ctx, acc.ID); errR == nil && tokR != "" && tokR != token {
					err = a.upstream.StreamChat(ctx, tokR, settingsR, label, accR.Email, req, emit)
					if err == nil {
						return
					}
					token, acc, settings = tokR, accR, settingsR
				}
				if err != nil && isAuthDeniedErr(err) {
					a.markAccountAuthDenied(acc.ID, err.Error())
					if nextID := a.pickNonExhausted(acc.ID); nextID != "" {
						if tok2, acc2, settings2, err2 := a.ensureCredsFor(ctx, nextID); err2 == nil {
							label2 := acc2.Label
							if label2 == "" {
								label2 = acc2.Email
							}
							runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{
								Type: "content",
								Text: "\n\n_Auth negada — trocando para " + label2 + "…_\n\n",
							})
							err = a.upstream.StreamChat(ctx, tok2, settings2, label2, acc2.Email, req, emit)
							if err == nil {
								return
							}
						}
					}
				}
			}
			if isQuotaExhaustedErr(err) {
				a.markAccountExhausted(acc.ID, err.Error())
				if nextID := a.pickNonExhausted(acc.ID); nextID != "" {
					if tok2, acc2, settings2, err2 := a.ensureCredsFor(ctx, nextID); err2 == nil {
						label2 := acc2.Label
						if label2 == "" {
							label2 = acc2.Email
						}
						runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{
							Type: "content",
							Text: "\n\n_Conta esgotada — trocando para " + label2 + "…_\n\n",
						})
						err = a.upstream.StreamChat(ctx, tok2, settings2, label2, acc2.Email, req, emit)
						if err == nil {
							return
						}
						if isQuotaExhaustedErr(err) {
							a.markAccountExhausted(acc2.ID, err.Error())
						}
					}
				}
			}
			runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: err.Error()})
			runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
		}
	}()
	return nil
}

func (a *App) setPhase(phase string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeReq != nil {
		a.activeReq.Phase = phase
		cp := *a.activeReq
		runtime.EventsEmit(a.ctx, "request:active", &cp)
	}
}

// safeEmit never lets a Wails/webview emit failure take down the process mid-proxy request.
func (a *App) safeEmit(event string, data any) {
	if a == nil || a.ctx == nil {
		return
	}
	defer func() { _ = recover() }()
	runtime.EventsEmit(a.ctx, event, data)
}

func (a *App) ensureCreds(ctx context.Context) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, "", false)
}

// forceRefreshAccount re-syncs CLI tokens and forces OAuth refresh for the given account.
func (a *App) forceRefreshAccount(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, accountID, true)
}

func (a *App) ensureCredsInner(ctx context.Context, preferID string, forceRefresh bool) (string, *store.Account, store.Settings, error) {
	if a.store == nil {
		return "", nil, store.Settings{}, fmt.Errorf("store not ready")
	}
	settings := a.store.Settings()
	// OllieChat is keyless — no xAI OAuth account required.
	if settings.IsOllie() {
		acc := &store.Account{
			ID:          "ollie",
			Provider:    store.ProviderOllie,
			Label:       "OllieChat",
			Email:       "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
			ClientID:    "ollie",
			Issuer:      "https://olliechat.vercel.app",
		}
		return store.OllieAPIKey, acc, settings, nil
	}
	// Gemini uses Application Default Credentials (no xAI account).
	if settings.IsGemini() {
		acc := &store.Account{
			ID:          "gemini-adc",
			Provider:    store.ProviderGemini,
			Label:       "Gemini (ADC)",
			Email:       settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
			ClientID:    "google-adc",
			Issuer:      "https://accounts.google.com",
		}
		return store.GeminiCredMarker, acc, settings, nil
	}
	// Kimi Work: multi-account pool of sk-kimi keys (session auth mode).
	if settings.IsKimiWork() {
		var acc *store.Account
		var ok bool
		if preferID != "" {
			acc, ok = a.store.GetAccount(preferID)
			if ok && acc != nil && acc.NormalizedProvider() != store.ProviderKimiWork {
				ok = false
			}
			if ok && acc != nil && (acc.Exhausted() || acc.AuthDenied()) && !forceRefresh {
				ok = false
			}
		}
		if !ok || acc == nil {
			acc, ok = a.store.PreferUsableAccount()
		}
		if !ok || acc == nil || acc.NormalizedProvider() != store.ProviderKimiWork {
			return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work — use Adicionar (Desktop / JWT / sk-kimi)")
		}
		tok := acc.BearerToken()
		if tok == "" {
			return "", nil, settings, fmt.Errorf("conta Kimi sem sk-kimi")
		}
		if settings.ActiveAccountID != acc.ID {
			_ = a.store.SetActiveAccount(acc.ID)
			settings = a.store.Settings()
		}
		return tok, acc, settings, nil
	}
	// Sync CLI tokens at most once per 30s — Codex fires many parallel /v1 calls.
	a.lastCLISyncMu.Lock()
	doSync := time.Since(a.lastCLISync) > 30*time.Second
	if doSync {
		a.lastCLISync = time.Now()
	}
	a.lastCLISyncMu.Unlock()
	if doSync {
		if n, err := a.store.SyncFromGrokCLI(); err == nil && n > 0 {
			_ = a.store.PreferHealthyActive()
			a.safeEmit("account:cli_synced", map[string]any{"updated": n})
		}
	}
	settings = a.store.Settings()

	var acc *store.Account
	var ok bool
	if preferID != "" {
		acc, ok = a.store.GetAccount(preferID)
		if ok && acc != nil && (acc.Exhausted() || acc.AuthDenied()) && !forceRefresh {
			ok = false
		}
	}
	if !ok || acc == nil {
		// Prefer a still-usable active account; if active is exhausted/auth-denied, rotate.
		acc, ok = a.store.PreferUsableAccount()
	}
	if !ok || acc == nil {
		if raw, has := a.store.ActiveAccount(); has && raw != nil && (raw.Exhausted() || raw.AuthDenied()) {
			a.maybeAutoCreate(firstNonEmpty(raw.ExhaustReason, raw.AuthDeniedReason))
			return "", nil, settings, fmt.Errorf("todas as contas esgotadas ou bloqueadas — adicione conta ou relogue")
		}
		return "", nil, settings, fmt.Errorf("nenhuma conta — adicione uma conta em Contas")
	}

	// Skip JWT bot-flagged tokens (chat endpoint returns 403 permission-denied).
	if oauth.BotFlagged(acc.AccessToken) {
		_, _ = a.store.MarkAuthDenied(acc.ID, "bot_flag_source present in access token JWT")
		if nextID := a.store.NextUsableAccountID(acc.ID); nextID != "" && nextID != acc.ID {
			return a.ensureCredsFor(ctx, nextID)
		}
		return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag) — relogue ou use outra conta")
	}

	// Keep settings.active in sync when we auto-skipped a dead one.
	if settings.ActiveAccountID != acc.ID {
		_ = a.store.SetActiveAccount(acc.ID)
		a.safeEmit("account:rotated", map[string]any{"id": acc.ID, "reason": "skip_unusable"})
		settings = a.store.Settings()
	}

	needRefresh := forceRefresh || acc.ExpiresSoon(5*time.Minute) || acc.Expired()
	if needRefresh && acc.RefreshToken != "" {
		if err := a.refreshAccountLocked(ctx, acc); err != nil {
			if forceRefresh || acc.Expired() {
				if nextID := a.store.NextUsableAccountID(acc.ID); nextID != "" && nextID != acc.ID {
					return a.ensureCredsFor(ctx, nextID)
				}
				return "", nil, settings, fmt.Errorf("token expirado — faça login de novo: %v", err)
			}
			// Soft failure near expiry: keep current token only if not fully expired.
		} else {
			// Re-check bot flag after refresh.
			if oauth.BotFlagged(acc.AccessToken) {
				_, _ = a.store.MarkAuthDenied(acc.ID, "bot_flag_source present after refresh")
				if nextID := a.store.NextUsableAccountID(acc.ID); nextID != "" && nextID != acc.ID {
					return a.ensureCredsFor(ctx, nextID)
				}
				return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag) — relogue ou use outra conta")
			}
		}
	}
	if acc.AccessToken == "" {
		return "", nil, settings, fmt.Errorf("conta sem access_token")
	}
	if acc.Expired() && !forceRefresh {
		// Last resort: try refresh already done; still expired → hard fail / rotate.
		if nextID := a.store.NextUsableAccountID(acc.ID); nextID != "" && nextID != acc.ID {
			return a.ensureCredsFor(ctx, nextID)
		}
		return "", nil, settings, fmt.Errorf("token expirado — faça login de novo")
	}
	return acc.AccessToken, acc, settings, nil
}

func (a *App) accountRefreshMu(id string) *sync.Mutex {
	v, _ := a.refreshGates.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// refreshAccountLocked performs OAuth refresh with per-account mutex and persists tokens.
func (a *App) refreshAccountLocked(ctx context.Context, acc *store.Account) error {
	if acc == nil || acc.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	mu := a.accountRefreshMu(acc.ID)
	mu.Lock()
	defer mu.Unlock()
	// Re-read after lock — another goroutine may have refreshed already.
	if latest, ok := a.store.GetAccount(acc.ID); ok && latest != nil {
		if !latest.ExpiresSoon(5*time.Minute) && latest.AccessToken != "" && latest.AccessToken != acc.AccessToken {
			*acc = *latest
			return nil
		}
		acc.RefreshToken = latest.RefreshToken
		acc.AccessToken = latest.AccessToken
		acc.ExpiresAt = latest.ExpiresAt
	}
	tok, err := a.oauth.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
	if err != nil {
		return err
	}
	acc.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		acc.RefreshToken = tok.RefreshToken
	}
	claims := oauth.ParseAccessClaims(tok.AccessToken)
	if !claims.Exp.IsZero() {
		acc.ExpiresAt = claims.Exp
	} else {
		acc.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	acc.UpdatedAt = time.Now().UTC()
	// Successful refresh clears prior auth denial (quota left alone).
	acc.AuthDeniedAt = time.Time{}
	acc.AuthDeniedReason = ""
	if err := a.store.UpsertAccount(*acc); err != nil {
		return err
	}
	_ = a.store.ClearAuthDenied(acc.ID)
	return nil
}

func (a *App) ensureCredsFor(ctx context.Context, id string) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, id, false)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// StartAutoSignup creates a web account via chrome isolate + darkemail, then starts device OAuth.
func (a *App) StartAutoSignup() error {
	a.mu.Lock()
	if a.signupRunning {
		a.mu.Unlock()
		return fmt.Errorf("signup já em andamento")
	}
	a.signupRunning = true
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Minute)
	a.signupCancel = cancel
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.signupRunning = false
			a.signupCancel = nil
			a.mu.Unlock()
		}()
		emit := func(msg string) {
			runtime.EventsEmit(a.ctx, "signup:progress", map[string]any{"message": msg})
		}
		emit("iniciando criação automática…")
		res, err := signup.RunFullCapture(ctx, "", "", "", "Alex", "Rivera", false, emit)
		if err != nil {
			runtime.EventsEmit(a.ctx, "signup:error", err.Error())
			return
		}
		email, password, userID := "", "", ""
		if res != nil {
			email = res.Email
			password = res.Password
			userID = signup.ExtractUserIDFromCookies(res.Cookies)
			if userID == "" {
				userID = signup.ExtractUserIDFromCookies(res.Credentials.Cookie)
			}
		}
		emit("conta web criada: " + email)
		href := ""
		if res != nil {
			href = res.FinalUI.Href
		}
		runtime.EventsEmit(a.ctx, "signup:web_ok", map[string]any{
			"email": email, "password": password, "user_id": userID, "href": href,
		})
		emit("iniciando device OAuth para tokens da API…")
		st, err := a.StartDeviceLogin()
		if err != nil {
			runtime.EventsEmit(a.ctx, "signup:error", "web ok, device login falhou: "+err.Error())
			return
		}
		runtime.EventsEmit(a.ctx, "signup:device", map[string]any{
			"email": email, "password": password,
			"user_code": st.UserCode, "verification_url": st.VerificationURL,
		})
		runtime.EventsEmit(a.ctx, "signup:done", map[string]any{
			"email": email, "password": password, "phase": "waiting_device_oauth",
		})
	}()
	return nil
}

func (a *App) CancelAutoSignup() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signupCancel != nil {
		a.signupCancel()
		a.signupCancel = nil
	}
	a.signupRunning = false
}

func (a *App) IsSignupRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.signupRunning
}

func (a *App) SetAutoCreateOnExhausted(enabled bool) {
	a.mu.Lock()
	a.autoCreate = enabled
	a.mu.Unlock()
}

func (a *App) GetAutoCreateOnExhausted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoCreate
}

func (a *App) markAccountExhausted(id, reason string) {
	acc, err := a.store.MarkExhausted(id, reason)
	if err != nil || acc == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "account:exhausted", map[string]any{
		"id": id, "email": acc.Email, "reason": reason, "exhausted_at": acc.ExhaustedAt,
	})
	next := a.pickNonExhausted(id)
	if next != "" {
		_ = a.store.SetActiveAccount(next)
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": next, "reason": "quota"})
		return
	}
	a.maybeAutoCreate(reason)
}

func (a *App) markAccountAuthDenied(id, reason string) {
	acc, err := a.store.MarkAuthDenied(id, reason)
	if err != nil || acc == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "account:auth_denied", map[string]any{
		"id": id, "email": acc.Email, "reason": reason, "auth_denied_at": acc.AuthDeniedAt,
	})
	next := a.pickNonExhausted(id)
	if next != "" {
		_ = a.store.SetActiveAccount(next)
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": next, "reason": "auth_denied"})
		return
	}
	// Do not auto-create on auth denial by default — user should re-login the real account.
}

func (a *App) maybeAutoCreate(reason string) {
	a.mu.Lock()
	auto := a.autoCreate
	running := a.signupRunning
	a.mu.Unlock()
	if auto && !running {
		runtime.EventsEmit(a.ctx, "signup:auto_triggered", map[string]any{"reason": reason})
		_ = a.StartAutoSignup()
	}
}

func (a *App) pickNonExhausted(except string) string {
	if a.store == nil {
		return ""
	}
	return a.store.NextUsableAccountID(except)
}

func isQuotaExhaustedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "usage balance exhausted") || strings.Contains(s, "balance exhausted") {
		return true
	}
	if strings.Contains(s, "http 402") {
		return true
	}
	if strings.Contains(s, "http 429") && (strings.Contains(s, "exhausted") || strings.Contains(s, "quota") || strings.Contains(s, "balance")) {
		return true
	}
	// Some gateways only return the message without status in the error string.
	if strings.Contains(s, "quota") && (strings.Contains(s, "exceed") || strings.Contains(s, "exhaust")) {
		return true
	}
	return false
}

func isAuthDeniedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "permission-denied") || strings.Contains(s, "access to the chat endpoint is denied") {
		return true
	}
	if strings.Contains(s, "http 403") || strings.Contains(s, "status 403") {
		return true
	}
	if strings.Contains(s, "http 401") || strings.Contains(s, "invalid or expired credentials") {
		return true
	}
	if strings.Contains(s, "no auth context") {
		return true
	}
	return false
}

// SyncGrokCLI is exposed to the UI / selftest to pull tokens from ~/.grok/auth.json.
func (a *App) SyncGrokCLI() map[string]any {
	if a.store == nil {
		return map[string]any{"ok": false, "error": "store not ready"}
	}
	n, err := a.store.SyncFromGrokCLI()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	switched := a.store.PreferHealthyActive()
	acc, _ := a.store.ActiveAccount()
	email := ""
	id := ""
	if acc != nil {
		email = acc.Email
		id = acc.ID
	}
	return map[string]any{
		"ok": true, "updated": n, "switched_active": switched,
		"active_id": id, "active_email": email,
		"accounts": a.store.PublicAccounts(),
	}
}

func (a *App) injectAgentContext(msgs []upstream.ChatMessage) []upstream.ChatMessage {
	var blocks []string
	if a.skills != nil {
		if cat := a.skills.CatalogPrompt(); cat != "" {
			blocks = append(blocks, cat)
		}
	}
	if a.mcp != nil {
		if cat := a.mcp.CatalogPrompt(); cat != "" {
			blocks = append(blocks, cat)
		}
	}
	blocks = append(blocks, `Skills authoring: when the user asks you to create or update a skill, call the app capability by describing it clearly as:
CREATE_SKILL:
name: <id-or-title>
description: <one line>
---
<body markdown>
The desktop app can persist skills under AppData/skills. MCP servers with API keys (e.g. Context7) are configured in Settings → MCP with environment secrets.`)
	if len(blocks) == 0 {
		return msgs
	}
	extra := ""
	for i, b := range blocks {
		if i > 0 {
			extra += "\n\n"
		}
		extra += b
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		if !containsStr(msgs[0].Content, "Available local skills") && !containsStr(msgs[0].Content, "CREATE_SKILL:") {
			msgs[0].Content = msgs[0].Content + "\n\n" + extra
		}
		return msgs
	}
	return append([]upstream.ChatMessage{{Role: "system", Content: extra}}, msgs...)
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

// ---------- Skills ----------

func (a *App) ListSkills() ([]skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.List()
}

func (a *App) GetSkill(id string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.Get(id)
}

func (a *App) CreateSkill(name, description, body string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	sk, err := a.skills.Create(name, description, body)
	if err != nil {
		return nil, err
	}
	return sk, nil
}

func (a *App) UpdateSkill(id, name, description, body string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.Update(id, name, description, body)
}

func (a *App) DeleteSkill(id string) error {
	if a.skills == nil {
		return fmt.Errorf("skills store not ready")
	}
	return a.skills.Delete(id)
}

// ---------- MCP servers (with API key / SK support) ----------

func (a *App) ListMCPServers() []map[string]any {
	if a.mcp == nil {
		return nil
	}
	return a.mcp.List(true)
}

func (a *App) UpsertMCPServer(cfg map[string]any) (map[string]any, error) {
	if a.mcp == nil {
		return nil, fmt.Errorf("mcp store not ready")
	}
	sv := mcpconfig.Server{
		ID:      strMap(cfg, "id"),
		Name:    strMap(cfg, "name"),
		Type:    strMap(cfg, "type"),
		URL:     strMap(cfg, "url"),
		Enabled: true,
	}
	if v, ok := cfg["enabled"].(bool); ok {
		sv.Enabled = v
	}
	if v, ok := cfg["timeout_ms"].(float64); ok {
		sv.TimeoutMs = int(v)
	}
	if cmd, ok := cfg["command"].([]any); ok {
		for _, c := range cmd {
			if s, ok := c.(string); ok {
				sv.Command = append(sv.Command, s)
			}
		}
	}
	if env, ok := cfg["environment"].(map[string]any); ok {
		sv.Environment = map[string]string{}
		for k, v := range env {
			if s, ok := v.(string); ok {
				sv.Environment[k] = s
			}
		}
	}
	if hdr, ok := cfg["headers"].(map[string]any); ok {
		sv.Headers = map[string]string{}
		for k, v := range hdr {
			if s, ok := v.(string); ok {
				sv.Headers[k] = s
			}
		}
	}
	if sv.ID == "" {
		return nil, fmt.Errorf("id required")
	}
	if err := a.mcp.Upsert(sv); err != nil {
		return nil, err
	}
	got, _ := a.mcp.Get(sv.ID)
	return got.Public(true), nil
}

func (a *App) DeleteMCPServer(id string) error {
	if a.mcp == nil {
		return fmt.Errorf("mcp store not ready")
	}
	return a.mcp.Delete(id)
}

func strMap(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
