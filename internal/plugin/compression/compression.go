// Package compression defines the per-chunk compression contract.
//
// Why per-chunk and not per-file or per-backup?
//
//   - The CAS deduplicates on plaintext SHA-256, so two identical
//     plaintext bytes always collapse to one stored object regardless
//     of the backup that produced them. We need the on-disk format
//     to be self-describing — each stored chunk must declare which
//     codec produced its bytes — so multiple backups using different
//     codecs can co-exist in one repo.
//
//   - Different content tolerates different codecs differently. WAL
//     pages and PG heap files compress well with zstd; already-
//     compressed types (pg_largeobject blobs, TOASTed bytea with
//     PGLZ already applied) gain little.+ may add per-chunk
//     adaptive selection.
//
// On-disk envelope:
//
//	[1] EnvelopeVersion = 0x01
//	[1] AlgorithmID    (0=none, 1=zstd)
//	[N] codec-specific payload
//
// AlgorithmID 0 means the payload IS the plaintext. v0.1 always
// writes an envelope; chunks lacking the version byte are treated
// as ErrCorruptEnvelope (the legacy raw-bytes mode predates this
// contract and is not in the released format).
package compression

import (
	"errors"
	"fmt"
)

// EnvelopeVersion is the on-disk envelope version that NEW writes
// produce. We currently emit v0x02 unconditionally; v0x01 is the
// pre-encryption format kept readable for the 24-month back-compat
// commitment.
//
// On-disk layouts:
//
//	v0x01 (compression-only):
//	  [1] version=0x01
//	  [1] CompressionAlgo
//	  [N] payload
//
//	v0x02 (compression + optional encryption):
//	  [1] version=0x02
//	  [1] CompressionAlgo
//	  [1] EncryptionAlgo  (0 = none)
//	  [12] Nonce          (zero bytes when EncryptionAlgo == 0)
//	  [N] payload         (post-encryption-if-any)
//
// Both versions are accepted by ReadEnvelope; only v0x02 is emitted
// by WriteEnvelope. WriteEnvelopeV1 is preserved for tests that need
// to fabricate legacy bytes.
const (
	EnvelopeVersionV1 = byte(0x01)
	EnvelopeVersion   = byte(0x02)
)

// AlgorithmID enumerates compression codecs. Stable across releases —
// the byte value goes on disk and into the 24-month backward-read
// commitment.
type AlgorithmID byte

const (
	// AlgoNone stores plaintext verbatim. Useful for tiny chunks (where
	// header overhead exceeds savings) and for testing.
	AlgoNone AlgorithmID = 0

	// AlgoZstd uses zstandard. Default for v0.1.
	AlgoZstd AlgorithmID = 1
)

// String returns the canonical lowercase name. Used in manifests
// (stored as "zstd:<level>" — the level is encoded in the codec
// itself; the algo string is just the family).
func (a AlgorithmID) String() string {
	switch a {
	case AlgoNone:
		return "none"
	case AlgoZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown-algo-%d", a)
	}
}

// Compressor is the per-codec contract.
//
// Compress takes a plaintext slice and returns a payload byte slice
// (NOT including the envelope prefix) plus the algorithm ID the
// caller should record. Implementations may inspect plaintext size
// and short-circuit to AlgoNone when compression would be net-
// negative; the returned algo is authoritative.
//
// Decompress takes the payload (NOT including the envelope prefix)
// and reproduces the plaintext.
//
// Algorithm returns the codec's primary AlgorithmID — what NewCAS
// registers for the read path, and what the encoded envelope's
// CompressionAlgo byte will be for non-short-circuited inputs. A
// codec whose Compress may short-circuit to AlgoNone (zstd does, for
// inputs below the frame-overhead threshold) MUST still report its
// "real" algorithm here so the read-back registry has both
// (Algorithm() AND AlgoNone) covered.
type Compressor interface {
	Name() string
	Algorithm() AlgorithmID
	Compress(plaintext []byte) (payload []byte, algo AlgorithmID, err error)
	Decompress(payload []byte) (plaintext []byte, err error)
}

// ErrCorruptEnvelope means the on-disk bytes don't begin with the
// envelope version. Returned by ReadEnvelope when the storage backend
// has handed us bytes that aren't a chunk we wrote.
var ErrCorruptEnvelope = errors.New("compression: corrupt or non-pg_hardstorage envelope")

// ErrUnknownAlgorithm means the envelope's algo byte refers to a codec
// we don't recognise. This is the failure mode for "the repo was
// written by a future pg_hardstorage version".
var ErrUnknownAlgorithm = errors.New("compression: unknown algorithm in envelope")

// Envelope-layout offsets and sizes. Authoritative — every read /
// write path MUST consult these constants rather than literal byte
// counts so a future format bump (v0x03 with, say, a length-prefixed
// payload) is a single point of change.
const (
	// nonceLen is the on-disk reservation for the encryption nonce.
	// Matches AES-GCM's 96-bit nonce; the field is reserved even
	// when EncryptionAlgo == 0 so the envelope is fixed-shape.
	nonceLen = 12

	// v1HeaderLen is [version, compressionAlgo].
	v1HeaderLen = 2

	// v2HeaderLen is [version, compressionAlgo, encryptionAlgo, nonce(12)].
	v2HeaderLen = 1 + 1 + 1 + nonceLen

	// Field offsets within the v2 header.
	v2OffsetVersion         = 0
	v2OffsetCompressionAlgo = 1
	v2OffsetEncryptionAlgo  = 2
	v2OffsetNonce           = 3
	v2OffsetPayload         = v2HeaderLen
)

