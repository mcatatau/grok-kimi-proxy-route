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
	"strings"
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

	// autoReloginKimi tries HTTP-only re-login for exhausted Kimi accounts so the proxy
	// does not return quota errors to the client while recovery is in progress.
	autoReloginKimi := func(accountID, reason string) bool {
		acc, ok := st.GetAccount(accountID)
		if !ok || acc == nil {
			return false
		}
		// If account has a Google refresh token, try HTTP-only re-login first.
		if gr := strings.TrimSpace(acc.GoogleRefreshToken); gr != "" {
			log.Printf("proxy: quota on %s — trying HTTP Google refresh re-login...", accountID)
			if sess, err := kimi.LoginWithGoogleRefresh(gr); err == nil && sess != nil {
				log.Printf("proxy: HTTP re-login OK for %s (%s)", accountID, sess.Email)
				// Upsert the recovered account
				acc.AccessToken = sess.AccessToken
				acc.RefreshToken = sess.RefreshToken
				acc.GoogleRefreshToken = sess.GoogleRefreshToken
				if sess.Email != "" {
					acc.Email = sess.Email
				}
				acc.ExhaustedAt = time.Time{}
				acc.ExhaustReason = ""
				acc.AuthDeniedAt = time.Time{}
				acc.AuthDeniedReason = ""
				acc.UpdatedAt = time.Now().UTC()
				_ = st.UpsertAccount(*acc)
				// Try to mint a fresh work key immediately
				_ = tryRemintKimiWork(st, acc)
				_ = st.SetActiveAccount(accountID)
				return true
			} else {
				log.Printf("proxy: HTTP Google refresh failed for %s: %v", accountID, err)
			}
		}
		// If account has a web session but no Google refresh, try reminting the key.
		if kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
			log.Printf("proxy: quota on %s — trying remint work key...", accountID)
			if tryRemintKimiWork(st, acc) {
				log.Printf("proxy: remint OK for %s", accountID)
				acc, _ = st.GetAccount(accountID)
				if acc != nil && acc.Usable() {
					_ = st.SetActiveAccount(accountID)
					return true
				}
			} else {
				log.Printf("proxy: remint failed for %s", accountID)
			}
		}
		return false
	}

	srv := proxyhttp.New(st, up, ensure)
	srv.SetForceRefresh(forceRefresh)
	srv.SetQuotaHandler(func(accountID, reason string) bool {
		if acc, ok := st.GetAccount(accountID); ok && acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork {
			// Prefer HTTP re-login of THIS account (keeps pool size; no min-3 requirement).
			if autoReloginKimi(accountID, reason) {
				return true
			}
			// Remote delete on kimi.com when web session exists, but KEEP local row exhausted
			// so Google refresh remains for the next auto re-login cycle.
			if kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
				if _, err := kimi.LogoffWithSession(acc.AccessToken, acc.RefreshToken); err != nil {
					log.Printf("kimi logoff failed for %s: %v — mark exhausted", accountID, err)
				} else {
					log.Printf("kimi logoff OK — remote deleted %s (%s); local row kept for re-login", accountID, acc.Email)
				}
			} else {
				log.Printf("kimi quota: %s has no web session (sk-kimi only?) — mark exhausted", accountID)
			}
		}
		_, _ = st.MarkExhausted(accountID, reason)
		if next := st.NextUsableAccountID(accountID); next != "" {
			_ = st.SetActiveAccount(next)
			// Replenish exhausted account in background (HTTP only in headless proxy).
			go func(id string) {
				if autoReloginKimi(id, reason+" (rotated; replenish)") {
					log.Printf("proxy: replenished Kimi account %s", id)
				}
			}(accountID)
			return true
		}
		// No other usable account — try recover ANY Kimi account via HTTP (blocks until done).
		for _, a := range st.ListAccounts() {
			if a.NormalizedProvider() != store.ProviderKimiWork {
				continue
			}
			if autoReloginKimi(a.ID, reason) {
				return true
			}
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

// tryRemintKimiWork attempts to refresh the web JWT and mint a new sk-kimi WORK key.
// Returns true if a usable key was obtained.
func tryRemintKimiWork(st *store.Store, acc *store.Account) bool {
	if acc == nil {
		return false
	}
	if !kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
		return false
	}
	access, refresh, err := kimi.EnsureAccessToken(acc.AccessToken, acc.RefreshToken)
	if err != nil {
		return false
	}
	minted, err := kimi.MintWorkAPIKey(access, "grok-desktop-proxy")
	if err != nil {
		return false
	}
	acc.APIKey = minted.APIKey
	acc.AccessToken = access
	if refresh != "" {
		acc.RefreshToken = refresh
	}
	if minted.DeviceID != "" {
		acc.DeviceID = minted.DeviceID
	}
	if !minted.ExpiresAt.IsZero() {
		acc.ExpiresAt = minted.ExpiresAt
	}
	acc.ExhaustedAt = time.Time{}
	acc.ExhaustReason = ""
	acc.AuthDeniedAt = time.Time{}
	acc.AuthDeniedReason = ""
	acc.UpdatedAt = time.Now().UTC()
	_ = st.UpsertAccount(*acc)
	_ = st.ClearAuthState(acc.ID)
	return true
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
