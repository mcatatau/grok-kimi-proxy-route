package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"grok-desktop/internal/mcpconfig"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/pricing"
	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/register"
	"grok-desktop/internal/skills"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

const DebugCreds = false // set to true to log account state on each ensureCreds

type App struct {
	ctx context.Context

	store    *store.Store
	oauth    *oauth.Client
	upstream *upstream.Client
	proxy    *proxyhttp.Server
	register *register.Runner
	skills   *skills.Store
	mcp      *mcpconfig.Store

	mu           sync.Mutex
	deviceCancel context.CancelFunc
	deviceState  *deviceLoginState
	reqCancel    context.CancelFunc
	activeReq    *ActiveRequest
	regCancel    context.CancelFunc
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

	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)

	st, err := store.Open("")
	if err != nil {
		runtime.LogErrorf(ctx, "store open: %v", err)
		return
	}
	a.store = st
	a.oauth = oauth.New()
	a.upstream = upstream.New()
	a.proxy = proxyhttp.New(st, a.upstream, a.ensureCreds, a.ImportSSO)
	a.proxy.OnUsage = func(sample store.RequestSample) {
		if a.ctx == nil {
			return
		}
		runtime.EventsEmit(a.ctx, "usage:update", a.store.UsageSnapshot())
		runtime.EventsEmit(a.ctx, "stats:sample", sample)
		a.emitAccountsUpdate()
	}
	a.proxy.OnAccountChange = func() {
		a.emitAccountsUpdate()
	}
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
	// Prefer embedded bot extracted under AppData; still allow sibling/monorepo override.
	var extractedBot string
	if register.HasEmbeddedBot() {
		if dest, err := register.ExtractEmbeddedBot(st.Root()); err != nil {
			runtime.LogErrorf(ctx, "register: extract embedded bot: %v", err)
		} else {
			extractedBot = dest
			runtime.LogInfof(ctx, "register: embedded bot extracted to %s (ver=%s)", dest, register.BotEmbedVersion())
		}
	}
	pyPath, botDir := resolveRegisterPaths(exeDir, settings, extractedBot)
	a.register = register.New(pyPath, botDir)
	a.register.CredsDir = st.Root()
	applyRegisterSettings(a.register, settings)
	runtime.LogInfof(ctx, "register: python=%s bot_dir=%s auto=%v", pyPath, botDir, settings.AutoRegisterEnabled)
	// Warm bot deps in background (venv + pip) so first register is faster.
	go func() {
		py, err := register.EnsureBotDeps(context.Background(), st.Root(), pyPath, botDir, nil)
		if err != nil {
			runtime.LogErrorf(ctx, "register: ensure deps: %v", err)
			return
		}
		if a.register != nil && py != "" {
			a.register.PythonPath = py
			runtime.LogInfof(ctx, "register: venv python=%s", py)
		}
	}()

	// Auto-watch APPDATA/sso-watch/ for SSO token files (E2E, zero config)
	ssoDir := filepath.Join(st.Root(), "sso-watch")
	if err := os.MkdirAll(ssoDir, 0o700); err == nil {
		go a.watchSSODir(ssoDir)
		runtime.LogInfof(ctx, "sso watch: %s", ssoDir)
	} else {
		runtime.LogErrorf(ctx, "sso watch mkdir: %v", err)
	}
	if settings.AutoRegisterEnabled {
		go a.autoRegisterLoop()
	}

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
	if a.regCancel != nil {
		a.regCancel()
	}
	a.mu.Unlock()
}

// ---------- Public API for frontend ----------

