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

// FullSignupResult is the parsed JSON from capture.mjs --mode full.
type FullSignupResult struct {
	OK       bool     `json:"ok"`
	Step     string   `json:"step"`
	Error    string   `json:"error"`
	Email    string   `json:"email"`
	Password string   `json:"password"`
	OTP      string   `json:"otp"`
	Given    string   `json:"given"`
	Family   string   `json:"family"`
	Message  string   `json:"message"`
	Cookies  string   `json:"cookies"`
	Notes    []string `json:"notes"`
	FinalUI  struct {
		Href string `json:"href"`
		Text string `json:"text"`
	} `json:"finalUi"`
	Credentials struct {
		Submitted  bool   `json:"submitted"`
		Redirected bool   `json:"redirected"`
		HrefAfter  string `json:"hrefAfter"`
		Cookie     string `json:"cookie"`
		Text       string `json:"text"`
	} `json:"credentials"`
}

// RunFullCapture executes the node isolate full signup and returns structured result.
func RunFullCapture(ctx context.Context, script, email, password, given, family string, headless bool, progress func(string)) (*FullSignupResult, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if script == "" {
		var err error
		script, err = resolveCaptureScript()
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("capture script: %w", err)
	}

	args := []string{script, "--mode", "full", "--mail-timeout", "180000"}
	if headless {
		args = append(args, "--headless")
	}
	if email != "" {
		args = append(args, "--email", email)
	}
	if password != "" {
		args = append(args, "--password", password)
	}
	if given != "" {
		args = append(args, "--given", given)
	}
	if family != "" {
		args = append(args, "--family", family)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()
	}

	progress("starting chrome isolate signup")
	cmd := exec.CommandContext(ctx, "node", args...)
	if root := findGoModRoot(script); root != "" {
		cmd.Dir = root
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res, perr := parseFullJSON(stdout.String())
	if res != nil {
		if res.Email != "" {
			progress("signup email " + res.Email)
		}
		if res.OTP != "" {
			progress("otp " + res.OTP)
		}
		if res.FinalUI.Href != "" {
			progress("final " + res.FinalUI.Href)
		}
	}
	if err != nil {
		if res != nil && res.Error != "" {
			return res, fmt.Errorf("%s", res.Error)
		}
		return res, fmt.Errorf("capture exit: %w; stderr=%s", err, truncate(stderr.String(), 400))
	}
	if perr != nil {
		return nil, fmt.Errorf("parse capture output: %w; stderr=%s", perr, truncate(stderr.String(), 300))
	}
	if res == nil {
		return nil, fmt.Errorf("empty capture result")
	}
	if !res.OK {
		if strings.Contains(res.FinalUI.Href, "grok.com") || strings.Contains(res.Cookies, "x-userid=") {
			res.OK = true
		}
	}
	if !res.OK && res.Error != "" {
		return res, fmt.Errorf("%s", res.Error)
	}
	if !res.OK {
		return res, fmt.Errorf("signup incomplete step=%s", res.Step)
	}
	return res, nil
}

func parseFullJSON(stdout string) (*FullSignupResult, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var raw string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		line = strings.TrimPrefix(line, "\ufeff")
		if strings.HasPrefix(line, "{") {
			raw = line
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("no json line in capture stdout")
	}
	var res FullSignupResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func resolveCaptureScript() (string, error) {
	candidates := []string{
		filepath.Join("scripts", "castle_isolate", "capture.mjs"),
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "scripts", "castle_isolate", "capture.mjs"),
			filepath.Join(wd, "..", "scripts", "castle_isolate", "capture.mjs"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "scripts", "castle_isolate", "capture.mjs"),
			filepath.Join(dir, "..", "scripts", "castle_isolate", "capture.mjs"),
			filepath.Join(dir, "..", "..", "scripts", "castle_isolate", "capture.mjs"),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("scripts/castle_isolate/capture.mjs not found")
}

func findGoModRoot(start string) string {
	dir := filepath.Dir(start)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ExtractUserIDFromCookies parses x-userid=... from a cookie string.
func ExtractUserIDFromCookies(cookies string) string {
	for _, part := range strings.Split(cookies, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "x-userid=") {
			return strings.TrimPrefix(part, "x-userid=")
		}
	}
	return ""
}
