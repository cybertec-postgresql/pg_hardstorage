//go:build linux || darwin

package prompt

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f points at a TTY.  Re-implements the
// minimal IsTerminal needed here so we don't pull in
// golang.org/x/term as a dep for one syscall.  Falls back to false
// on any error — the safe behaviour is "no ANSI, plain text".
//
// Linux + macOS use the Termios ioctl; getTermiosIoctl is pinned
// per-OS in isatty_linux.go / isatty_darwin.go.  Other platforms
// (Windows, freebsd, plan9, ...) use the stub in isatty_other.go.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(getTermiosIoctl),
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}
