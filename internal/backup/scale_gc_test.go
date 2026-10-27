package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// TestScale_GCOverHugeManifest drives the engine behind `repo gc`
// (repo.Sweep) against a repository whose manifest holds
// PGHS_SCALE_CHUNKS chunk references (default 6M, a ~1.4 GB manifest the
// held readers refuse), and checks every cleanup it owns:
//
//   - dry run reports exactly the planted orphans and deletes nothing;
//   - apply deletes exactly those, keeps every referenced chunk (the
//     reference set is spilled: MemEntries is far below the chunk count),
//     reaps the stale manifest temp and the crashed writer's hidden
//     staging file, spares the fresh staging file, leaves its run
//     record marked finished, and stays inside a bounded heap;
//   - a second apply is a no-op;
//   - a tombstoned huge backup keeps its chunks inside the grace window
//     and releases them (only theirs, never a chunk another backup still
//     references) once the grace is disabled.
//
// Only a sample of the referenced chunks exist as objects: gc deletes
// orphans, it does not need the referenced bodies. Opt-in: PGHS_SCALE=1.
func TestScale_GCOverHugeManifest(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the gc large-manifest proof")
	}
	n := 6_000_000
	if v := os.Getenv("PGHS_SCALE_CHUNKS"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	const (
		chunksPerFile = 16384
		wantLive      = 20000 // referenced chunks that exist as objects
		wantOrphans   = 20000
		shared        = 500 // chunks both backups reference
	)
	{
		var st syscall.Statfs_t
		if err := syscall.Statfs(os.TempDir(), &st); err == nil {
			free := int64(st.Bavail) * int64(st.Bsize)
			need := int64(n) * 235 * 3
			if need > free/5 {
				t.Skipf("would need %d GiB of %d GiB free (> 20%%); lower PGHS_SCALE_CHUNKS", need>>30, free>>30)
			}
		}
	}
	stride := max(n/wantLive, 1)

	saved := repo.GCFenceSettle
	repo.GCFenceSettle = 0 // the settle wait is wall-clock and has its own tests
	t.Cleanup(func() { repo.GCFenceSettle = saved })

	ctx := context.Background()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	store := backup.NewManifestStore(sp)
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	chunkHash := func(i int) repo.Hash {
		var b [9]byte
		b[0] = 'c'
		for k := 0; k < 8; k++ {
			b[1+k] = byte(i >> (8 * k))
		}
		return repo.HashOf(b[:])
	}
	orphanHash := func(j int) repo.Hash { return repo.HashOf([]byte(fmt.Sprint("orphan-", j))) }
	putChunk := func(h repo.Hash) {
		body := []byte("x")
		if _, err := sp.Put(ctx, repo.ChunkKey(h), bytes.NewReader(body), storage.PutOptions{ContentLength: 1}); err != nil {
			t.Fatalf("put chunk: %v", err)
		}
	}
	exists := func(h repo.Hash) bool {
		_, err := sp.Stat(ctx, repo.ChunkKey(h))
		return err == nil
	}

	// Backup A: n distinct chunk references, committed through the spool.
	commit := func(dep, id string, hashes func(yield func(repo.Hash) bool), spillMem int) {
		t.Helper()
		spool, err := backup.NewManifestSpool(t.TempDir(), spillMem)
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
		var cur []backup.ChunkRef
		var off int64
		flush := func(f int) {
			if len(cur) == 0 {
				return
			}
			fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/%d", 100000+f), Mode: 0o600, Size: off, Chunks: cur}
			if err := spool.Add(&fe); err != nil {
				t.Fatal(err)
			}
			cur, off = nil, 0
		}
		f := 0
		hashes(func(h repo.Hash) bool {
			cur = append(cur, backup.ChunkRef{Hash: h, Offset: off, Len: 65536})
			off += 65536
			if len(cur) == chunksPerFile {
				flush(f)
				f++
			}
			return true
		})
		flush(f)
		h := validManifest()
		h.Files, h.Deployment, h.BackupID = nil, dep, id
		if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
			t.Fatalf("CommitSpooled %s: %v", id, err)
		}
	}
	const idA, idB = "db1.full.20260101T000000Z.0001", "db2.full.20260101T000000Z.0001"
	commit("db1", idA, func(yield func(repo.Hash) bool) {
		for i := 0; i < n; i++ {
			if !yield(chunkHash(i)) {
				return
			}
		}
	}, 500_000)
	// Backup B shares `shared` of A's existing chunks and adds its own.
	commit("db2", idB, func(yield func(repo.Hash) bool) {
		for j := 0; j < shared; j++ {
			yield(chunkHash(j * stride))
		}
		for j := 0; j < shared; j++ {
			yield(chunkHash(n + j))
		}
	}, 0)

	var liveA, onlyA, onlyB []repo.Hash
	for i := 0; i < n; i += stride {
		liveA = append(liveA, chunkHash(i))
		putChunk(chunkHash(i))
		if i/stride >= shared { // not one of B's shared chunks
			onlyA = append(onlyA, chunkHash(i))
		}
	}
	for j := 0; j < shared; j++ {
		onlyB = append(onlyB, chunkHash(n+j))
		putChunk(chunkHash(n + j))
	}
	orphans := make([]repo.Hash, wantOrphans)
	for j := range orphans {
		orphans[j] = orphanHash(j)
		putChunk(orphans[j])
	}

	// Cleanup targets: a stale manifest temp, a crashed writer's hidden
	// staging file, and a fresh staging file (an in-flight write).
	staleTemp := "manifests/db1/backups/crashed/manifest.json.tmp.1234"
	if _, err := sp.Put(ctx, staleTemp, bytes.NewReader([]byte("{")), storage.PutOptions{ContentLength: 1}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(staleTemp)), old, old); err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(root, "chunks", "sha256", "ab", "cd")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldStage := filepath.Join(stageDir, "abcd.chk.deferred-crashed")
	freshStage := filepath.Join(stageDir, "abcd.chk.deferred-inflight")
	for _, f := range []string{oldStage, freshStage} {
		if err := os.WriteFile(f, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldStage, old, old); err != nil {
		t.Fatal(err)
	}

	// Peak heap sampled while a sweep runs.
	var peak atomic.Uint64
	sweep := func(o repo.SweepOptions) *repo.SweepResult {
		t.Helper()
		o.MinChunkAge = -1
		o.RefSpill = spill.Options{MemEntries: 500_000, Dir: t.TempDir()}
		o.OnWarning = func(m string) { t.Errorf("gc warning: %s", m) }
		runtime.GC()
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			tk := time.NewTicker(25 * time.Millisecond)
			defer tk.Stop()
			for {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak.Load() {
					peak.Store(ms.HeapInuse)
				}
				select {
				case <-stop:
					return
				case <-tk.C:
				}
			}
		}()
		start := time.Now()
		res, err := repo.Sweep(ctx, sp, o)
		close(stop)
		<-done
		if err != nil {
			t.Fatalf("Sweep(apply=%v): %v", o.Apply, err)
		}
		t.Logf("sweep apply=%v: refs=%d orphans=%d deleted=%d in %s", o.Apply, res.RefCount, res.OrphanCount, res.Deleted, time.Since(start).Round(time.Millisecond))
		return res
	}
	assertExist := func(what string, hs []repo.Hash, want bool) {
		t.Helper()
		bad := 0
		for _, h := range hs {
			if exists(h) != want {
				bad++
			}
		}
		if bad != 0 {
			t.Errorf("%s: %d of %d chunks have the wrong existence (want exists=%v)", what, bad, len(hs), want)
		}
	}

	// 1. Dry run: reports, deletes nothing.
	res := sweep(repo.SweepOptions{})
	if res.RefCount != n+shared {
		t.Errorf("RefCount = %d, want %d distinct referenced chunks", res.RefCount, n+shared)
	}
	if res.OrphanCount != wantOrphans || res.Deleted != 0 {
		t.Errorf("dry run: %d orphans, %d deleted; want %d and 0", res.OrphanCount, res.Deleted, wantOrphans)
	}
	if len(res.StaleTemps) != 1 || res.StaleTempDeleted != 0 {
		t.Errorf("dry run stale temps = %v deleted=%d", res.StaleTemps, res.StaleTempDeleted)
	}
	assertExist("orphans after dry run", orphans, true)

	// 2. Apply: exactly the orphans go; everything referenced stays.
	res = sweep(repo.SweepOptions{Apply: true})
	if res.Deleted != wantOrphans || len(res.Failures) != 0 || res.SkippedReferenced != 0 {
		t.Errorf("apply: deleted=%d failures=%v skipped=%d; want %d, none, 0", res.Deleted, res.Failures, res.SkippedReferenced, wantOrphans)
	}
	assertExist("orphans after apply", orphans, false)
	assertExist("A's referenced chunks after apply", liveA, true)
	assertExist("B's chunks after apply", onlyB, true)
	if res.StaleTempDeleted != 1 {
		t.Errorf("stale manifest temp not reaped (deleted=%d)", res.StaleTempDeleted)
	}
	if _, err := sp.Stat(ctx, staleTemp); err == nil {
		t.Error("stale manifest temp survived gc --apply")
	}
	if res.StagingReaped != 1 {
		t.Errorf("StagingReaped = %d, want 1", res.StagingReaped)
	}
	if _, err := os.Stat(oldStage); !os.IsNotExist(err) {
		t.Errorf("crashed writer's staging file survived: %v", err)
	}
	if _, err := os.Stat(freshStage); err != nil {
		t.Errorf("gc removed an in-flight staging file: %v", err)
	}
	// The run record outlives the run (later runs reap it after its
	// retention) but must read "finished", or writers would treat gc as live.
	runs, _ := filepath.Glob(filepath.Join(root, "gc", "runs", "*.json"))
	if len(runs) != 1 {
		t.Errorf("want exactly one run record after one apply, got %d", len(runs))
	}
	for _, rf := range runs {
		var rec struct {
			State string `json:"state"`
		}
		b, _ := os.ReadFile(rf)
		if err := json.Unmarshal(b, &rec); err != nil || rec.State != "finished" {
			t.Errorf("gc run record %s is %q (%v), want finished", filepath.Base(rf), rec.State, err)
		}
	}
	// The huge backup must still be readable and signed after the sweep.
	if _, err := store.ReadStreaming(ctx, "db1", idA, verifier); err != nil {
		t.Errorf("huge manifest unreadable after gc: %v", err)
	}

	// 3. Idempotent.
	if res = sweep(repo.SweepOptions{Apply: true}); res.Deleted != 0 || res.OrphanCount != 0 {
		t.Errorf("second apply: deleted=%d orphans=%d, want a no-op", res.Deleted, res.OrphanCount)
	}

	// 4. Tombstone the huge backup. Inside the grace window it counts as live.
	if _, err := sp.Put(ctx, backup.TombstonePath("db1", idA), bytes.NewReader([]byte(`{"schema":"pg_hardstorage.tombstone.v1"}`)),
		storage.PutOptions{ContentLength: 40}); err != nil {
		t.Fatal(err)
	}
	if res = sweep(repo.SweepOptions{Apply: true}); res.Deleted != 0 {
		t.Errorf("a freshly tombstoned backup lost %d chunks inside the grace window", res.Deleted)
	}
	assertExist("A's chunks inside the grace window", liveA, true)

	// 5. Grace disabled: A's own chunks go, B's (shared and unique) stay.
	res = sweep(repo.SweepOptions{Apply: true, TombstoneGrace: -1})
	if res.Deleted != len(onlyA) || len(res.Failures) != 0 {
		t.Errorf("tombstone reap: deleted=%d failures=%v, want %d", res.Deleted, res.Failures, len(onlyA))
	}
	assertExist("chunks only the tombstoned backup referenced", onlyA, false)
	assertExist("chunks the live backup still references", append(liveA[:shared:shared], onlyB...), true)
	if res.RefCount != 2*shared {
		t.Errorf("RefCount after tombstone = %d, want %d (only B)", res.RefCount, 2*shared)
	}

	limit := uint64(128<<20) + uint64(n)*10
	t.Logf("%d chunk refs: peak heap during gc %d MiB (bound %d MiB)", n, peak.Load()>>20, limit>>20)
	if peak.Load() > limit {
		t.Errorf("peak heap %d MiB over the %d MiB bound", peak.Load()>>20, limit>>20)
	}
}