func (a *App) GetBootstrap() map[string]any {
	if a.store == nil {
		return map[string]any{"error": "store not ready"}
	}
	// Clear exhausted flags past 24h so UI badges match ensureCreds
	a.store.RecoverExhaustedAccounts()
	s := a.store.Settings()
	acc, _ := a.store.ActiveAccount()
	active := map[string]any{}
	if acc != nil {
		active = map[string]any{
			"id": acc.ID, "email": acc.Email, "label": acc.Label, "expires_at": acc.ExpiresAt,
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
		"accounts":       a.store.PublicAccounts(),
		"active":         active,
		"usage":          a.store.UsageSnapshot(),
		"proxy_addr":     proxyAddr,
		"proxy_base":     fmt.Sprintf("http://%s/v1", proxyAddr),
		"data_dir":       dataDir,
		"active_request": a.GetActiveRequest(),
		"skills":         skillsList,
		"mcp_servers":    mcpList,
		"endpoints": []string{
			"/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/messages",
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
		if v, ok := patch["default_model"].(string); ok && v != "" {
			s.DefaultModel = v
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
		if v, ok := patch["auto_register_enabled"].(bool); ok {
			s.AutoRegisterEnabled = v
		}
		if v, ok := patch["auto_register_min_active"].(float64); ok && int(v) > 0 {
			s.AutoRegisterMinActive = int(v)
		}
		if v, ok := patch["auto_register_max_active"].(float64); ok && int(v) > 0 {
			s.AutoRegisterMaxActive = int(v)
		}
		if v, ok := patch["python_path"].(string); ok {
			s.PythonPath = v
		}
		if v, ok := patch["bot_dir"].(string); ok {
			s.BotDir = v
		}
		if v, ok := patch["duckmail_url"].(string); ok {
			s.DuckMailURL = v
		}
		if v, ok := patch["duckmail_key"].(string); ok {
			s.DuckMailKey = v
		}
		if v, ok := patch["email_providers"].([]any); ok {
			var ps []string
			for _, x := range v {
				if s2, ok := x.(string); ok && s2 != "" {
					ps = append(ps, s2)
				}
			}
			s.EmailProviders = ps
		}
	})
	if err != nil {
		return store.Settings{}, err
	}
	// refresh register paths + restart proxy if needed
	s := a.store.Settings()
	if a.register != nil {
		exe, _ := os.Executable()
		var extracted string
		if register.HasEmbeddedBot() && a.store != nil {
			if dest, err := register.ExtractEmbeddedBot(a.store.Root()); err == nil {
				extracted = dest
			}
		}
		py, bot := resolveRegisterPaths(filepath.Dir(exe), s, extracted)
		a.register.PythonPath = py
		a.register.BotDir = bot
		applyRegisterSettings(a.register, s)
	}
	if s.ProxyEnabled {
		_ = a.proxy.Stop(context.Background())
		_ = a.proxy.Start(s.ProxyListen)
	} else {
		_ = a.proxy.Stop(context.Background())
	}
	return s, nil
}

func (a *App) ListAccounts() []map[string]any {
	if a.store == nil {
		return nil
	}
	return a.store.PublicAccounts()
}

func (a *App) emitAccountsUpdate() {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "accounts:update", a.store.PublicAccounts())
}

func (a *App) SetActiveAccount(id string) error {
	err := a.store.SetActiveAccount(id)
	if err == nil {
		a.emitAccountsUpdate()
	}
	return err
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

func (a *App) ResetAccount(id string) error {
	return a.store.ResetAccountExhausted(id)
}

func (a *App) RecoverAccounts() {
	a.store.RecoverExhaustedAccounts()
	a.emitAccountsUpdate()
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
			if prev.Label != "" && prev.Label != prev.Email && prev.Label != "Grok account" {
				acc.Label = prev.Label
			}
			acc.CreatedAt = prev.CreatedAt
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
			"accounts": a.store.PublicAccounts(),
			"count":    len(a.store.ListAccounts()),
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

// CancelRegisterBatch aborts CreateAccounts / CreateAccountFromDevice (bot + poll).
func (a *App) CancelRegisterBatch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.regCancel != nil {
		a.regCancel()
		a.regCancel = nil
	}
	log.Printf("CancelRegisterBatch: cancelled")
}

// ImportSSO imports an SSO token from grok-register as a new account.
func (a *App) ImportSSO(ssoToken string) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	if ssoToken = strings.TrimSpace(ssoToken); ssoToken == "" {
		return nil, fmt.Errorf("SSO token vazio")
	}

	teamID, userID := oauth.ClaimsFromAccess(ssoToken)
	email, uid := a.oauth.UserInfo(context.Background(), ssoToken, a.oauth.Issuer)
	if uid != "" {
		userID = uid
	}

	// Build account
	exp := time.Now().UTC().Add(90 * 24 * time.Hour) // SSO tokens are long-lived
	id := userID
	if id == "" {
		id = fmt.Sprintf("sso_%d", time.Now().UnixNano())
	}

	label := "SSO"
	if email != "" {
		label = email
	} else if len(id) >= 8 {
		label = "SSO " + id[:8]
	}

	acc := store.Account{
		ID:          id,
		Label:       label,
		Email:       email,
		AccessToken: ssoToken,
		ExpiresAt:   exp,
		ClientID:    a.oauth.ClientID,
		Issuer:      a.oauth.Issuer,
		TeamID:      teamID,
		UserID:      userID,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if prev, ok := a.store.GetAccount(acc.ID); ok && prev != nil {
		if prev.Label != "" && prev.Label != prev.Email && !strings.HasPrefix(prev.Label, "SSO") {
			acc.Label = prev.Label
		}
		acc.CreatedAt = prev.CreatedAt
	}

	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, fmt.Errorf("salvar conta: %w", err)
	}

	_ = a.store.SetActiveAccount(acc.ID)

	runtime.EventsEmit(a.ctx, "auth:success", map[string]any{
		"id":       acc.ID,
		"email":    acc.Email,
		"label":    acc.Label,
		"accounts": a.store.PublicAccounts(),
		"count":    len(a.store.ListAccounts()),
	})

	return map[string]any{
		"id":     acc.ID,
		"email":  acc.Email,
		"label":  acc.Label,
		"teamID": teamID,
	}, nil
}

// ImportSSOFromFile reads a file with SSO tokens (one per line) and imports each.
func (a *App) ImportSSOFromFile(filePath string) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("abrir arquivo: %w", err)
	}
	defer f.Close()

	var imported []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// Lines may be "email:password:SSO" (accounts.txt) or just "SSO" (grok.txt)
		token := line
		if parts := strings.SplitN(line, ":", 3); len(parts) == 3 && parts[2] != "" {
			token = parts[2]
		}
		acc, err := a.ImportSSO(token)
		if err != nil {
			runtime.LogErrorf(a.ctx, "import SSO from file: %v", err)
			continue
		}
		imported = append(imported, acc)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ler arquivo: %w", err)
	}

	runtime.EventsEmit(a.ctx, "sso:import-done", map[string]any{
		"imported": len(imported),
		"file":     filePath,
	})

	return map[string]any{
		"imported": len(imported),
		"file":     filePath,
	}, nil
}

