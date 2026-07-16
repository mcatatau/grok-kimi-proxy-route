package proxyhttp



import (

	"bytes"

	"context"

	"encoding/json"

	"fmt"

	"io"

	"log"

	"net"

	"net/http"

	"strconv"

	"strings"

	"sync"

	"time"



	"github.com/google/uuid"



	"grok-desktop/internal/gemini"


	"grok-desktop/internal/store"

	"grok-desktop/internal/upstream"

)



// Server is a local OpenAI + Anthropic compatible reverse proxy.

// Supported:

//

//	GET  /health

//	GET  /v1/models

//	POST /v1/chat/completions   (OpenAI)

//	POST /v1/responses          (OpenAI Responses)

//	POST /v1/messages           (Anthropic Messages)

//	POST /v1/search             (native xAI web_search + x_search)

//

// Not supported (explicit 404): /v1/completions (legacy).

type Server struct {

	mu       sync.Mutex

	store    *store.Store

	upstream *upstream.Client

	ensure   func(ctx context.Context) (token string, account *store.Account, settings store.Settings, err error)

	// forceRefresh re-syncs CLI tokens and forces OAuth refresh for a specific account.

	forceRefresh func(ctx context.Context, accountID string) (token string, account *store.Account, settings store.Settings, err error)

	// onQuota is optional: called when upstream returns 402 / balance exhausted so the app can

	// mark the account and rotate active. Return true if a different usable account is now active.

	onQuota func(accountID, reason string) (rotated bool)

	// onAuthFail is optional: called when upstream returns 401/403 permission-denied after a

	// failed force-refresh. Should mark auth-denied and rotate. Return true if another account is active.

	onAuthFail func(accountID, reason string) (rotated bool)

	srv        *http.Server

	ln         net.Listener

	addr       string

}



func New(

	st *store.Store,

	up *upstream.Client,

	ensure func(ctx context.Context) (string, *store.Account, store.Settings, error),

) *Server {

	return &Server{store: st, upstream: up, ensure: ensure}

}



// SetQuotaHandler registers a callback invoked on quota exhaustion (402 / balance exhausted).

func (s *Server) SetQuotaHandler(fn func(accountID, reason string) (rotated bool)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.onQuota = fn

}



// SetAuthFailHandler registers a callback for chat auth denial (403 permission-denied / 401).

func (s *Server) SetAuthFailHandler(fn func(accountID, reason string) (rotated bool)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.onAuthFail = fn

}



// SetForceRefresh registers force OAuth refresh (used before marking auth-denied).

func (s *Server) SetForceRefresh(fn func(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.forceRefresh = fn

}



func (s *Server) quotaHandler() func(accountID, reason string) (rotated bool) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.onQuota

}



func (s *Server) authFailHandler() func(accountID, reason string) (rotated bool) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.onAuthFail

}



func (s *Server) forceRefreshFn() func(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.forceRefresh

}



func (s *Server) Addr() string {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.addr

}



func (s *Server) Start(listen string) error {

	s.mu.Lock()

	defer s.mu.Unlock()

	if s.srv != nil {

		return nil

	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)

	mux.HandleFunc("/v1/models", s.handleModels)

	mux.HandleFunc("/models", s.handleModels)

	// OpenAI

	mux.HandleFunc("/v1/chat/completions", s.handleChat)

	mux.HandleFunc("/chat/completions", s.handleChat)

	mux.HandleFunc("/v1/responses", s.handleResponses)

	mux.HandleFunc("/responses", s.handleResponses)

	// Anthropic

	mux.HandleFunc("/v1/messages", s.handleMessages)

	mux.HandleFunc("/messages", s.handleMessages)

	// Native xAI search (OpenAI-compatible helper; reuses proxy auth)

	mux.HandleFunc("/v1/search", s.handleSearch)

	mux.HandleFunc("/search", s.handleSearch)

	mux.HandleFunc("/v1/web_search", s.handleSearch)

	mux.HandleFunc("/v1/x_search", s.handleSearch)

	// Explicitly reject legacy completions

	mux.HandleFunc("/v1/completions", s.handleLegacyCompletions)

	mux.HandleFunc("/completions", s.handleLegacyCompletions)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		if r.URL.Path == "/" {

			w.Header().Set("Content-Type", "application/json")

			_ = json.NewEncoder(w).Encode(map[string]any{

				"name": "grok-proxy-plus",

				"endpoints": []string{

					"/v1/models",

					"/v1/chat/completions",

					"/v1/responses",

					"/v1/messages",

					"/v1/search",

				},

				"not_supported": []string{"/v1/completions"},

			})

			return

		}

		http.NotFound(w, r)

	})



	ln, err := net.Listen("tcp", listen)

	if err != nil {

		return err

	}

	s.ln = ln

	s.addr = ln.Addr().String()

	// Recover panics per-request. If headers were already sent (SSE mid-stream),

	// writing another status panics again — nested recover swallows that so the

	// Wails process does not die while Codex is streaming Grok 4.5.

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		defer func() {

			if rec := recover(); rec != nil {

				log.Printf("proxyhttp panic on %s %s: %v", r.Method, r.URL.Path, rec)

				func() {

					defer func() { _ = recover() }()

					w.Header().Set("Content-Type", "application/json")

					w.WriteHeader(http.StatusInternalServerError)

					_, _ = w.Write([]byte(`{"error":{"message":"internal proxy panic","type":"server_error"}}`))

				}()

			}

		}()

		mux.ServeHTTP(w, r)

	})

	s.srv = &http.Server{

		Handler:           handler,

		ReadHeaderTimeout: 30 * time.Second,

		// IdleTimeout 0: long-lived SSE streams from Codex must not be cut.

		// WriteTimeout 0: Grok 4.5 long reasoning streams can exceed minutes.

		ErrorLog: log.Default(),

	}

	go func() {

		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {

			log.Printf("proxyhttp serve error (listener stopped): %v", err)

		}

	}()

	log.Printf("proxyhttp: listening on http://%s", s.addr)

	return nil

}



