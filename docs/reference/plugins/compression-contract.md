---
title: Compression plugin contract
description: The Compressor interface — per-chunk codecs, on-disk envelope format.
tags:
  - plugins
  - compression
  - reference
---

# Compression plugin contract

A compression plugin is a per-chunk codec.  pg_hardstorage
deduplicates on plaintext SHA-256 in a content-addressable
store, so two identical plaintext bytes always collapse to
one stored object regardless of which backup produced
them.  The on-disk format is therefore self-describing —
each stored chunk declares its codec via an `AlgorithmID`
byte — so multiple backups using different codecs can
co-exist in one repo.

!!! note "Reference implementation"
    `internal/plugin/compression/zstd/zstd.go` —
    klauspost/compress, pure-Go (no cgo, no FIPS-build
    headache), with a `MaxDecodedSize` bomb-guard.  Read
    it before writing your own; the bomb-guard pattern is
    not optional.

## Interface

```go
// internal/plugin/compression/compression.go

package compression

type Compressor interface {
    Name() string
    Algorithm() AlgorithmID
    Compress(plaintext []byte) (payload []byte, algo AlgorithmID, err error)
    Decompress(payload []byte) (plaintext []byte, err error)
}
```

## Per-method contract

### `Name() string`

Lowercase canonical name — `"zstd"`, `"lz4"`, `"none"`.
Stable across releases.  Used in manifests and audit
events; the name family is decoupled from the level
encoding (the manifest stores `"zstd:9"` — the algo name
plus the level — but the level is internal to the codec).

### `Algorithm() AlgorithmID`

The codec's primary `AlgorithmID`.  This is what the
codec registers in `CodecRegistry` for the read path,
and what the encoded envelope's `CompressionAlgo` byte
will be for non-short-circuited inputs.

```go
type AlgorithmID byte

const (
    AlgoNone AlgorithmID = 0   // plaintext, no codec
    AlgoZstd AlgorithmID = 1
    // 2..255 reserved for future codecs
)
```

The byte values go on disk and into the **24-month
backward-read commitment**.  Choosing a new
`AlgorithmID` is a versioned decision: once shipped, it
must remain readable.

### `Compress(plaintext) (payload, algo, err)`

Compress `plaintext`.  Returns:

- `payload` — the codec-specific bytes (NOT including
  the envelope prefix).
- `algo` — the algorithm ID the **caller should record
  in the envelope**.  This is *authoritative* — a codec
  MAY short-circuit to `AlgoNone` for inputs where
  compression would be net-negative (zstd does this for
  small inputs below the frame-overhead threshold).
- `err` — any codec error.  Compress MUST be infallible
  for well-formed inputs; errors here indicate
  programmer bugs (nil slices, etc.) rather than data
  conditions.

The caller (CAS) calls `WriteEnvelope(algo, encryption,
payload)` with the returned `algo`, NOT with
`Algorithm()`.  This is the only difference between the
two methods: `Algorithm()` is the codec's *primary*,
`Compress` reports the *actual* algorithm used for THIS
input.

### `Decompress(payload) (plaintext, err)`

Decompress `payload` (NOT including the envelope prefix)
back to plaintext.  Errors include compression-bomb
guard failures (see the `MaxDecodedSize` field on the
zstd impl) and codec-internal errors.

A codec used in production **MUST guard against
decompression bombs.**  A maliciously crafted or
corrupted chunk that decompresses to a larger plaintext
than the operator could foresee is a denial-of-service
vector against restore.  The zstd codec's
`DefaultMaxDecodedSize` is 256 MiB — comfortably above
the FastCDC max chunk size (256 KiB) so legitimate
chunks always fit.  Pick a similar bound; document it.

## On-disk envelope

The CAS wraps every chunk in a small envelope:

```
v0x02 (compression + optional encryption — current default):

  [1] EnvelopeVersion = 0x02
  [1] CompressionAlgo
  [1] EncryptionAlgo  (0 = none)
  [12] Nonce          (zero bytes when EncryptionAlgo == 0)
  [N] payload         (post-encryption-if-any, post-compression)

v0x01 (legacy, compression-only):

  [1] EnvelopeVersion = 0x01
  [1] CompressionAlgo
  [N] payload
```

Only `v0x02` is written by current `WriteEnvelope`;
`v0x01` is preserved for the 24-month backward-read
commitment.  `WriteEnvelopeV1` is preserved for tests
that need to fabricate legacy bytes.

Use the `compression` package's `WriteEnvelope`,
`ReadEnvelope`, and the `v2OffsetX` constants — never
hand-roll byte indexing.  A future format bump (v0x03 with,
say, a length-prefixed payload) is a single point of
change because of those constants.

