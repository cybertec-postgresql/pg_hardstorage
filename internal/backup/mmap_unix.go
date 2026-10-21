//go:build unix

package backup

import (
	"os"
	"syscall"
)

// mapReadOnly maps the first size bytes of f read-only. The pages are
// file-backed, so a manifest far larger than RAM can be handed to
// ed25519.Verify (which needs the whole message as one []byte) without
// the Go heap or the process's anonymous memory growing: the kernel
// pages it in and out of the page cache.
func mapReadOnly(f *os.File, size int64) ([]byte, func() error, error) {
	if size == 0 {
		return nil, func() error { return nil }, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
