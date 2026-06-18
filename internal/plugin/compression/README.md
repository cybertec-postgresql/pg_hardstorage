# plugin/compression/

The compression tier: the codec the chunker runs each chunk through before the
encryption tier seals it.

## What lives here

The `Compressor` interface and its two implementations. Compression sits between
the chunker and the encryption tier — compress plaintext first (ciphertext is
incompressible) and seal the compressed bytes. The manifest records the codec so
restore can reverse the order.

## Compressor interface

`NewWriter(io.Writer) io.WriteCloser`, `NewReader(io.Reader) io.ReadCloser`,
`Name() string`. Streaming so a chunk never has to fit in memory.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `none` | Pass-through; for pre-compressed chunks or benchmarking | real |
| `zstd` | Zstandard with per-backup level (1..22), optional dictionary | real |

## Key files

- `compression.go` — `Compressor` interface, registry, level normalisation
- `compression_test.go` — round-trip + interface-conformance tests
- `none/` — pass-through
- `zstd/` — Zstandard via `github.com/klauspost/compress/zstd`

## Read next

- `../encryption/README.md` — next stage in the chunk pipeline
- `../../backup/chunker/` — where compression actually runs
- `docs/reference/compression-levels.md` — sizing/speed tradeoffs

## Don't put X here

- Application-layer codecs (gzip-of-tar etc.) — chunks are content-defined; do
  not pre-frame.
- Filter codecs (delta encoding) — chunker boundary handles deduplication.
- gzip / brotli — deliberately not shipped; Zstandard dominates the tradeoff
  curve.
