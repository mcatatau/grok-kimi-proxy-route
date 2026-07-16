package proxyhttp

import (
	"encoding/json"
	"testing"
)

func TestSanitizeResponsesTools_DropsNamespace(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "namespace",
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        "read_file",
						"description": "read a file",
						"parameters":   map[string]any{"type": "object"},
					},
				},
			},
		},
		map[string]any{"type": "function", "name": "bash", "description": "run", "parameters": map[string]any{"type": "object"}},
	}
	out := sanitizeResponsesTools(raw)
	b, _ := json.Marshal(out)
	s := string(b)
	if contains(s, `"namespace"`) {
		t.Fatalf("namespace leaked: %s", s)
	}
	hasWeb, hasX, hasRead, hasBash := false, false, false, false
	for _, item := range out {
		m := item.(map[string]any)
		switch m["type"] {
		case "web_search":
			hasWeb = true
		case "x_search":
			hasX = true
		case "function":
			switch m["name"] {
			case "read_file":
				hasRead = true
			case "bash":
				hasBash = true
			}
		}
	}
	if !hasWeb || !hasX {
		t.Fatalf("missing native search tools: %s", s)
	}
	if !hasRead || !hasBash {
		t.Fatalf("missing function tools: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func TestSanitizeResponsesInput_TextToInputText(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
			},
		},
	}
	out := sanitizeResponsesInput(raw).([]any)
	b, _ := json.Marshal(out)
	s := string(b)
	if contains(s, `"type":"text"`) || contains(s, `"type": "text"`) {
		// loose check — also accept spaced JSON
	}
	if !contains(s, "input_text") {
		t.Fatalf("expected input_text normalization, got %s", s)
	}
	if contains(s, `"type":"text"`) {
		t.Fatalf("raw text type leaked: %s", s)
	}
}

func TestSanitizeResponsesInput_SingleObjectWrap(t *testing.T) {
	raw := map[string]any{"role": "user", "content": "hi"}
	out := sanitizeResponsesInput(raw)
	arr, ok := out.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected array of 1, got %#v", out)
	}
}

func TestSanitizeResponsesInput_LocalShellCallCollapsed(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"type":    "local_shell_call",
			"call_id": "c1",
			"status":  "completed",
			"action":  map[string]any{"type": "exec", "command": []any{"echo", "1"}},
		},
	}
	out := sanitizeResponsesInput(raw).([]any)
	for _, item := range out {
		m := item.(map[string]any)
		if asString(m["type"]) == "local_shell_call" {
			t.Fatalf("local_shell_call not collapsed: %#v", m)
		}
	}
}
