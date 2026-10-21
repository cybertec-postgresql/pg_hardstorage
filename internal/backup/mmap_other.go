//go:build !unix

package backup

import (
	"io"
	"os"
)

// mapReadOnly falls back to reading the file into memory where mmap is
// not available (windows). A manifest larger than RAM cannot be verified
// there; every other platform maps the file.
func mapReadOnly(f *os.File, size int64) ([]byte, func() error, error) {
	b := make([]byte, size)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, nil, err
	}
	return b, func() error { return nil }, nil
}
