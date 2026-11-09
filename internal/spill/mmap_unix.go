//go:build unix

package spill

import (
	"fmt"
	"os"
	"syscall"
)

// mapReadOnly maps the first size bytes of f read-only. The pages are
// file-backed, so a file far larger than RAM can be handed to
// ed25519.Sign/Verify (which need the whole message as one []byte)
// without the Go heap or the process's anonymous memory growing: the
// kernel pages it in and out of the page cache.
func mapReadOnly(f *os.File, size, _ int64) ([]byte, func() error, error) {
	if size == 0 {
		return nil, func() error { return nil }, nil
	}
	// A mapping is addressed by int: on a 32-bit platform a file past
	// 2 GiB cannot be mapped, and the conversion must not wrap silently.
	if int64(int(size)) != size {
		return nil, nil, fmt.Errorf("spill: cannot map %d bytes on this platform", size)
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