func (s *Server) Stop(ctx context.Context) error {

	s.mu.Lock()

	defer s.mu.Unlock()

	if s.srv == nil {

		return nil

	}

	err := s.srv.Shutdown(ctx)

	s.srv = nil

	s.ln = nil

	s.addr = ""

	return err

}



func (s *Server) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(http.StatusNotFound)

	_ = json.NewEncoder(w).Encode(map[string]any{

		"error": map[string]any{

			"message": "Legacy /v1/completions is not supported. Use /v1/chat/completions (OpenAI), /v1/responses (OpenAI), or /v1/messages (Anthropic).",

			"type":    "invalid_request_error",

			"code":    "endpoint_not_supported",

		},

	})

}



func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	settings := s.store.Settings()

	email := ""

	exhausted := false

	switch {

	case settings.IsOllie():

		email = "OllieChat (keyless)"

	case settings.IsGemini():

		email = "Gemini ADC · " + settings.EffectiveGeminiProject()

	case settings.IsKimiWork():

		acc, ok := s.store.PreferUsableAccount()

		if !ok {

			acc, ok = s.store.ActiveAccount()

		}

		if ok && acc != nil {

			email = acc.Label

			if email == "" {

				email = acc.Email

			}

			exhausted = acc.Exhausted()

		} else {

			email = "Kimi Work (no account)"

		}

	default:

		acc, ok := s.store.PreferUsableAccount()

		if !ok {

			acc, ok = s.store.ActiveAccount()

		}

		if ok && acc != nil {

			email = acc.Email

			exhausted = acc.Exhausted()

		}

	}

	_ = json.NewEncoder(w).Encode(map[string]any{

		"status":    "ok",

		"addr":      s.Addr(),

		"account":   email,

		"exhausted": exhausted,

		"provider":  settings.NormalizedProvider(),

		"auth_mode": settings.ProviderAuthMode(),

		"upstream":  settings.EffectiveUpstream(),

		"model":     settings.ResolveModel("default"),

	})

}



func (s *Server) gate(r *http.Request) bool {

	key := s.store.Settings().ProxyAPIKey

	if key == "" {

		return true

	}

	auth := r.Header.Get("Authorization")

	if strings.HasPrefix(strings.ToLower(auth), "bearer ") && strings.TrimSpace(auth[7:]) == key {

		return true

	}

	return r.Header.Get("X-API-Key") == key || r.Header.Get("x-api-key") == key

}



func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {

	if !s.gate(r) {

		http.Error(w, `{"error":{"message":"unauthorized","type":"invalid_request_error"}}`, http.StatusUnauthorized)

		return

	}

	settings := s.store.Settings()

	// Optional ensure so provider-specific live lists can work.

	token, _, active, ensureErr := s.ensure(r.Context())

	if ensureErr == nil && active.NormalizedProvider() != "" {

		settings = active

	}

	_ = token

	var models []upstream.ModelInfo

	switch settings.NormalizedProvider() {

	case store.ProviderKimiWork:

		models = []upstream.ModelInfo{

			{ID: "kimi-for-coding", Name: "Kimi For Coding", Description: "Kimi Work · chat/completions", APIMode: "chat"},

			{ID: "k3-agent", Name: "K3 Max (Work)", Description: "alias → kimi-for-coding", APIMode: "chat"},

			{ID: "k3-agent-swarm", Name: "K3 Swarm Max (Work)", Description: "alias → kimi-for-coding", APIMode: "chat"},

			{ID: "k2d6-agent", Name: "K2.6 Agent (Work)", Description: "alias → kimi-for-coding", APIMode: "chat"},

		}

	case store.ProviderOllie:

		models = []upstream.ModelInfo{

			{ID: "claude-sonnet-5", Name: "claude-sonnet-5", Description: "OllieChat", APIMode: "chat"},

			{ID: "claude-fable-5", Name: "claude-fable-5", Description: "OllieChat", APIMode: "chat"},

			{ID: "claude-opus-4-8", Name: "claude-opus-4-8", Description: "OllieChat", APIMode: "chat"},

			{ID: "deepseek-v4-flash-free", Name: "deepseek-v4-flash-free", Description: "OllieChat", APIMode: "chat"},

		}

	case store.ProviderGemini:

		for _, id := range gemini.ListModels(r.Context(), settings) {

			models = append(models, upstream.ModelInfo{ID: id, Name: id, Description: "Vertex AI · ADC", APIMode: "chat"})

		}

	default: // xAI Grok — Responses only

		models = []upstream.ModelInfo{

			{ID: "grok-4.5", Name: "Grok 4.5", Description: "xAI · /v1/responses", APIMode: "responses"},

		}

		// best-effort live list, keep only response-capable ids

		if token != "" && !settings.IsKimiWork() {

			xai := settings

			xai.Provider = store.ProviderXAI

			xai.UpstreamBase = store.DefaultUpstream

			if xm, err := s.upstream.ListModels(r.Context(), token, xai); err == nil {

				seen := map[string]bool{"grok-4.5": true}

				for _, m := range xm {

					id := strings.ToLower(m.ID)

					if strings.Contains(id, "grok") && !seen[m.ID] {

						m.APIMode = "responses"

						if m.Description == "" {

							m.Description = "xAI · /v1/responses"

						}

						models = append(models, m)

						seen[m.ID] = true

					}

				}

			}

		}

	}

	data := make([]map[string]any, 0, len(models)+1)

	for _, m := range models {

		data = append(data, enrichModelMeta(m, settings.NormalizedProvider()))

	}

	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(map[string]any{

		"object":          "list",

		"data":            data,

		"active_provider": settings.NormalizedProvider(),

		"active_model":    settings.DefaultModel,

		"api_policy": map[string]string{

			"xai":       "responses",

			"kimi_work": "chat",

		},

	})

}


