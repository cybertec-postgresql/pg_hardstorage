//go:build !unix

package spill

import "os"

// mapReadOnly falls back to reading the file into memory where mmap is
// not available (windows), up to maxHeld; a larger file fails with
// ErrTooLargeToHold rather than exhausting memory. Every unix platform
// maps the file and has no such limit.
func mapReadOnly(f *os.File, size, maxHeld int64) ([]byte, func() error, error) {
	return readFileBounded(f, size, maxHeld)
}
