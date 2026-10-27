//go:build !unix

package backup

import "os"

// mapReadOnly falls back to reading the file into memory where mmap is
// not available (windows), up to maxHeldSignBytes; a larger manifest fails
// with a clear error rather than exhausting memory. Every unix platform
// maps the file and has no such limit.
func mapReadOnly(f *os.File, size int64) ([]byte, func() error, error) {
	return readFileBounded(f, size, maxHeldSignBytes)
}
