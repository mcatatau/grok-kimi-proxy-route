package proxyhttp

import (
	"testing"

	"grok-desktop/internal/pricing"
	"grok-desktop/internal/store"
)

func TestDiagnoseRateLimit(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{429, `{"error":"too many"}`, "rate_limit"},
		{402, `payment`, "rate_limit"},
		{401, `unauthorized`, "auth_error"},
		{403, `version mismatch`, "client_version"},
		{403, `forbidden`, "auth_error"},
		{403, `Forbidden: Access to the chat endpoint is denied. Please ensure you're using the correct credentials.`, "chat_denied"},
		{403, `please log into console.x.ai and update the permissions`, "chat_denied"},
		{500, `boom`, "server_error"},
	}
	for _, c := range cases {
		got := diagnoseUpstreamError(c.status, []byte(c.body), "/chat/completions", "0.2.93")
		if got.Classification != c.want {
			t.Fatalf("status %d: got %s want %s", c.status, got.Classification, c.want)
		}
	}
}

func TestRecordUsageViaServer(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(store.Account{
		ID: "acc", Label: "L", AccessToken: "t",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	s := &Server{store: st}
	var hooked bool
	s.OnUsage = func(sample store.RequestSample) {
		hooked = true
		if sample.CostUSD <= 0 {
			t.Errorf("expected cost > 0, got %v", sample.CostUSD)
		}
		if sample.AccountID != "acc" {
			t.Errorf("account %s", sample.AccountID)
		}
	}
	s.recordUsage("acc", 200, sseUsageCapture{
		promptTokens: 1000, completionTokens: 200, totalTokens: 1200,
	}, "/chat/completions", store.DefaultModel)
	if !hooked {
		t.Fatal("OnUsage not called")
	}
	// pricing sanity
	cost := pricing.CostUSD(store.DefaultModel, 1000, 200, 0, 0)
	if cost <= 0 {
		t.Fatal("pricing zero")
	}
	snap := st.UsageSnapshot()
	if snap["acc"].Requests != 1 {
		t.Fatalf("requests=%d", snap["acc"].Requests)
	}
}

func TestNotifyAccountChange(t *testing.T) {
	s := &Server{}
	var n int
	s.OnAccountChange = func() { n++ }
	s.notifyAccountChange()
	if n != 1 {
		t.Fatal(n)
	}
}