// watchSSODir periodically scans a directory for new SSO token files and imports them.
func (a *App) watchSSODir(dir string) {
	// key = name + mtime so re-dropped / edited files reimport
	seen := map[string]bool{}
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s@%d", e.Name(), info.ModTime().UnixNano())
			if seen[key] {
				continue
			}
			fp := filepath.Join(dir, e.Name())
			result, err := a.ImportSSOFromFile(fp)
			if err != nil {
				runtime.LogErrorf(a.ctx, "sso watch import %s: %v", e.Name(), err)
			} else {
				n := result["imported"].(int)
				if n > 0 {
					runtime.EventsEmit(a.ctx, "notification", map[string]any{
						"title": "SSO importado",
						"body":  fmt.Sprintf("%d tokens de %s", n, e.Name()),
					})
				}
			}
			seen[key] = true
		}
		time.Sleep(30 * time.Second)
	}
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
	apiKey := a.store.Settings().ProxyAPIKey
	if apiKey == "" {
		apiKey = "grok-desktop" // placeholder for clients that require a key
	}
	model := a.store.Settings().DefaultModel
	if model == "" {
		model = store.DefaultModel
	}

	// Open Code / Continue / Cursor style snippets
	openCodeJSON := fmt.Sprintf(`{
  "provider": {
    "grok-desktop": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Grok Desktop",
      "options": {
        "baseURL": "%s",
        "apiKey": "%s"
      },
      "models": {
        "%s": {
          "name": "Grok 4.5"
        },
        "%s-responses": {
          "name": "Grok 4.5 (Responses)"
        }
      }
    }
  }
}`, baseURL, apiKey, model, model)

	openaiEnv := fmt.Sprintf(`OPENAI_BASE_URL=%s
OPENAI_API_KEY=%s
OPENAI_MODEL=%s`, baseURL, apiKey, model)

	curlExample := fmt.Sprintf(`curl %s/chat/completions \
  -H "Authorization: Bearer %s" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}"`,
		baseURL, apiKey, model)

	rate := pricing.RateFor(model)

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
			"base_url":   baseURL,
			"api_key":    apiKey,
			"model":      model,
			"openai_env": openaiEnv,
			"opencode":   openCodeJSON,
			"curl":       curlExample,
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
		if req.APIMode == "" {
			req.APIMode = settings.APIMode
		}
		if req.APIMode == "" {
			req.APIMode = "responses"
		}

		// Inject skills + MCP catalog into conversation context.
		req.Messages = a.injectAgentContext(req.Messages)

		err := a.upstream.StreamChat(ctx, token, settings, label, acc.Email, req, emit)
		if err != nil && ctx.Err() == nil {
			errStr := err.Error()
			if isRateLimitErr(errStr) {
				_ = a.store.MarkAccountExhausted(acc.ID)
				a.emitAccountsUpdate()
			} else if isChatDeniedErr(errStr) {
				_ = a.store.MarkAccountChatDenied(acc.ID, errStr)
				a.emitAccountsUpdate()
			}
			runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: errStr})
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

