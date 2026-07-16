package signup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IsolateCastle runs a tiny Chrome (via the Node CDP helper) only long
// enough to capture Castle token and/or run in-page signup RPCs.
type IsolateCastle struct {
	Script    string
	ChromeExe string
	PageURL   string
	Timeout   time.Duration
	// Headless: only when true passes --headless (default headed — more reliable)
	Headless bool
	WorkDir  string
	Email    string
}

type isolateResult struct {
	OK           bool   `json:"ok"`
	Token        string `json:"token"`
	Error        string `json:"error"`
	Sitekey      string `json:"sitekey"`
	CastlePK     string `json:"castlePk"`
	HasTurnstile bool   `json:"hasTurnstile"`
	Cookies      string `json:"cookies"`
	Note         string `json:"note"`
	Mode         string `json:"mode"`
	Email        string `json:"email"`
	OTP          string `json:"otp"`
	TokenLen     int    `json:"tokenLen"`
	Message      string `json:"message"`
	// nested raw for create/verify
	Create  json.RawMessage `json:"create"`
	Verify  json.RawMessage `json:"verify"`
	Step    string          `json:"step"`
	RawNotes []string       `json:"notes"`
}

func (p *IsolateCastle) resolveScript() (string, error) {
	script := p.Script
	if script != "" {
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			return script, nil
		}
	}
	candidates := []string{
		filepath.Join("scripts", "castle_isolate", "capture.mjs"),
		filepath.Join("..", "scripts", "castle_isolate", "capture.mjs"),
		filepath.Join("..", "..", "scripts", "castle_isolate", "capture.mjs"),
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append([]string{
			filepath.Join(wd, "scripts", "castle_isolate", "capture.mjs"),
		}, candidates...)
	}
	// also relative to this source tree via executable path is overkill
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("castle isolate script not found")
}

func (p *IsolateCastle) run(ctx context.Context, mode string, extra []string) (*isolateResult, error) {
	script, err := p.resolveScript()
	if err != nil {
		return nil, err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	page := p.PageURL
	if page == "" {
		page = DefaultAccountsBase + "/sign-up?redirect=grok-com&return_to=%2F"
	}

	args := []string{script, "--mode", mode, "--url", page}
	if p.ChromeExe != "" {
		args = append(args, "--chrome", p.ChromeExe)
	}
	if p.Headless {
		args = append(args, "--headless")
	}
	if p.Email != "" {
		args = append(args, "--email", p.Email)
	}
	args = append(args, extra...)

	cmd := exec.CommandContext(ctx, "node", args...)
	if p.WorkDir != "" {
		cmd.Dir = p.WorkDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// still try parse json
		if res, perr := parseIsolateJSON(stdout.String()); perr == nil && res != nil {
			if res.Error != "" {
				return res, fmt.Errorf("%s", res.Error)
			}
			return res, fmt.Errorf("isolate exit: %w", err)
		}
		return nil, fmt.Errorf("isolate run: %w; stderr=%s stdout=%s", err, truncate(stderr.String(), 400), truncate(stdout.String(), 400))
	}
	res, err := parseIsolateJSON(stdout.String())
	if err != nil {
		return nil, fmt.Errorf("%v; stderr=%s", err, truncate(stderr.String(), 300))
	}
	return res, nil
}

func parseIsolateJSON(stdout string) (*isolateResult, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var raw string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") {
			raw = line
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("no json from isolate")
	}
	var res isolateResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, fmt.Errorf("parse isolate json: %w (%s)", err, truncate(raw, 200))
	}
	return &res, nil
}

func (p *IsolateCastle) CreateRequestToken(ctx context.Context) (string, error) {
	res, err := p.run(ctx, "token", nil)
	if err != nil {
		return "", err
	}
	if !res.OK || res.Token == "" {
		if res.Error != "" {
			return "", fmt.Errorf("isolate: %s", res.Error)
		}
		return "", fmt.Errorf("isolate returned empty token")
	}
	return res.Token, nil
}

// CreateCodeInBrowser runs castle+CreateEmailValidationCode entirely inside Chrome.
func (p *IsolateCastle) CreateCodeInBrowser(ctx context.Context, email string) (*isolateResult, error) {
	p.Email = email
	res, err := p.run(ctx, "create-code", nil)
	if res == nil {
		return nil, err
	}
	if err != nil && !res.OK {
		return res, err
	}
	if !res.OK {
		if res.Error != "" {
			return res, fmt.Errorf("%s", res.Error)
		}
		return res, fmt.Errorf("create-code failed")
	}
	return res, nil
}

// FullInBrowser: create-code + darkemail OTP + verify (+ credentials best-effort).
func (p *IsolateCastle) FullInBrowser(ctx context.Context, email, password, given, family string) (*isolateResult, error) {
	p.Email = email
	extra := []string{}
	if password != "" {
		extra = append(extra, "--password", password)
	}
	if given != "" {
		extra = append(extra, "--given", given)
	}
	if family != "" {
		extra = append(extra, "--family", family)
	}
	// full mode can wait mail up to 3m — bump timeout
	old := p.Timeout
	if p.Timeout < 4*time.Minute {
		p.Timeout = 4 * time.Minute
	}
	res, err := p.run(ctx, "full", extra)
	p.Timeout = old
	if res == nil {
		return nil, err
	}
	if err != nil && !res.OK {
		return res, err
	}
	if !res.OK {
		if res.Error != "" {
			return res, fmt.Errorf("%s", res.Error)
		}
		return res, fmt.Errorf("full signup failed")
	}
	return res, nil
}

// StaticCastle is for replaying a captured token (debug only; tokens expire fast).
type StaticCastle struct {
	Token string
}

func (s StaticCastle) CreateRequestToken(context.Context) (string, error) {
	if s.Token == "" {
		return "", fmt.Errorf("empty static castle token")
	}
	return s.Token, nil
}
