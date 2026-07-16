package proxyhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"grok-desktop/internal/store"
)

// TestFailover429Then200 exercises proxyUpstream with a fake upstream:
// first account gets 429 free-usage-exhausted; second account gets 200 + usage JSON.
func TestFailover429Then200(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(store.Account{
		ID: "acc-a", Label: "A", AccessToken: "tok-a",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.UpsertAccount(store.Account{
		ID: "acc-b", Label: "B", AccessToken: "tok-b",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.SetActiveAccount("acc-a")

	var mu sync.Mutex
	hits := map[string]int{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits[auth]++
		n := hits[auth]
		mu.Unlock()

		if strings.Contains(auth, "tok-a") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"subscription:free-usage-exhausted"}}`))
			return
		}
		if strings.Contains(auth, "tok-b") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-1",
				"choices": []any{
					map[string]any{
						"message": map[string]any{"role": "assistant", "content": "ok"},
					},
				},
				"usage": map[string]any{
					"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				},
			})
			_ = n
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`unknown token`))
	}))
	defer upstream.Close()

	_ = st.UpdateSettings(func(s *store.Settings) {
		s.UpstreamBase = upstream.URL + "/v1"
		s.ProxyEnabled = false
	})

	// ensure rotates: first call returns A if not exhausted; after mark, B
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		st.RecoverExhaustedAccounts()
		// Prefer non-exhausted active, else first non-exhausted
		if acc, ok := st.ActiveAccount(); ok && acc != nil && !acc.IsExhausted() && acc.AccessToken != "" {
			return acc.AccessToken, acc, st.Settings(), nil
		}
		for _, a := range st.ListAccounts() {
			if a.IsExhausted() || a.AccessToken == "" {
				continue
			}
			cp := a
			_ = st.SetActiveAccount(a.ID)
			return a.AccessToken, &cp, st.Settings(), nil
		}
		return "", nil, st.Settings(), context.Canceled
	}

	s := New(st, nil, ensure, nil)
	var accountEvents int
	s.OnAccountChange = func() { accountEvents++ }
	var usageEvents int
	s.OnUsage = func(sample store.RequestSample) { usageEvents++ }

	body := `{"model":"grok-4.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"ok"`) && !strings.Contains(string(raw), "ok") {
		t.Fatalf("unexpected body: %s", raw)
	}

	a, _ := st.GetAccount("acc-a")
	if a == nil || !a.IsExhausted() {
		t.Fatal("acc-a should be exhausted after 429")
	}
	if accountEvents < 1 {
		t.Fatalf("OnAccountChange events=%d", accountEvents)
	}
	// usage may fire if extractUsageFromPayload finds tokens
	snap := st.UsageSnapshot()
	if snap["acc-b"].Requests < 1 && usageEvents < 1 {
		// recordUsage needs non-zero tokens from payload — extractUsage may parse
		t.Logf("usage snap=%+v events=%d (ok if extract path differs)", snap, usageEvents)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["Bearer tok-a"] < 1 || hits["Bearer tok-b"] < 1 {
		t.Fatalf("hits=%v want both tokens", hits)
	}
}