func enrichModelMeta(m upstream.ModelInfo, provider string) map[string]any {
	ctxWindow := 256000
	owner := "xAI"
	prov := strings.ToLower(strings.TrimSpace(provider))
	switch prov {
	case store.ProviderOllie, "olliechat":
		owner = "OllieChat"
		prov = store.ProviderOllie
		ctxWindow = 128000
	case store.ProviderGemini, "google", "vertex":
		owner = "Google"
		prov = store.ProviderGemini
		ctxWindow = 1048576
	case store.ProviderKimiWork, "kimi", "kimi-work":
		owner = "Kimi"
		prov = store.ProviderKimiWork
		ctxWindow = 1048576
		if strings.Contains(strings.ToLower(m.ID), "k2") {
			ctxWindow = 262144
		}
	default:
		owner = "xAI"
		prov = store.ProviderXAI
		if strings.Contains(strings.ToLower(m.ID), "4.5") {
			ctxWindow = 500000
		}
	}
	desc := m.Description
	if desc == "" {
		desc = owner
	}
	apiMode := m.APIMode
	if apiMode == "" {
		if prov == store.ProviderXAI {
			apiMode = "responses"
		} else {
			apiMode = "chat"
		}
	}
	return map[string]any{
		"id":             m.ID,
		"object":         "model",
		"created":        time.Now().Unix(),
		"owned_by":       owner,
		"provider":       prov,
		"name":           firstNonEmpty(m.Name, m.ID),
		"description":    desc,
		"api_mode":       apiMode,
		"root":           m.Root,
		"context_window": ctxWindow,
		"context_length": ctxWindow,
		"max_tokens":     ctxWindow,
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {

	s.proxyUpstream(w, r, "/chat/completions")

}



func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {

	s.proxyUpstream(w, r, "/responses")

}



// injectTemporalIntoMessages prepends a system line with today's date/year (e.g. 2026).

func injectTemporalIntoMessages(msgs []any) []any {

	now := time.Now()

	line := "Temporal context: today is " + now.Format("Monday, January 2, 2006") +

		". The current year is " + strconv.Itoa(now.Year()) +

		". Treat this as ground truth for \"today\", \"this year\", recency, and time-sensitive answers — do not assume you are stuck in 2023–2024."



	if len(msgs) > 0 {

		if first, ok := msgs[0].(map[string]any); ok {

			role, _ := first["role"].(string)

			if role == "system" {

				content := contentToString(first["content"])

				if !strings.Contains(content, "Temporal context:") {

					first["content"] = line + "\n\n" + content

					msgs[0] = first

				}

				return msgs

			}

		}

	}

	sys := map[string]any{"role": "system", "content": line}

	return append([]any{sys}, msgs...)

}



func contentToString(c any) string {

	switch t := c.(type) {

	case string:

		return t

	case []any:

		var b strings.Builder

		for _, p := range t {

			if m, ok := p.(map[string]any); ok {

				if s := asString(m["text"]); s != "" {

					b.WriteString(s)

				}

			} else if s, ok := p.(string); ok {

				b.WriteString(s)

			}

		}

		return b.String()

	default:

		return ""

	}

}



func (s *Server) proxyUpstream(w http.ResponseWriter, r *http.Request, path string) {

	if r.Method != http.MethodPost {

		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)

		return

	}

	if !s.gate(r) {

		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)

		return

	}

	token, acc, settings, err := s.ensure(r.Context())

	if err != nil {

		http.Error(w, err.Error(), http.StatusUnauthorized)

		return

	}

	// Endpoint policy by active provider (no silent rewrite chaos):
	//   xAI  → /v1/responses only
	//   Kimi → /v1/chat/completions only
	if settings.IsXAI() && path == "/chat/completions" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Grok (xAI) uses /v1/responses only. Switch provider or call POST /v1/responses.",
				"type":    "invalid_request_error",
				"code":    "grok_responses_only",
			},
		})
		return
	}
	if settings.IsKimiWork() && path == "/responses" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Kimi Work uses /v1/chat/completions only (agent-gw has no /responses).",
				"type":    "invalid_request_error",
				"code":    "kimi_chat_only",
			},
		})
		return
	}

	startedAt := time.Now()

	body, err := io.ReadAll(r.Body)

	if err != nil {

		http.Error(w, err.Error(), http.StatusBadRequest)

		return

	}



	stream := false

	clientPath := path // original path before any Ollie rewrite (/responses → /chat/completions)

	codexClient := isCodexRequest(r)

	resolvedModel := settings.ResolveModelForClient("")

	_ = codexClient // kept for optional headers / future; no longer forces model

	var m map[string]any

	if json.Unmarshal(body, &m) == nil {

		if v, ok := m["stream"].(bool); ok {

			stream = v

		}

		// Inherit global reasoning effort when client omits it (both providers).

		// Ollie still sanitizes high/xhigh + max_tokens below so content is not emptied.

		if _, ok := m["reasoning_effort"]; !ok {

			if settings.ReasoningEffort != "" {

				m["reasoning_effort"] = settings.ReasoningEffort

			}

		}

		// HTTP proxy: honor client model. UI default model is ONLY for desktop chat.
		// Only empty/alias models map to DefaultModel of the ACTIVE provider.

		reqModel, _ := m["model"].(string)

		// Keep active provider from settings — do NOT auto-switch provider from model id.

		m["model"] = settings.ResolveModelForClient(reqModel)

		// Kimi wire id

		if settings.IsKimiWork() {

			m["model"] = settings.ResolveModelForClient(reqModel)

		}

		resolvedModel, _ = m["model"].(string)

		// alias last_response_id

		if prev, ok := m["last_response_id"].(string); ok && prev != "" {

			m["previous_response_id"] = prev

			delete(m, "last_response_id")

		}



		// Non-xAI providers without native Responses: rewrite /responses → chat/completions.

		// Kimi Work coding gateway is OpenAI chat/completions-native; map responses → chat

		// and still accept /v1/chat/completions (recommended default remains responses in UI).

		if (settings.IsOllie() || settings.IsGemini() || settings.IsKimiWork()) && path == "/responses" {

			path = "/chat/completions"

			m = responsesBodyToChatCompletions(m, settings)

			if mid, ok := m["model"].(string); ok && mid != "" {

				resolvedModel = mid

			}

		}



		if path == "/chat/completions" {

			if msgs, ok := m["messages"].([]any); ok {

				m["messages"] = injectTemporalIntoMessages(msgs)

			}

			// Sanitize tools — drop namespace / invalid types (prevents 422)

			if _, ok := m["tools"]; ok {

				tools := sanitizeChatTools(m["tools"])

				if len(tools) == 0 {

					delete(m, "tools")

					delete(m, "tool_choice")

				} else {

					m["tools"] = tools

				}

			}

			if stream {

				if _, ok := m["stream_options"]; !ok {

					m["stream_options"] = map[string]any{"include_usage": true}

				}

			}

			if settings.IsOllie() {

				sanitizeOllieChatBody(m)

			}

			if settings.IsGemini() {

				// Vertex path ignores OpenAI-only junk.

				delete(m, "store")

				delete(m, "previous_response_id")

				delete(m, "stream_options")

				delete(m, "tools")

				delete(m, "tool_choice")

			}

		}



		if path == "/responses" {

			if settings.StoreResponses {

				if _, ok := m["store"]; !ok {

					m["store"] = true

				}

			}

			if _, ok := m["reasoning"]; !ok {

				if eff, _ := m["reasoning_effort"].(string); eff != "" {

					m["reasoning"] = map[string]any{"effort": eff, "summary": "auto"}

				} else if settings.ReasoningEffort != "" {

					m["reasoning"] = map[string]any{"effort": settings.ReasoningEffort, "summary": "auto"}

				}

			}

			// CRITICAL: sanitize tools (fixes OpenCode 422 unknown variant `namespace`)

			if raw, ok := m["tools"]; ok {

				m["tools"] = sanitizeResponsesTools(raw)

			} else if settings.IsXAI() {

				m["tools"] = nativeSearchTools()

			}

			if _, ok := m["tool_choice"]; !ok {

				if _, hasTools := m["tools"]; hasTools {

					m["tool_choice"] = "auto"

				}

			}

			// CRITICAL: sanitize input (fixes Codex 422 untagged enum ModelInput)

			if raw, ok := m["input"]; ok {

				m["input"] = sanitizeResponsesInput(raw)

			}

			if input, ok := m["input"].([]any); ok {

				m["input"] = injectTemporalIntoResponsesInput(input)

			}

		}

		body, _ = json.Marshal(m)

	}



	// Gemini: handle entirely via Vertex REST + ADC (not reverse-proxy to UpstreamBase).

	if settings.IsGemini() {

		if m == nil {

			_ = json.Unmarshal(body, &m)

			if m == nil {

				m = map[string]any{}

			}

			reqModel, _ := m["model"].(string)

			if codexClient {

				m["model"] = settings.ResolveModelForCodex(reqModel)

			} else {

				m["model"] = settings.ResolveModelForClient(reqModel)

			}

			if clientPath == "/responses" {

				m = responsesBodyToChatCompletions(m, settings)

			}

			if v, ok := m["stream"].(bool); ok {

				stream = v

			}

		}

		s.handleGeminiUpstream(w, r.Context(), clientPath, stream, m, settings)

		return

	}



	// Up to 3 attempts: original → force-refresh same account (auth) → rotated account.

	accountID := ""

	if acc != nil {

		accountID = acc.ID

	}

	maxAttempts := 3

	if settings.IsOllie() {

		maxAttempts = 1

		accountID = ""

	}

	authRetried := false

	for attempt := 0; attempt < maxAttempts; attempt++ {

		url := strings.TrimRight(settings.EffectiveUpstream(), "/") + path

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))

		if err != nil {

			http.Error(w, err.Error(), 500)

			return

		}

		setUpstreamAuthHeaders(req, token, settings)

		if v := r.Header.Get("Accept"); v != "" {

			req.Header.Set("Accept", v)

		} else if stream {

			req.Header.Set("Accept", "text/event-stream")

		} else {

			req.Header.Set("Accept", "application/json")

		}



		resp, err := http.DefaultClient.Do(req)

		if err != nil {

			http.Error(w, err.Error(), http.StatusBadGateway)

			return

		}



		// Read error body only when status is bad; keep stream body for SSE success path.

		if resp.StatusCode >= 400 {

			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

			_ = resp.Body.Close()

			reason := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(errBody))



			// Kimi global capacity — fail fast, no account rotate / re-mint spam.

			if settings.IsKimiWork() && isKimiCapacityBusy(resp.StatusCode, errBody) {

				log.Printf("proxyhttp: kimi capacity busy (no rotate): %s", truncateForLog(string(errBody), 200))

				writeKimiCapacityError(w, errBody)

				return

			}



			// Auth denial: force-refresh same account once, then rotate.

			if isAuthStatus(resp.StatusCode, errBody) && accountID != "" {

				if !authRetried {

					authRetried = true

					if fr := s.forceRefreshFn(); fr != nil {

						tok2, acc2, settings2, err2 := fr(r.Context(), accountID)

						if err2 == nil && tok2 != "" && tok2 != token {

							token, acc, settings = tok2, acc2, settings2

							if acc2 != nil {

								accountID = acc2.ID

							}

							log.Printf("proxyhttp: auth failure — force-refreshed account %s and retrying", accountID)

							continue

						}

					}

					// Also re-run ensure (pulls CLI sync) in case refresh path was unavailable.

					tok2, acc2, settings2, err2 := s.ensure(r.Context())

					if err2 == nil && tok2 != "" && tok2 != token {

						token, acc, settings = tok2, acc2, settings2

						if acc2 != nil {

							accountID = acc2.ID

						}

						log.Printf("proxyhttp: auth failure — ensure got different token, retrying")

						continue

					}

				}

				// Mark auth-denied and rotate to another account.

				if fn := s.authFailHandler(); fn != nil {

					if rotated := fn(accountID, reason); rotated {

						tok2, acc2, settings2, err2 := s.ensure(r.Context())

						if err2 == nil && (acc2 == nil || acc2.ID != accountID) {

							prev := accountID

							token, acc, settings = tok2, acc2, settings2

							if acc2 != nil {

								accountID = acc2.ID

							}

							log.Printf("proxyhttp: auth denied on %s — rotated to %s and retrying", prev, accountID)

							continue

						}

					}

				} else {

					_, _ = s.store.MarkAuthDenied(accountID, reason)

					if next := s.store.NextUsableAccountID(accountID); next != "" {

						_ = s.store.SetActiveAccount(next)

						tok2, acc2, settings2, err2 := s.ensure(r.Context())

						if err2 == nil {

							token, acc, settings = tok2, acc2, settings2

							if acc2 != nil {

								accountID = acc2.ID

							}

							continue

						}

					}

				}

			}



			if isQuotaStatus(resp.StatusCode, errBody) {

				if fn := s.quotaHandler(); fn != nil && accountID != "" {

					if rotated := fn(accountID, reason); rotated {

						// Pick fresh creds for the newly active account and retry once.

						tok2, acc2, settings2, err2 := s.ensure(r.Context())

						if err2 == nil && (acc2 == nil || acc2.ID != accountID) {

							token, acc, settings = tok2, acc2, settings2

							if acc2 != nil {

								accountID = acc2.ID

							}

							log.Printf("proxyhttp: quota on account — rotated and retrying")

							continue

						}

					}

				} else if accountID != "" {

					// No handler — still stamp store so UI/next ensure skips this account.

					_, _ = s.store.MarkExhausted(accountID, reason)

					if next := s.store.NextUsableAccountID(accountID); next != "" {

						_ = s.store.SetActiveAccount(next)

						tok2, acc2, settings2, err2 := s.ensure(r.Context())

						if err2 == nil {

							token, acc, settings = tok2, acc2, settings2

							if acc2 != nil {

								accountID = acc2.ID

							}

							continue

						}

					}

				}

			}

			// Final error to client

			for k, vv := range resp.Header {

				if strings.EqualFold(k, "Content-Length") {

					continue

				}

				for _, v := range vv {

					w.Header().Add(k, v)

				}

			}

			w.Header().Set("Content-Type", "application/json")

			w.WriteHeader(resp.StatusCode)

			_, _ = w.Write(errBody)

			return

		}



		ct := resp.Header.Get("Content-Type")

		isSSE := stream || strings.Contains(ct, "text/event-stream")



		// Ollie/Gemini: client asked for Responses but we hit chat/completions — translate wire format.

		if settings.IsOllie() && clientPath == "/responses" {

			if isSSE {

				if err := pipeOllieChatSSEToResponses(w, resp.Body, resolvedModel); err != nil {

					log.Printf("proxyhttp ollie responses sse: %v", err)

				}

				_ = resp.Body.Close()

				return

			}

			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

			_ = resp.Body.Close()

			out, err := chatCompletionJSONToResponse(raw, resolvedModel)

			if err != nil {

				// Fall back to raw upstream body.

				w.Header().Set("Content-Type", "application/json")

				w.WriteHeader(resp.StatusCode)

				_, _ = w.Write(raw)

				return

			}

			accountID := ""

			if acc != nil {

				accountID = acc.ID

			}

			s.recordUsageFromJSONBody(raw, accountID, resolvedModel, time.Since(startedAt).Milliseconds())

			w.Header().Set("Content-Type", "application/json")

			w.WriteHeader(http.StatusOK)

			_ = json.NewEncoder(w).Encode(out)

			return

		}



		if isSSE {

			for k, vv := range resp.Header {

				if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") {

					continue

				}

				for _, v := range vv {

					w.Header().Add(k, v)

				}

			}

			tee := newUsageTeeReader(resp.Body)

			if err := pipeSSE(w, tee); err != nil {

				log.Printf("proxyhttp sse: %v", err)

			}

			_ = resp.Body.Close()

			accountID := ""

			if acc != nil {

				accountID = acc.ID

			}

			if len(tee.lastJSON) > 0 {

				s.recordUsageFromSSECapture(tee.lastJSON, accountID, resolvedModel, time.Since(startedAt).Milliseconds())

			}

			return

		}



		// JSON success

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

		_ = resp.Body.Close()

		accountID := ""

		if acc != nil {

			accountID = acc.ID

		}

		s.recordUsageFromJSONBody(raw, accountID, resolvedModel, time.Since(startedAt).Milliseconds())

		for k, vv := range resp.Header {

			if strings.EqualFold(k, "Content-Length") {

				continue

			}

			for _, v := range vv {

				w.Header().Add(k, v)

			}

		}

		w.WriteHeader(resp.StatusCode)

		_, _ = w.Write(raw)

		return

	}

}



