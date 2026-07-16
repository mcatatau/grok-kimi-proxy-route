package proxyhttp

import (
	"context"
	"net/http"
	"strings"

	"grok-desktop/internal/store"
)

// isCodexRequest detects OpenAI Codex CLI / Codex VS Code clients.
// Only these clients are forced onto the proxy global DefaultModel.
func isCodexRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	checks := []string{
		r.Header.Get("User-Agent"),
		r.Header.Get("user-agent"),
		r.Header.Get("Originator"),
		r.Header.Get("originator"),
		r.Header.Get("X-Client-Name"),
		r.Header.Get("x-client-name"),
		r.Header.Get("X-Title"),
		r.Header.Get("x-title"),
		r.Header.Get("OpenAI-Project"),
		r.Header.Get("openai-project"),
	}
	for _, h := range checks {
		low := strings.ToLower(h)
		if strings.Contains(low, "codex") {
			return true
		}
	}
	// Some builds send a dedicated header.
	if v := strings.TrimSpace(r.Header.Get("X-Codex")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return true
	}
	return false
}

// tokenAccountForSettings returns credentials matching the (possibly re-routed) provider.
// Does not mutate the global active provider in the store.
func tokenAccountForSettings(
	s *Server,
	ctx context.Context,
	token string,
	acc *store.Account,
	settings store.Settings,
) (string, *store.Account) {
	switch {
	case settings.IsOllie():
		return store.OllieAPIKey, &store.Account{
			ID: "ollie", Label: "OllieChat", Email: "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
		}
	case settings.IsGemini():
		return store.GeminiCredMarker, &store.Account{
			ID: "gemini-adc", Label: "Gemini (ADC)", Email: settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
		}
	default:
		// Need a real xAI token. If current ensure already produced one, keep it.
		if token != "" && token != store.OllieAPIKey && token != store.GeminiCredMarker {
			return token, acc
		}
		// Try store usable xAI account without switching active provider permanently.
		if s == nil || s.store == nil {
			return token, acc
		}
		if a, ok := s.store.PreferUsableAccount(); ok && a != nil && a.AccessToken != "" {
			return a.AccessToken, a
		}
		if a, ok := s.store.ActiveAccount(); ok && a != nil && a.AccessToken != "" &&
			a.AccessToken != store.OllieAPIKey && a.AccessToken != store.GeminiCredMarker {
			return a.AccessToken, a
		}
		// Last resort: full ensure (may follow current global provider).
		if tok2, acc2, _, err := s.ensure(ctx); err == nil && tok2 != "" &&
			tok2 != store.OllieAPIKey && tok2 != store.GeminiCredMarker {
			return tok2, acc2
		}
		return token, acc
	}
}
