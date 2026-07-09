package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ToolCall is an OpenAI-style function call (legacy chat wire shape).
type ToolCall struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func strField(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// temporalContextLine tells the model the real present date (dynamic; e.g. 2026).
func temporalContextLine() string {
	now := time.Now()
	return fmt.Sprintf(
		"Temporal context: today is %s. The current year is %d. Treat this as ground truth for \"today\", \"this year\", recency, and time-sensitive answers — do not assume you are stuck in 2023–2024.",
		now.Format("Monday, January 2, 2006"),
		now.Year(),
	)
}

// ensureTemporalContext prepends/merges the current date/year into a system message.
func ensureTemporalContext(msgs []ChatMessage) []ChatMessage {
	line := temporalContextLine()
	if len(msgs) > 0 && msgs[0].Role == "system" {
		if !strings.Contains(msgs[0].Content, "Temporal context:") {
			msgs[0].Content = line + "\n\n" + msgs[0].Content
		}
		return msgs
	}
	return append([]ChatMessage{{Role: "system", Content: line}}, msgs...)
}

// AccumUsage merges usage counters.
func AccumUsage(dst *Usage, src *Usage) *Usage {
	if src == nil {
		return dst
	}
	if dst == nil {
		cp := *src
		return &cp
	}
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.CachedTokens += src.CachedTokens
	dst.TotalTokens += src.TotalTokens
	if dst.TotalTokens == 0 {
		dst.TotalTokens = dst.PromptTokens + dst.CompletionTokens
	}
	return dst
}