// isKimiCapacityBusy detects Kimi Work / Desktop overload that cannot be fixed by rotating accounts.

// Example: "Too many people are chatting with Kimi right now. Please try again soon."

func isKimiCapacityBusy(code int, body []byte) bool {

	low := strings.ToLower(string(body))

	if strings.Contains(low, "too many people are chatting with kimi") {

		return true

	}

	if strings.Contains(low, "too many people are chatting") && strings.Contains(low, "kimi") {

		return true

	}

	if strings.Contains(low, "please try again soon") && strings.Contains(low, "kimi") {

		return true

	}

	// Some gateways return 429/503 with generic capacity wording.

	if (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) &&

		(strings.Contains(low, "too many people") ||

			strings.Contains(low, "server is busy") ||

			strings.Contains(low, "capacity") && strings.Contains(low, "kimi") ||

			strings.Contains(low, "overloaded")) {

		return true

	}

	return false

}



func writeKimiCapacityError(w http.ResponseWriter, upstreamBody []byte) {

	msg := "Too many people are chatting with Kimi right now. Please try again soon."

	// Prefer upstream message when present.

	if m, ok := extractUpstreamMessage(upstreamBody); ok && m != "" {

		msg = m

	}

	w.Header().Set("Content-Type", "application/json")

	w.Header().Set("Retry-After", "30")

	w.WriteHeader(http.StatusServiceUnavailable)

	_ = json.NewEncoder(w).Encode(map[string]any{

		"error": map[string]any{

			"message": msg,

			"type":    "kimi_capacity_error",

			"code":    "kimi_server_busy",

			"retryable": true,

			// Explicit: not an auth/account problem — do not rotate keys or re-mint.

			"hint": "Kimi upstream servers are overloaded. Wait and retry; rotating accounts will not help.",

		},

	})

}



