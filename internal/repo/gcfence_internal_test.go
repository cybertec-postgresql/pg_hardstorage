package repo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// fenceWorld is a fs repo with fast fence timings.
func fenceWorld(t *testing.T) (storage.StoragePlugin, string) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	saved := [4]time.Duration{GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait}
	t.Cleanup(func() {
		GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait = saved[0], saved[1], saved[2], saved[3]
	})
	GCFenceSettle = 0
	GCFenceWriterBudget = time.Hour
	gcFencePoll = 5 * time.Millisecond
	GCFenceMaxWait = 10 * time.Second
	return sp, root
}

// oldOrphan writes a chunk aged past every floor and returns its hash.
func oldOrphan(t *testing.T, sp storage.StoragePlugin, root, body string) Hash {
	t.Helper()
	ci, err := NewCAS(sp).PutChunk(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(ChunkKey(ci.Hash))), old, old); err != nil {
		t.Fatal(err)
	}
	return ci.Hash
}

func commitRefManifest(t *testing.T, sp storage.StoragePlugin, id string, hs ...Hash) {
	t.Helper()
	var parts []string
	for _, h := range hs {
		parts = append(parts, `{"hash":"`+h.String()+`"}`)
	}
	body := `{"files":[{"chunks":[` + strings.Join(parts, ",") + `]}]}`
	if err := putJSONRaw(sp, "manifests/db1/backups/"+id+"/manifest.json", body); err != nil {
		t.Fatal(err)
	}
}

func putJSONRaw(sp storage.StoragePlugin, key, body string) error {
	_, err := sp.Put(context.Background(), key, strings.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))})
	return err
}

