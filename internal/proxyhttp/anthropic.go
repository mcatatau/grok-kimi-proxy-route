package proxyhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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
	_, _, settings, err := s.ensure(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err.Error())
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

	model := asString(req["model"])
	if model == "" {
		model = settings.DefaultModel
	}
	stream, _ := req["stream"].(bool)
	maxTokens := asInt(req["max_tokens"], 4096)
	effort := settings.ReasoningEffort
	if v := asString(req["reasoning_effort"]); v != "" {
		effort = v
	}
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
	raw, _ := json.Marshal(oaBody)

	url := strings.TrimRight(settings.UpstreamBase, "/") + "/chat/completions"

	tried := map[string]bool{}
	maxTries := 5
	for attempt := 0; attempt < maxTries; attempt++ {
		token2, acc2, _, err2 := s.ensure(r.Context())
		if err2 != nil {
			if attempt == maxTries-1 {
				writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err2.Error())
				return
			}
			continue
		}
		if acc2 != nil && tried[acc2.ID] {
			continue
		}

		upReq2, err2 := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(raw))
		if err2 != nil {
			if attempt == maxTries-1 {
				writeAnthropicError(w, 500, "api_error", err2.Error())
				return
			}
			continue
		}
		upReq2.Header.Set("Authorization", "Bearer "+token2)
		upReq2.Header.Set("Content-Type", "application/json")
		upReq2.Header.Set("x-grok-client-version", settings.ClientVersion)
		if stream {
			upReq2.Header.Set("Accept", "text/event-stream")
		} else {
			upReq2.Header.Set("Accept", "application/json")
		}

		resp, err2 := http.DefaultClient.Do(upReq2)
		if err2 != nil {
			if attempt == maxTries-1 {
				writeAnthropicError(w, http.StatusBadGateway, "api_error", err2.Error())
				return
			}
			continue
		}

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			errInfo := diagnoseUpstreamError(resp.StatusCode, b, "/chat/completions", settings.ClientVersion)
			log.Printf("proxyhttp UPSTREAM ERROR [anthropic]: status %d | classification=%s | attempt=%d/%d | account=%s",
				resp.StatusCode, errInfo.Classification, attempt+1, maxTries, labelOr(acc2))

			if errInfo.Classification == "rate_limit" && acc2 != nil {
				log.Printf("proxyhttp account exhausted [anthropic] (switching): %s (%s)", acc2.Label, acc2.Email)
				_ = s.store.MarkAccountExhausted(acc2.ID)
				s.notifyAccountChange()
				tried[acc2.ID] = true
				continue
			}
			if errInfo.Classification == "chat_denied" && acc2 != nil {
				log.Printf("proxyhttp chat denied [anthropic] (switching): %s (%s)", acc2.Label, acc2.Email)
				_ = s.store.MarkAccountChatDenied(acc2.ID, string(b))
				s.notifyAccountChange()
				tried[acc2.ID] = true
				continue
			}

			w.Header().Set("X-Account-Status", errInfo.Classification)
			writeAnthropicError(w, resp.StatusCode, "api_error", string(b))
			return
		}
		defer resp.Body.Close()

		if !stream {
			b, _ := io.ReadAll(resp.Body)
			out, err2 := openAIChatToAnthropicMessage(b, model)
			if err2 != nil {
				writeAnthropicError(w, 500, "api_error", err2.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			if acc2 != nil {
				usage := extractUsageFromPayload(string(b), "", "/chat/completions")
				s.recordUsage(acc2.ID, resp.StatusCode, usage, "/chat/completions", model)
			}
			return
		}

		usage, err2 := streamOpenAIToAnthropic(r.Context(), w, resp.Body, model)
		if err2 != nil {
			_ = err2
		}
		if acc2 != nil && (usage.promptTokens > 0 || usage.totalTokens > 0 || usage.completionTokens > 0) {
			s.recordUsage(acc2.ID, resp.StatusCode, usage, "/chat/completions", model)
		}
		return
	}

	writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "all accounts exhausted")
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

func streamOpenAIToAnthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) (sseUsageCapture, error) {
	var usage sseUsageCapture
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
			return usage, ctx.Err()
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
			if pt := asInt(u["prompt_tokens"], 0); pt > 0 {
				usage.promptTokens = int64(pt)
			}
			if ct := asInt(u["completion_tokens"], 0); ct > 0 {
				usage.completionTokens = int64(ct)
			}
			if tt := asInt(u["total_tokens"], 0); tt > 0 {
				usage.totalTokens = int64(tt)
			}
			if rt := asInt(u["reasoning_tokens"], 0); rt > 0 {
				usage.reasoningTokens = int64(rt)
			}
			if usage.totalTokens == 0 && (usage.promptTokens > 0 || usage.completionTokens > 0) {
				usage.totalTokens = usage.promptTokens + usage.completionTokens + usage.reasoningTokens
			}
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
	if usage.promptTokens == 0 && inputTok > 0 {
		usage.promptTokens = int64(inputTok)
	}
	if usage.completionTokens == 0 && outputTok > 0 {
		usage.completionTokens = int64(outputTok)
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.promptTokens + usage.completionTokens + usage.reasoningTokens
	}
	return usage, sc.Err()
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