## Compression-encryption interaction

The envelope folds compression and encryption together
because they're applied in the same order on the write
path: **compress, then encrypt**.  This is the canonical
ordering — encrypting compressed bytes preserves the
encryption's IND-CPA property; the reverse leaks
plaintext-size information through ciphertext-size
patterns.

Codecs do NOT know about encryption.  The CAS layers them:
`Compress(plaintext)` → `Encrypt(payload)` →
`WriteEnvelope(compressionAlgo, encryption, ciphertext)`.
Read path is the inverse.

## Registration

```go
// in your codec's package
package mycodec

import "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"

func init() {
    // Wired by the CAS at construction-time, not at process init —
    // codecs need per-instance state (level, max-decoded-size) so
    // they don't fit a global registry. The CAS calls
    // codecRegistry.Register on the per-backup registry it builds.
}
```

Registration is **per-CAS-instance** rather than
process-global because most codecs need configuration
(level, bomb-guard, dictionary) that comes from the
backup config.  The runner that constructs the CAS for
a backup walks the configured codecs, calls
`registry.Register(codec.Algorithm(), codec)`, and hands
the registry to the CAS.

For codecs that need NO configuration (the trivial
`AlgoNone` plaintext passthrough), self-registration via
`init()` against a process-global default registry is
acceptable — but no shipped codec uses that pattern
today.

Double-registration of the same `AlgorithmID` panics:

```go
func (r *CodecRegistry) Register(algo AlgorithmID, c Compressor) {
    if _, ok := r.codecs[algo]; ok {
        panic(fmt.Sprintf("compression: algorithm %d already registered", algo))
    }
    r.codecs[algo] = c
}
```

## Error sentinels

```go
var (
    ErrCorruptEnvelope  = errors.New("compression: corrupt or non-pg_hardstorage envelope")
    ErrUnknownAlgorithm = errors.New("compression: unknown algorithm in envelope")
)
```

`ErrCorruptEnvelope` means the bytes don't begin with a
recognized envelope version — the failure mode for a
backend handing us bytes that aren't a chunk we wrote.

`ErrUnknownAlgorithm` means the envelope's algo byte
refers to a codec we don't recognize — the failure mode
for "this repo has chunks written by a future
pg_hardstorage version with a codec we don't ship".  The
CAS's read path returns this verbatim so the operator
sees `"compression: unknown algorithm in envelope: 7"`
rather than an opaque parse error.

## Concurrency contract

Codec implementations SHOULD be goroutine-safe:
multiple readers / writers across goroutines is the
norm.  The zstd codec uses one cached encoder and one
cached decoder per `Compressor` instance with a
`sync.Once`-guarded init.  If your codec's underlying
library isn't goroutine-safe, document it and pool
instances at the caller level.

## Short-circuit behaviour

A `Compress` that decides compression isn't worth it
returns `(plaintext, AlgoNone, nil)`.  The CAS records
`AlgoNone` in the envelope and the read path doesn't
need to instantiate the codec — it sees `AlgoNone` and
returns the payload directly.  This keeps tiny chunks
(below the frame-overhead threshold) from paying the
codec-instantiation cost on read.

Codecs that *can* short-circuit MUST still report their
"real" algorithm via `Algorithm()` — the per-instance
registry needs the codec registered under both
`Algorithm()` AND `AlgoNone` for the read path to cover
both kinds of input.  The CAS handles the AlgoNone
registration centrally; codec authors don't need to do
anything special beyond returning AlgoNone from
`Compress` when they choose to.

## What codec authors MUST get right

1. **Decompression-bomb guard.**  No codec ships without
   a configurable max-decoded-size.  Default ~256 MiB.
2. **AlgorithmID stability.**  Once an `AlgorithmID` is
   shipped, it MUST remain readable for 24 months
   (the backward-read commitment).  No re-numbering.
3. **`Compress` infallibility.**  Errors here are
   programmer bugs.  A working codec on well-formed
   input never fails Compress.
4. **Short-circuit honesty.**  If `Compress` returns
   `AlgoNone`, the payload IS the plaintext bytes — no
   codec framing.

## Further reading

- The CAS implementation: `internal/storage/cas/`.
- Envelope format: `internal/plugin/compression/compression.go`
  (the `WriteEnvelope` / `ReadEnvelope` helpers).
- Encryption codec contract:
  [Encryption contract](encryption-contract.md) (note:
  that page documents the KEK side; chunk-side encryption
  codecs use a parallel
  `internal/plugin/encryption.Encryptor` interface).
