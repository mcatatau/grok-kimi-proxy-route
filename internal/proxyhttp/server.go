package proxyhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/pricing"
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
//	POST /v1/sso                (import SSO token — E2E automation)
//
// Not supported (explicit 404): /v1/completions (legacy).
type Server struct {
	mu        sync.Mutex
	store     *store.Store
	upstream  *upstream.Client
	ensure    func(ctx context.Context) (token string, account *store.Account, settings store.Settings, err error)
	importSSO func(ssoToken string) (map[string]any, error)
	srv       *http.Server
	ln        net.Listener
	addr      string

	// Optional hooks for the desktop App (usage UI + account cards).
	OnUsage         func(sample store.RequestSample)
	OnAccountChange func()
}

func New(
	st *store.Store,
	up *upstream.Client,
	ensure func(ctx context.Context) (string, *store.Account, store.Settings, error),
	importSSO func(ssoToken string) (map[string]any, error),
) *Server {
	return &Server{store: st, upstream: up, ensure: ensure, importSSO: importSSO}
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
	// Explicitly reject legacy completions
	mux.HandleFunc("/v1/completions", s.handleLegacyCompletions)
	mux.HandleFunc("/completions", s.handleLegacyCompletions)
	mux.HandleFunc("/v1/sso", s.handleSSO)
	mux.HandleFunc("/sso", s.handleSSO)
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
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("proxyhttp: %v", err)
		}
	}()
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
	acc, ok := s.store.ActiveAccount()
	email := ""
	if ok {
		email = acc.Email
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"addr":    s.Addr(),
		"account": email,
	})
}