func chunkExists(t *testing.T, sp storage.StoragePlugin, h Hash) bool {
	t.Helper()
	_, err := sp.Stat(context.Background(), ChunkKey(h))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func waitForPin(t *testing.T, sp storage.StoragePlugin) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for info, err := range sp.List(context.Background(), gcPinsPrefix) {
			if err == nil && strings.HasSuffix(info.Key, ".json") {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("writer never published its pin")
}

// H43, the race the old gc left open: a writer adopts orphan X and
// starts committing WHILE gc is between its snapshot and its delete
// batch. The writer sees the live run, pins X and waits; gc's batch
// checkpoint reads the pin and spares X; the writer commits.
func TestGCFence_PinnedWriterIsSpared(t *testing.T) {
	sp, root := fenceWorld(t)
	x := oldOrphan(t, sp, root, "adopted by a backup mid-sweep")

	writerDone := make(chan error, 1)
	opts := SweepOptions{Apply: true, beforeBatch: func(b int) {
		if b != 0 {
			return
		}
		go func() {
			fence, err := BeginCommitFence(context.Background(), sp, []Hash{x}, FenceOptions{Owner: "test"})
			if err != nil {
				writerDone <- err
				return
			}
			if _, err := sp.Stat(context.Background(), ChunkKey(x)); err != nil {
				writerDone <- fmt.Errorf("adopted chunk gone before commit: %w", err)
				return
			}
			commitRefManifest(t, sp, "b1", x)
			writerDone <- fence.Confirm(context.Background())
		}()
		waitForPin(t, sp) // the pin exists before this batch reads pins
	}}
	res, err := Sweep(context.Background(), sp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if !chunkExists(t, sp, x) {
		t.Fatal("gc deleted a chunk a committing writer had pinned: the committed manifest is unrestorable")
	}
	if res.SkippedReferenced != 1 || res.Deleted != 0 {
		t.Errorf("SkippedReferenced=%d Deleted=%d; want 1/0", res.SkippedReferenced, res.Deleted)
	}
}

// The other half: the writer pins AFTER gc already read pins for the
// batch that deletes X. It must wait for that batch to finish, then its
// re-Stat sees X gone and it refuses to commit — never a manifest over a
// deleted chunk.
func TestGCFence_WriterWaitsOutInFlightBatchAndRefuses(t *testing.T) {
	sp, root := fenceWorld(t)
	x := oldOrphan(t, sp, root, "adopted just too late")

	var committed atomic.Bool
	writerDone := make(chan error, 1)
	opts := SweepOptions{Apply: true, beforeDelete: func(b int) {
		if b != 0 {
			return
		}
		go func() {
			_, err := FencedCommit(context.Background(), sp, NewCAS(sp), []Hash{x}, FenceOptions{Owner: "test"},
				func(context.Context) error { committed.Store(true); commitRefManifest(t, sp, "b2", x); return nil })
			writerDone <- err
		}()
		waitForPin(t, sp) // pinned, but this batch has already read pins
		// Give a writer that did NOT wait every chance to Stat and
		// commit before this batch deletes; the correct writer is
		// blocked on the batch checkpoint and cannot.
		time.Sleep(100 * time.Millisecond)
	}}
	if _, err := Sweep(context.Background(), sp, opts); err != nil {
		t.Fatal(err)
	}
	err := <-writerDone
	if !errors.Is(err, ErrAdoptedChunkSwept) {
		t.Fatalf("writer err = %v, want ErrAdoptedChunkSwept", err)
	}
	if committed.Load() {
		t.Fatal("writer committed a manifest over a chunk gc deleted in the batch it waited out")
	}
}

// No gc was running when the writer began, and the commit overran the
// writer budget while a gc run started and swept X: Confirm must notice
// the new run and report the committed manifest as broken.
func TestGCFence_ConfirmCatchesOverrunCommit(t *testing.T) {
	sp, root := fenceWorld(t)
	GCFenceWriterBudget = 10 * time.Millisecond
	x := oldOrphan(t, sp, root, "slow commit")

	fence, err := BeginCommitFence(context.Background(), sp, []Hash{x}, FenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fence.pinned {
		t.Fatal("no gc running: nothing to pin")
	}
	// gc runs to completion between the writer's Stat and its commit.
	if _, err := Sweep(context.Background(), sp, SweepOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	commitRefManifest(t, sp, "b3", x)
	time.Sleep(20 * time.Millisecond) // the commit "overran" the budget
	if err := fence.Confirm(context.Background()); !errors.Is(err, ErrAdoptedChunkSwept) {
		t.Fatalf("Confirm = %v, want ErrAdoptedChunkSwept", err)
	}
}

// Inside the budget, with no gc run started since R1, Confirm is free.
func TestGCFence_ConfirmQuietWithoutGC(t *testing.T) {
	sp, root := fenceWorld(t)
	x := oldOrphan(t, sp, root, "quiet")
	GCFenceWriterBudget = 0
	fence, err := BeginCommitFence(context.Background(), sp, []Hash{x}, FenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitRefManifest(t, sp, "b4", x)
	if err := fence.Confirm(context.Background()); err != nil {
		t.Fatalf("Confirm with no gc activity: %v", err)
	}
}

// A manifest committed after the snapshot, by a writer that knows
// nothing of the fence (an older binary), is still caught by the
// per-batch rescan.
func TestSweep_RescanSparesManifestCommittedMidSweep(t *testing.T) {
	sp, root := fenceWorld(t)
	x := oldOrphan(t, sp, root, "claimed mid-sweep")
	res, err := Sweep(context.Background(), sp, SweepOptions{Apply: true, beforeBatch: func(int) {
		commitRefManifest(t, sp, "late", x)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !chunkExists(t, sp, x) || res.SkippedReferenced != 1 {
		t.Fatalf("exists=%v skipped=%d: a manifest committed before the batch did not spare its chunk", chunkExists(t, sp, x), res.SkippedReferenced)
	}
}

// A backup lease that appears mid-sweep stops the remaining batches with
// a retry-safe error.
func TestSweep_LeaseAppearingMidSweepStops(t *testing.T) {
	sp, root := fenceWorld(t)
	for i := 0; i < 3; i++ {
		oldOrphan(t, sp, root, fmt.Sprintf("o%d", i))
	}
	calls := 0
	res, err := Sweep(context.Background(), sp, SweepOptions{Apply: true, BatchSize: 1,
		LiveLeases: func(context.Context) ([]string, error) {
			calls++
			if calls >= 3 { // start check, batch 0 ok, batch 1 sees a backup
				return []string{"db1"}, nil
			}
			return nil, nil
		}})
	if !errors.Is(err, ErrSweepBackupInFlight) {
		t.Fatalf("err = %v, want ErrSweepBackupInFlight", err)
	}
	if res == nil || res.Deleted != 1 || res.StoppedEarly == "" {
		t.Fatalf("res = %+v; want one batch deleted then a stop", res)
	}
}

// Never a chunk younger than the run's start, even with every floor off.
func TestSweep_NeverDeletesChunkYoungerThanRun(t *testing.T) {
	sp, root := fenceWorld(t)
	old := oldOrphan(t, sp, root, "old")
	fresh, err := NewCAS(sp).PutChunk(context.Background(), []byte("written after the run began"))
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now().Add(-time.Hour) // the run "started" an hour ago
	res, err := Sweep(context.Background(), sp, SweepOptions{Apply: true, MinChunkAge: -1, TombstoneGrace: -1,
		Now: func() time.Time { return begin }})
	if err != nil {
		t.Fatal(err)
	}
	if !chunkExists(t, sp, fresh.Hash) {
		t.Fatal("deleted a chunk written after the gc run started")
	}
	if chunkExists(t, sp, old) || res.Deleted != 1 {
		t.Fatalf("old orphan not reaped (deleted=%d)", res.Deleted)
	}
}

// The run record is published, marked finished, and a later writer sees
// no live run.
func TestSweep_RunRecordLifecycle(t *testing.T) {
	sp, _ := fenceWorld(t)
	res, err := Sweep(context.Background(), sp, SweepOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := listGCRuns(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d run records, want 1", len(runs))
	}
	for _, r := range runs {
		if r.RunID != res.RunID || r.State != "finished" || r.liveAt(time.Now()) {
			t.Fatalf("run record %+v; want finished run %s", r, res.RunID)
		}
	}
}