func extractUpstreamMessage(body []byte) (string, bool) {

	var m map[string]any

	if json.Unmarshal(body, &m) != nil {

		// plain text body

		s := strings.TrimSpace(string(body))

		if s != "" && len(s) < 500 {

			return s, true

		}

		return "", false

	}

	if errObj, ok := m["error"].(map[string]any); ok {

		if msg, ok := errObj["message"].(string); ok && msg != "" {

			return msg, true

		}

	}

	if msg, ok := m["message"].(string); ok && msg != "" {

		return msg, true

	}

	return "", false

}



func isQuotaStatus(code int, body []byte) bool {

	if code == http.StatusPaymentRequired { // 402

		return true

	}

	low := strings.ToLower(string(body))

	if strings.Contains(low, "usage balance exhausted") || strings.Contains(low, "balance exhausted") {

		return true

	}

	if code == http.StatusTooManyRequests && (strings.Contains(low, "exhausted") || strings.Contains(low, "quota") || strings.Contains(low, "balance")) {

		return true

	}

	return false

}



// isAuthStatus detects upstream auth failures that should force-refresh / rotate accounts.

func isAuthStatus(code int, body []byte) bool {

	if code == http.StatusUnauthorized {

		return true

	}

	low := strings.ToLower(string(body))

	if code == http.StatusForbidden {

		if strings.Contains(low, "permission-denied") ||

			strings.Contains(low, "access to the chat endpoint is denied") ||

			strings.Contains(low, "invalid or expired credentials") ||

			strings.Contains(low, "no auth context") {

			return true

		}

		// Generic 403 from cli-chat-proxy with structured JSON code field.

		if strings.Contains(low, `"code":"permission-denied"`) || strings.Contains(low, `"code": "permission-denied"`) {

			return true

		}

	}

	if strings.Contains(low, "invalid or expired credentials") || strings.Contains(low, "no auth context") {

		return true

	}

	return false

}



