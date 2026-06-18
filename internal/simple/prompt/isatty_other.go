//go:build !linux && !darwin

package prompt

import "os"

// isTerminal is the catch-all implementation for platforms without
// a Termios ioctl in this minimal source tree (Windows, freebsd,
// plan9, …).  Returns false unconditionally — the safe default is
// "plain text, no ANSI", which is functionally correct everywhere.
// Operators running the binary on a Windows terminal will see no
// colour, but every prompt + answer still works.
func isTerminal(_ *os.File) bool { return false }
