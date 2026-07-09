package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

type Client struct {
	HTTP *http.Client
}

func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 0, // streaming
		},
	}
}

type ChatRequest struct {
	Model              string        `json:"model"`
	Messages           []ChatMessage `json:"messages"`
	Input              string        `json:"input,omitempty"`
	Stream             bool          `json:"stream"`
	ReasoningEffort    string        `json:"reasoning_effort"`
	PreviousResponseID string        `json:"previous_response_id"`
	LastResponseID     string        `json:"last_response_id"`
	APIMode            string        `json:"api_mode"` // chat | responses
	Temperature        float64       `json:"temperature"`
	MaxTokens          int           `json:"max_tokens"`
	// WebSearch runs DuckDuckGo before the model call and injects results.
	WebSearch bool   `json:"web_search"`
	SearchQuery string `json:"search_query"` // optional override; default = last user message
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type StreamEvent struct {
	Type      string `json:"type"` // thinking | content | usage | done | error | meta | metrics
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	Model     string `json:"model,omitempty"`
	ID        string `json:"id,omitempty"`
	Account   string `json:"account,omitempty"`
	Email     string `json:"email,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	TTFTMs    int64  `json:"ttft_ms,omitempty"`
	Estimated bool   `json:"estimated,omitempty"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	APIMode     string `json:"api_mode,omitempty"`
	Root        string `json:"root,omitempty"`
}

func (c *Client) baseURL(s store.Settings) string {
	b := strings.TrimRight(s.UpstreamBase, "/")
	if b == "" {
		b = store.DefaultUpstream
	}
	return b
}

func (c *Client) authHeaders(token, version string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream, application/json")
	if version == "" {
		version = store.DefaultClientVersion
	}
	h.Set("x-grok-client-version", version)
	h.Set("User-Agent", "grok-desktop/"+version)
	return h
}

func (c *Client) ListModels(ctx context.Context, token string, settings store.Settings) ([]ModelInfo, error) {
	url := c.baseURL(settings) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.authHeaders(token, settings.ClientVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("models HTTP %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(parsed.Data)*2)
	for _, m := range parsed.Data {
		out = append(out, ModelInfo{
			ID: m.ID, Name: m.Name, Description: m.Description, APIMode: "chat",
		})
		out = append(out, ModelInfo{
			ID: m.ID + "-responses", Name: m.Name + " (Responses)",
			Description: m.Description + " — multi-turn token saving",
			APIMode:     "responses", Root: m.ID,
		})
	}
	return out, nil
}

func stripResponsesSuffix(model string) (string, bool) {
	m := strings.TrimSpace(model)
	low := strings.ToLower(m)
	switch {
	case strings.HasSuffix(low, "-responses"):
		return m[:len(m)-len("-responses")], true
	case strings.HasSuffix(low, "@responses"):
		return m[:len(m)-len("@responses")], true
	case strings.HasSuffix(low, "/responses"):
		return m[:len(m)-len("/responses")], true
	case strings.HasPrefix(low, "responses/"):
		return m[len("responses/"):], true
	default:
		return m, false
	}
}

func extractPrevID(req ChatRequest) string {
	for _, v := range []string{req.PreviousResponseID, req.LastResponseID} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// StreamChat emits thinking/content/usage/done events.
func (c *Client) StreamChat(
	ctx context.Context,
	token string,
	settings store.Settings,
	accountLabel string,
	accountEmail string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	model, forceResp := stripResponsesSuffix(req.Model)
	if model == "" {
		model = settings.DefaultModel
	}
	effort := req.ReasoningEffort
	if effort == "" {
		effort = settings.ReasoningEffort
	}
	mode := strings.ToLower(req.APIMode)
	if mode == "" {
		mode = strings.ToLower(settings.APIMode)
	}
	useResponses := forceResp || mode == "responses" || extractPrevID(req) != ""

	emit(StreamEvent{
		Type:    "meta",
		Account: accountLabel,
		Email:   accountEmail,
		Model:   model,
	})

	if useResponses {
		return c.streamResponses(ctx, token, settings, model, effort, req, emit)
	}
	return c.streamChatCompletions(ctx, token, settings, model, effort, req, emit)
}

func (c *Client) streamChatCompletions(
	ctx context.Context,
	token string,
	settings store.Settings,
	model, effort string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	body := map[string]any{
		"model":            model,
		"messages":         req.Messages,
		"stream":           true,
		"reasoning_effort": effort,
		// Critical: without this many providers omit usage on SSE streams
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	raw, _ := json.Marshal(body)
	url := c.baseURL(settings) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header = c.authHeaders(token, settings.ClientVersion)

	t0 := time.Now()
	var ttftMs int64
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat HTTP %d: %s", resp.StatusCode, string(b))
	}

	var usage *Usage
	var id, outModel string
	var contentLen, thinkLen int
	promptEst := estimatePromptTokens(req.Messages)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if id == "" {
			if v, ok := chunk["id"].(string); ok {
				id = v
			}
		}
		if outModel == "" {
			if v, ok := chunk["model"].(string); ok {
				outModel = v
			}
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			ch, _ := choices[0].(map[string]any)
			delta, _ := ch["delta"].(map[string]any)
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				thinkLen += len(rc)
				emit(StreamEvent{Type: "thinking", Text: rc, ID: id, Model: outModel})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				contentLen += len(ct)
				emit(StreamEvent{Type: "content", Text: ct, ID: id, Model: outModel})
			}
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = parseChatUsage(u)
		}
	}
	lat := time.Since(t0).Milliseconds()
	estimated := false
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0) {
		// Fallback estimate ~4 chars/token
		usage = &Usage{
			PromptTokens:     promptEst,
			CompletionTokens: int64((contentLen + 3) / 4),
			ReasoningTokens:  int64((thinkLen + 3) / 4),
			TotalTokens:      0,
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
		estimated = true
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	emit(StreamEvent{
		Type: "usage", Usage: usage, ID: id, Model: outModel,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	emit(StreamEvent{
		Type: "done", ID: id, Model: outModel, Usage: usage,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	return sc.Err()
}

func (c *Client) streamResponses(
	ctx context.Context,
	token string,
	settings store.Settings,
	model, effort string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	prev := extractPrevID(req)
	var input any
	if strings.TrimSpace(req.Input) != "" {
		input = req.Input
	} else if len(req.Messages) > 0 {
		// only last user turn when chaining
		msgs := req.Messages
		if prev != "" {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "user" {
					msgs = []ChatMessage{msgs[i]}
					break
				}
			}
		}
		input = messagesToResponsesInput(msgs)
	} else {
		return fmt.Errorf("no input/messages")
	}

	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": true,
		"reasoning": map[string]any{
			"effort":  effort,
			"summary": "auto",
		},
	}
	if settings.StoreResponses || prev != "" {
		body["store"] = true
	}
	if prev != "" {
		body["previous_response_id"] = prev
	}
	raw, _ := json.Marshal(body)
	url := c.baseURL(settings) + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header = c.authHeaders(token, settings.ClientVersion)

	t0 := time.Now()
	var ttftMs int64
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("responses HTTP %d: %s", resp.StatusCode, string(b))
	}

	var usage *Usage
	var id, outModel string
	var contentLen, thinkLen int
	promptEst := estimatePromptTokens(req.Messages)
	if strings.TrimSpace(req.Input) != "" {
		promptEst = int64((len(req.Input) + 3) / 4)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventName string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		typ, _ := obj["type"].(string)
		if typ == "" {
			typ = eventName
		}
		switch typ {
		case "response.created", "response.in_progress":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					id = v
				}
				if v, ok := r["model"].(string); ok {
					outModel = v
				}
			}
		case "response.reasoning_summary_text.delta":
			if d, ok := obj["delta"].(string); ok && d != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				thinkLen += len(d)
				emit(StreamEvent{Type: "thinking", Text: d, ID: id, Model: outModel})
			}
		case "response.output_text.delta":
			if d, ok := obj["delta"].(string); ok && d != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				contentLen += len(d)
				emit(StreamEvent{Type: "content", Text: d, ID: id, Model: outModel})
			}
		case "response.completed":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					id = v
				}
				if v, ok := r["model"].(string); ok {
					outModel = v
				}
				if u, ok := r["usage"].(map[string]any); ok {
					usage = parseResponsesUsage(u)
				}
			}
		}
		eventName = ""
	}
	lat := time.Since(t0).Milliseconds()
	estimated := false
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		usage = &Usage{
			PromptTokens:     promptEst,
			CompletionTokens: int64((contentLen + 3) / 4),
			ReasoningTokens:  int64((thinkLen + 3) / 4),
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
		estimated = true
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	emit(StreamEvent{
		Type: "usage", Usage: usage, ID: id, Model: outModel,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	emit(StreamEvent{
		Type: "done", ID: id, Model: outModel, Usage: usage,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	return sc.Err()
}

func messagesToResponsesInput(msgs []ChatMessage) any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "system" {
			role = "user"
		}
		partType := "input_text"
		if m.Role == "assistant" {
			partType = "output_text"
		}
		out = append(out, map[string]any{
			"role": role,
			"content": []map[string]any{
				{"type": partType, "text": m.Content},
			},
		})
	}
	return out
}

func parseChatUsage(u map[string]any) *Usage {
	out := &Usage{
		PromptTokens:     asInt64(u["prompt_tokens"]),
		CompletionTokens: asInt64(u["completion_tokens"]),
		TotalTokens:      asInt64(u["total_tokens"]),
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = asInt64(d["reasoning_tokens"])
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		out.CachedTokens = asInt64(d["cached_tokens"])
	}
	return out
}

func parseResponsesUsage(u map[string]any) *Usage {
	out := &Usage{
		PromptTokens:     asInt64(u["input_tokens"]),
		CompletionTokens: asInt64(u["output_tokens"]),
		TotalTokens:      asInt64(u["total_tokens"]),
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = asInt64(d["reasoning_tokens"])
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		out.CachedTokens = asInt64(d["cached_tokens"])
	}
	return out
}

func estimatePromptTokens(msgs []ChatMessage) int64 {
	n := 0
	for _, m := range msgs {
		n += len(m.Role) + len(m.Content) + 8
	}
	return int64((n + 3) / 4)
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// Ensure client used with deadline for non-stream
func init() {
	_ = time.Second
}