func (a *App) ensureCreds(ctx context.Context) (string, *store.Account, store.Settings, error) {
	if a.store == nil {
		return "", nil, store.Settings{}, fmt.Errorf("store not ready")
	}
	settings := a.store.Settings()

	// Log available accounts for debugging account exhaustion
	if DebugCreds {
		accs := a.store.ListAccounts()
		for _, ac := range accs {
			log.Printf("ensureCreds: account %s | exhausted=%v exhaustedAt=%v expired=%v hasToken=%v",
				ac.Label, ac.Exhausted, ac.ExhaustedAt, ac.Expired(), ac.AccessToken != "")
		}
	}

	// Auto-recover first
	a.store.RecoverExhaustedAccounts()

	acc, ok := a.store.ActiveAccount()
	if !ok || acc == nil {
		return "", nil, settings, fmt.Errorf("nenhuma conta — adicione uma conta em Contas")
	}

	// Skip exhausted accounts — find first non-exhausted
	accounts := a.store.ListAccounts()
	if acc.IsExhausted() || acc.IsChatDenied() || acc.Expired() {
		for _, candidate := range accounts {
			if candidate.IsExhausted() {
				continue
			}
			if candidate.IsChatDenied() {
				continue
			}
			if candidate.AccessToken == "" {
				continue
			}
			// Allow expired — try refresh below
			_ = a.store.SetActiveAccount(candidate.ID)
			a.emitAccountsUpdate()
			acc, ok = a.store.ActiveAccount()
			if ok && acc != nil {
				break
			}
		}
		if acc == nil || acc.IsExhausted() || acc.IsChatDenied() {
			return "", nil, settings, fmt.Errorf("nenhuma conta utilizável — cota esgotada ou chat negado (Forbidden)")
		}
		if !ok {
			return "", nil, settings, fmt.Errorf("nenhuma conta disponível — adicione ou importe contas")
		}
	}

	// Also verify AccessToken is non-empty — token can be invalid without being "exhausted"
	if acc.AccessToken == "" {
		return "", nil, settings, fmt.Errorf("conta %s sem access_token", acc.Label)
	}

	// refresh if needed — try up to 3 accounts
	for attempt := 0; attempt < 3; attempt++ {
		if !acc.ExpiresSoon(5*time.Minute) || acc.RefreshToken == "" {
			break
		}
		tok, err := a.oauth.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
		if err != nil {
			if acc.Expired() {
				log.Printf("ensureCreds: refresh falhou para %s (expirada): %v — pulando para próxima conta", acc.Label, err)
				next, err2 := a.nextNonExhaustedWithContext(ctx)
				if err2 != nil {
					return "", nil, settings, err2
				}
				a.emitAccountsUpdate()
				acc = next
				continue
			}
			break
		}
		acc.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			acc.RefreshToken = tok.RefreshToken
		}
		acc.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
		acc.UpdatedAt = time.Now().UTC()
		_ = a.store.UpsertAccount(*acc)
		break
	}
	if acc.AccessToken == "" {
		return "", nil, settings, fmt.Errorf("conta %s sem access_token", acc.Label)
	}
	if acc.IsExhausted() {
		return "", nil, settings, fmt.Errorf("todas as contas exauridas — aguarde o reset de 24h ou clique em Resetar")
	}
	return acc.AccessToken, acc, settings, nil
}

func (a *App) nextNonExhaustedWithContext(ctx context.Context) (*store.Account, error) {
	accounts := a.store.ListAccounts()
	for _, candidate := range accounts {
		if candidate.IsExhausted() {
			continue
		}
		if candidate.IsChatDenied() {
			continue
		}
		if candidate.AccessToken == "" {
			continue
		}
		_ = a.store.SetActiveAccount(candidate.ID)
		acc, ok := a.store.ActiveAccount()
		if ok && acc != nil {
			return acc, nil
		}
	}
	return nil, fmt.Errorf("nenhuma conta disponível — adicione ou importe contas")
}

