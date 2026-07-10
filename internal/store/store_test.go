package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsExhaustedWindow(t *testing.T) {
	a := Account{Exhausted: true, ExhaustedAt: time.Now().UTC().Add(-2 * time.Hour)}
	if !a.IsExhausted() {
		t.Fatal("expected exhausted within 24h")
	}
	a.ExhaustedAt = time.Now().UTC().Add(-25 * time.Hour)
	if a.IsExhausted() {
		t.Fatal("expected recovered after 24h")
	}
	a.Exhausted = false
	if a.IsExhausted() {
		t.Fatal("flag false should not be exhausted")
	}
}

func TestRecoverExhaustedAccounts(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := Account{
		ID:          "acc1",
		Label:       "t",
		AccessToken: "tok",
		ClientID:    DefaultClientID,
		Issuer:      DefaultIssuer,
		Exhausted:   true,
		ExhaustedAt: time.Now().UTC().Add(-30 * time.Hour),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.UpsertAccount(old); err != nil {
		t.Fatal(err)
	}
	st.RecoverExhaustedAccounts()
	got, ok := st.GetAccount("acc1")
	if !ok {
		t.Fatal("missing account")
	}
	if got.Exhausted {
		t.Fatal("should have recovered")
	}
}

func TestMarkAndPublicAccounts(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Account{
		ID: "a1", Label: "one", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAccountExhausted("a1"); err != nil {
		t.Fatal(err)
	}
	pub := st.PublicAccounts()
	if len(pub) != 1 {
		t.Fatalf("want 1 account, got %d", len(pub))
	}
	if pub[0]["exhausted"] != true {
		t.Fatalf("exhausted flag: %v", pub[0]["exhausted"])
	}
	if err := st.ResetAccountExhausted("a1"); err != nil {
		t.Fatal(err)
	}
	pub = st.PublicAccounts()
	if pub[0]["exhausted"] != false {
		t.Fatal("reset failed")
	}
}

func TestRecordRequestCost(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(Account{
		ID: "a1", Label: "one", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	err = st.RecordRequest(RequestSample{
		ID: "r1", At: time.Now().UTC().Format(time.RFC3339),
		AccountID: "a1", Model: DefaultModel,
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
		CostUSD: 0.005,
	})
	if err != nil {
		t.Fatal(err)
	}
	u := st.UsageSnapshot()
	if u["a1"].CostUSD <= 0 || u["_global"].CostUSD <= 0 {
		t.Fatalf("cost not recorded: %+v", u)
	}
	if u["a1"].Requests != 1 {
		t.Fatalf("requests=%d", u["a1"].Requests)
	}
}

func TestDefaultAutoRegisterOff(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Settings()
	if s.AutoRegisterEnabled {
		t.Fatal("auto_register should default false")
	}
	if s.AutoRegisterMinActive != 2 || s.AutoRegisterMaxActive != 5 {
		t.Fatalf("min/max: %d %d", s.AutoRegisterMinActive, s.AutoRegisterMaxActive)
	}
	// settings file exists after update
	if err := st.UpdateSettings(func(s *Settings) { s.AutoRegisterEnabled = true }); err != nil {
		t.Fatal(err)
	}
	if !st.Settings().AutoRegisterEnabled {
		t.Fatal("update failed")
	}
	// reload
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Settings().AutoRegisterEnabled {
		t.Fatal("persist failed")
	}
	// ensure settings.json path
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPublicAccountsBadges(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// expired access, no refresh → needs_login
	_ = st.UpsertAccount(Account{
		ID: "sso1", Label: "sso", AccessToken: "x",
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	// expired access, has refresh → not needs_login
	_ = st.UpsertAccount(Account{
		ID: "oauth1", Label: "oauth", AccessToken: "y", RefreshToken: "rt",
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	// exhausted within window
	_ = st.UpsertAccount(Account{
		ID: "ex1", Label: "ex", AccessToken: "z",
		Exhausted: true, ExhaustedAt: time.Now().UTC(),
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	pub := st.PublicAccounts()
	byID := map[string]map[string]any{}
	for _, m := range pub {
		byID[m["id"].(string)] = m
	}
	if byID["sso1"]["needs_login"] != true {
		t.Fatalf("sso needs_login: %+v", byID["sso1"])
	}
	if byID["oauth1"]["needs_login"] != false {
		t.Fatalf("oauth should refresh: %+v", byID["oauth1"])
	}
	if byID["oauth1"]["has_refresh"] != true {
		t.Fatal("has_refresh")
	}
	if byID["ex1"]["exhausted"] != true {
		t.Fatal("exhausted")
	}
}

func TestExhaustedRemaining(t *testing.T) {
	a := Account{Exhausted: true, ExhaustedAt: time.Now().UTC().Add(-2 * time.Hour)}
	rem := a.ExhaustedRemaining()
	if rem < 21*time.Hour || rem > 23*time.Hour {
		t.Fatalf("remaining ~22h, got %v", rem)
	}
	a.ExhaustedAt = time.Now().UTC().Add(-30 * time.Hour)
	if a.ExhaustedRemaining() != 0 {
		t.Fatal("expected 0 after window")
	}
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(Account{
		ID: "ex", AccessToken: "t", Label: "ex",
		Exhausted: true, ExhaustedAt: time.Now().UTC().Add(-1 * time.Hour),
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	pub := st.PublicAccounts()
	var got map[string]any
	for _, m := range pub {
		if m["id"] == "ex" {
			got = m
			break
		}
	}
	if got == nil {
		t.Fatal("missing")
	}
	sec, ok := got["exhausted_remaining_sec"].(int64)
	if !ok || sec < 20*3600 || sec > 24*3600 {
		t.Fatalf("remaining_sec=%v %T", got["exhausted_remaining_sec"], got["exhausted_remaining_sec"])
	}
	if got["exhausted_until"] == "" {
		t.Fatal("exhausted_until empty")
	}
}

func TestChatDenied(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(Account{
		ID: "d1", Label: "denied", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	if err := st.MarkAccountChatDenied("d1", "Forbidden: Access to the chat endpoint is denied"); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetAccount("d1")
	if !ok || !got.IsChatDenied() {
		t.Fatal("expected chat denied")
	}
	pub := st.PublicAccounts()
	var row map[string]any
	for _, m := range pub {
		if m["id"] == "d1" {
			row = m
			break
		}
	}
	if row["chat_denied"] != true {
		t.Fatalf("%+v", row)
	}
	if err := st.ResetAccountExhausted("d1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAccount("d1")
	if got.IsChatDenied() {
		t.Fatal("reset should clear chat denied")
	}
}
