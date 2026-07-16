package proxyhttp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sanitizeResponsesInput normalizes OpenAI/Codex Responses `input` so xAI accepts it.
//
// xAI error this fixes:
//
//	422 Failed to deserialize ... untagged enum ModelInput
//
// Known Codex/OpenAI shapes that break xAI:
//   - content part type "text" (must be "input_text")
//   - single message object as `input` (must be string or array)
//   - item types like local_shell_call / computer_call_output that xAI does not model
func sanitizeResponsesInput(raw any) any {
	switch t := raw.(type) {
	case nil:
		return nil
	case string:
		return t
	case map[string]any:
		// Single object → wrap as array (Codex/OpenAI sometimes send one message)
		items := sanitizeResponsesInputItems([]any{t})
		if len(items) == 0 {
			return ""
		}
		if len(items) == 1 {
			if s, ok := items[0].(string); ok {
				return s
			}
		}
		return items
	case []any:
		return sanitizeResponsesInputItems(t)
	default:
		// last resort: stringify
		return fmt.Sprint(t)
	}
}

func sanitizeResponsesInputItems(items []any) []any {
	out := make([]any, 0, len(items)+2)
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": v},
				},
			})
		case map[string]any:
			out = append(out, sanitizeOneInputItem(v)...)
		default:
			// drop unknown scalars/arrays nested incorrectly
			continue
		}
	}
	return out
}

func sanitizeOneInputItem(m map[string]any) []any {
	typ := strings.ToLower(strings.TrimSpace(asString(m["type"])))
	role := strings.ToLower(strings.TrimSpace(asString(m["role"])))

	// Easy message: role + content (type may be empty or "message")
	if typ == "" || typ == "message" {
		if role == "" {
			role = "user"
		}
		// xAI accepts system/user/assistant/developer in our tests
		if role == "tool" {
			// Convert legacy chat-style tool message → function_call_output if possible
			callID := firstNonEmpty(asString(m["tool_call_id"]), asString(m["call_id"]))
			text := contentToPlainText(m["content"])
			if callID == "" {
				callID = "tool_call"
			}
			return []any{map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  text,
			}}
		}
		content := sanitizeMessageContent(m["content"], role)
		if content == nil {
			return nil
		}
		out := map[string]any{
			"type":    "message",
			"role":    role,
			"content": content,
		}
		// preserve optional id when present
		if id := asString(m["id"]); id != "" {
			out["id"] = id
		}
		return []any{out}
	}

	// Supported item types for multi-turn tool loops
	switch typ {
	case "function_call":
		// Keep if it looks complete enough
		name := asString(m["name"])
		callID := firstNonEmpty(asString(m["call_id"]), asString(m["id"]))
		args := m["arguments"]
		if name == "" {
			return nil
		}
		out := map[string]any{
			"type":      "function_call",
			"name":      name,
			"call_id":   callID,
			"arguments": coerceArgsString(args),
		}
		return []any{out}
	case "function_call_output", "custom_tool_call_output":
		callID := firstNonEmpty(asString(m["call_id"]), asString(m["id"]))
		out := map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  contentToPlainText(m["output"]),
		}
		if out["output"] == "" {
			out["output"] = contentToPlainText(m["content"])
		}
		if callID == "" {
			// fallback as user note so the turn still works
			return []any{map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Tool result:\n" + asString(out["output"])},
				},
			}}
		}
		return []any{out}
	case "item_reference":
		// Pass through id references if present
		if id := asString(m["id"]); id != "" {
			return []any{map[string]any{"type": "item_reference", "id": id}}
		}
		return nil
	case "reasoning":
		// xAI may accept reasoning items in history; if not, drop (safe)
		// Prefer drop — encrypted reasoning from OpenAI is useless to xAI and often 422s.
		return nil
	case "web_search_call", "x_search_call", "file_search_call", "code_interpreter_call",
		"mcp_call", "mcp_list_tools", "image_generation_call", "computer_call",
		"computer_call_output", "local_shell_call", "shell_call", "apply_patch_call",
		"custom_tool_call", "code_execution_call":
		// Collapse unsupported tool-call history into a short user note so multi-turn
		// Codex sessions don't hard-fail ModelInput.
		summary := summarizeUnsupportedItem(m)
		if summary == "" {
			return nil
		}
		return []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": summary},
			},
		}}
	default:
		// Unknown type: try treat as message if role+content exist
		if role != "" && m["content"] != nil {
			content := sanitizeMessageContent(m["content"], role)
			if content == nil {
				return nil
			}
			return []any{map[string]any{
				"type":    "message",
				"role":    role,
				"content": content,
			}}
		}
		// Last resort: dump as user text
		b, _ := json.Marshal(m)
		if len(b) == 0 || string(b) == "null" || string(b) == "{}" {
			return nil
		}
		// Avoid huge dumps
		s := string(b)
		if len(s) > 4000 {
			s = s[:4000] + "…"
		}
		return []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "[normalized input item]\n" + s},
			},
		}}
	}
}

