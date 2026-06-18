// Package chunker implements content-defined chunking — the splitter that
// turns an arbitrary byte stream into fingerprint-aligned chunks.
//
// Why content-defined? Because dedup. If we split every 64 KiB on fixed
// offsets, inserting one byte at the start shifts every chunk that
// follows and dedup goes to zero. Content-defined chunking finds the
// boundary based on the data itself: insert one byte, and the next
// boundary is found at the same content position regardless. The chunks
// before the change are bit-identical; only the chunk containing the
// change (and maybe the next one or two) differ.
//
// Algorithm: FastCDC (FAST '16). A single 32-bit "gear hash" is updated
// per byte (one shift, one table lookup, one add) — much cheaper than
// Rabin chunking. The boundary check is `hash & mask == 0`. We use two
// masks: a tight mask before reaching the average chunk size (boundary
// detection is rare, so chunks tend to grow toward the avg) and a loose
// mask after (boundaries are common, so chunks don't blow past the max).
// This produces a more uniform chunk-size distribution than vanilla CDC.
//
// Reproducibility: the gear table is generated from a fixed seed so
// every build, on every host, on every architecture, computes the same
// boundaries on the same input. That's part of the dedup contract: a
// chunk produced today must match a chunk produced last month.
package chunker

import (
	"errors"
	"io"
	"iter"
	mathrand "math/rand"
)

// Default size bounds. Chosen to match docs/SPEC.md:
//
//   - 4 KiB minimum keeps per-object overhead bounded on object stores.
//   - 64 KiB average is the sweet spot between dedup granularity and the
//     fixed cost of a PUT (S3 charges per request as well as per byte).
//   - 256 KiB maximum caps worst-case memory + latency per chunk.
const (
	DefaultMinSize = 4 * 1024
	DefaultAvgSize = 64 * 1024
	DefaultMaxSize = 256 * 1024
)

// gearTableSeed is part of the dedup contract: changing it invalidates
// every previously-stored chunk. If we ever need to rotate (e.g. for a
// per-tenant salt), we'll do so under an explicit chunker version.
const gearTableSeed = 0xC0FFEE_BEAD_42

// gearTable is a 256-entry table of 32-bit values. Each input byte
// indexes into the table; the table value is added (with shift) into
// the rolling hash. We pre-compute it at init() so the cost is paid
// once per process.
var gearTable [256]uint32

func init() {
	r := mathrand.New(mathrand.NewSource(gearTableSeed))
	for i := range gearTable {
		gearTable[i] = r.Uint32()
	}
}

// Chunker splits a byte stream into content-defined chunks.
//
// A single Chunker can be reused across many streams; it carries no
// per-stream state. Iter is the canonical entry point.
type Chunker struct {
	min, avg, max int
	maskTight     uint32
	maskLoose     uint32
}

// New returns a Chunker with the default size bounds.
func New() *Chunker {
	return NewWithParams(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
}

// NewWithParams returns a Chunker with explicit bounds. Bounds must
// satisfy 0 < min <= avg <= max. Powers of two are recommended but not
// required; the masks are derived from log2(avg) regardless.
func NewWithParams(min, avg, max int) *Chunker {
	if min <= 0 || avg < min || max < avg {
		panic("chunker: invalid bounds (need 0 < min <= avg <= max)")
	}
	bits := log2Floor(uint(avg))
	// Tight mask: target ~4x rarer than 1/avg, so chunks tend to reach avg.
	// Loose mask: target ~4x more common, so post-avg chunks cut quickly.
	tight := bitsToMask(bits + 2)
	loose := bitsToMask(bits - 2)
	return &Chunker{
		min:       min,
		avg:       avg,
		max:       max,
		maskTight: tight,
		maskLoose: loose,
	}
}

// Sizes reports the current bounds.
func (c *Chunker) Sizes() (min, avg, max int) { return c.min, c.avg, c.max }

// Chunk is one piece produced by the chunker. Offset is the byte
// offset of the chunk's first byte in the original stream.
//
// Data ownership semantics depend on which iterator produced the
// chunk:
//   - Iter (advanced, no-copy): Data shares storage with the
//     chunker's working buffer.  The next iteration OVERWRITES
//     the bytes, so callers MUST copy before retaining or before
//     advancing the iterator.  An audit flagged this as a
//     latent silent-corruption footgun for any caller that
//     forgets to copy.
//   - IterCopying (safe default for new code): Data is a fresh
//     allocation per chunk; callers can retain it indefinitely.
//
// The two paths share the underlying CDC implementation; choose
// IterCopying unless you've measured Iter's no-copy gain.
type Chunk struct {
	Data   []byte
	Offset int64
}

// IterCopying is the safe default for new code: it wraps Iter and
// copies each chunk's Data into a fresh allocation before yielding,
// so callers can retain the chunk past the next iteration without
// risk of silent corruption.
//
// An audit (chunker buffer reuse): the no-copy Iter is a
// latent footgun — a caller that forgets to copy Data before the
// next iteration silently observes corrupted bytes (the chunker's
// working buffer is being rewritten in place).  IterCopying makes
// the safe path the obvious one; performance-sensitive callers
// who've measured the cost can reach for Iter explicitly.
func (c *Chunker) IterCopying(r io.Reader) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		for ck, err := range c.Iter(r) {
			if err != nil {
				if !yield(Chunk{}, err) {
					return
				}
				continue
			}
			cp := make([]byte, len(ck.Data))
			copy(cp, ck.Data)
			if !yield(Chunk{Data: cp, Offset: ck.Offset}, nil) {
				return
			}
		}
	}
}

