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
//
// Not supported (explicit 404): /v1/completions (legacy).
type Server struct {
	mu       sync.Mutex
	store    *store.Store
	upstream *upstream.Client
	ensure   func(ctx context.Context) (token string, account *store.Account, settings store.Settings, err error)
	srv      *http.Server
	ln       net.Listener
	addr     string
}

func New(
	st *store.Store,
	up *upstream.Client,
	ensure func(ctx context.Context) (string, *store.Account, store.Settings, error),
) *Server {
	return &Server{store: st, upstream: up, ensure: ensure}
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
	token, _, settings, err := s.ensure(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stream := false
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if v, ok := m["stream"].(bool); ok {
			stream = v
		}
		if _, ok := m["reasoning_effort"]; !ok {
			if settings.ReasoningEffort != "" {
				m["reasoning_effort"] = settings.ReasoningEffort
			}
		}
		if _, ok := m["model"]; !ok || m["model"] == "" {
			m["model"] = settings.DefaultModel
		}
		// strip -responses suffix for upstream model id when needed
		if mid, ok := m["model"].(string); ok {
			low := strings.ToLower(mid)
			if strings.HasSuffix(low, "-responses") {
				m["model"] = mid[:len(mid)-len("-responses")]
			}
		}
		// alias last_response_id
		if prev, ok := m["last_response_id"].(string); ok && prev != "" {
			m["previous_response_id"] = prev
			delete(m, "last_response_id")
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
			} else {
				m["tools"] = nativeSearchTools()
			}
			if _, ok := m["tool_choice"]; !ok {
				m["tool_choice"] = "auto"
			}
			// inject temporal into input if array of messages
			if input, ok := m["input"].([]any); ok {
				m["input"] = injectTemporalIntoResponsesInput(input)
			}
		}
		body, _ = json.Marshal(m)
	}

	url := strings.TrimRight(settings.UpstreamBase, "/") + path
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-grok-client-version", settings.ClientVersion)
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
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	isSSE := stream || strings.Contains(ct, "text/event-stream")

	if isSSE && resp.StatusCode < 400 {
		// Robust SSE: set headers, flush per event
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		if err := pipeSSE(w, resp.Body); err != nil {
			log.Printf("proxyhttp sse: %v", err)
		}
		return
	}

	// JSON / error path
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