func (a *App) injectAgentContext(msgs []upstream.ChatMessage) []upstream.ChatMessage {
	// Only inject real catalogs from disk. No CREATE_SKILL / fake MCP settings claims
	// until a real UI + MCP bridge ships (hardening D1 option Y).
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
	if len(blocks) == 0 {
		return msgs
	}
	extra := strings.Join(blocks, "\n\n")
	if len(msgs) > 0 && msgs[0].Role == "system" {
		if !containsStr(msgs[0].Content, "Available local skills") && !containsStr(msgs[0].Content, "MCP") {
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

func (a *App) StartDevice() map[string]string {
	dev, err := a.oauth.StartDevice(context.Background())
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	return map[string]string{
		"user_code":                dev.UserCode,
		"verification_uri":        dev.VerificationURI,
		"verification_uri_complete": dev.VerificationURIComplete,
		"device_code":             dev.DeviceCode,
	}
}

// CreateAccount runs only the browser bot (no PollDevice / no account save).
// Prefer CreateAccountFromDevice or CreateAccounts for end-to-end registration.
func (a *App) CreateAccount(verificationURL, userCode string) *register.Result {
	if a.register == nil {
		return &register.Result{Status: "error", Reason: "register runner not ready"}
	}
	log.Printf("register: botDir=%s python=%s", a.register.BotDir, a.register.PythonPath)
	ctx, cancel := context.WithTimeout(a.ctx, 300*time.Second)
	defer cancel()

	result, err := a.register.CreateAccount(ctx, verificationURL, userCode, func(p register.Progress) {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "register:progress", map[string]any{"step": p.Step, "message": p.Message})
		}
		runtime.LogInfof(a.ctx, "register step: %s", p.Step)
	})
	if err != nil {
		return &register.Result{Status: "error", Reason: err.Error()}
	}
	return result
}

func (a *App) activeAccountCount() int {
	accounts := a.store.ListAccounts()
	count := 0
	for _, acc := range accounts {
		if acc.AccessToken == "" {
			continue
		}
		if acc.Expired() {
			continue
		}
		if acc.IsExhausted() {
			continue
		}
		if acc.IsChatDenied() {
			continue
		}
		count++
	}
	return count
}

func (a *App) autoRegisterLoop() {
	const interval = 5 * time.Minute
	for {
		time.Sleep(interval)
		if a.ctx == nil {
			continue
		}
		select {
		case <-a.ctx.Done():
			return
		default:
		}
		s := a.store.Settings()
		if !s.AutoRegisterEnabled {
			continue
		}
		minActive := s.AutoRegisterMinActive
		if minActive <= 0 {
			minActive = 2
		}
		maxActive := s.AutoRegisterMaxActive
		if maxActive <= 0 {
			maxActive = 5
		}
		n := a.activeAccountCount()
		runtime.LogInfof(a.ctx, "auto-register check: %d active accounts (need >= %d)", n, minActive)
		if n >= minActive {
			continue
		}
		// Create up to (minActive - n), but never more than maxActive per wave (batch size).
		need := minActive - n
		if need > maxActive {
			need = maxActive
		}
		if need <= 0 {
			continue
		}
		runtime.LogInfof(a.ctx, "auto-register: creating %d account(s)...", need)
		for i := 0; i < need; i++ {
			result, err := a.CreateAccountFromDevice()

			if err != nil || result == nil || result.Status != "success" {
				reason := "unknown"
				if err != nil {
					reason = err.Error()
				} else if result != nil {
					reason = result.Reason
				}
				runtime.LogErrorf(a.ctx, "auto-register: attempt %d failed: %s", i+1, reason)
				break
			}
			runtime.LogInfof(a.ctx, "auto-register: account %d/%d created", i+1, need)
		}
	}
}

// CreateAccountFromDevice starts a device login, runs the bot, and polls for the token.
// Wails binding (single account) — emits auth:success.
func (a *App) CreateAccountFromDevice() (*register.Result, error) {
	ctx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	if a.regCancel != nil {
		a.regCancel()
	}
	a.regCancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		if a.regCancel != nil {
			// only clear if still ours
			a.regCancel = nil
		}
		a.mu.Unlock()
	}()
	return a.createAccountFromDevice(ctx, false)
}

