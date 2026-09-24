package fs_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// DeleteLazy removes the object (visible at once) with Delete's
// idempotent not-found semantics; only the directory fsync is skipped.
func TestDeleteLazy_RemovesAndIsIdempotent(t *testing.T) {
	p := openFS(t)
	ctx := context.Background()
	if _, err := p.Put(ctx, "chunks/sha256/aa/bb/x.chk", bytes.NewReader([]byte("x")), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var _ storage.LazyDeleter = p
	if err := storage.DeleteLazy(ctx, p, "chunks/sha256/aa/bb/x.chk"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, "chunks/sha256/aa/bb/x.chk"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object still visible after DeleteLazy: %v", err)
	}
	if err := p.DeleteLazy(ctx, "chunks/sha256/aa/bb/x.chk"); err != nil {
		t.Fatalf("second DeleteLazy of a missing key: %v", err)
	}
}
