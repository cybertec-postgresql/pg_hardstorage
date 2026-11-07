package backup

import (
	"errors"
	"fmt"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// maxHeldSignBytes bounds the in-memory fallback used where mmap is not
// available (windows; see spill.MapReadOnly). Ed25519 (RFC 8032, the only
// scheme in the manifest format) hashes the message twice, with the nonce
// prefix first and the public key first, so it cannot be computed over a
// stream; the message must be addressable as one []byte. Changing that
// would change the signature format, which must stay compatible. The
// fallback therefore refuses, with a clear error, instead of exhausting
// memory.
// A variable so tests can lower it.
var maxHeldSignBytes int64 = MaxManifestBytes

// mapReadOnly returns the first size bytes of the spooled canonical
// manifest f as one []byte for signing or verifying: memory-mapped on
// unix (no limit, no heap), held in RAM up to maxHeldSignBytes elsewhere.
func mapReadOnly(f *os.File, size int64) ([]byte, func() error, error) {
	b, unmap, err := spill.MapReadOnly(f, size, maxHeldSignBytes)
	if errors.Is(err, spill.ErrTooLargeToHold) {
		return nil, nil, fmt.Errorf("manifest canonical form is %d bytes, over the %d-byte limit of "+
			"platforms without mmap: Ed25519 signatures cover the whole message and cannot be "+
			"computed or verified in a stream; sign and verify this manifest on a unix host", size, maxHeldSignBytes)
	}
	return b, unmap, err
}
