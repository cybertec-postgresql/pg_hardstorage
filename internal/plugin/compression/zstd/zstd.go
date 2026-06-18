// Package zstd is the zstandard compressor.
//
// Why zstd by default?
//
//   - Best speed-vs-ratio profile for PG-style content (heap pages,
//     index pages, WAL records). Beats gzip at every level on both
//     axes; comparable with lz4 on speed at level 1, much better
//     ratio at levels 3+.
//
//   - Pure-Go via klauspost/compress — no cgo, no FIPS-build
//     complications, no C shared-library version ambiguity.
//
//   - Output format is stable and standardised (RFC 8478) so a
//     future reader can decompress without our binary being
//     present.
//
// Default level is 9 (the spec target). Levels 1..3 are "fast" tier;
// 7..12 is the "balanced" sweet spot for backups; 13..22 is "max" at
// large CPU cost.
package zstd

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
)

// Compressor is configured with a single zstd level. New writers and
// readers are created per Compress / Decompress call (they're cheap
// to construct in klauspost/compress) — though we cache one writer
// and one reader for hot-path reuse.
type Compressor struct {
	Level zstd.EncoderLevel

	// MaxDecodedSize bounds the size of a single decoded chunk to
	// defend against zstd "compression bombs": a maliciously crafted
	// or corrupted chunk that decompresses to a much larger plaintext
	// than the operator could foresee. Zero means "use the package
	// default" (DefaultMaxDecodedSize = 256 MiB), comfortably above
	// the FastCDC max (256 KiB) so legitimate chunks always fit.
	MaxDecodedSize uint64

	once sync.Once
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// DefaultMaxDecodedSize is the per-chunk decompression-bomb guard.
// 256 MiB is ~1024× the FastCDC maximum chunk size, so legitimate
// inputs never approach the bound; a payload that wants to expand
// past it is by definition adversarial.
const DefaultMaxDecodedSize uint64 = 256 << 20

// New returns a Compressor configured at the given encoder level.
// Pass zstd.SpeedDefault (~level 3) for max throughput; SpeedBetterCompression
// (~level 7) for our default. zstd.SpeedBestCompression (~level 11)
// for archive-tier backups that won't be re-read often.
func New(level zstd.EncoderLevel) *Compressor {
	return &Compressor{Level: level}
}

// NewDefault returns a Compressor at SpeedBetterCompression — the
// "balanced" sweet spot we recommend for v0.1.
func NewDefault() *Compressor { return New(zstd.SpeedBetterCompression) }

// Name implements compression.Compressor.
func (c *Compressor) Name() string {
	return fmt.Sprintf("zstd:%s", c.Level.String())
}

// Algorithm implements compression.Compressor.
func (c *Compressor) Algorithm() compression.AlgorithmID { return compression.AlgoZstd }

// init lazily builds the encoder and decoder. We keep them as
// instance fields so zstd's internal state pools amortise across
// many Compress/Decompress calls within a single backup or restore.
//
// The decoder is constructed with WithDecoderMaxMemory so a single
// DecodeAll call cannot exceed the configured size bound. This is
// the compression-bomb guard — a corrupted or malicious chunk that
// claims to decompress to GB will return an error rather than OOM
// the agent.
func (c *Compressor) init() {
	c.once.Do(func() {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(c.Level))
		if err != nil {
			// klauspost/compress's NewWriter never returns a non-nil
			// error in practice for valid level constants; we panic
			// here because anything else would silently degrade
			// compression to none.
			panic(fmt.Sprintf("zstd: NewWriter: %v", err))
		}
		max := c.MaxDecodedSize
		if max == 0 {
			max = DefaultMaxDecodedSize
		}
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(max))
		if err != nil {
			panic(fmt.Sprintf("zstd: NewReader: %v", err))
		}
		c.enc = enc
		c.dec = dec
	})
}

// Compress produces a zstd-encoded payload.
//
// Short-circuit: for plaintext under 64 bytes we return AlgoNone
// because the zstd frame header itself is ~14 bytes; below that
// threshold compression is always net-negative. The caller still
// records the algo we return, so this is invisible above us.
func (c *Compressor) Compress(plaintext []byte) ([]byte, compression.AlgorithmID, error) {
	if len(plaintext) < 64 {
		out := make([]byte, len(plaintext))
		copy(out, plaintext)
		return out, compression.AlgoNone, nil
	}
	c.init()
	out := c.enc.EncodeAll(plaintext, make([]byte, 0, len(plaintext)/2))
	return out, compression.AlgoZstd, nil
}

// Decompress decodes a zstd payload. The decoder allocates a fresh
// destination slice each call — chunks are expected to be modestly
// sized (typical FastCDC range is 4 KiB–256 KiB).
func (c *Compressor) Decompress(payload []byte) ([]byte, error) {
	c.init()
	return c.dec.DecodeAll(payload, nil)
}
