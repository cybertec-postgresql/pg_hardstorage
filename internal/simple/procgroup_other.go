//go:build !unix

package simple

import "os/exec"

// detachFromTerminalSignals is a no-op off unix: there are no process
// groups to split, and cmd.Cancel falls back to Kill there anyway.
func detachFromTerminalSignals(*exec.Cmd) {}
