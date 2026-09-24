//go:build unix

package simple

import (
	"os/exec"
	"syscall"
)

// detachFromTerminalSignals starts cmd in its own process group so a
// terminal Ctrl-C reaches only this binary, which forwards exactly one
// interrupt via cmd.Cancel (see flowStream.Run).
func detachFromTerminalSignals(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
