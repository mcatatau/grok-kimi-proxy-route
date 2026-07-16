// Package gemini provides a minimal public stub for Vertex/Gemini ADC.
// Full local ADC implementations may exist only on developer machines.
package gemini

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"grok-desktop/internal/store"
)

type Client struct {
	HTTP *http.Client
}

func New() *Client {
	return &Client{HTTP: http.DefaultClient}
}

func ListModels(ctx context.Context, settings store.Settings) []string {
	_ = ctx
	_ = settings
	return []string{
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-2.0-flash-001",
	}
}

func (c *Client) StreamEvents(
	ctx context.Context,
	settings store.Settings,
	model string,
	messages []map[string]any,
	emit func(kind, text string),
	effort string,
) error {
	_ = c
	_ = ctx
	_ = settings
	_ = model
	_ = messages
	_ = effort
	if emit != nil {
		emit("content", "Gemini ADC is not configured in this build.")
	}
	return nil
}

func (c *Client) ChatCompletions(ctx context.Context, settings store.Settings, body map[string]any) (map[string]any, error) {
	_ = c
	_ = ctx
	_ = settings
	_ = body
	return map[string]any{
		"error": map[string]any{
			"message": "Gemini ADC is not configured in this build",
			"type":    "proxy_error",
			"code":    "gemini_stub",
		},
	}, nil
}

func NormalizeModel(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndex(id, "/models/"); i >= 0 {
		return id[i+len("/models/"):]
	}
	return id
}

var ErrLocalOnly = fmt.Errorf("gemini stub")
