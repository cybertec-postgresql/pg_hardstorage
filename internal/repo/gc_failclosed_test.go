package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestCollectReferences_FailsClosedOnUnparseableBackupHash pins
// poor-error-handling / data-loss audit #1: a live backup manifest whose
// chunk hash GC can't parse must FAIL reference collection, not skip the
// hash. Skipping it would drop the chunk from the live set, and a later
// GC would delete a still-referenced chunk → an unrestorable backup.
func TestCollectReferences_FailsClosedOnUnparseableBackupHash(t *testing.T) {
	sp, _ := newGCRepo(t)
	bad := `{"files":[{"chunks":[{"hash":"not-a-valid-64-char-lowercase-hex-sha256-value"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(bad), storage.PutOptions{ContentLength: int64(len(bad))}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.CollectReferences(context.Background(), sp)
	if err == nil {
		t.Fatal("CollectReferences must fail closed on an unparseable chunk hash; got nil — a partial ref set would let GC delete live chunks")
	}
	if !strings.Contains(err.Error(), "unparseable chunk hash") {
		t.Errorf("error should name the unparseable-hash cause; got %v", err)
	}
}

// TestCollectReferences_FailsClosedOnUnparseableWALHash: same guarantee
// for the WAL-segment-manifest harvest path.
func TestCollectReferences_FailsClosedOnUnparseableWALHash(t *testing.T) {
	sp, _ := newGCRepo(t)
	bad := `{"chunks":[{"hash":"zzzz"}]}`
	if _, err := sp.Put(context.Background(), "wal/db1/00000001/000000010000000000000003.json",
		readerOf(bad), storage.PutOptions{ContentLength: int64(len(bad))}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.CollectReferences(context.Background(), sp)
	if err == nil {
		t.Fatal("CollectReferences must fail closed on an unparseable WAL chunk hash; got nil")
	}
	if !strings.Contains(err.Error(), "unparseable chunk hash") {
		t.Errorf("error should name the unparseable-hash cause; got %v", err)
	}
}

// TestCollectReferences_ValidHashesStillSucceed: the fail-closed change
// must not break the happy path — a well-formed manifest collects cleanly.
func TestCollectReferences_ValidHashesStillSucceed(t *testing.T) {
	sp, cas := newGCRepo(t)
	ci, err := cas.PutChunk(context.Background(), []byte("a real chunk"))
	if err != nil {
		t.Fatal(err)
	}
	good := `{"files":[{"chunks":[{"hash":"` + ci.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(good), storage.PutOptions{ContentLength: int64(len(good))}); err != nil {
		t.Fatal(err)
	}
	refs, err := repo.CollectReferences(context.Background(), sp)
	if err != nil {
		t.Fatalf("valid manifest must collect cleanly; got %v", err)
	}
	if refs.Len() != 1 {
		t.Errorf("refs.Len() = %d, want 1", refs.Len())
	}
}
