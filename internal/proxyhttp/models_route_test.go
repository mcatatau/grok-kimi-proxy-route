package proxyhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func TestHandleModelsUnifiedCatalog(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
		Route string `json:"route"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Route != "model" {
		t.Fatalf("route=%q want model", out.Route)
	}
	seen := map[string]string{}
	for _, m := range out.Data {
		seen[m.ID] = m.Provider
	}
	if seen["grok-4.5"] != store.ProviderXAI {
		t.Fatalf("missing grok-4.5: %#v", seen)
	}
	if seen["kimi-for-coding"] != store.ProviderKimiWork {
		t.Fatalf("missing kimi-for-coding: %#v", seen)
	}
	if seen["k3-agent"] != store.ProviderKimiWork {
		t.Fatalf("missing k3-agent: %#v", seen)
	}
}

func TestWithProviderForModelRouting(t *testing.T) {
	base := store.Settings{Provider: store.ProviderXAI}
	k := base.WithProviderForModel("kimi-for-coding")
	if !k.IsKimiWork() {
		t.Fatalf("want kimi, got %s", k.NormalizedProvider())
	}
	if k.APIMode != "chat" {
		t.Fatalf("kimi api mode %q", k.APIMode)
	}
	g := base.WithProviderForModel("grok-4.5")
	if !g.IsXAI() {
		t.Fatalf("want xai, got %s", g.NormalizedProvider())
	}
}