func (s *Server) handleSSO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if s.importSSO == nil {
		http.Error(w, `{"error":"SSO import not available"}`, http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Token  string   `json:"token"`
		Tokens []string `json:"tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	tokens := body.Tokens
	if body.Token != "" {
		tokens = append(tokens, body.Token)
	}
	if len(tokens) == 0 {
		http.Error(w, `{"error":"no tokens provided"}`, http.StatusBadRequest)
		return
	}
	imported := 0
	var lastErr string
	for _, t := range tokens {
		if _, err := s.importSSO(t); err != nil {
			lastErr = err.Error()
		} else {
			imported++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"imported": imported,
		"errors":   lastErr,
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
	token, _, settings, err := s.ensure(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	models, err := s.upstream.ListModels(r.Context(), token, settings)
	if err != nil {
		// Fallback metadata so OpenCode doesn't degrade on empty list
		models = []upstream.ModelInfo{
			{ID: "grok-4.5", Name: "Grok 4.5", Description: "xAI frontier model", APIMode: "chat"},
			{ID: "grok-4.5-responses", Name: "Grok 4.5 (Responses)", Description: "Responses API + native search", APIMode: "responses", Root: "grok-4.5"},
		}
	}
	data := make([]map[string]any, 0, len(models)+2)
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
		data = append(data, enrichModelMeta(m))
	}
	// Ensure grok-4.5 always present with rich metadata (OpenCode warning fix)
	if !seen["grok-4.5"] {
		data = append([]map[string]any{enrichModelMeta(upstream.ModelInfo{
			ID: "grok-4.5", Name: "Grok 4.5", Description: "xAI frontier model", APIMode: "chat",
		})}, data...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func enrichModelMeta(m upstream.ModelInfo) map[string]any {
	// OpenCode / clients look for context lengths & modality metadata
	ctxWindow := 256000
	if strings.Contains(strings.ToLower(m.ID), "4.5") {
		ctxWindow = 500000
	}
	return map[string]any{
		"id":            m.ID,
		"object":        "model",
		"created":       time.Now().Unix(),
		"owned_by":      "xAI",
		"name":          firstNonEmpty(m.Name, m.ID),
		"description":   m.Description,
		"api_mode":      m.APIMode,
		"root":          m.Root,
		"context_window": ctxWindow,
		"context_length": ctxWindow,
		"max_tokens":    ctxWindow,
		"architecture": map[string]any{
			"modality":         "text+image->text",
			"input_modalities": []string{"text", "image"},
			"output_modalities": []string{"text"},
		},
		"pricing": map[string]any{
			"prompt":     "0.000002",
			"completion": "0.000006",
		},
		"supported_parameters": []string{
			"tools", "tool_choice", "reasoning_effort", "temperature", "max_tokens", "stream",
		},
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stream := false
	var m map[string]any
	modelName := ""
	if json.Unmarshal(body, &m) == nil {
		if v, ok := m["stream"].(bool); ok {
			stream = v
		}
		modelName, _ = m["model"].(string)
	}

	// Do original request with failover retry
	tried := map[string]bool{}
	maxTries := 5
	for attempt := 0; attempt < maxTries; attempt++ {
		// Clone body for each attempt
		var m2 map[string]any
		body2 := body
		if attempt > 0 || len(tried) > 0 {
			// Re-compute enriched body only on retry (first time uses original)
			if json.Unmarshal(body, &m2) == nil {
				enrichBody(m2, path, stream, s.store.Settings())
				body2, _ = json.Marshal(m2)
			}
		} else if m != nil {
			enrichBody(m, path, stream, s.store.Settings())
			body2, _ = json.Marshal(m)
		}

		token, acc, settings, err2 := s.ensure(r.Context())
		if err2 != nil {
			http.Error(w, err2.Error(), http.StatusUnauthorized)
			return
		}

		// Skip already-tried accounts
		if acc != nil && tried[acc.ID] {
			continue
		}

		reqBodyStr := string(body2)

		url := strings.TrimRight(settings.UpstreamBase, "/") + path
		upReq, err2 := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body2))
		if err2 != nil {
			http.Error(w, err2.Error(), 500)
			return
		}
		upReq.Header.Set("Authorization", "Bearer "+token)
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("x-grok-client-version", settings.ClientVersion)
		if v := r.Header.Get("Accept"); v != "" {
			upReq.Header.Set("Accept", v)
		} else if stream {
			upReq.Header.Set("Accept", "text/event-stream")
		} else {
			upReq.Header.Set("Accept", "application/json")
		}

		resp, err2 := http.DefaultClient.Do(upReq)
		if err2 != nil {
			if attempt == maxTries-1 {
				http.Error(w, err2.Error(), http.StatusBadGateway)
				return
			}
			continue
		}

		ct := resp.Header.Get("Content-Type")
		isSSE := stream || strings.Contains(ct, "text/event-stream")

		if isSSE && resp.StatusCode < 400 {
			for k, vv := range resp.Header {
				if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") {
					continue
				}
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			usage := pipeSSEWithUsage(w, resp.Body, path)
			resp.Body.Close()
			if acc != nil && (usage.promptTokens > 0 || usage.totalTokens > 0) {
				s.recordUsage(acc.ID, resp.StatusCode, usage, path, modelName)
			}
			return
		}

		if resp.StatusCode >= 400 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()

			errInfo := diagnoseUpstreamError(resp.StatusCode, bodyBytes, path, settings.ClientVersion)
			log.Printf("proxyhttp UPSTREAM ERROR [%s]: status %d | classification=%s | attempt=%d/%d | account=%s | sent_req=%.300s",
				path, resp.StatusCode, errInfo.Classification, attempt+1, maxTries, labelOr(acc), reqBodyStr)

			if errInfo.Classification == "rate_limit" && acc != nil {
				log.Printf("proxyhttp account exhausted (switching): %s (%s)", acc.Label, acc.Email)
				_ = s.store.MarkAccountExhausted(acc.ID)
				s.notifyAccountChange()
				tried[acc.ID] = true
				// Try another account on next iteration
				continue
			}
			if errInfo.Classification == "chat_denied" && acc != nil {
				log.Printf("proxyhttp chat denied (switching): %s (%s) — %s", acc.Label, acc.Email, errInfo.Detail)
				_ = s.store.MarkAccountChatDenied(acc.ID, string(bodyBytes))
				s.notifyAccountChange()
				tried[acc.ID] = true
				continue
			}

			// Non-recoverable errors are terminal (or all accounts already tried)
			if errInfo.Classification == "rate_limit" {
				w.Header().Set("X-Account-Status", "all-exhausted")
			} else if errInfo.Classification == "chat_denied" {
				w.Header().Set("X-Account-Status", "chat_denied")
			} else {
				w.Header().Set("X-Account-Status", errInfo.Classification)
			}
			for k, vv := range resp.Header {
				if strings.EqualFold(k, "Content-Length") {
					continue
				}
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(bodyBytes)
			return
		}

		// Success
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		if acc != nil {
			usage := extractUsageFromPayload(string(bodyBytes), "", path)
			s.recordUsage(acc.ID, resp.StatusCode, usage, path, modelName)
		}
		return
	}

	// All exhausted
	http.Error(w, `{"error":{"message":"all accounts exhausted","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
}

func enrichBody(m map[string]any, path string, stream bool, settings store.Settings) {
	if _, ok := m["reasoning_effort"]; !ok {
		if settings.ReasoningEffort != "" {
			m["reasoning_effort"] = settings.ReasoningEffort
		}
	}
	if _, ok := m["model"]; !ok || m["model"] == "" {
		m["model"] = settings.DefaultModel
	}
	if mid, ok := m["model"].(string); ok {
		low := strings.ToLower(mid)
		if strings.HasSuffix(low, "-responses") {
			m["model"] = mid[:len(mid)-len("-responses")]
		}
	}
	if prev, ok := m["last_response_id"].(string); ok && prev != "" {
		m["previous_response_id"] = prev
		delete(m, "last_response_id")
	}

	if path == "/chat/completions" {
		if msgs, ok := m["messages"].([]any); ok {
			m["messages"] = injectTemporalIntoMessages(msgs)
		}
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
			existingOpts, hasOpts := m["stream_options"].(map[string]any)
			if !hasOpts {
				m["stream_options"] = map[string]any{"include_usage": true}
			} else {
				existingOpts["include_usage"] = true
				m["stream_options"] = existingOpts
			}
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
		if raw, ok := m["tools"]; ok {
			m["tools"] = sanitizeResponsesTools(raw)
		} else {
			m["tools"] = nativeSearchTools()
		}
		if _, ok := m["tool_choice"]; !ok {
			m["tool_choice"] = "auto"
		}
		if input, ok := m["input"].([]any); ok {
			m["input"] = injectTemporalIntoResponsesInput(input)
		}
	}
}

func labelOr(acc *store.Account) string {
	if acc == nil {
		return "none"
	}
	return acc.Label + " <" + acc.Email + ">"
}

type errClassification struct {
	Classification string `json:"classification"` // rate_limit, auth_error, invalid_request, server_error, unknown
	Detail         string `json:"detail"`
}

func diagnoseUpstreamError(status int, body []byte, path, clientVer string) errClassification {
	bodyStr := strings.ToLower(string(body))
	switch {
	case status == 429:
		return errClassification{"rate_limit", "upstream retornou HTTP 429 (Too Many Requests)"}
	case status == 402:
		return errClassification{"rate_limit", "upstream retornou HTTP 402 (Payment Required) — quota exaurida"}
	case status == 403 && strings.Contains(bodyStr, "version"):
		return errClassification{"client_version", "cli-chat-proxy rejeitou a versão do cliente (" + clientVer + ")"}
	case status == 403 && (strings.Contains(bodyStr, "access to the chat endpoint is denied") ||
		strings.Contains(bodyStr, "chat endpoint is denied") ||
		strings.Contains(bodyStr, "update the permissions") ||
		(strings.Contains(bodyStr, "permission") && strings.Contains(bodyStr, "denied"))):
		return errClassification{"chat_denied", "Forbidden: Access to the chat endpoint is denied (permissions)"}
	case status == 403:
		return errClassification{"auth_error", "upstream retornou HTTP 403 — token/permisão inválida"}
	case status == 401:
		return errClassification{"auth_error", "upstream retornou HTTP 401 — token expirado ou inválido"}
	case status == 422 || status == 400:
		detail := "formato de requisição rejeitado pelo upstream"
		if strings.Contains(bodyStr, "model") {
			detail += " — modelo inválido/não suportado: " + path
		}
		if strings.Contains(bodyStr, "tools") || strings.Contains(bodyStr, "tool_choice") || strings.Contains(bodyStr, "variant") || strings.Contains(bodyStr, "namespace") {
			detail += " — formato de ferramentas (tools) inválido"
		}
		if strings.Contains(bodyStr, "input") {
			detail += " — formato do input inválido"
		}
		return errClassification{"invalid_request", detail}
	case status >= 500:
		return errClassification{"server_error", "upstream retornou HTTP " + strconv.Itoa(status) + " — erro interno do servidor"}
	default:
		return errClassification{"unknown", "upstream retornou HTTP " + strconv.Itoa(status)}
	}
}

func (s *Server) recordUsage(accountID string, statusCode int, u sseUsageCapture, path, model string) {
	if u.promptTokens == 0 && u.completionTokens == 0 && u.totalTokens == 0 {
		return
	}
	total := u.totalTokens
	if total == 0 {
		total = u.promptTokens + u.completionTokens + u.reasoningTokens
	}
	modelID := model
	if modelID == "" {
		modelID = s.store.Settings().DefaultModel
	}
	cost := pricing.CostUSD(modelID, u.promptTokens, u.completionTokens, u.reasoningTokens, 0)
	sample := store.RequestSample{
		ID:               uuid.NewString(),
		At:               time.Now().UTC().Format(time.RFC3339),
		AccountID:        accountID,
		Model:            modelID,
		PromptTokens:     u.promptTokens,
		CompletionTokens: u.completionTokens,
		ReasoningTokens:  u.reasoningTokens,
		TotalTokens:      total,
		CostUSD:          cost,
		LatencyMs:        0,
		TTFTMs:           0,
		Estimated:        statusCode >= 400,
	}
	_ = s.store.RecordRequest(sample)
	if s.OnUsage != nil {
		s.OnUsage(sample)
	}
}

func (s *Server) notifyAccountChange() {
	if s.OnAccountChange != nil {
		s.OnAccountChange()
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
	sys := map[string]any{
		"role": "system",
		"content": []map[string]any{
			{"type": "input_text", "text": line},
		},
	}
	return append([]any{sys}, input...)
}