// createAccountFromDevice is the full flow. silent=true skips auth:success so batch UI is not torn down mid-loop.
// parent is the batch/account cancel context (must already carry deadline or cancel).
func (a *App) createAccountFromDevice(parent context.Context, silent bool) (*register.Result, error) {
	if a.register == nil {
		return nil, fmt.Errorf("register runner not ready")
	}
	if parent == nil {
		parent = a.ctx
	}
	emitProgress := func(step, msg string) {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "register:progress", map[string]any{"step": step, "message": msg, "silent": silent})
			runtime.LogInfof(a.ctx, "register step: %s %s", step, msg)
		}
	}

	// Per-account budget for device grant (~5 min), nested under parent cancel
	ctx, cancel := context.WithTimeout(parent, 300*time.Second)
	defer cancel()

	if err := parent.Err(); err != nil {
		return &register.Result{Status: "error", Reason: "cancelled"}, nil
	}

	emitProgress("device", "starting device login")
	start, err := a.oauth.StartDevice(ctx)
	if err != nil {
		if parent.Err() != nil {
			return &register.Result{Status: "error", Reason: "cancelled"}, nil
		}
		return nil, fmt.Errorf("device start: %w", err)
	}
	url := start.VerificationURIComplete
	if url == "" {
		url = start.VerificationURI
	}

	result, err := a.register.CreateAccount(ctx, url, start.UserCode, func(p register.Progress) {
		emitProgress(p.Step, p.Message)
	})
	if err != nil {
		if parent.Err() != nil || ctx.Err() != nil {
			return &register.Result{Status: "error", Reason: "cancelled"}, nil
		}
		return nil, fmt.Errorf("bot: %w", err)
	}
	if result == nil || result.Status != "success" {
		if result == nil {
			return &register.Result{Status: "error", Reason: "no result from bot"}, nil
		}
		if result.Reason == "timeout/cancelled" || parent.Err() != nil {
			result.Status = "error"
			result.Reason = "cancelled"
		}
		return result, nil
	}

	emitProgress("poll", "waiting for OAuth token")
	// Bot already finished; grant should complete quickly if Allow was clicked.
	// Cap poll so a false bot "success" does not spin for the full 5m budget.
	pollCtx, pollCancel := context.WithTimeout(ctx, 45*time.Second)
	defer pollCancel()
	token, err := a.oauth.PollDevice(pollCtx, start.DeviceCode, start.Interval)
	if err != nil {
		log.Printf("CreateAccountFromDevice: PollDevice error: %v", err)
		if parent.Err() != nil {
			return &register.Result{
				Status: "error",
				Reason: "cancelled",
				Creds:  result.Creds,
			}, nil
		}
		if pollCtx.Err() != nil {
			return &register.Result{
				Status: "error",
				Reason: "OAuth still pending after bot — Allow likely not completed",
				Creds:  result.Creds,
			}, nil
		}
		return &register.Result{
			Status: "error",
			Reason: fmt.Sprintf("poll device: %v", err),
			Creds:  result.Creds,
		}, nil
	}

	// OAuth account with refresh_token (not ImportSSO)
	acc := oauth.AccountFromToken(token, a.oauth.ClientID, a.oauth.Issuer)
	email, uid := a.oauth.UserInfo(context.Background(), token.AccessToken, a.oauth.Issuer)
	if email != "" {
		acc.Email = email
	}
	if uid != "" {
		acc.UserID = uid
		acc.ID = uid
	}
	if prev, ok := a.store.GetAccount(acc.ID); ok && prev != nil {
		if prev.Label != "" && prev.Label != prev.Email && prev.Label != "Grok account" {
			acc.Label = prev.Label
		}
		acc.CreatedAt = prev.CreatedAt
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
	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, fmt.Errorf("salvar conta: %w", err)
	}
	_ = a.store.SetActiveAccount(acc.ID)
	a.emitAccountsUpdate()
	if !silent {
		runtime.EventsEmit(a.ctx, "auth:success", map[string]any{
			"id":       acc.ID,
			"email":    acc.Email,
			"label":    acc.Label,
			"accounts": a.store.PublicAccounts(),
			"count":    len(a.store.ListAccounts()),
		})
	}
	if result.Creds == nil {
		result.Creds = map[string]string{}
	}
	if acc.Email != "" {
		result.Creds["email"] = acc.Email
	}
	result.Creds["account_id"] = acc.ID
	emitProgress("done", "account saved")
	log.Printf("CreateAccountFromDevice: account %s saved (refresh=%v silent=%v)", acc.ID, acc.RefreshToken != "", silent)

	return result, nil
}

