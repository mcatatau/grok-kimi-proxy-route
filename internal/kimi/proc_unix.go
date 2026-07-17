//go:build !windows

package kimi

import "os/exec"

func hideConsoleWindow(cmd *exec.Cmd) {}
func fullyHideConsoleWindow(cmd *exec.Cmd) {}
