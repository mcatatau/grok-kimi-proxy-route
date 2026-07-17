package proxyhttp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/upstream"
)

// SearchRequest is a thin OpenAI-ish body for native xAI web/x search.
//
//	POST /v1/search
//	{
//	  "query": "what is grok 4.5",
//	  "model": "grok-4.5",           // optional
//	  "sources": ["web","x"],        // optional, default both
//	  "max_results": 10,             // optional (hint for response shaping)
//	  "reasoning_effort": "low"      // optional
//	}
type SearchRequest struct {
	Query           string   `json:"query"`
	Q               string   `json:"q"` // alias
	Model           string   `json:"model"`
	Sources         []string `json:"sources"`
	MaxResults      int      `json:"max_results"`
	ReasoningEffort string   `json:"reasoning_effort"`
	Stream          bool     `json:"stream"` // ignored for now (always non-stream JSON)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.store.Settings().IsOllie() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Native /v1/search (xAI web_search/x_search) is not available on OllieChat. Switch provider to xai or use tools in the client.",
				"type":    "not_supported_error",
				"code":    "provider_ollie",
			},
		})
		return
	}
	if r.Method != http.MethodPost {
		// also allow GET ?q=
		if r.Method == http.MethodGet {
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			if q == "" {
				q = strings.TrimSpace(r.URL.Query().Get("query"))
			}
			if q == "" {
				http.Error(w, `{"error":{"message":"missing query","type":"invalid_request_error"}}`, http.StatusBadRequest)
				return
			}
			s.runSearch(w, r, SearchRequest{
				Query:   q,
				Model:   r.URL.Query().Get("model"),
				Sources: splitCSV(r.URL.Query().Get("sources")),
			})
			return
		}
		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	if !s.gate(r) {
		http.Error(w, `{"error":{"message":"unauthorized","type":"invalid_request_error"}}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req SearchRequest
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	if strings.TrimSpace(req.Query) == "" {
		req.Query = strings.TrimSpace(req.Q)
	}
	if strings.TrimSpace(req.Query) == "" {
		// also accept OpenAI chat-like {"messages":[{"role":"user","content":"..."}]}
		var loose map[string]any
		if json.Unmarshal(body, &loose) == nil {
			if msgs, ok := loose["messages"].([]any); ok {
				for i := len(msgs) - 1; i >= 0; i-- {
					m, _ := msgs[i].(map[string]any)
					if m == nil {
						continue
					}
					if asString(m["role"]) == "user" {
						req.Query = contentToString(m["content"])
						break
					}
				}
			}
			if req.Query == "" {
				req.Query = asString(loose["input"])
			}
		}
	}
	if strings.TrimSpace(req.Query) == "" {
		http.Error(w, `{"error":{"message":"query is required","type":"invalid_request_error","param":"query"}}`, http.StatusBadRequest)
		return
	}
	s.runSearch(w, r, req)
}

func (s *Server) runSearch(w http.ResponseWriter, r *http.Request, req SearchRequest) {
	if !s.gate(r) {
		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	token, acc, settings, err := s.ensure(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = settings.DefaultModel
	}
	// strip -responses suffix if clients pass it
	if strings.HasSuffix(strings.ToLower(model), "-responses") {
		model = model[:len(model)-len("-responses")]
	}
	effort := req.ReasoningEffort
	if effort == "" {
		effort = "low"
	}
	sources := normalizeSearchSources(req.Sources)
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = 12
	}

	accountID := ""
	if acc != nil {
		accountID = acc.ID
	}
	t0 := time.Now()
	var result *upstream.SearchResponse
	authRetried := false
	for attempt := 0; attempt < 3; attempt++ {
		result, err = s.upstream.NativeSearch(r.Context(), token, settings, model, effort, req.Query, sources)
		if err == nil {
			break
		}
		// bubble upstream status-ish messages as 402/429 when quota
		msg := err.Error()
		low := strings.ToLower(msg)
		auth := strings.Contains(low, "http 401") || strings.Contains(low, "http 403") ||
			strings.Contains(low, "permission-denied") || strings.Contains(low, "invalid or expired credentials") ||
			strings.Contains(low, "no auth context") || strings.Contains(low, "access to the chat endpoint is denied")
		if auth && accountID != "" {
			if !authRetried {
				authRetried = true
				if fr := s.forceRefreshFn(); fr != nil {
					tok2, acc2, settings2, err2 := fr(r.Context(), accountID)
					if err2 == nil && tok2 != "" && tok2 != token {
						token, acc, settings = tok2, acc2, settings2
						if acc2 != nil {
							accountID = acc2.ID
						}
						continue
					}
				}
			}
			if fn := s.authFailHandler(); fn != nil {
				if rotated := fn(accountID, msg); rotated {
					tok2, acc2, settings2, err2 := s.ensure(r.Context())
					if err2 == nil && (acc2 == nil || acc2.ID != accountID) {
						token, acc, settings = tok2, acc2, settings2
						if acc2 != nil {
							accountID = acc2.ID
						}
						continue
					}
				}
			}
		}
		quota := strings.Contains(low, "http 402") || strings.Contains(low, "balance exhausted") ||
			strings.Contains(low, "usage limit") || strings.Contains(low, "resource_exhausted") ||
			strings.Contains(low, "access_terminated") || strings.Contains(low, "billing cycle") ||
			(strings.Contains(low, "http 429") && (strings.Contains(low, "exhausted") || strings.Contains(low, "quota") || strings.Contains(low, "balance")))
		if quota && accountID != "" {
			if fn := s.quotaHandler(); fn != nil {
				if rotated := fn(accountID, msg); rotated {
					tok2, acc2, settings2, err2 := s.ensure(r.Context())
					if err2 == nil && tok2 != "" && (acc2 == nil || acc2.Usable()) {
						token, acc, settings = tok2, acc2, settings2
						if acc2 != nil {
							accountID = acc2.ID
						}
						continue
					}
				}
			} else {
				_, _ = s.store.MarkExhausted(accountID, msg)
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
		status := http.StatusBadGateway
		if strings.Contains(low, "http 402") || strings.Contains(low, "balance exhausted") {
			status = http.StatusPaymentRequired
		} else if strings.Contains(low, "http 429") {
			status = http.StatusTooManyRequests
		} else if strings.Contains(low, "http 401") {
			status = http.StatusUnauthorized
		} else if strings.Contains(low, "http 403") || strings.Contains(low, "permission-denied") {
			status = http.StatusForbidden
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "upstream_error",
			},
		})
		return
	}
	if result == nil {
		http.Error(w, `{"error":{"message":"empty search result"}}`, http.StatusBadGateway)
		return
	}

	// trim results
	if len(result.Results) > maxResults {
		result.Results = result.Results[:maxResults]
	}

	email := ""
	if acc != nil {
		email = acc.Email
	}

	// OpenAI-compatible-ish envelope + rich search payload
	out := map[string]any{
		"object":  "xai.search",
		"id":      result.ID,
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(result.Model, model),
		"query":   req.Query,
		"sources": sources,
		// primary list (easy for any client)
		"results": result.Results,
		// aliases some UIs expect
		"data": result.Results,
		// optional model prose if xAI returned any
		"answer": result.Answer,
		"usage": map[string]any{
			"prompt_tokens":     result.Usage.PromptTokens,
			"completion_tokens": result.Usage.CompletionTokens,
			"reasoning_tokens":  result.Usage.ReasoningTokens,
			"total_tokens":      result.Usage.TotalTokens,
		},
		"latency_ms": time.Since(t0).Milliseconds(),
		"account":    email,
		// OpenAI Responses-shaped convenience block
		"output": []map[string]any{
			{
				"type":   "search_results",
				"query":  req.Query,
				"status": "completed",
				"results": result.Results,
			},
		},
	}
	if result.Answer != "" {
		out["output"] = append(out["output"].([]map[string]any), map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": result.Answer},
			},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func normalizeSearchSources(in []string) []string {
	if len(in) == 0 {
		return []string{"web", "x"}
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "web", "web_search", "internet":
			s = "web"
		case "x", "x_search", "twitter":
			s = "x"
		default:
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{"web", "x"}
	}
	return out
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
