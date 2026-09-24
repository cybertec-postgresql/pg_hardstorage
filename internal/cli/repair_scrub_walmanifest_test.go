package cli

// The scrubs read WAL segment manifests through the PRODUCER's own type
// (walsink.ParseSegmentManifest) — they need its encryption envelope to
// decrypt the chunks (issue #106). The old local anonymous-struct decoder
// could drift from the producer's JSON tags and silently decode zero
// chunks; decoding with the real type removes that class of bug, and
// these tests pin the remaining contract: the producer's bytes round-trip,
// and every failure is an error (counted as an unverifiable manifest by
// the caller), never an empty chunk list.

import (
	"context"
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

func TestReadWALSegmentManifest_MatchesProducerEncoding(t *testing.T) {
	sp := scrubTestStore(t)
	want := []repo.Hash{hashN(0x11), hashN(0x22), hashN(0x33)}
	m := &walsink.SegmentManifest{
		Schema:        walsink.Schema,
		Deployment:    "dep",
		Timeline:      1,
		SegmentNumber: 7,
		SegmentName:   "000000010000000000000007",
		SegmentSize:   16 << 20,
		CreatedAt:     time.Unix(0, 0).UTC(),
		Encryption:    &walsink.EncryptionInfo{Scheme: "aes-256-gcm", KEKRef: "local:default", WrappedDEK: "AAAA"},
	}
	for i, h := range want {
		m.Chunks = append(m.Chunks, walsink.ChunkRef{Hash: h, Offset: int64(i) * 100, Len: 100})
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := "wal/dep/00000001/000000010000000000000007.json"
	putBytes(t, sp, key, body)

	got, err := readWALSegmentManifest(context.Background(), sp, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunks) != len(want) {
		t.Fatalf("decoded %d chunks from a manifest carrying %d", len(got.Chunks), len(want))
	}
	for i := range want {
		if got.Chunks[i].Hash != want[i] {
			t.Errorf("hash[%d] = %s, want %s", i, got.Chunks[i].Hash, want[i])
		}
	}
	if got.Encryption == nil || got.Encryption.KEKRef != "local:default" {
		t.Errorf("encryption envelope not decoded: %+v", got.Encryption)
	}
}

// Every failure is an error — the caller counts it as an unverifiable
// manifest — and never a manifest with nothing to verify.
func TestReadWALSegmentManifest_ErrorsAreErrors(t *testing.T) {
	sp := scrubTestStore(t)
	ctx := context.Background()
	if _, err := readWALSegmentManifest(ctx, sp, "wal/dep/00000001/absent.json"); err == nil {
		t.Error("a missing manifest must return an error")
	}
	putBytes(t, sp, "wal/dep/garbage.json", []byte("{not json"))
	if _, err := readWALSegmentManifest(ctx, sp, "wal/dep/garbage.json"); err == nil {
		t.Error("an unparseable manifest must return an error")
	}
	putBytes(t, sp, "wal/dep/badhash.json",
		[]byte(`{"schema":"`+walsink.Schema+`","chunks":[{"hash":"zznothex"}]}`))
	if _, err := readWALSegmentManifest(ctx, sp, "wal/dep/badhash.json"); err == nil {
		t.Error("a manifest with an unparseable chunk hash must fail as a whole, not drop the chunk")
	}
}

func TestIsWALSegmentKey(t *testing.T) {
	for key, want := range map[string]bool{
		"wal/db1/00000001/000000010000000000000007.json":          true,
		"wal/db1/gaps/00000001-1700000000.json":                   false,
		"wal/db1/00000001/000000010000000000000007.json.tmp.abcd": false,
		"wal/db1/00000001/000000010000000000000007.history":       false,
	} {
		if got := isWALSegmentKey(key); got != want {
			t.Errorf("isWALSegmentKey(%q) = %v, want %v", key, got, want)
		}
	}
}