// EncryptionFields carries the encryption metadata for an envelope.
// Zero value (EncryptionAlgo == 0, Nonce all-zero) means no encryption.
type EncryptionFields struct {
	EncryptionAlgo byte // a value from internal/plugin/encryption.AlgorithmID
	Nonce          [nonceLen]byte
}

// IsEncrypted reports whether the fields describe an encrypted payload.
func (e EncryptionFields) IsEncrypted() bool {
	return e.EncryptionAlgo != 0
}

// WriteEnvelope returns the on-disk byte slice for (compressionAlgo,
// encryption, payload). Always emits v0x02.
//
// Total length: v2HeaderLen + len(payload).
func WriteEnvelope(compressionAlgo AlgorithmID, encryption EncryptionFields, payload []byte) []byte {
	out := make([]byte, v2HeaderLen+len(payload))
	out[v2OffsetVersion] = EnvelopeVersion
	out[v2OffsetCompressionAlgo] = byte(compressionAlgo)
	out[v2OffsetEncryptionAlgo] = encryption.EncryptionAlgo
	copy(out[v2OffsetNonce:v2OffsetNonce+nonceLen], encryption.Nonce[:])
	copy(out[v2OffsetPayload:], payload)
	return out
}

// WriteEnvelopeV1 emits a legacy v0x01 envelope. Tests use this to
// fabricate pre-encryption bytes; production code calls WriteEnvelope.
func WriteEnvelopeV1(compressionAlgo AlgorithmID, payload []byte) []byte {
	out := make([]byte, v1HeaderLen+len(payload))
	out[0] = EnvelopeVersionV1
	out[1] = byte(compressionAlgo)
	copy(out[v1HeaderLen:], payload)
	return out
}

// ReadEnvelope parses an on-disk envelope. Accepts both v0x01 and
// v0x02. v0x01 inputs report EncryptionAlgo=0 (none) and a zero
// nonce — semantically equivalent to "this chunk is not encrypted."
//
// Returns ErrCorruptEnvelope for unknown versions or short input.
func ReadEnvelope(b []byte) (AlgorithmID, EncryptionFields, []byte, error) {
	var fields EncryptionFields
	if len(b) < 1 {
		return 0, fields, nil, fmt.Errorf("%w: empty body", ErrCorruptEnvelope)
	}
	switch b[0] {
	case EnvelopeVersionV1:
		if len(b) < v1HeaderLen {
			return 0, fields, nil, fmt.Errorf("%w: v1 short body (%d bytes)", ErrCorruptEnvelope, len(b))
		}
		return AlgorithmID(b[1]), fields, b[v1HeaderLen:], nil
	case EnvelopeVersion:
		if len(b) < v2HeaderLen {
			return 0, fields, nil, fmt.Errorf("%w: v2 short body (%d bytes; need %d)",
				ErrCorruptEnvelope, len(b), v2HeaderLen)
		}
		fields.EncryptionAlgo = b[v2OffsetEncryptionAlgo]
		copy(fields.Nonce[:], b[v2OffsetNonce:v2OffsetNonce+nonceLen])
		return AlgorithmID(b[v2OffsetCompressionAlgo]), fields, b[v2OffsetPayload:], nil
	}
	return 0, fields, nil, fmt.Errorf("%w: version byte = %#x (supported: %#x, %#x)",
		ErrCorruptEnvelope, b[0], EnvelopeVersionV1, EnvelopeVersion)
}

// CodecRegistry maps an AlgorithmID to the Compressor that handles
// it on the read path. Populated at process start by importing the
// concrete codec packages (which call Register in their init).
//
// We intentionally do NOT import the codec packages from this file —
// keeping the registry decoupled lets a tiny binary opt out of
// dragging in zstd if it really doesn't need it.
//
// Scoping: production callers wire one CodecRegistry **per CAS
// instance**, not a single process-global registry.  Plugin authors
// reading this package should use NewRegistry + Register from the
// CAS-construction site rather than a package-level init() — the
// CAS layer owns the codec set so two CAS instances in the same
// process can carry different compression posture (e.g. one
// zstd-only, one with a vendor codec from a Tier-2 plugin).
type CodecRegistry struct {
	codecs map[AlgorithmID]Compressor
}

// NewRegistry returns an empty registry.
func NewRegistry() *CodecRegistry {
	return &CodecRegistry{codecs: map[AlgorithmID]Compressor{}}
}

// Register installs c under algo. Panics if algo is already taken
// (the registry is built at init time; double-registration is a
// programmer error).
func (r *CodecRegistry) Register(algo AlgorithmID, c Compressor) {
	if _, ok := r.codecs[algo]; ok {
		panic(fmt.Sprintf("compression: algorithm %d already registered", algo))
	}
	r.codecs[algo] = c
}

// Lookup returns the Compressor for algo, or ErrUnknownAlgorithm.
func (r *CodecRegistry) Lookup(algo AlgorithmID) (Compressor, error) {
	c, ok := r.codecs[algo]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownAlgorithm, algo)
	}
	return c, nil
}

// Has reports whether algo is registered. Useful for the CAS's
// "do we know how to read this back?" pre-check.
func (r *CodecRegistry) Has(algo AlgorithmID) bool {
	_, ok := r.codecs[algo]
	return ok
}
