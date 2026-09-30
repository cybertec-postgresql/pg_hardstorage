package repo_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// vanishOnGetPlugin makes every Get of a key under one of the given
// prefixes behave as if a concurrent writer deleted the object between
// gc's List and its Get — the exact interleaving `wal prune` or
// retention produces when it runs beside gc.
type vanishOnGetPlugin struct {
	storage.StoragePlugin
	vanish []string
}

func (v *vanishOnGetPlugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	for _, p := range v.vanish {
		if strings.HasPrefix(key, p) {
			_ = v.StoragePlugin.Delete(ctx, key)
			return nil, storage.ErrNotFound
		}
	}
	return v.StoragePlugin.Get(ctx, key)
}

// A manifest deleted between List and Get references nothing: it must
// be skipped, not abort the whole collection. Aborting made every gc
// that overlapped a `wal prune` fail with collect_refs_failed, so on a
// busy shared repository gc could not complete at all.
func TestCollectReferences_ManifestDeletedBetweenListAndGet(t *testing.T) {
	sp, cas := newGCRepo(t)
	ctx := context.Background()

	live, err := cas.PutChunk(ctx, []byte("live backup chunk"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(key, body string) {
		t.Helper()
		if _, err := sp.Put(ctx, key, readerOf(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}
	put("manifests/db1/backups/b1/manifest.json",
		`{"files":[{"chunks":[{"hash":"`+live.Hash.String()+`"}]}]}`)
	put("wal/db1/00000001/000000010000000000000001.json",
		`{"chunks":[{"hash":"`+strings.Repeat("ab", 32)+`"}]}`)
	put("logical/db1/s1/0-0.json",
		`{"chunks":[{"hash":"`+strings.Repeat("cd", 32)+`"}]}`)

	v := &vanishOnGetPlugin{StoragePlugin: sp, vanish: []string{"wal/", "logical/"}}
	refs, err := repo.CollectReferences(ctx, v)
	if err != nil {
		t.Fatalf("CollectReferences aborted on a manifest deleted mid-walk: %v", err)
	}
	if !refs.Has(live.Hash) {
		t.Error("the surviving backup manifest's chunk is missing from the reference set")
	}
	if refs.Len() != 1 {
		t.Errorf("refs.Len() = %d, want 1 (the vanished manifests reference nothing)", refs.Len())
	}
}
