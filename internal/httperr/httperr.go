package httperr

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Format builds a short, UI-safe error from an HTTP status and body.
// Never returns multi-kilobyte HTML (e.g. Google robot 404 pages).
func Format(prefix string, status int, contentType string, body []byte) string {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") || looksLikeHTML(body) {
		return fmt.Sprintf("%s HTTP %d: upstream returned an HTML error page (not a model response). Check model id, project, ADC, and Vertex access.", prefix, status)
	}
	if msg, ok := parseJSONError(body); ok {
		return fmt.Sprintf("%s HTTP %d: %s", prefix, status, truncate(msg, 400))
	}
	return fmt.Sprintf("%s HTTP %d: %s", prefix, status, truncate(string(body), 300))
}

func looksLikeHTML(b []byte) bool {
	s := strings.TrimSpace(string(b))
	if len(s) < 15 {
		return false
	}
	low := strings.ToLower(s[:min(200, len(s))])
	return strings.HasPrefix(low, "<!doctype html") ||
		strings.HasPrefix(low, "<html") ||
		strings.Contains(low, "<title>error") ||
		strings.Contains(low, "that's an error")
}

func parseJSONError(b []byte) (string, bool) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", false
	}
	// OpenAI style
	if errObj, ok := m["error"].(map[string]any); ok {
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return msg, true
		}
		if msg, ok := errObj["error"].(string); ok && msg != "" {
			return msg, true
		}
	}
	// Google style
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if len(s) <= n {
		return s
	}
	// avoid cutting mid-rune
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