func setUpstreamAuthHeaders(req *http.Request, token string, settings store.Settings) {

	version := settings.ClientVersion

	if version == "" {

		version = store.DefaultClientVersion

	}

	if token == "" && settings.IsOllie() {

		token = store.OllieAPIKey

	}

	req.Header.Set("Authorization", "Bearer "+token)

	req.Header.Set("Content-Type", "application/json")

	if settings.IsOllie() {

		req.Header.Set("User-Agent", "grok-desktop-ollie/"+version)

		return

	}

	if settings.IsKimiWork() {

		req.Header.Set("User-Agent", store.KimiWorkUserAgent)

		req.Header.Set("X-Msh-Platform", "kimi-code-cli")

		req.Header.Set("X-Msh-Version", "0.23.5")

		return

	}

	req.Header.Set("x-grok-client-version", version)

	req.Header.Set("x-grok-client-surface", "grok-cli")

	req.Header.Set("User-Agent", "grok/"+version)

}



// responsesBodyToChatCompletions converts an OpenAI Responses body into chat/completions

// so clients that only speak /v1/responses still work against OllieChat / Gemini.

// Model is assumed already resolved by the caller (Codex vs client policy).

func responsesBodyToChatCompletions(m map[string]any, settings store.Settings) map[string]any {

	out := map[string]any{}

	if model, ok := m["model"].(string); ok && model != "" {

		// Prefer already-resolved id; only normalize aliases/suffixes without re-forcing.

		out["model"] = settings.ResolveModelForClient(model)

	} else {

		out["model"] = settings.ResolveModelForClient("")

	}

	if v, ok := m["stream"].(bool); ok {

		out["stream"] = v

	}

	if v, ok := m["temperature"]; ok {

		out["temperature"] = v

	}

	if v, ok := m["top_p"]; ok {

		out["top_p"] = v

	}

	if v, ok := m["max_output_tokens"]; ok {

		out["max_tokens"] = v

	} else if v, ok := m["max_tokens"]; ok {

		out["max_tokens"] = v

	}

	// Only forward client-set effort (never invent global xhigh — empties free-model answers).

	if v, ok := m["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {

		out["reasoning_effort"] = v

	} else if r, ok := m["reasoning"].(map[string]any); ok {

		// Codex sends reasoning: { effort, summary } on /v1/responses.

		if v, ok := r["effort"].(string); ok && strings.TrimSpace(v) != "" {

			out["reasoning_effort"] = v

		}

	}

	// tools: Responses-style tools → chat tools when possible; drop native xAI search types.

	if tools, ok := m["tools"]; ok {

		if sanitized := sanitizeChatTools(tools); len(sanitized) > 0 {

			out["tools"] = sanitized

			if tc, ok := m["tool_choice"]; ok {

				out["tool_choice"] = tc

			}

		}

	}

	msgs := responsesInputToChatMessages(m["input"])

	// Codex system prompt lives in top-level instructions — must become a system message.

	if instr, ok := m["instructions"].(string); ok && strings.TrimSpace(instr) != "" {

		content := strings.TrimSpace(instr)

		// When tools are available, append a concise nudge that encourages the model

		// to actually USE them rather than reasoning indefinitely.  Fable-5 / Claude

		// reasoning models on OllieChat tend to think at length without acting; this

		// helps break the reasoning-only loop in Codex agent sessions.

		if _, hasTools := out["tools"]; hasTools {

			content += "\n\nIMPORTANT: You have tools available. Use them directly to accomplish tasks. " +

				"Keep your reasoning concise and act promptly. Do not reason at length without taking action. " +

				"If you know which tool to call, call it immediately rather than planning extensively."

		}

		sys := map[string]any{"role": "system", "content": content}

		msgs = append([]any{sys}, msgs...)

	}

	if len(msgs) == 0 {

		msgs = []any{map[string]any{"role": "user", "content": "Hello"}}

	}

	out["messages"] = msgs

	if settings.IsOllie() {

		sanitizeOllieChatBody(out)

	}

	return out

}



