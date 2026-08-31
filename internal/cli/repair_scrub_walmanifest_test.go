package cli

// The `repair scrub` command verifies WAL chunks by re-decoding each WAL
// segment manifest through a LOCAL anonymous struct rather than through
// walsink.SegmentManifest — the real type would pull an import cycle. The
// source comment states the resulting constraint plainly:
//
//	Stable as long as the manifest's `chunks[].hash` key stays.
//
// Nothing enforced that. If the producer's JSON tag ever drifts,
// json.Unmarshal still SUCCEEDS against the local struct and simply
// yields zero chunks — so scrubManifestAware verifies zero WAL chunks
// and the scrub reports a clean repository while every WAL chunk goes
// unexamined. A corruption scanner that silently scans nothing is worse
// than no scanner: it produces a green result an operator will trust.
//
// These tests bind the decoder to the producing type, so the drift the
// comment warns about turns the suite red at the moment the tag changes.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func scrubTestStore(t *testing.T) storage.StoragePlugin {
	t.Helper()
	u, err := url.Parse("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func putBytes(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, strings.NewReader(string(body)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func hashN(b byte) repo.Hash {
	var h repo.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// The contract test: bytes produced by the REAL producer type must decode
// to the exact hashes it carried.
func TestScrubWALManifestHashes_MatchesProducerEncoding(t *testing.T) {
	sp := scrubTestStore(t)
	want := []repo.Hash{hashN(0x11), hashN(0x22), hashN(0x33)}

	m := &walsink.SegmentManifest{
		Schema:        "wal-segment-manifest/v1",
		Deployment:    "dep",
		Timeline:      1,
		SegmentNumber: 7,
		SegmentName:   "000000010000000000000007",
		SegmentSize:   16 << 20,
		CreatedAt:     time.Unix(0, 0).UTC(),
	}
	for i, h := range want {
		m.Chunks = append(m.Chunks, walsink.ChunkRef{Hash: h, Offset: int64(i) * 100, Len: 100})
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	putBytes(t, sp, "wal/dep/000000010000000000000007.json", body)

	got, err := scrubWALManifestHashes(context.Background(), sp, "wal/dep/000000010000000000000007.json")
	if err != nil {
		t.Fatalf("scrubWALManifestHashes: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d hashes from a manifest carrying %d — `repair scrub` would verify "+
			"only those and still report the repository clean", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hash[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// The drift this guards against, spelled out: a manifest whose chunk
// entries use a different key decodes to nothing without an error. The
// caller treats (nil, nil) as "this segment has no chunks", not as a
// problem — which is why the contract test above has to exist.
func TestScrubWALManifestHashes_TagDriftDecodesToNothingSilently(t *testing.T) {
	sp := scrubTestStore(t)
	drifted := []byte(`{"schema":"wal-segment-manifest/v1","chunks":[` +
		`{"chunk_hash":"1111111111111111111111111111111111111111111111111111111111111111"}]}`)
	putBytes(t, sp, "wal/dep/drift.json", drifted)

	got, err := scrubWALManifestHashes(context.Background(), sp, "wal/dep/drift.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d hashes; the point of this test is that drift yields ZERO "+
			"with no error — if that changed, revisit the guard above", len(got))
	}
}

// A malformed hash is dropped rather than reported. That is deliberate
// (a scrub should not die on one bad byte), but it must not take the
// manifest's GOOD hashes down with it.
func TestScrubWALManifestHashes_MalformedHashDoesNotDropTheGoodOnes(t *testing.T) {
	sp := scrubTestStore(t)
	good := hashN(0xAB)
	raw := map[string]any{
		"schema": "wal-segment-manifest/v1",
		"chunks": []any{
			map[string]any{"hash": "zznothex"},                             // not hex
			map[string]any{"hash": "abcd"},                                 // right alphabet, wrong length
			map[string]any{"hash": strings.Repeat("ab", len(repo.Hash{}))}, // valid
			map[string]any{"hash": ""},                                     // empty
		},
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	putBytes(t, sp, "wal/dep/mixed.json", body)

	got, err := scrubWALManifestHashes(context.Background(), sp, "wal/dep/mixed.json")
	if err != nil {
		t.Fatalf("scrubWALManifestHashes: %v", err)
	}
	if len(got) != 1 || got[0] != good {
		t.Fatalf("got %d hashes (%s), want exactly the one valid entry %s — a scrub that "+
			"drops verifiable chunks alongside unparseable ones under-reports corruption",
			len(got), got, good)
	}
}

// Storage and parse failures must be distinguishable from "no chunks":
// the caller skips the manifest on error but treats a nil slice as a
// segment with nothing to verify.
func TestScrubWALManifestHashes_ErrorsAreErrors(t *testing.T) {
	sp := scrubTestStore(t)

	if _, err := scrubWALManifestHashes(context.Background(), sp, "wal/dep/absent.json"); err == nil {
		t.Error("a missing manifest must return an error, not an empty hash list")
	}

	putBytes(t, sp, "wal/dep/garbage.json", []byte("{not json"))
	if _, err := scrubWALManifestHashes(context.Background(), sp, "wal/dep/garbage.json"); err == nil {
		t.Error("an unparseable manifest must return an error, not an empty hash list")
	}
}
