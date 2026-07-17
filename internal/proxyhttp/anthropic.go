package proxyhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/store"
)

// handleMessages implements Anthropic Messages API: POST /v1/messages
// Translates to upstream chat/completions (with streaming translation).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	if !s.gateAnthropic(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json")
		return
	}

	// Route by client model only — never UI global provider.
	reqModel := asString(req["model"])
	settings := s.store.Settings().WithProviderForModel(reqModel)
	ctx := WithRouteProvider(r.Context(), settings.NormalizedProvider())
	token, acc, settings2, err := s.ensure(ctx)
	if err != nil {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	settings = settings2.WithProvider(settings.NormalizedProvider())
	token, acc = tokenAccountForSettings(s, ctx, token, acc, settings)
	model := settings.ResolveModelForClient(reqModel)
	_ = acc
	stream, _ := req["stream"].(bool)
	maxTokens := asInt(req["max_tokens"], 4096)
	effort := settings.ReasoningEffort
	if v := asString(req["reasoning_effort"]); v != "" {
		effort = v
	}
	// Anthropic thinking / effort aliases
	if th, ok := req["thinking"].(map[string]any); ok {
		if t := asString(th["type"]); t == "enabled" {
			if effort == "" {
				effort = "high"
			}
		}
	}

	messages := anthropicToOpenAIMessages(req)
	messages = injectTemporalIntoMessages(messages)

	oaBody := map[string]any{
		"model":            model,
		"messages":         messages,
		"stream":           stream,
		"max_tokens":       maxTokens,
		"reasoning_effort": effort,
	}
	if stream {
		oaBody["stream_options"] = map[string]any{"include_usage": true}
	}
	if tools := sanitizeChatTools(req["tools"]); len(tools) > 0 {
		oaBody["tools"] = tools
		oaBody["tool_choice"] = mapAnthropicToolChoice(req["tool_choice"])
	}
	if t, ok := req["temperature"].(float64); ok {
		oaBody["temperature"] = t
	}
	if t, ok := req["top_p"].(float64); ok {
		oaBody["top_p"] = t
	}
	// Ollie natively supports Anthropic /v1/messages — passthrough is more faithful.
	if settings.IsOllie() {
		s.proxyOllieAnthropic(w, r, token, settings, body, stream)
		return
	}

	raw, _ := json.Marshal(oaBody)

	url := strings.TrimRight(settings.EffectiveUpstream(), "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		writeAnthropicError(w, 500, "api_error", err.Error())
		return
	}
	setUpstreamAuthHeaders(upReq, token, settings)
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeAnthropicError(w, resp.StatusCode, "api_error", string(b))
		return
	}

	if !stream {
		b, _ := io.ReadAll(resp.Body)
		out, err := openAIChatToAnthropicMessage(b, model)
		if err != nil {
			writeAnthropicError(w, 500, "api_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}

	if err := streamOpenAIToAnthropic(r.Context(), w, resp.Body, model); err != nil {
		// client likely disconnected
		_ = err
	}
}

func (s *Server) gateAnthropic(r *http.Request) bool {
	key := s.store.Settings().ProxyAPIKey
	if key == "" {
		return true
	}
	// Anthropic: x-api-key or Authorization: Bearer
	if r.Header.Get("x-api-key") == key {
		return true
	}
	return s.gate(r)
}

// proxyOllieAnthropic forwards Anthropic Messages JSON as-is to OllieChat.
func (s *Server) proxyOllieAnthropic(w http.ResponseWriter, r *http.Request, token string, settings store.Settings, body []byte, stream bool) {
	// Ensure required max_tokens for Anthropic shape.
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if _, ok := m["max_tokens"]; !ok {
			m["max_tokens"] = 4096
		}
		reqModel, _ := m["model"].(string)
		// Anthropic Ollie passthrough: honor client model (no Codex force here).
		m["model"] = settings.ResolveModelForClient(reqModel)
		if stream {
			m["stream"] = true
		}
		body, _ = json.Marshal(m)
	}
	url := strings.TrimRight(settings.EffectiveUpstream(), "/") + "/messages"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, 500, "api_error", err.Error())
		return
	}
	setUpstreamAuthHeaders(upReq, token, settings)
	upReq.Header.Set("anthropic-version", "2023-06-01")
	if v := r.Header.Get("anthropic-version"); v != "" {
		upReq.Header.Set("anthropic-version", v)
	}
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer resp.Body.Close()
	// Pass through status + body (JSON or SSE).
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

func anthropicToOpenAIMessages(req map[string]any) []any {
	out := make([]any, 0, 8)
	// system can be string or array of text blocks
	if sys := req["system"]; sys != nil {
		text := anthropicContentToText(sys)
		if text != "" {
			out = append(out, map[string]any{"role": "system", "content": text})
		}
	}
	msgs, _ := req["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			continue
		}
		// tool_result messages → OpenAI tool role
		if role == "user" {
			// content may include tool_result blocks
			if blocks, ok := m["content"].([]any); ok {
				var textParts []string
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					switch asString(bm["type"]) {
					case "tool_result":
						out = append(out, map[string]any{
							"role":         "tool",
							"tool_call_id": asString(bm["tool_use_id"]),
							"content":      anthropicContentToText(bm["content"]),
						})
					case "text":
						textParts = append(textParts, asString(bm["text"]))
					default:
						if t := asString(bm["text"]); t != "" {
							textParts = append(textParts, t)
						}
					}
				}
				if len(textParts) > 0 {
					out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
				}
				continue
			}
		}
		if role == "assistant" {
			if blocks, ok := m["content"].([]any); ok {
				var textParts []string
				var toolCalls []any
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					switch asString(bm["type"]) {
					case "text":
						textParts = append(textParts, asString(bm["text"]))
					case "tool_use":
						args := bm["input"]
						argStr, _ := json.Marshal(args)
						toolCalls = append(toolCalls, map[string]any{
							"id":   asString(bm["id"]),
							"type": "function",
							"function": map[string]any{
								"name":      asString(bm["name"]),
								"arguments": string(argStr),
							},
						})
					}
				}
				msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
				if len(toolCalls) > 0 {
					msg["tool_calls"] = toolCalls
				}
				out = append(out, msg)
				continue
			}
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": anthropicContentToText(m["content"]),
		})
	}
	return out
}

func anthropicContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			switch t := b.(type) {
			case string:
				parts = append(parts, t)
			case map[string]any:
				if asString(t["type"]) == "text" || t["type"] == nil {
					if s := asString(t["text"]); s != "" {
						parts = append(parts, s)
					}
				} else if s := asString(t["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func mapAnthropicToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "any", "none":
			if t == "any" {
				return "required"
			}
			return t
		default:
			return "auto"
		}
	case map[string]any:
		// {"type":"tool","name":"foo"} → OpenAI tool choice
		if asString(t["type"]) == "tool" {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": asString(t["name"]),
				},
			}
		}
	}
	return "auto"
}

func openAIChatToAnthropicMessage(raw []byte, model string) ([]byte, error) {
	var oa map[string]any
	if err := json.Unmarshal(raw, &oa); err != nil {
		return nil, err
	}
	id := "msg_" + uuid.NewString()
	content := make([]any, 0, 2)
	stopReason := "end_turn"
	choices, _ := oa["choices"].([]any)
	if len(choices) > 0 {
		ch, _ := choices[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		if msg != nil {
			if t := asString(msg["content"]); t != "" {
				content = append(content, map[string]any{"type": "text", "text": t})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
				stopReason = "tool_use"
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					fn, _ := tcm["function"].(map[string]any)
					var input any = map[string]any{}
					_ = json.Unmarshal([]byte(asString(fn["arguments"])), &input)
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    asString(tcm["id"]),
						"name":  asString(fn["name"]),
						"input": input,
					})
				}
			}
		}
		if fr := asString(ch["finish_reason"]); fr == "tool_calls" {
			stopReason = "tool_use"
		} else if fr == "length" {
			stopReason = "max_tokens"
		}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u, ok := oa["usage"].(map[string]any); ok {
		usage["input_tokens"] = asInt(u["prompt_tokens"], 0)
		usage["output_tokens"] = asInt(u["completion_tokens"], 0)
	}
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
	return json.Marshal(out)
}

func streamOpenAIToAnthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	msgID := "msg_" + uuid.NewString()
	// message_start
	_ = writeSSEJSON(fw, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}, json.Marshal)

	textStarted := false
	textIndex := 0
	toolIndex := 0
	toolStarted := map[string]int{} // id → content block index
	inputTok, outputTok := 0, 0
	stopReason := "end_turn"

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			inputTok = asInt(u["prompt_tokens"], inputTok)
			outputTok = asInt(u["completion_tokens"], outputTok)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if fr := asString(ch["finish_reason"]); fr != "" {
			if fr == "tool_calls" {
				stopReason = "tool_use"
			} else if fr == "length" {
				stopReason = "max_tokens"
			} else {
				stopReason = "end_turn"
			}
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		// reasoning → thinking-ish text (emit as text for compatibility)
		if rc := asString(delta["reasoning_content"]); rc != "" {
			if !textStarted {
				_ = writeSSEJSON(fw, "content_block_start", map[string]any{
					"type": "content_block_start", "index": textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}, json.Marshal)
				textStarted = true
			}
			_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textIndex,
				"delta": map[string]any{"type": "text_delta", "text": rc},
			}, json.Marshal)
		}
		if t := asString(delta["content"]); t != "" {
			if !textStarted {
				_ = writeSSEJSON(fw, "content_block_start", map[string]any{
					"type": "content_block_start", "index": textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}, json.Marshal)
				textStarted = true
			}
			_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textIndex,
				"delta": map[string]any{"type": "text_delta", "text": t},
			}, json.Marshal)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				id := asString(tcm["id"])
				fn, _ := tcm["function"].(map[string]any)
				name := asString(fn["name"])
				args := asString(fn["arguments"])
				idx, ok := toolStarted[id]
				if !ok || id == "" {
					// new tool block
					if textStarted {
						_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
							"type": "content_block_stop", "index": textIndex,
						}, json.Marshal)
						textStarted = false
						toolIndex = textIndex + 1
					}
					if id == "" {
						id = "toolu_" + uuid.NewString()
					}
					idx = toolIndex
					toolStarted[id] = idx
					toolIndex++
					_ = writeSSEJSON(fw, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": map[string]any{},
						},
					}, json.Marshal)
					stopReason = "tool_use"
				}
				if name != "" {
					// name only on start; ignore subsequent
				}
				if args != "" {
					_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					}, json.Marshal)
				}
			}
		}
	}

	// close open blocks
	if textStarted {
		_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": textIndex,
		}, json.Marshal)
	}
	for _, idx := range toolStarted {
		_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": idx,
		}, json.Marshal)
	}

	_ = writeSSEJSON(fw, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTok},
	}, json.Marshal)
	_ = writeSSEJSON(fw, "message_stop", map[string]any{"type": "message_stop"}, json.Marshal)
	// final blank
	_, _ = io.WriteString(fw, "\n")
	_ = inputTok
	_ = time.Now()
	return sc.Err()
}

func asInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return def
	}
}