// Iter returns an iter.Seq2 yielding chunks from r until EOF.
//
// On a read error, the iterator yields (zero, err) and stops. After EOF
// the trailing buffer (which may be smaller than min) is yielded as the
// final chunk. An empty stream yields nothing.
//
// The Data slice is reused across iterations — callers that need to
// retain a chunk MUST copy it before requesting the next one. The
// callback may return false to stop iteration early.
//
// Prefer IterCopying for new code unless you've measured the no-copy
// gain — the audit explanation in the Chunk doc lays out the
// silent-corruption risk this iterator's contract carries.
func (c *Chunker) Iter(r io.Reader) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		// Working buffer holds up to max bytes. We refill from r as we
		// consume chunks. Reused across iterations to avoid GC pressure.
		buf := make([]byte, 0, c.max)
		var offset int64
		eof := false

		for {
			// Try to fill the buffer to max.
			if !eof {
				need := c.max - len(buf)
				if need > 0 {
					tmp := make([]byte, need)
					n, err := io.ReadFull(r, tmp)
					buf = append(buf, tmp[:n]...)
					switch {
					case err == nil:
						// full read; loop body proceeds
					case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
						eof = true
					default:
						yield(Chunk{}, err)
						return
					}
				}
			}

			if len(buf) == 0 {
				return // clean EOF
			}

			// If we're at EOF and buf is shorter than min, emit it as is.
			if eof && len(buf) <= c.min {
				if !yield(Chunk{Data: buf, Offset: offset}, nil) {
					return
				}
				return
			}

			// Find the next boundary in buf. boundary is a length, not
			// an index: chunk = buf[:boundary].
			boundary := c.findBoundary(buf)
			if boundary > len(buf) {
				boundary = len(buf)
			}

			// Yield this chunk. Data shares storage with buf; the next
			// iteration overwrites it, so callers must copy.
			chunkData := buf[:boundary]
			if !yield(Chunk{Data: chunkData, Offset: offset}, nil) {
				return
			}
			offset += int64(boundary)

			// Advance the buffer. We move the tail to the front rather
			// than slicing-with-leftover so subsequent appends fit in
			// the same allocation.
			buf = append(buf[:0], buf[boundary:]...)
		}
	}
}

// findBoundary returns the cut position within buf in the range
// [min, len(buf)]. min is honored as long as buf is at least that long;
// callers handle the eof-shorter-than-min case before calling.
func (c *Chunker) findBoundary(buf []byte) int {
	n := len(buf)
	if n <= c.min {
		return n
	}

	var hash uint32
	i := c.min

	// Phase 1: tight mask, until we've consumed avg bytes (or buf ends).
	avgEnd := c.avg
	if avgEnd > n {
		avgEnd = n
	}
	for ; i < avgEnd; i++ {
		hash = (hash << 1) + gearTable[buf[i]]
		if hash&c.maskTight == 0 {
			return i + 1
		}
	}

	// Phase 2: loose mask, until max (already capped by buf length).
	for ; i < n; i++ {
		hash = (hash << 1) + gearTable[buf[i]]
		if hash&c.maskLoose == 0 {
			return i + 1
		}
	}

	// No boundary found: cut at end of buf (which is <= max).
	return n
}

// log2Floor returns floor(log2(v)) for positive v. Returns 0 for v=0.
func log2Floor(v uint) int {
	if v == 0 {
		return 0
	}
	out := 0
	for v >>= 1; v != 0; v >>= 1 {
		out++
	}
	return out
}

// bitsToMask returns (1<<bits)-1, clamped so we never produce a
// negative-shift result. Bits are clamped to [0, 31].
func bitsToMask(bits int) uint32 {
	if bits <= 0 {
		return 0
	}
	if bits >= 32 {
		return ^uint32(0)
	}
	return (uint32(1) << bits) - 1
}
