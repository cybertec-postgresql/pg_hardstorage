//go:build linux

package prompt

import "syscall"

// getTermiosIoctl is the TCGETS ioctl number for the local platform.
// Linux uses TCGETS; macOS uses TIOCGETA.  Pinning per-OS via build
// tags keeps the call portable without dragging in a TTY library.
const getTermiosIoctl = syscall.TCGETS
