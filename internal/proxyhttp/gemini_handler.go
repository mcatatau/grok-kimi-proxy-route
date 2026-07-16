package proxyhttp

import (
	"context"
	"encoding/json"
	"net/http"

	"grok-desktop/internal/gemini"
	"grok-desktop/internal/store"
)

// handleGeminiUpstream — public stub (full ADC stays local if present).
func (s *Server) handleGeminiUpstream(w http.ResponseWriter, ctx context.Context, clientPath string, stream bool, body map[string]any, settings store.Settings) {
	_ = s
	_ = ctx
	_ = clientPath
	_ = stream
	gc := gemini.New()
	out, err := gc.ChatCompletions(ctx, settings, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, ok := out["error"]; ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(out)
}
