package walsink_test

// adopted_refs_test.go — the WAL half of the dedup-vs-GC race gate.
//
// The backup half (internal/backup/runner.verifyAdoptedChunks) is
// backed by gc's timing guards: gc refuses --apply under a live backup
// lease. WAL has NO timing guard at all — a streamer holds no backup
// lease and runs for days, so lease-blocking gc on it would mean gc
// never runs. The commit-time gate is the only protection, which makes
// its failing-direction test matter more here than anywhere.
//
// The adoption route also differs. The streaming CAS carries no dedup
// hints, so the only way it adopts is the lost-IfNotExists-race branch:
// PutChunk attempts the write, the backend answers AlreadyExists, and
// the CAS trusts the existing object. Identical plaintext recurs in
// WAL — an unchanged page resurfacing as a full-page image — and if
// the matching chunk's only referents were expired-tombstone backups,
// gc legitimately sweeps it mid-stream.
//
// Staging note: the pipeline is asynchronous — a filled segment chunks
// and commits inside the background processor, so a test cannot
// interleave the sweep from outside. The first version of this file
// tried (OnRecord, then Delete, then Close) and the manifest was
// already committed before the delete ran: the swept-case test failed
// for the wrong reason. The FaultHook checkpoint
// "before_manifest_commit" is the supported seam — chunks adopted and
// durable, manifest unwritten — which is exactly where a concurrent
// gc sweep lands.

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// openFsRepo opens an empty fs-backed repo.
func openFsRepo(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// firstChunkOf learns, by a dry run of the same pipeline, the exact
// plaintext of one chunk the chunker emits for body — so the orphan
// can be pre-seeded with genuinely colliding content instead of
// guessing chunk boundaries.
func firstChunkOf(t *testing.T, body []byte) []byte {
	t.Helper()
	sp := openFsRepo(t)
	s, err := walsink.New(casdefault.New(sp), sp, walsink.Options{
		Deployment: "probe", Timeline: 1, SystemIdentifier: "7388123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0), Data: body,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("probe sink close: %v", err)
	}
	for info, lerr := range sp.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		h, perr := repo.ParseChunkKey(info.Key)
		if perr != nil {
			continue
		}
		pt, gerr := casdefault.New(sp).GetChunkBytes(context.Background(), h)
		if gerr != nil {
			t.Fatal(gerr)
		}
		return pt
	}
	t.Fatal("probe produced no chunks")
	return nil
}

// TestSink_AdoptedChunkSweptMidStream_RefusesCommit is the race,
// replayed at the walsink layer with the sweep landing at the
// before_manifest_commit checkpoint.
func TestSink_AdoptedChunkSweptMidStream_RefusesCommit(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, int(walsink.SegmentSize))
	recurring := firstChunkOf(t, body)
	h := repo.HashOf(recurring)

	sp := openFsRepo(t)
	// The orphan: written long ago by a backup whose tombstone expired.
	if _, err := casdefault.New(sp).PutChunk(context.Background(), recurring); err != nil {
		t.Fatal(err)
	}

	// gc's sweep lands after the chunks are adopted, before the
	// manifest commits.
	swept := false
	s, err := walsink.New(casdefault.New(sp), sp, walsink.Options{
		Deployment:       "db1",
		Timeline:         1,
		SystemIdentifier: "7388123456789",
		FaultHook: func(ctx context.Context, checkpoint string) error {
			if checkpoint == "before_manifest_commit" && !swept {
				swept = true
				if derr := sp.Delete(ctx, repo.ChunkKey(h)); derr != nil {
					t.Errorf("mid-pipeline delete: %v", derr)
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0), Data: body,
	}); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}

	err = s.Close(context.Background())
	if !swept {
		t.Fatal("the before_manifest_commit checkpoint never fired; the sweep was not " +
			"staged and this test proves nothing")
	}
	if err == nil {
		t.Fatal("the segment manifest committed over an adopted chunk that gc had deleted.\n\n" +
			"WAL has no other protection: streamers hold no backup lease, so gc's " +
			"live-lease refusal never fires for them. A committed segment referencing a " +
			"missing chunk is WAL that cannot be fetched at recovery — the archive looks " +
			"gap-free and is not.")
	}
	if !strings.Contains(err.Error(), "deduplicated against") {
		t.Errorf("refusal does not explain the adoption: %v", err)
	}
}

// TestSink_AdoptedChunkIntact_Commits: the same adoption with the
// chunk still present must commit — this path runs on every segment
// whose content recurs, and a false refusal kills healthy streams.
func TestSink_AdoptedChunkIntact_Commits(t *testing.T) {
	body := bytes.Repeat([]byte{0xCD}, int(walsink.SegmentSize))
	recurring := firstChunkOf(t, body)

	sp := openFsRepo(t)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), recurring); err != nil {
		t.Fatal(err)
	}
	s, err := walsink.New(casdefault.New(sp), sp, walsink.Options{
		Deployment: "db1", Timeline: 1, SystemIdentifier: "7388123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0), Data: body,
	}); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("a healthy adopted segment failed to commit: %v", err)
	}
	if _, err := sp.Stat(context.Background(),
		walsink.SegmentPath("db1", 1, walsink.SegmentFileName(1, 0, walsink.SegmentSize))); err != nil {
		t.Errorf("segment manifest not committed: %v", err)
	}
}
