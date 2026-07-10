package register

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
)

// Progress represents a step event from the signup bot.
type Progress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// Result is the final outcome from the bot.
type Result struct {
	Status      string            `json:"status"`     // "success" | "error"
	Reason      string            `json:"reason,omitempty"`
	Step        string            `json:"step,omitempty"`
	Screenshot  string            `json:"screenshot,omitempty"`
	Creds       map[string]string `json:"creds,omitempty"` // email, name, password, provider
	AccessToken string            `json:"access_token,omitempty"`
}

// Runner manages the Python signup bot subprocess (DrissionPage).
type Runner struct {
	PythonPath     string // default "python3"
	BotDir         string // path to grok-signup-bot/
	CredsDir       string // directory to save auto_creds.json
	EmailProviders []string
	DuckMailURL    string
	DuckMailKey    string
}

// New creates a Runner with sensible defaults.
func New(pythonPath, botDir string) *Runner {
	if pythonPath == "" {
		pythonPath = "python3"
	}
	return &Runner{PythonPath: pythonPath, BotDir: botDir}
}

// CreateAccount runs the signup bot with the given verification URL.
// It streams progress events via the progress callback and returns
// the final Result. The context timeout should match the device grant TTL.
func (r *Runner) CreateAccount(
	ctx context.Context,
	verificationURL string,
	userCode string,
	progress func(Progress),
) (*Result, error) {
	script := filepath.Join(r.BotDir, "grok_signup.py")
	args := []string{
		script,
		"--verification-url", verificationURL,
		"--headless", "false",
	}
	if userCode != "" {
		args = append(args, "--user-code", userCode)
	}
	if r.CredsDir != "" {
		args = append(args, "--creds-dir", r.CredsDir)
	}
	if len(r.EmailProviders) > 0 {
		joined := r.EmailProviders[0]
		for i := 1; i < len(r.EmailProviders); i++ {
			joined += "," + r.EmailProviders[i]
		}
		args = append(args, "--email-providers", joined)
	}
	if r.DuckMailURL != "" {
		args = append(args, "--duckmail-url", r.DuckMailURL)
	}
	if r.DuckMailKey != "" {
		args = append(args, "--duckmail-key", r.DuckMailKey)
	}

	cmd := exec.CommandContext(ctx, r.PythonPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("register stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("register stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("register start: %w", err)
	}

	resultCh := make(chan *Result, 1)
	var creds map[string]string
	go func() {
		defer close(resultCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for sc.Scan() {
			line := sc.Text()
			if len(line) < 2 {
				continue
			}
			if line == "__STEP__ done" {
				continue
			}
			if len(line) > 9 && line[:9] == "__STEP__ " {
				step := line[9:]
				p := Progress{Step: step}
				if progress != nil {
					progress(p)
				}
				continue
			}
			if len(line) > 9 && line[:9] == "__CREDS__" && line[9] == ' ' {
				payload := line[10:]
				var c map[string]string
				if err := json.Unmarshal([]byte(payload), &c); err != nil {
					log.Printf("register: bad creds json: %v", err)
					continue
				}
				creds = c
				continue
			}
			if len(line) > 10 && line[:10] == "__RESULT__" && line[10] == ' ' {
				payload := line[11:]
				var res Result
				if err := json.Unmarshal([]byte(payload), &res); err != nil {
					log.Printf("register: bad result json: %v", err)
					continue
				}
				res.Creds = creds
				log.Printf("register: got __RESULT__ status=%s", res.Status)
				resultCh <- &res
				return
			}
			log.Printf("register stdout: %s", line)
		}
		if err := sc.Err(); err != nil {
			log.Printf("register: stdout scan: %v", err)
		} else {
			log.Printf("register: stdout EOF (no __RESULT__)")
		}
	}()

	// Capture stderr for diagnostics
	go func() {
		stderrData, _ := io.ReadAll(stderr)
		if len(stderrData) > 0 {
			log.Printf("register stderr: %s", string(stderrData))
		}
	}()

	// Wait for result or context cancellation
	var result *Result
	select {
	case res := <-resultCh:
		result = res
	case <-ctx.Done():
		result = &Result{Status: "error", Reason: "timeout/cancelled"}
	}

	// Clean up
	_ = cmd.Wait()

	if result == nil {
		result = &Result{Status: "error", Reason: "no result from bot"}
	}
	return result, nil
}