package spill

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrTooLargeToHold is returned (wrapped) by MapReadOnly on a platform
// without mmap when the file is over the caller's in-memory limit.
var ErrTooLargeToHold = errors.New("spill: file is over the in-memory limit of a platform without mmap")

// MapReadOnly returns the first size bytes of f as one addressable []byte
// and a function that releases it.
//
// It exists for Ed25519 (RFC 8032): the scheme hashes the message twice,
// with the nonce prefix first and the public key first, so a signature
// cannot be computed or checked over a stream; the message must be one
// []byte. Changing that would change the signature formats, which must
// stay compatible. A caller that spools the message to a temp file and
// maps it pays disk and page cache instead of heap.
//
// On unix the bytes are a read-only file mapping and maxHeld is ignored.
// Elsewhere (windows) the file is read into memory, up to maxHeld bytes;
// a larger one fails with ErrTooLargeToHold instead of exhausting memory.
//
// f must stay open, and must not be truncated, until release is called.
func MapReadOnly(f *os.File, size, maxHeld int64) (b []byte, release func() error, err error) {
	return mapReadOnly(f, size, maxHeld)
}

// readFileBounded reads the first size bytes of f into memory, or fails
// without allocating if size exceeds max.
func readFileBounded(f *os.File, size, max int64) ([]byte, func() error, error) {
	if size > max {
		return nil, nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLargeToHold, size, max)
	}
	b := make([]byte, size)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, nil, err
	}
	return b, func() error { return nil }, nil
}
