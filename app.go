package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"grok-desktop/internal/mcpconfig"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/pricing"
	"grok-desktop/internal/proxyhttp"
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

	mu           sync.Mutex
	deviceCancel context.CancelFunc
	deviceState  *deviceLoginState
	reqCancel    context.CancelFunc
	activeReq    *ActiveRequest
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
	a.upstream = upstream.New()
	a.proxy = proxyhttp.New(st, a.upstream, a.ensureCreds)
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
	})
	if err != nil {
		return store.Settings{}, err
	}
	// restart proxy if needed
	s := a.store.Settings()
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

		// Always Responses + native server-side web_search / x_search.
		if req.ReasoningEffort == "" {
			req.ReasoningEffort = settings.ReasoningEffort
		}
		req.APIMode = "responses"

		// Inject skills + MCP catalog into conversation context.
		req.Messages = a.injectAgentContext(req.Messages)

		err := a.upstream.StreamChat(ctx, token, settings, label, acc.Email, req, emit)
		if err != nil && ctx.Err() == nil {
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

func (a *App) ensureCreds(ctx context.Context) (string, *store.Account, store.Settings, error) {
	if a.store == nil {
		return "", nil, store.Settings{}, fmt.Errorf("store not ready")
	}
	settings := a.store.Settings()
	acc, ok := a.store.ActiveAccount()
	if !ok || acc == nil {
		return "", nil, settings, fmt.Errorf("nenhuma conta — adicione uma conta em Contas")
	}
	// refresh if needed
	if acc.ExpiresSoon(5*time.Minute) && acc.RefreshToken != "" {
		tok, err := a.oauth.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
		if err != nil {
			if acc.Expired() {
				return "", nil, settings, fmt.Errorf("token expirado — faça login de novo: %v", err)
			}
		} else {
			acc.AccessToken = tok.AccessToken
			if tok.RefreshToken != "" {
				acc.RefreshToken = tok.RefreshToken
			}
			acc.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
			acc.UpdatedAt = time.Now().UTC()
			_ = a.store.UpsertAccount(*acc)
		}
	}
	if acc.AccessToken == "" {
		return "", nil, settings, fmt.Errorf("conta sem access_token")
	}
	return acc.AccessToken, acc, settings, nil
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
