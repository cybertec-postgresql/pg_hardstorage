package chunker_test

// The chunker's output IS the backup. Every base backup, WAL segment
// and logical CDC batch is written as the sequence Iter yields, and
// each Chunk.Offset becomes a ChunkRef.Offset in the signed manifest —
// the number restore uses to decide where those bytes belong in the
// reconstructed file. So three properties have to hold for every input,
// not just for the couple of megabyte-scale bodies the existing
// round-trip tests use:
//
//   1. concat(chunks) == input      — no byte lost, added or reordered
//   2. Offset[i] == sum(len[0..i))  — offsets address what they claim
//   3. len(chunks[i]) <= max        — the envelope/decompression caps
//                                     downstream assume it
//
// The suite already pins contiguity, determinism and golden boundaries
// against the pre-cursor implementation, all at a single large size.
// What was missing is SIZE as a dimension: bodies at and around the
// min/avg/max bounds, tiny bodies, empty ones, and the highly
// compressible or repetitive shapes where a content-defined chunker's
// boundary search behaves least like it does on random data — a
// database file is far closer to those than to /dev/urandom.

import (
	"bytes"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
)

// checkChunkingInvariants runs the three properties against one body.
func checkChunkingInvariants(t *testing.T, c *chunker.Chunker, body []byte) {
	t.Helper()
	_, _, max := c.Sizes()

	var rebuilt []byte
	var wantOffset int64
	n := 0
	for ch, err := range c.Iter(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("len=%d: chunker error: %v", len(body), err)
		}
		if ch.Offset != wantOffset {
			t.Fatalf("len=%d chunk %d: Offset=%d, want %d.\n\n"+
				"Chunk.Offset becomes ChunkRef.Offset in the manifest; restore places the "+
				"chunk's bytes at that position, so a wrong offset writes correct data to "+
				"the wrong place in the file.", len(body), n, ch.Offset, wantOffset)
		}
		if len(ch.Data) > max {
			t.Fatalf("len=%d chunk %d: %d bytes exceeds max %d", len(body), n, len(ch.Data), max)
		}
		if len(ch.Data) == 0 {
			t.Fatalf("len=%d chunk %d: empty chunk yielded", len(body), n)
		}
		// Copy: Iter's slices alias an internal buffer and are rewritten
		// on the next iteration.
		rebuilt = append(rebuilt, ch.Data...)
		wantOffset += int64(len(ch.Data))
		n++
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatalf("len=%d: concatenation of %d chunks != input (rebuilt %d bytes).\n\n"+
			"This is the backup being different from the thing backed up.",
			len(body), n, len(rebuilt))
	}
}

// TestChunker_InvariantsAcrossSizes sweeps the sizes the fixed-size
// round-trip tests never reach: the bounds themselves and their
// neighbours, plus the degenerate small cases.
func TestChunker_InvariantsAcrossSizes(t *testing.T) {
	c := chunker.New()
	min, avg, max := c.Sizes()

	sizes := []int{
		0, 1, 2, 63, 64, 65, 4095, 4096, 4097,
		min - 1, min, min + 1,
		avg - 1, avg, avg + 1,
		max - 1, max, max + 1,
		2*max - 1, 2 * max, 2*max + 1,
		3*max + 12345,
	}
	// Patterns a real data directory actually contains: zero-filled
	// pages, repeating records, and incompressible payloads.
	patterns := map[string]func(i int) byte{
		"zeros":     func(int) byte { return 0 },
		"ones":      func(int) byte { return 0xFF },
		"repeating": func(i int) byte { return byte(i % 251) },
		"pagey": func(i int) byte {
			if i%8192 < 24 {
				return byte(i)
			}
			return 0
		},
		"pseudorand": func(i int) byte { return byte((i*2654435761 + 1013904223) >> 13) },
	}

	for name, gen := range patterns {
		for _, sz := range sizes {
			if sz < 0 {
				continue
			}
			body := make([]byte, sz)
			for i := range body {
				body[i] = gen(i)
			}
			t.Run(name, func(t *testing.T) {
				checkChunkingInvariants(t, c, body)
			})
		}
	}
}

// FuzzChunker_Invariants lets the fuzzer search for inputs the fixed
// table misses. Same three properties.
func FuzzChunker_Invariants(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("a"))
	f.Add(bytes.Repeat([]byte{0}, 70000))
	f.Add(bytes.Repeat([]byte("postgresql"), 9000))
	f.Add(append(bytes.Repeat([]byte{0xAA}, 40000), bytes.Repeat([]byte{0}, 40000)...))

	c := chunker.New()
	f.Fuzz(func(t *testing.T, body []byte) {
		checkChunkingInvariants(t, c, body)
	})
}

// A chunker is reused across files in production (tarsink holds one for
// the whole stream). Chunking body A must not influence body B.
func TestChunker_ReuseAcrossBodiesIsIndependent(t *testing.T) {
	c := chunker.New()
	_, _, max := c.Sizes()
	a := bytes.Repeat([]byte{0x5A}, 3*max)
	b := bytes.Repeat([]byte{0x00}, 3*max)

	// Chunk a, then b, then b again on a FRESH chunker. The two b runs
	// must agree, or state leaked across bodies.
	checkChunkingInvariants(t, c, a)
	var first [][]byte
	for ch, err := range c.Iter(bytes.NewReader(b)) {
		if err != nil {
			t.Fatal(err)
		}
		first = append(first, append([]byte(nil), ch.Data...))
	}
	var second [][]byte
	for ch, err := range chunker.New().Iter(bytes.NewReader(b)) {
		if err != nil {
			t.Fatal(err)
		}
		second = append(second, append([]byte(nil), ch.Data...))
	}
	if len(first) != len(second) {
		t.Fatalf("reused chunker produced %d chunks for b, fresh one produced %d — "+
			"state leaked from the previous body, so dedup would depend on what was "+
			"backed up before it", len(first), len(second))
	}
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("chunk %d differs between a reused and a fresh chunker", i)
		}
	}
}