func sanitizeMessageContent(content any, role string) any {
	switch c := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(c) == "" {
			return nil
		}
		// Array of parts is more reliable across clients
		return []any{map[string]any{"type": "input_text", "text": c}}
	case []any:
		parts := make([]any, 0, len(c))
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": s})
				}
				continue
			}
			pt := strings.ToLower(strings.TrimSpace(asString(pm["type"])))
			switch pt {
			case "", "text", "input_text", "output_text":
				// Codex sometimes uses "text"; assistant history may use "output_text".
				// xAI ModelInput content wants input_text for easy content parts.
				text := firstNonEmpty(asString(pm["text"]), contentToPlainText(pm["content"]))
				if text == "" {
					continue
				}
				// For assistant messages, input_text still works on xAI in practice.
				parts = append(parts, map[string]any{"type": "input_text", "text": text})
			case "input_image", "image_url", "image":
				// Keep image parts when possible; normalize keys
				img := map[string]any{"type": "input_image"}
				if u := asString(pm["image_url"]); u != "" {
					img["image_url"] = u
				} else if u := asString(pm["url"]); u != "" {
					img["image_url"] = u
				} else if nested, ok := pm["image_url"].(map[string]any); ok {
					if u := asString(nested["url"]); u != "" {
						img["image_url"] = u
					}
				} else if u := asString(pm["image"]); u != "" {
					img["image_url"] = u
				}
				if asString(img["image_url"]) == "" {
					continue
				}
				parts = append(parts, img)
			case "input_file", "file":
				// Drop files for now (often 422 / unsupported on xAI proxy path)
				continue
			case "refusal":
				text := asString(pm["refusal"])
				if text == "" {
					text = asString(pm["text"])
				}
				if text != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": text})
				}
			default:
				// Unknown part → plain text dump of text field if any
				if text := asString(pm["text"]); text != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": text})
				}
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return parts
	default:
		s := contentToPlainText(c)
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": s}}
	}
}

func contentToPlainText(c any) string {
	switch t := c.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m, ok := p.(map[string]any); ok {
				if s := asString(m["text"]); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
					continue
				}
			}
			if s, ok := p.(string); ok && s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	case map[string]any:
		if s := asString(t["text"]); s != "" {
			return s
		}
		if s := asString(t["output"]); s != "" {
			return s
		}
		b, _ := json.Marshal(t)
		return string(b)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func coerceArgsString(args any) string {
	switch t := args.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(t) == "" {
			return "{}"
		}
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

func summarizeUnsupportedItem(m map[string]any) string {
	typ := asString(m["type"])
	name := firstNonEmpty(asString(m["name"]), firstNonEmpty(asString(m["call_id"]), asString(m["id"])))
	// Try common result fields
	for _, k := range []string{"output", "result", "stdout", "content", "arguments", "action"} {
		if v, ok := m[k]; ok && v != nil {
			s := contentToPlainText(v)
			if s == "" {
				continue
			}
			if len(s) > 2000 {
				s = s[:2000] + "…"
			}
			return fmt.Sprintf("[prior %s %s]\n%s", typ, name, s)
		}
	}
	if typ != "" {
		return fmt.Sprintf("[prior %s %s]", typ, name)
	}
	return ""
}
