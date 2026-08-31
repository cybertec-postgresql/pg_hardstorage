package repo_test

// GC's reference collection decodes every manifest in a repository
// through LOCAL anonymous shapes (backupManifestShape,
// walManifestShape) rather than through the types that produce them —
// importing backup, walsink and the logical sink into internal/repo
// would be an import cycle. The three producers are:
//
//	backup.Manifest            files[].chunks[].hash
//	walsink.SegmentManifest    chunks[].hash
//	chunked.SegmentManifest    chunks[].hash   (logical CDC)
//
// Nothing bound the decoders to the producers. The existing coverage
// feeds CollectReferences hand-written JSON literals, which assert
// gc's shape against a string the test author typed — so a producer
// renaming its tag leaves those tests green.
//
// The consequence is not a missed scan. json.Unmarshal SUCCEEDS against
// the local shape and yields zero chunks, so every chunk that manifest
// referenced looks unreferenced, and `repo gc --apply` DELETES it.
// gc's own source says so about the logical prefix: "Without walking
// it, every chunk a logical stream archived looks unreferenced and
// `repo gc --apply` reaps it — silently destroying the logical
// replication archive." Tag drift produces that outcome even with the
// walk in place.
//
// And drift is invited, not hypothetical: chunked.ChunkRef carries the
// comment "typed separately so a future schema evolution doesn't drag
// the physical schema with it".
//
// These tests marshal the REAL producer types and assert the chunks
// come back referenced, so the day a tag changes the suite fails
// instead of the archive disappearing.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func putBody(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// assertReferencedAndNotOrphan is the property under test for each
// producer: gc sees the hash, and a sweep would not delete it.
func assertReferencedAndNotOrphan(t *testing.T, sp storage.StoragePlugin, h repo.Hash, producer string) {
	t.Helper()
	refs, err := repo.CollectReferences(context.Background(), sp)
	if err != nil {
		t.Fatalf("CollectReferences: %v", err)
	}
	if !refs.Has(h) {
		t.Fatalf("gc did not harvest the chunk hash out of a real %s — its decoder and that "+
			"type have drifted apart, and `repo gc --apply` would DELETE this chunk", producer)
	}
	orphans, err := repo.FindOrphans(context.Background(), sp, refs)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("%d chunk(s) flagged orphan though a real %s references them — a sweep would "+
			"delete live data", len(orphans), producer)
	}
}

// The logical CDC archive: the producer whose type comment explicitly
// anticipates evolving independently.
func TestGC_HarvestsChunkedLogicalSegmentManifest(t *testing.T) {
	sp, cas := newGCRepo(t)
	ci, err := cas.PutChunk(context.Background(), []byte("logical-cdc-batch-payload"))
	if err != nil {
		t.Fatal(err)
	}

	m := &chunked.SegmentManifest{
		Schema:     chunked.Schema,
		Deployment: "db1",
		StreamName: "stream1",
		Slot:       "slot1",
		Plugin:     "pgoutput",
		StartLSN:   "0/3000028",
		EndLSN:     "0/30001A0",
		Records:    12,
		BytesIn:    int64(len("logical-cdc-batch-payload")),
		Chunks: []chunked.ChunkRef{
			{Hash: ci.Hash, Offset: 0, Len: ci.Size},
		},
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	// The sink commits with json.MarshalIndent (chunked.go), so the
	// test encodes the same way — the point is the TYPE and its tags,
	// not the whitespace.
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal logical segment manifest: %v", err)
	}
	putBody(t, sp, "logical/db1/stream1/0-3000028.json", body)

	assertReferencedAndNotOrphan(t, sp, ci.Hash, "chunked.SegmentManifest")
}

// The physical WAL archive.
func TestGC_HarvestsWALSinkSegmentManifest(t *testing.T) {
	sp, cas := newGCRepo(t)
	ci, err := cas.PutChunk(context.Background(), []byte("wal-segment-payload"))
	if err != nil {
		t.Fatal(err)
	}

	m := &walsink.SegmentManifest{
		Schema:        walsink.Schema,
		Deployment:    "db1",
		Timeline:      1,
		SegmentNumber: 7,
		SegmentName:   walsink.SegmentFileName(1, 7, walsink.SegmentSize),
		StartLSN:      "0/7000000",
		EndLSN:        "0/8000000",
		SegmentSize:   walsink.SegmentSize,
		Chunks: []walsink.ChunkRef{
			{Hash: ci.Hash, Offset: 0, Len: ci.Size},
		},
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal WAL segment manifest: %v", err)
	}
	putBody(t, sp,
		walsink.SegmentPath("db1", 1, walsink.SegmentFileName(1, 7, walsink.SegmentSize)), body)

	assertReferencedAndNotOrphan(t, sp, ci.Hash, "walsink.SegmentManifest")
}

// The backup manifest — a nested shape (files[].chunks[]), so it can
// drift at either level.
func TestGC_HarvestsBackupManifest(t *testing.T) {
	sp, cas := newGCRepo(t)
	ci, err := cas.PutChunk(context.Background(), []byte("base-backup-file-payload"))
	if err != nil {
		t.Fatal(err)
	}

	m := &backup.Manifest{
		Schema:     backup.Schema,
		BackupID:   "db1.full.001",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		Files: []backup.FileEntry{{
			Path: "base/1/1259",
			Size: ci.Size,
			Chunks: []backup.ChunkRef{
				{Hash: ci.Hash, Offset: 0, Len: ci.Size},
			},
		}},
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal backup manifest: %v", err)
	}
	putBody(t, sp, backup.PrimaryPath("db1", "db1.full.001"), body)

	assertReferencedAndNotOrphan(t, sp, ci.Hash, "backup.Manifest")
}
