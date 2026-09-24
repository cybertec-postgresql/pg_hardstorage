package walsink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// After a refusal over a swept chunk the CAS must forget that chunk:
// otherwise the retry's PutChunk hits the in-memory seen cache,
// "dedups" against the deleted chunk again and fails the same way on
// every attempt — the stream can never archive that segment.
func TestSink_SweptChunkIsForgottenSoTheRetryRewritesIt(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, int(walsink.SegmentSize))
	recurring := firstChunkOf(t, body)
	h := repo.HashOf(recurring)

	sp := openFsRepo(t)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), recurring); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp)
	swept := false
	s, err := walsink.New(cas, sp, walsink.Options{
		Deployment: "db1", Timeline: 1, SystemIdentifier: "7388123456789",
		FaultHook: func(ctx context.Context, checkpoint string) error {
			if checkpoint == "before_manifest_commit" && !swept {
				swept = true
				_ = sp.Delete(ctx, repo.ChunkKey(h))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), replication.XLogRecord{WALStart: pglogrepl.LSN(0), Data: body}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err == nil || !swept {
		t.Fatalf("setup: want a refusal over the swept chunk (swept=%v, err=%v)", swept, err)
	}
	if cas.WasAdopted(h) {
		t.Fatal("the swept chunk is still marked adopted after the refusal")
	}
	info, err := cas.PutChunk(context.Background(), recurring)
	if err != nil {
		t.Fatal(err)
	}
	if info.Deduped {
		t.Fatal("the retry's PutChunk deduplicated against the deleted chunk instead of rewriting it")
	}
	if _, err := sp.Stat(context.Background(), repo.ChunkKey(h)); err != nil {
		t.Fatalf("retry did not rewrite the chunk: %v", err)
	}
}

// A WAL commit over adopted chunks while a `repo gc --apply` run is live
// must pin those chunks and wait for the run's in-flight batch — WAL
// writers hold no backup lease, so the fence is their only exclusion.
func TestSink_CommitDuringLiveGCRunPinsAdoptedChunks(t *testing.T) {
	body := bytes.Repeat([]byte{0xCD}, int(walsink.SegmentSize))
	recurring := firstChunkOf(t, body)

	sp := openFsRepo(t)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), recurring); err != nil {
		t.Fatal(err)
	}
	// A live gc run (the record gc publishes before deciding deletions).
	now := time.Now().UTC()
	writeRun := func(state string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"schema": "pg_hardstorage.gc_run.v1", "run_id": "t1", "state": state, "seq": 1,
			"started_at": now, "heartbeat_at": time.Now().UTC(), "expires_at": time.Now().UTC().Add(time.Hour),
		})
		if _, err := sp.Put(context.Background(), "gc/runs/t1.json", bytes.NewReader(b), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	writeRun("running")

	s, err := walsink.New(casdefault.New(sp), sp, walsink.Options{
		Deployment: "db1", Timeline: 1, SystemIdentifier: "7388123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), replication.XLogRecord{WALStart: pglogrepl.LSN(0), Data: body}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close(context.Background()) }()

	pinned := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !pinned {
		for info, lerr := range sp.List(context.Background(), "gc/pins/") {
			if lerr == nil && info.Key != "" {
				pinned = true
			}
		}
		if !pinned {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !pinned {
		t.Fatal("the WAL commit wrote no gc pin while a gc run was live — it is unprotected against the sweep")
	}
	select {
	case err := <-done:
		t.Fatalf("the commit did not wait for the live gc run's batch (returned %v)", err)
	default:
	}
	writeRun("finished")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("commit after the gc run finished: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the commit never proceeded after the gc run finished")
	}
}
