//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so a timeout reaches grandchildren
// too (Node only signalled the direct child — the `sh` — leaving the real worker running).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
