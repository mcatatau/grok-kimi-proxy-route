//go:build windows

package register

import "os/exec"

func prepareProcessGroup(cmd *exec.Cmd) {
	// Windows uses taskkill /T for tree kill; no process group setup needed.
}
