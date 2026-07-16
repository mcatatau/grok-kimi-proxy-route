package signup

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"grok-desktop/internal/darkemail"
)

type ProgressFunc func(step, total int, message string)

type Result struct {
	Email    string
	Password string
	OTP      string
	Given    string
	Family   string
	Notes    []string
	// Raw isolate payload when browser path used
	IsolateRaw map[string]any
}

type Options struct {
	Email       string
	Password    string
	Given       string
	Family      string
	MailTimeout time.Duration
	Progress    ProgressFunc
	Castle      CastleProvider
	// TokenOnly: only mint castle token
	TokenOnly bool
	// Prefer in-page RPCs via isolate (recommended — same TLS/cookies as Chrome)
	InBrowser bool
}

// RunEmailSignup drives the email path.
// Default robust path: InBrowser=true → node isolate does create-code/verify in page.
func RunEmailSignup(ctx context.Context, opt Options) (*Result, error) {
	progress := opt.Progress
	if progress == nil {
		progress = func(int, int, string) {}
	}

	mail := darkemail.New()
	progress(1, 6, "probe darkemail")
	if err := mail.Probe(ctx); err != nil {
		return nil, fmt.Errorf("darkemail probe: %w", err)
	}

	email := opt.Email
	if email == "" {
		email = mail.GenEmail()
	}
	password := opt.Password
	if password == "" {
		password = genPassword()
	}
	given := opt.Given
	if given == "" {
		given = "Alex"
	}
	family := opt.Family
	if family == "" {
		family = "Rivera"
	}

	// default castle provider
	iso, _ := opt.Castle.(*IsolateCastle)
	if opt.Castle == nil {
		iso = &IsolateCastle{Headless: false, Timeout: 120 * time.Second, Email: email}
		opt.Castle = iso
	}
	if iso == nil {
		// wrap unknown provider — InBrowser needs IsolateCastle
		if opt.InBrowser {
			iso = &IsolateCastle{Headless: false, Timeout: 120 * time.Second, Email: email}
		}
	} else {
		iso.emailEnsure(email)
	}

	if opt.TokenOnly {
		progress(2, 6, "castle isolate token")
		tok, err := opt.Castle.CreateRequestToken(ctx)
		if err != nil {
			return nil, err
		}
		return &Result{
			Email: email, Password: password, Given: given, Family: family,
			Notes: []string{"token_only", fmt.Sprintf("castle_len=%d", len(tok)), "prefix=" + prefix(tok, 24)},
		}, nil
	}

	// -------- preferred path: everything anti-bot in browser --------
	if opt.InBrowser || iso != nil {
		if iso == nil {
			iso = &IsolateCastle{Headless: false, Timeout: 4 * time.Minute, Email: email}
		}
		iso.emailEnsure(email)
		progress(2, 6, "browser full signup (castle+create+otp+verify)")
		res, err := iso.FullInBrowser(ctx, email, password, given, family)
		out := &Result{Email: email, Password: password, Given: given, Family: family}
		if res != nil {
			if res.Email != "" {
				out.Email = res.Email
			}
			if res.OTP != "" {
				out.OTP = res.OTP
			}
			out.Notes = append(out.Notes, res.RawNotes...)
			if res.Message != "" {
				out.Notes = append(out.Notes, res.Message)
			}
			if res.Step != "" {
				out.Notes = append(out.Notes, "step="+res.Step)
			}
			if res.TokenLen > 0 {
				out.Notes = append(out.Notes, fmt.Sprintf("token_len=%d", res.TokenLen))
			}
			if res.Error != "" {
				out.Notes = append(out.Notes, "err="+res.Error)
			}
		}
		if err != nil {
			return out, err
		}
		out.Notes = append(out.Notes, "in_browser_ok")
		return out, nil
	}

	// -------- fallback: Go HTTP RPC (often CF-blocked) --------
	progress(2, 6, "castle token (fallback http path)")
	acc, err := NewAccountsClient(opt.Castle)
	if err != nil {
		return nil, err
	}
	progress(3, 6, "bootstrap accounts.x.ai")
	if err := acc.Bootstrap(ctx); err != nil {
		progress(3, 6, "bootstrap warn: "+err.Error())
	}
	progress(4, 6, "CreateEmailValidationCode "+email)
	if err := acc.CreateEmailValidationCode(ctx, email); err != nil {
		return &Result{Email: email, Password: password, Given: given, Family: family, Notes: []string{"create_code_failed:" + err.Error()}}, err
	}
	mailTimeout := opt.MailTimeout
	if mailTimeout <= 0 {
		mailTimeout = 3 * time.Minute
	}
	progress(5, 6, "waiting OTP darkemail")
	msg, err := mail.WaitForMessage(ctx, email, mailTimeout)
	if err != nil {
		return &Result{Email: email, Password: password, Given: given, Family: family, Notes: []string{"mail_timeout"}}, err
	}
	otp := darkemail.ExtractOTP(msg)
	if otp == "" {
		return &Result{Email: email, Password: password, Given: given, Family: family, Notes: []string{"no_otp", "subject=" + msg.Subject}}, fmt.Errorf("OTP not found")
	}
	progress(6, 6, "VerifyEmailValidationCode")
	if err := acc.VerifyEmailValidationCode(ctx, email, otp); err != nil {
		return &Result{Email: email, Password: password, OTP: otp, Given: given, Family: family, Notes: []string{"verify_failed:" + err.Error()}}, err
	}
	return &Result{
		Email: email, Password: password, OTP: otp, Given: given, Family: family,
		Notes: []string{"email_verified_http", "credentials_pending"},
	}, nil
}

func (p *IsolateCastle) emailEnsure(email string) {
	if p != nil && p.Email == "" {
		p.Email = email
	}
}

func genPassword() string {
	return "Gx" + randStr(12) + "!9aA"
}

func randStr(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// silence unused import if strings only used in older draft
var _ = strings.Contains
