// Package gccontract drives repo.Sweep (`repo gc`) against any storage
// backend: live chunks survive, orphans and stale temps go, dry-run equals
// the real run and a second gc is a no-op.
package gccontract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func put(t *testing.T, sp storage.StoragePlugin, key, body string) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader([]byte(body)),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func exists(t *testing.T, sp storage.StoragePlugin, key string) bool {
	t.Helper()
	_, err := sp.Stat(context.Background(), key)
	if err != nil && !isNotFound(err) {
		t.Fatalf("stat %s: %v", key, err)
	}
	return err == nil
}

func isNotFound(err error) bool {
	return err != nil && (err == storage.ErrNotFound || errors.Is(err, storage.ErrNotFound))
}

// Run seeds a repository on sp and sweeps it. The backend's mtimes are
// real, so the age floors are disabled and the settle shortened; the age
// logic has its own tests on fs.
func Run(t *testing.T, sp storage.StoragePlugin) {
	ctx := context.Background()
	savedSettle := repo.GCFenceSettle
	repo.GCFenceSettle = 0
	t.Cleanup(func() { repo.GCFenceSettle = savedSettle })

	cas := repo.NewCAS(sp)
	var live, dead []repo.Hash
	for i := 0; i < 12; i++ {
		ci, err := cas.PutChunk(ctx, []byte(fmt.Sprintf("gccontract-%d-%s", i, t.Name())))
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			dead = append(dead, ci.Hash)
		} else {
			live = append(live, ci.Hash)
		}
	}
	var chunks string
	for i, h := range live {
		if i > 0 {
			chunks += ","
		}
		chunks += `{"hash":"` + h.String() + `"}`
	}
	put(t, sp, "manifests/db1/backups/b1/manifest.json", `{"files":[{"chunks":[`+chunks+`]}]}`)
	put(t, sp, "manifests/db1/backups/b2/manifest.json.tmp.zzz", "partial")
	put(t, sp, "chunks/sha256/zz/notachunk", "x")
	time.Sleep(1100 * time.Millisecond) // second-granular backends: everything older than the run start

	opts := repo.SweepOptions{TombstoneGrace: -1, MinChunkAge: -1}
	dry, err := repo.Sweep(ctx, sp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if dry.OrphanCount != len(dead) || len(dry.StaleTemps) != 1 || dry.Deleted != 0 {
		t.Fatalf("dry run: %+v (want %d orphans, 1 stale)", dry, len(dead))
	}
	for _, h := range dead {
		if !exists(t, sp, repo.ChunkKey(h)) {
			t.Fatal("dry run deleted an orphan")
		}
	}
	opts.Apply = true
	res, err := repo.Sweep(ctx, sp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanCount != dry.OrphanCount || res.Deleted != len(dead) || res.FailureCount != 0 || res.StaleTempDeleted != 1 {
		t.Fatalf("apply != dry run: dry=%+v apply=%+v", dry, res)
	}
	for _, h := range live {
		if !exists(t, sp, repo.ChunkKey(h)) {
			t.Fatalf("live chunk %s deleted", h)
		}
	}
	for _, h := range dead {
		if exists(t, sp, repo.ChunkKey(h)) {
			t.Fatalf("orphan %s survived", h)
		}
	}
	if exists(t, sp, "manifests/db1/backups/b2/manifest.json.tmp.zzz") {
		t.Fatal("stale temp survived")
	}
	if !exists(t, sp, "chunks/sha256/zz/notachunk") || !exists(t, sp, "manifests/db1/backups/b1/manifest.json") {
		t.Fatal("gc touched a non-orphan object")
	}
	again, err := repo.Sweep(ctx, sp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.OrphanCount != 0 || again.Deleted != 0 || len(again.StaleTemps) != 0 {
		t.Fatalf("second gc not a no-op: %+v", again)
	}
	// Only gc's own run records may remain beside the repo's objects.
	for info, err := range sp.List(ctx, "gc/") {
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Key) < 8 || info.Key[:8] != "gc/runs/" {
			t.Fatalf("unexpected gc leftover %s", info.Key)
		}
	}
}