// sanitizeOllieChatBody makes chat/completions bodies safe for the free OllieChat gateway.

//

// We do NOT clamp reasoning effort or impose a low max_tokens ceiling.  The reasoning

// loop was caused by (1) reasoning text leaking into message content, (2) finish_reason

// "length" being reported as "completed", and (3) max_tokens floors being too low

// (1024-2048).  All three are now fixed elsewhere.  Here we just:

//   - strip Responses-only fields that chat/completions rejects

//   - clamp xhigh→high (gateway doesn't support xhigh)

//   - set a high default max_tokens ONLY when the client sent nothing at all

//

// If the client (Codex) sends its own max_tokens, we respect it as-is — no floor,

// no ceiling.  The model thinks as much as it wants.

func sanitizeOllieChatBody(m map[string]any) {

	if m == nil {

		return

	}

	delete(m, "store")

	delete(m, "previous_response_id")

	delete(m, "last_response_id")

	delete(m, "reasoning") // Responses-style object; not valid on chat/completions



	// Clamp xhigh → high (the gateway rejects xhigh). Everything else stays as-is.

	if re, ok := m["reasoning_effort"].(string); ok {

		switch strings.ToLower(strings.TrimSpace(re)) {

		case "xhigh", "extra_high", "extra-high", "max", "maximum":

			m["reasoning_effort"] = "high"

		case "", "none", "off", "minimal":

			delete(m, "reasoning_effort")

		}

	}



	// Only set a default when the client sent NOTHING.  If the client sent a value

	// (high or low) we respect it — no floor, no ceiling, no interference.

	if _, ok := m["max_tokens"]; !ok {

		if _, ok2 := m["max_completion_tokens"]; !ok2 {

			m["max_tokens"] = 65536

		}

	}

}



func ensureMinMaxTokens(m map[string]any, min int) {

	if min <= 0 {

		return

	}

	cur := 0

	switch v := m["max_tokens"].(type) {

	case float64:

		cur = int(v)

	case int:

		cur = v

	case int64:

		cur = int(v)

	case json.Number:

		i, _ := v.Int64()

		cur = int(i)

	}

	if cur < min {

		m["max_tokens"] = min

	}

}



