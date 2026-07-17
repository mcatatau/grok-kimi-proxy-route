package proxyhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesJSONToChatCompletion_ToolCalls(t *testing.T) {
	raw := []byte(`{
		"id": "resp_1",
		"model": "grok-4.5",
		"output": [
			{"type": "reasoning", "id": "rs_1"},
			{"type": "web_search_call", "id": "ws_1", "status": "completed"},
			{
				"type": "function_call",
				"id": "fc_1",
				"call_id": "call_abc",
				"name": "bash",
				"arguments": "{\"command\":\"ls\"}",
				"status": "completed"
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "running"}]
			}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	}`)
	out, err := responsesJSONToChatCompletion(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%v want tool_calls", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["content"] != "running" {
		t.Fatalf("content=%v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len=%d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_abc" {
		t.Fatalf("id=%v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("name=%v", fn["name"])
	}
	if fn["arguments"] != `{"command":"ls"}` {
		t.Fatalf("args=%v", fn["arguments"])
	}
}

func TestPipeResponsesSSEToChat_ToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_xyz","name":"read","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","call_id":"call_xyz","delta":"{\"file"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"Path\":\"a.go\"}"}`,
		``,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","call_id":"call_xyz","arguments":"{\"filePath\":\"a.go\"}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"grok-4.5","output":[{"type":"function_call","id":"fc_1","call_id":"call_xyz","name":"read","arguments":"{\"filePath\":\"a.go\"}"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	if err := pipeResponsesSSEToChat(context.Background(), rec, strings.NewReader(sse), "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) && !strings.Contains(body, `"finish_reason": "tool_calls"`) {
		// json.Marshal has no spaces
		if !strings.Contains(body, `finish_reason":"tool_calls"`) {
			t.Fatalf("missing finish_reason tool_calls in:\n%s", body)
		}
	}
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("missing tool_calls in:\n%s", body)
	}
	if !strings.Contains(body, `"name":"read"`) {
		t.Fatalf("missing tool name in:\n%s", body)
	}
	// arguments deltas should appear (not only full done — done is skipped after deltas)
	if !strings.Contains(body, `file`) {
		t.Fatalf("missing args fragments in:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE] in:\n%s", body)
	}

	// Parse chunks: ensure at least one tool_calls delta with id+name
	foundStart := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if asString(tc["id"]) == "call_xyz" && asString(fn["name"]) == "read" {
				foundStart = true
			}
		}
	}
	if !foundStart {
		t.Fatalf("did not find tool call start chunk in:\n%s", body)
	}
}

func TestPipeResponsesSSEToChat_TextOnlyStop(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	if err := pipeResponsesSSEToChat(context.Background(), rec, strings.NewReader(sse), "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing content:\n%s", body)
	}
	if !strings.Contains(body, `finish_reason":"stop"`) {
		t.Fatalf("want finish stop:\n%s", body)
	}
	if strings.Contains(body, `finish_reason":"tool_calls"`) {
		t.Fatalf("unexpected tool_calls finish:\n%s", body)
	}
}

func TestChatCompletionJSONToResponse_ToolCalls(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl_1",
		"model": "kimi-for-coding",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "bash", "arguments": "{\"command\":\"pwd\"}"}
				}]
			}
		}]
	}`)
	out, err := chatCompletionJSONToResponse(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	output := out["output"].([]any)
	found := false
	for _, rawItem := range output {
		item := rawItem.(map[string]any)
		if item["type"] == "function_call" && item["name"] == "bash" {
			found = true
			if item["call_id"] != "call_1" {
				t.Fatalf("call_id=%v", item["call_id"])
			}
		}
	}
	if !found {
		b, _ := json.Marshal(out)
		t.Fatalf("missing function_call in %s", b)
	}
	_ = bytes.Buffer{}
}
