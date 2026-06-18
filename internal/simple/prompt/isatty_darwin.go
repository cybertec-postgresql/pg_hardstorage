//go:build darwin

package prompt

import "syscall"

// macOS variant of getTermiosIoctl; see isatty_linux.go's comment.
const getTermiosIoctl = syscall.TIOCGETA
