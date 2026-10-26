package backup

import (
	"fmt"
	"io"
	"os"
)

// maxHeldSignBytes bounds the in-memory fallback used where mmap is not
// available (see mmap_other.go). Ed25519 (RFC 8032, the only scheme in the
// manifest format) hashes the message twice, with the nonce prefix first
// and the public key first, so it cannot be computed over a stream; the
// message must be addressable as one []byte. Changing that would change
// the signature format, which must stay compatible. The fallback therefore
// refuses, with a clear error, instead of exhausting memory.
// A variable so tests can lower it.
var maxHeldSignBytes int64 = MaxManifestBytes

// readFileBounded reads the first size bytes of f into memory, or fails
// without allocating if size exceeds max.
func readFileBounded(f *os.File, size, max int64) ([]byte, func() error, error) {
	if size > max {
		return nil, nil, fmt.Errorf("manifest canonical form is %d bytes, over the %d-byte limit of "+
			"platforms without mmap: Ed25519 signatures cover the whole message and cannot be "+
			"computed or verified in a stream; sign and verify this manifest on a unix host", size, max)
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
