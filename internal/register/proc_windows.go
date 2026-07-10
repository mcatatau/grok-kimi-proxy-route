//go:build windows

package register

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// hideConsoleWindow prevents a black console flash for short-lived python/pip/taskkill.
// Do NOT use this for the signup bot when Chrome must be visible — CREATE_NO_WINDOW /
// HideWindow can prevent GUI child processes (Chrome) from showing a window.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NO_WINDOW
	cmd.SysProcAttr.CreationFlags |= 0x08000000
	cmd.SysProcAttr.HideWindow = true
}

// allowGUIChildren leaves console inheritance alone so Chromium can open a normal window.
// Prefer this for the long-running signup bot (headless=false).
func allowGUIChildren(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// No CREATE_NO_WINDOW / HideWindow — Chrome needs a visible desktop session.
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
