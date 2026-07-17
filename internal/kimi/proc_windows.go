//go:build windows

package kimi

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow prevents a black console flash when launching node/playwright.
// Uses HideWindow only (no CREATE_NO_WINDOW) so Chromium child can still show.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

// fullyHideConsoleWindow uses CREATE_NO_WINDOW for headless mode.
func fullyHideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
