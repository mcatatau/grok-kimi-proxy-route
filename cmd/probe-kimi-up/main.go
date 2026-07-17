package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"grok-desktop/internal/store"
)

func main() {
	st, err := store.Open("")
	if err != nil {
		panic(err)
	}
	defer st.Close()
	accs := st.ListAccountsForProvider(store.ProviderKimiWork)
	if len(accs) == 0 {
		fmt.Println("no kimi accounts")
		return
	}
	a := accs[0]
	tok := a.BearerToken()
	fmt.Printf("id=%s denied=%v reason=%q usable=%v\n", a.ID, a.AuthDenied(), a.AuthDeniedReason, a.Usable())
	fmt.Printf("token_prefix=%s len=%d\n", trunc(tok, 20), len(tok))
	body, _ := json.Marshal(map[string]any{
		"model": "kimi-for-coding", "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "Say OK only"}},
	})
	req, _ := http.NewRequest(http.MethodPost, store.KimiWorkUpstream+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		fmt.Println("err", err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Println("upstream", resp.StatusCode)
	if len(b) > 700 {
		fmt.Println(string(b[:700]))
	} else {
		fmt.Println(string(b))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