// CreateAccounts creates up to n accounts in one batch (sequential attempts).
// Cap is per-batch concurrency/size only (default max 5) — there is NO pool size limit.
func (a *App) CreateAccounts(n int) []map[string]any {
	requested := n
	if requested < 1 {
		requested = 1
	}
	maxBatch := a.store.Settings().AutoRegisterMaxActive
	if maxBatch <= 0 {
		maxBatch = 5
	}
	// Hard safety on a single call
	if maxBatch > 20 {
		maxBatch = 20
	}
	if requested > maxBatch {
		runtime.LogInfof(a.ctx, "CreateAccounts: clamping batch %d → %d (max simultaneous/batch)", requested, maxBatch)
		requested = maxBatch
	}

	batchCtx, batchCancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	if a.regCancel != nil {
		a.regCancel()
	}
	a.regCancel = batchCancel
	a.mu.Unlock()
	defer func() {
		batchCancel()
		a.mu.Lock()
		a.regCancel = nil
		a.mu.Unlock()
	}()

	runtime.LogInfof(a.ctx, "CreateAccounts: batch size=%d (no pool cap; active now=%d)", requested, a.activeAccountCount())
	results := make([]map[string]any, 0, requested)
	runtime.EventsEmit(a.ctx, "register:batch", map[string]any{
		"phase": "start", "total": requested, "active": a.activeAccountCount(),
	})

	for i := 0; i < requested; i++ {
		if batchCtx.Err() != nil {
			results = append(results, map[string]any{
				"attempt": i + 1, "status": "error", "reason": "cancelled",
			})
			runtime.LogInfof(a.ctx, "CreateAccounts: cancelled before %d/%d", i+1, requested)
			break
		}
		runtime.LogInfof(a.ctx, "CreateAccounts: starting %d/%d", i+1, requested)
		runtime.EventsEmit(a.ctx, "register:progress", map[string]any{
			"step":    "batch",
			"message": fmt.Sprintf("conta %d/%d", i+1, requested),
			"index":   i + 1,
			"total":   requested,
		})

		result, err := a.createAccountFromDevice(batchCtx, true)
		entry := map[string]any{"attempt": i + 1}
		if err != nil {
			entry["status"] = "error"
			entry["reason"] = err.Error()
		} else if result == nil {
			entry["status"] = "error"
			entry["reason"] = "no result"
		} else {
			entry["status"] = result.Status
			if result.Reason != "" {
				entry["reason"] = result.Reason
			}
			if result.Creds != nil {
				entry["creds"] = result.Creds
			}
			if result.Status != "success" && entry["reason"] == nil {
				entry["reason"] = "bot status=" + result.Status
			}
		}
		results = append(results, entry)
		runtime.LogInfof(a.ctx, "CreateAccounts: finished %d/%d status=%v reason=%v",
			i+1, requested, entry["status"], entry["reason"])

		if batchCtx.Err() != nil || (result != nil && result.Reason == "cancelled") {
			runtime.LogInfof(a.ctx, "CreateAccounts: stopping batch after cancel")
			break
		}

		if i+1 < requested {
			select {
			case <-batchCtx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}

	a.emitAccountsUpdate()
	ok := 0
	for _, r := range results {
		if r["status"] == "success" {
			ok++
		}
	}
	runtime.EventsEmit(a.ctx, "register:batch", map[string]any{
		"phase": "done", "total": requested, "success": ok, "results": results,
	})
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "auth:success", map[string]any{
			"batch":     true,
			"count":     len(a.store.ListAccounts()),
			"accounts":  a.store.PublicAccounts(),
			"label":     fmt.Sprintf("%d/%d gerada(s)", ok, requested),
			"ok":        ok,
			"requested": requested,
		})
	}
	return results
}

