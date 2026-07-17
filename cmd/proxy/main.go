// Headless OpenAI-compatible proxy (same AppData store as Grok Desktop).
// Use this when you want the proxy to stay up without the GUI window.
//
//	go run ./cmd/proxy
//	go build -o grok-proxy.exe ./cmd/proxy
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"grok-desktop/internal/kimi"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	st, err := store.Open("")
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	oa := oauth.New()
	if v := st.Settings().ClientVersion; v != "" {
		oa.CLIVersion = v
	}
	up := upstream.New()

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return ensureCreds(ctx, st, oa, "", false)
	}
	forceRefresh := func(ctx context.Context, id string) (string, *store.Account, store.Settings, error) {
		return ensureCreds(ctx, st, oa, id, true)
	}

	srv := proxyhttp.New(st, up, ensure)
	srv.SetForceRefresh(forceRefresh)
	srv.SetQuotaHandler(func(accountID, reason string) bool {
		// Kimi: delete consumer account on kimi.com when quota ends (web JWT), then drop local.
		if acc, ok := st.GetAccount(accountID); ok && acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork {
			if kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
				if _, err := kimi.LogoffWithSession(acc.AccessToken, acc.RefreshToken); err != nil {
					log.Printf("kimi logoff failed for %s: %v — mark exhausted", accountID, err)
				} else {
					log.Printf("kimi logoff OK — deleted remote account %s (%s)", accountID, acc.Email)
					_ = st.RemoveAccount(accountID)
					if next := st.NextUsableAccountID(accountID); next != "" {
						_ = st.SetActiveAccount(next)
						return true
					}
					return false
				}
			} else {
				log.Printf("kimi quota: %s has no web session (sk-kimi only?) — mark exhausted", accountID)
			}
		}
		_, _ = st.MarkExhausted(accountID, reason)
		if next := st.NextUsableAccountID(accountID); next != "" {
			_ = st.SetActiveAccount(next)
			return true
		}
		return false
	})
	srv.SetAuthFailHandler(func(accountID, reason string) bool {
		_, _ = st.MarkAuthDenied(accountID, reason)
		if next := st.NextUsableAccountID(accountID); next != "" {
			_ = st.SetActiveAccount(next)
			return true
		}
		return false
	})

	settings := st.Settings()
	listen := settings.ProxyListen
	if listen == "" {
		listen = "127.0.0.1:8787"
	}
	if err := srv.Start(listen); err != nil {
		listen = "127.0.0.1:8788"
		if err2 := srv.Start(listen); err2 != nil {
			log.Fatalf("listen: %v / fallback: %v", err, err2)
		}
	}
	log.Printf("grok-proxy headless: http://%s  provider=%s model=%s",
		srv.Addr(), settings.NormalizedProvider(), settings.ResolveModel("default"))
	log.Printf("store: %s", st.Root())
	log.Printf("Ctrl+C to stop")

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
	fmt.Println("stopped")
}

func ensureCreds(
	ctx context.Context,
	st *store.Store,
	oa *oauth.Client,
	preferID string,
	forceRefresh bool,
) (string, *store.Account, store.Settings, error) {
	settings := st.Settings()
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = settings.WithProvider(rp)
	}
	if settings.IsOllie() {
		acc := &store.Account{
			ID: "ollie", Label: "OllieChat", Email: "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
		}
		return store.OllieAPIKey, acc, settings, nil
	}
	if settings.IsGemini() {
		acc := &store.Account{
			ID:          "gemini-adc",
			Label:       "Gemini (ADC)",
			Email:       settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
		}
		return store.GeminiCredMarker, acc, settings, nil
	}
	if settings.IsKimiWork() {
		_ = st.PreferHealthyActive()
		acc, ok := st.PreferUsableAccountForProvider(store.ProviderKimiWork)
		if preferID != "" {
			if a, ok2 := st.GetAccount(preferID); ok2 && a != nil && a.NormalizedProvider() == store.ProviderKimiWork {
				if !a.Exhausted() && !a.AuthDenied() {
					acc, ok = a, true
				}
			}
		}
		if !ok || acc == nil {
			return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work")
		}
		if cur, _ := st.ActiveAccount(); cur == nil || cur.NormalizedProvider() != store.ProviderKimiWork || cur.ID != acc.ID {
			_ = st.SetActiveAccount(acc.ID)
		}
		return acc.BearerToken(), acc, settings, nil
	}
	if n, err := st.SyncFromGrokCLI(); err == nil && n > 0 {
		_ = st.PreferHealthyActive()
	}
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = st.Settings().WithProvider(rp)
	} else {
		settings = st.Settings()
	}

	var acc *store.Account
	var ok bool
	if preferID != "" {
		acc, ok = st.GetAccount(preferID)
	}
	if !ok || acc == nil {
		acc, ok = st.PreferUsableAccountForProvider(store.ProviderXAI)
	}
	if !ok || acc == nil {
		return "", nil, settings, fmt.Errorf("nenhuma conta — faça login no Grok Desktop")
	}
	if oauth.BotFlagged(acc.AccessToken) {
		_, _ = st.MarkAuthDenied(acc.ID, "bot_flag_source")
		if next := st.NextUsableAccountID(acc.ID); next != "" {
			return ensureCreds(ctx, st, oa, next, false)
		}
		return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag)")
	}
	need := forceRefresh || acc.ExpiresSoon(5*time.Minute) || acc.Expired()
	if need && acc.RefreshToken != "" {
		tok, err := oa.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
		if err == nil {
			acc.AccessToken = tok.AccessToken
			if tok.RefreshToken != "" {
				acc.RefreshToken = tok.RefreshToken
			}
			claims := oauth.ParseAccessClaims(tok.AccessToken)
			if !claims.Exp.IsZero() {
				acc.ExpiresAt = claims.Exp
			} else {
				acc.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
			}
			_ = st.UpsertAccount(*acc)
			_ = st.ClearAuthDenied(acc.ID)
		} else if forceRefresh || acc.Expired() {
			if next := st.NextUsableAccountID(acc.ID); next != "" {
				return ensureCreds(ctx, st, oa, next, false)
			}
			return "", nil, settings, fmt.Errorf("token expirado: %v", err)
		}
	}
	_ = st.SetActiveAccount(acc.ID)
	return acc.AccessToken, acc, settings, nil
}
