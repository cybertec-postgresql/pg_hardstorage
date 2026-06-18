package cli

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

type recordRetSP struct {
	storage.StoragePlugin
	hit   bool
	key   string
	until time.Time
	mode  storage.WORMMode
}

func (s *recordRetSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	s.hit = true
	s.key = key
	s.until = until
	s.mode = mode
	return s.StoragePlugin.SetRetention(ctx, key, until, mode)
}

// TestInstallManifestOverwrite_AppliesWORMLock pins that repair manifest's
// primary overwrite and repair attestation's re-install both re-apply the
// repo's WORM lock — a repair on a compliance repo must not leave the copy
// it just rewrote freely deletable.
func TestInstallManifestOverwrite_AppliesWORMLock(t *testing.T) {
	root := t.TempDir()
	base := &fs.Plugin{}
	if err := base.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("fs open: %v", err)
	}
	defer base.Close()

	key := "manifests/db1/backups/x/manifest.json"
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	rec := &recordRetSP{StoragePlugin: base}
	if err := installManifestOverwrite(context.Background(), rec, key, []byte("manifest-body"), "test", until, storage.WORMCompliance); err != nil {
		t.Fatalf("installManifestOverwrite (worm): %v", err)
	}
	if !rec.hit {
		t.Fatal("installManifestOverwrite must apply WORM retention when a deadline is set")
	}
	if rec.key != key {
		t.Errorf("retention key = %q, want %q", rec.key, key)
	}
	if !rec.until.Equal(until) {
		t.Errorf("retention until = %v, want %v", rec.until, until)
	}
	if rec.mode != storage.WORMCompliance {
		t.Errorf("retention mode = %q, want compliance", rec.mode)
	}

	// No deadline → no retention call (plain non-WORM repo).
	rec2 := &recordRetSP{StoragePlugin: base}
	if err := installManifestOverwrite(context.Background(), rec2, key, []byte("again"), "test", time.Time{}, ""); err != nil {
		t.Fatalf("installManifestOverwrite (no worm): %v", err)
	}
	if rec2.hit {
		t.Error("no retention call should be made when the deadline is zero")
	}
}