func responsesInputToChatMessages(input any) []any {

	switch v := input.(type) {

	case string:

		if strings.TrimSpace(v) == "" {

			return nil

		}

		return []any{map[string]any{"role": "user", "content": v}}

	case []any:

		out := make([]any, 0, len(v))

		for _, raw := range v {

			item, ok := raw.(map[string]any)

			if !ok {

				continue

			}

			// Already a chat message shape (possibly with tool_calls).

			if role, _ := item["role"].(string); role != "" {

				role = strings.ToLower(role)

				msg := map[string]any{"role": role}

				content := flattenResponsesContent(item["content"])

				if content == "" {

					if s, ok := item["content"].(string); ok {

						content = s

					}

				}

				// Assistant tool_calls-only turns have empty content — still keep them.

				if tcs, ok := item["tool_calls"]; ok {

					msg["tool_calls"] = tcs

					if content != "" {

						msg["content"] = content

					} else {

						msg["content"] = nil

					}

					out = append(out, msg)

					continue

				}

				if role == "tool" {

					if id, ok := item["tool_call_id"].(string); ok && id != "" {

						msg["tool_call_id"] = id

					}

					msg["content"] = content

					out = append(out, msg)

					continue

				}

				if content == "" {

					continue

				}

				if role == "system" || role == "developer" || role == "assistant" {

					// keep

				} else {

					role = "user"

					msg["role"] = role

				}

				msg["content"] = content

				out = append(out, msg)

				continue

			}

			// Responses typed items (Codex multi-turn / tools).

			typ, _ := item["type"].(string)

			switch typ {

			case "message", "":

				role, _ := item["role"].(string)

				if role == "" {

					role = "user"

				}

				content := flattenResponsesContent(item["content"])

				if content == "" {

					continue

				}

				out = append(out, map[string]any{"role": role, "content": content})

			case "function_call", "custom_tool_call":

				// → assistant message with tool_calls

				callID, _ := item["call_id"].(string)

				if callID == "" {

					callID, _ = item["id"].(string)

				}

				name, _ := item["name"].(string)

				args := ""

				switch a := item["arguments"].(type) {

				case string:

					args = a

				default:

					if item["arguments"] != nil {

						b, _ := json.Marshal(item["arguments"])

						args = string(b)

					}

				}

				if callID == "" {

					callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]

				}

				out = append(out, map[string]any{

					"role":    "assistant",

					"content": nil,

					"tool_calls": []any{

						map[string]any{

							"id":   callID,

							"type": "function",

							"function": map[string]any{

								"name":      name,

								"arguments": args,

							},

						},

					},

				})

			case "function_call_output", "custom_tool_call_output":

				callID, _ := item["call_id"].(string)

				if callID == "" {

					callID, _ = item["id"].(string)

				}

				outText := ""

				switch o := item["output"].(type) {

				case string:

					outText = o

				default:

					if item["output"] != nil {

						b, _ := json.Marshal(item["output"])

						outText = string(b)

					}

				}

				if outText == "" {

					outText = flattenResponsesContent(item["content"])

				}

				msg := map[string]any{"role": "tool", "content": outText}

				if callID != "" {

					msg["tool_call_id"] = callID

				}

				out = append(out, msg)

			case "reasoning":

				// Skip encrypted/internal reasoning history for chat backends.

				continue

			}

		}

		return out

	case map[string]any:

		// single object input

		return responsesInputToChatMessages([]any{v})

	default:

		return nil

	}

}



func flattenResponsesContent(content any) string {

	switch c := content.(type) {

	case string:

		return c

	case []any:

		var b strings.Builder

		for _, p := range c {

			part, ok := p.(map[string]any)

			if !ok {

				if s, ok := p.(string); ok {

					b.WriteString(s)

				}

				continue

			}

			if t, ok := part["text"].(string); ok {

				b.WriteString(t)

				continue

			}

			// input_text / output_text

			if t, ok := part["type"].(string); ok && (t == "input_text" || t == "output_text" || t == "text") {

				if tx, ok := part["text"].(string); ok {

					b.WriteString(tx)

				}

			}

		}

		return b.String()

	default:

		return ""

	}

}



func injectTemporalIntoResponsesInput(input []any) []any {

	// Prepend a system-like user note if nothing temporal yet

	line := "Temporal context: today is " + time.Now().Format("Monday, January 2, 2006") +

		". The current year is " + strconv.Itoa(time.Now().Year()) + "."

	// Check first item

	if len(input) > 0 {

		if m, ok := input[0].(map[string]any); ok {

			blob, _ := json.Marshal(m)

			if strings.Contains(string(blob), "Temporal context:") {

				return input

			}

		}

	}

	// Use full message shape + input_text (xAI ModelInput-safe)

	sys := map[string]any{

		"type": "message",

		"role": "system",

		"content": []any{

			map[string]any{"type": "input_text", "text": line},

		},

	}

	return append([]any{sys}, input...)

}

// chatCompletionJSONToResponse maps a non-stream chat.completion JSON body to a Responses-like object.
func chatCompletionJSONToResponse(raw []byte, model string) (map[string]any, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	text := ""
	if choices, ok := in["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				if c, ok := msg["content"].(string); ok {
					text = c
				}
			}
		}
	}
	if model == "" {
		if m, ok := in["model"].(string); ok {
			model = m
		}
	}
	out := map[string]any{
		"id":      in["id"],
		"object":  "response",
		"model":   model,
		"status":  "completed",
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": text},
				},
			},
		},
	}
	if u, ok := in["usage"]; ok {
		out["usage"] = u
	}
	return out, nil
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