func strMap(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func resolveRegisterPaths(exeDir string, s store.Settings, extractedBot string) (pythonPath, botDir string) {
	// Prefer absolute paths so double-click / shortcut CWD never breaks bot discovery.
	if abs, err := filepath.Abs(exeDir); err == nil {
		exeDir = abs
	}
	if eval, err := filepath.EvalSymlinks(exeDir); err == nil && eval != "" {
		exeDir = eval
	}

	pythonPath = strings.TrimSpace(s.PythonPath)
	botDir = strings.TrimSpace(s.BotDir)

	// Relative settings → relative to the executable directory (not CWD).
	if pythonPath != "" && !filepath.IsAbs(pythonPath) {
		pythonPath = filepath.Join(exeDir, pythonPath)
	}
	if botDir != "" && !filepath.IsAbs(botDir) {
		botDir = filepath.Join(exeDir, botDir)
	}

	if pythonPath == "" {
		var pyCands []string
		if goruntime.GOOS == "windows" {
			pyCands = []string{
				filepath.Join(exeDir, ".venv", "Scripts", "python.exe"),
				filepath.Join(exeDir, "python", "python.exe"),
				filepath.Join(exeDir, "..", "..", ".venv", "Scripts", "python.exe"),
				filepath.Join(exeDir, "..", ".venv", "Scripts", "python.exe"),
			}
		} else {
			pyCands = []string{
				filepath.Join(exeDir, ".venv", "bin", "python3"),
				filepath.Join(exeDir, "..", "..", ".venv", "bin", "python3"),
				filepath.Join(exeDir, "..", ".venv", "bin", "python3"),
			}
		}
		for _, cand := range pyCands {
			cand = filepath.Clean(cand)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				pythonPath = cand
				break
			}
		}
		if pythonPath == "" {
			if goruntime.GOOS == "windows" {
				if p, err := exec.LookPath("python"); err == nil {
					pythonPath = p
				} else if p, err := exec.LookPath("python3"); err == nil {
					pythonPath = p
				} else if p, err := exec.LookPath("py"); err == nil {
					pythonPath = p
				} else {
					pythonPath = "python"
				}
			} else {
				if p, err := exec.LookPath("python3"); err == nil {
					pythonPath = p
				} else {
					pythonPath = "python3"
				}
			}
		}
	}

	if botDir == "" {
		// Prefer embedded extract (always available with bare .exe), then portable/monorepo trees.
		cwd, _ := os.Getwd()
		cands := []string{}
		if extractedBot != "" {
			cands = append(cands, extractedBot)
		}
		cands = append(cands,
			filepath.Join(exeDir, "grok-signup-bot"),
			filepath.Join(exeDir, "..", "grok-signup-bot"),
			filepath.Join(exeDir, "..", "..", "grok-signup-bot"),
		)
		if cwd != "" {
			cands = append(cands,
				filepath.Join(cwd, "grok-signup-bot"),
				filepath.Join(cwd, "..", "grok-signup-bot"),
			)
		}
		for _, cand := range cands {
			cand = filepath.Clean(cand)
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				if _, err := os.Stat(filepath.Join(cand, "grok_signup.py")); err == nil {
					botDir = cand
					break
				}
			}
		}
		if botDir == "" && extractedBot != "" {
			botDir = extractedBot
		}
		if botDir == "" {
			// Always absolute path next to exe — never bare relative (breaks when CWD ≠ exe dir).
			botDir = filepath.Join(exeDir, "grok-signup-bot")
		}
	}
	if abs, err := filepath.Abs(botDir); err == nil {
		botDir = abs
	}
	return pythonPath, botDir
}

func isRateLimitErr(s string) bool {
	low := strings.ToLower(s)
	return (strings.Contains(low, "rate") && strings.Contains(low, "limit")) ||
		strings.Contains(low, "free-usage-exhausted") ||
		strings.Contains(low, "too many requests") ||
		strings.Contains(low, "status 429") ||
		strings.Contains(low, " 429") ||
		strings.Contains(low, "payment required") ||
		strings.Contains(low, "402")
}

func isChatDeniedErr(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "access to the chat endpoint is denied") ||
		strings.Contains(low, "chat endpoint is denied") ||
		(strings.Contains(low, "forbidden") && strings.Contains(low, "chat")) ||
		(strings.Contains(low, "update the permissions") && strings.Contains(low, "console.x.ai"))
}

func applyRegisterSettings(r *register.Runner, s store.Settings) {
	if r == nil {
		return
	}
	r.EmailProviders = append([]string{}, s.EmailProviders...)
	r.DuckMailURL = s.DuckMailURL
	r.DuckMailKey = s.DuckMailKey
}
