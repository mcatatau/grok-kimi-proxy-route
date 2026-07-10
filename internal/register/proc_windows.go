//go:build windows

package register

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// hideConsoleWindow prevents a black console flash for python.exe child processes.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NO_WINDOW
	cmd.SysProcAttr.CreationFlags |= 0x08000000
	cmd.SysProcAttr.HideWindow = true
}

// killProcessTree terminates cmd and all descendants (Chrome launched by DrissionPage).
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// /T = tree, /F = force
	c := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	hideConsoleWindow(c)
	_ = c.Run()
	_ = cmd.Process.Kill()
}

// killHint is appended to cancel/timeout messages on this OS.
func killHint() string {
	return fmt.Sprintf(" (process tree killed)")
}
