package register

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Progress represents a step event from the signup bot.
type Progress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// Result is the final outcome from the bot.
type Result struct {
	Status      string            `json:"status"` // "success" | "error"
	Reason      string            `json:"reason,omitempty"`
	Step        string            `json:"step,omitempty"`
	Screenshot  string            `json:"screenshot,omitempty"`
	Creds       map[string]string `json:"creds,omitempty"` // email, name, password, provider
	AccessToken string            `json:"access_token,omitempty"`
}

// Runner manages the Python signup bot subprocess (DrissionPage).
type Runner struct {
	PythonPath     string // default "python3" / "python" on Windows
	BotDir         string // path to grok-signup-bot/
	CredsDir       string // directory to save auto_creds.json
	EmailProviders []string
	DuckMailURL    string
	DuckMailKey    string
}

// New creates a Runner with sensible defaults.
func New(pythonPath, botDir string) *Runner {
	if pythonPath == "" {
		if runtime.GOOS == "windows" {
			pythonPath = "python"
		} else {
			pythonPath = "python3"
		}
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
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return &Result{
			Status: "error",
			Reason: fmt.Sprintf("bot script missing: %s (set bot_dir / ship grok-signup-bot next to the exe)", script),
			Step:   "start",
		}, nil
	}
	if _, err := exec.LookPath(r.PythonPath); err != nil {
		// absolute path may not be on PATH
		if st, err2 := os.Stat(r.PythonPath); err2 != nil || st.IsDir() {
			return &Result{
				Status: "error",
				Reason: fmt.Sprintf("python not found: %q (install Python 3 + pip install -r grok-signup-bot/requirements.txt, or set python_path in settings)", r.PythonPath),
				Step:   "start",
			}, nil
		}
	}

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

	// On Windows, `py -3 script.py` needs the -3 flag before the script.
	var cmd *exec.Cmd
	base := strings.ToLower(filepath.Base(r.PythonPath))
	if runtime.GOOS == "windows" && (base == "py" || base == "py.exe") {
		cmd = exec.CommandContext(ctx, r.PythonPath, append([]string{"-3"}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, r.PythonPath, args...)
	}
	cmd.Dir = r.BotDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("register stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("register stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return &Result{
			Status: "error",
			Reason: fmt.Sprintf("failed to start bot: %v (python=%s script=%s)", err, r.PythonPath, script),
			Step:   "start",
		}, nil
	}

	var (
		stderrMu   sync.Mutex
		stderrBuf  strings.Builder
		resultCh   = make(chan *Result, 1)
		creds      map[string]string
	)

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

	// Capture stderr for diagnostics (import errors, chrome missing, etc.)
	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 0 {
			stderrMu.Lock()
			stderrBuf.Write(b)
			stderrMu.Unlock()
			// log truncated
			msg := string(b)
			if len(msg) > 2000 {
				msg = msg[:2000] + "…"
			}
			log.Printf("register stderr: %s", msg)
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

	waitErr := cmd.Wait()

	if result == nil {
		stderrMu.Lock()
		errText := strings.TrimSpace(stderrBuf.String())
		stderrMu.Unlock()
		reason := "no result from bot"
		if waitErr != nil {
			reason = fmt.Sprintf("no result from bot (exit: %v)", waitErr)
		}
		if errText != "" {
			// Keep reason short for UI; full stderr already logged
			snip := errText
			if len(snip) > 400 {
				snip = snip[:400] + "…"
			}
			snip = strings.ReplaceAll(snip, "\r\n", " ")
			snip = strings.ReplaceAll(snip, "\n", " ")
			reason = reason + ": " + snip
		} else {
			reason = reason + fmt.Sprintf(" (python=%s bot_dir=%s — check Python/DrissionPage/Chrome install)", r.PythonPath, r.BotDir)
		}
		result = &Result{Status: "error", Reason: reason, Step: "start"}
	}
	return result, nil
}
