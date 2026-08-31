package chunker_test

// IterCopying is documented as "the safe default for new code" and no
// production caller uses it — every one of them (walsink, tarsink, the
// logical chunked sink, Split) reaches for the no-copy Iter. So the
// path the documentation steers new callers toward is the path nothing
// exercises against the path the CAS was actually filled by.
//
// That asymmetry has one failure mode that matters: if IterCopying's
// boundaries ever diverge from Iter's, a caller who followed the advice
// produces chunk hashes that match nothing already in the repository.
// Dedup silently drops to zero and the two writers' chunks never
// coincide again. Today IterCopying delegates to Iter so they cannot
// diverge — this test is what makes that stay true if someone
// reimplements it (say, to avoid the second pass).
//
// The error path matters for the same reason as any truncation bug: an
// iterator that swallows a read error yields a short chunk stream, and
// the caller commits a manifest for a file whose tail was never read.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	mathrand "math/rand"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
)

type chunkFacts struct {
	offset int64
	length int
	sum    [32]byte
}

func collectIter(t *testing.T, c *chunker.Chunker, body []byte) []chunkFacts {
	t.Helper()
	var out []chunkFacts
	for ch, err := range c.Iter(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("Iter: %v", err)
		}
		out = append(out, chunkFacts{ch.Offset, len(ch.Data), sha256.Sum256(ch.Data)})
	}
	return out
}

func collectIterCopying(t *testing.T, c *chunker.Chunker, body []byte) []chunkFacts {
	t.Helper()
	var out []chunkFacts
	for ch, err := range c.IterCopying(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("IterCopying: %v", err)
		}
		out = append(out, chunkFacts{ch.Offset, len(ch.Data), sha256.Sum256(ch.Data)})
	}
	return out
}

func seeded(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(mathrand.New(mathrand.NewSource(seed)), b)
	return b
}

// The property: same input, same chunks, hash for hash.
func TestIterCopying_ProducesIdenticalChunksToIter(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"sub-minimum", []byte("tiny")},
		{"zeros-4MiB", make([]byte, 4<<20)},
		{"random-4MiB", seeded(4<<20, 0x51CE)},
		// Highly repetitive content is where content-defined boundaries
		// cluster; a refill-boundary bug shows up here first.
		{"repetitive-2MiB", bytes.Repeat([]byte("pg_hardstorage"), (2<<20)/14)},
		// Sizes straddling the internal buffer refill.
		{"just-over-1MiB", seeded((1<<20)+7, 0xBEEF)},
		{"just-under-1MiB", seeded((1<<20)-7, 0xBEEF)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := collectIter(t, chunker.New(), tc.body)
			got := collectIterCopying(t, chunker.New(), tc.body)

			if len(got) != len(want) {
				t.Fatalf("IterCopying produced %d chunks, Iter produced %d — a caller following "+
					"the documented \"safe default\" would write chunk hashes that match nothing "+
					"already in the CAS, and dedup against every existing chunk fails",
					len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("chunk %d differs: IterCopying{off=%d len=%d sha=%x} vs "+
						"Iter{off=%d len=%d sha=%x}",
						i, got[i].offset, got[i].length, got[i].sum[:8],
						want[i].offset, want[i].length, want[i].sum[:8])
				}
			}
		})
	}
}

type errAfter struct {
	body []byte
	n    int
	err  error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if e.n >= len(e.body) {
		return 0, e.err
	}
	n := copy(p, e.body[e.n:])
	e.n += n
	return n, nil
}

// A swallowed read error is a truncated backup that reports success.
func TestIterCopying_PropagatesReadErrors(t *testing.T) {
	boom := errors.New("disk went away")
	r := &errAfter{body: seeded(3<<20, 0xF00D), err: boom}

	var saw error
	chunks := 0
	for ch, err := range chunker.New().IterCopying(r) {
		if err != nil {
			saw = err
			break
		}
		chunks += len(ch.Data)
	}
	if saw == nil {
		t.Fatalf("IterCopying swallowed the read error after %d bytes — the caller would commit "+
			"a manifest for a file whose tail was never read", chunks)
	}
	if !errors.Is(saw, boom) {
		t.Errorf("propagated %v, want %v", saw, boom)
	}
}

// Retained slices must stay valid — the entire reason the copying
// variant exists. Reconstructing from every retained slice at the end
// proves no slice was rewritten underneath the caller.
func TestIterCopying_EveryRetainedSliceSurvivesToTheEnd(t *testing.T) {
	body := seeded(4<<20, 0xC0FFEE)
	var retained [][]byte
	for ch, err := range chunker.New().IterCopying(bytes.NewReader(body)) {
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, ch.Data)
	}
	var rebuilt []byte
	for _, s := range retained {
		rebuilt = append(rebuilt, s...)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatal("retained slices did not reconstruct the input — a chunk was rewritten " +
			"after being yielded, which is the exact corruption IterCopying exists to prevent")
	}
}

// Early termination must not leak or panic, and must stop promptly.
func TestIterCopying_StopsEarly(t *testing.T) {
	seen := 0
	for _, err := range chunker.New().IterCopying(bytes.NewReader(seeded(4<<20, 1))) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Errorf("iterated %d times after breaking at 3", seen)
	}
}

// Sizes must report the bounds actually in force: a caller sizing a
// buffer from Sizes and receiving a larger chunk overruns it.
func TestSizes_ReportsTheParametersInForce(t *testing.T) {
	min, avg, max := chunker.New().Sizes()
	if min != chunker.DefaultMinSize || avg != chunker.DefaultAvgSize || max != chunker.DefaultMaxSize {
		t.Errorf("New().Sizes() = (%d, %d, %d), want the documented defaults (%d, %d, %d)",
			min, avg, max, chunker.DefaultMinSize, chunker.DefaultAvgSize, chunker.DefaultMaxSize)
	}

	c := chunker.NewWithParams(1024, 4096, 16384)
	gotMin, gotAvg, gotMax := c.Sizes()
	if gotMin != 1024 || gotAvg != 4096 || gotMax != 16384 {
		t.Fatalf("Sizes() = (%d, %d, %d), want (1024, 4096, 16384)", gotMin, gotAvg, gotMax)
	}
	// And the bounds must actually hold, so Sizes is a promise rather
	// than a stored triple nothing honours.
	for ch, err := range c.Iter(bytes.NewReader(seeded(1<<20, 7))) {
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Data) > gotMax {
			t.Fatalf("chunk of %d bytes exceeds the max Sizes() reports (%d) — a caller sizing "+
				"a buffer from Sizes overruns it", len(ch.Data), gotMax)
		}
	}
}
