package cost_test

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cost"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestCompute_EmptyRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: "file://" + dir}); err != nil {
		t.Fatal(err)
	}

	sp := openFSAt(t, "file://"+dir)
	defer sp.Close()

	r, err := cost.Compute(ctx, sp, "file://"+dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Schema != cost.SchemaCost {
		t.Errorf("Schema = %q", r.Schema)
	}
	if r.PricePerGBMonth != cost.DefaultPricePerGBMonth {
		t.Errorf("default price not applied: %v", r.PricePerGBMonth)
	}
	if r.TotalPhysicalBytes != 0 {
		t.Errorf("empty repo should have 0 physical bytes; got %d", r.TotalPhysicalBytes)
	}
	if len(r.Deployments) != 0 {
		t.Errorf("empty repo should have no deployments; got %d", len(r.Deployments))
	}
}

func TestCompute_WithFakeManifest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: "file://" + dir}); err != nil {
		t.Fatal(err)
	}

	sp := openFSAt(t, "file://"+dir)
	defer sp.Close()

	// Drop a synthetic manifest under manifests/db1/backups/<id>/manifest.json.
	// The cost walker only needs the file to exist with bytes — actual
	// JSON parsing for logical bytes happens when Compute reads it
	// through ManifestStore.List, which silently skips parse failures.
	body := []byte(`{"schema":"x","backup_id":"x","deployment":"db1","files":[]}`)
	if err := putKey(ctx, sp, "manifests/db1/backups/test/manifest.json", body); err != nil {
		t.Fatal(err)
	}
	// And a fake WAL segment manifest.
	if err := putKey(ctx, sp, "wal/db1/00000001/000000010000000000000001.json", body); err != nil {
		t.Fatal(err)
	}

	r, err := cost.Compute(ctx, sp, "file://"+dir, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	if r.PricePerGBMonth != 0.05 {
		t.Errorf("custom price not honoured: %v", r.PricePerGBMonth)
	}
	if r.ManifestBytes == 0 {
		t.Error("expected manifest bytes > 0")
	}
	if r.WALBytes == 0 {
		t.Error("expected WAL bytes > 0")
	}
	if r.TotalPhysicalBytes < r.ManifestBytes+r.WALBytes {
		t.Errorf("total %d should be >= manifest %d + wal %d",
			r.TotalPhysicalBytes, r.ManifestBytes, r.WALBytes)
	}
	if len(r.Deployments) != 1 || r.Deployments[0].Name != "db1" {
		t.Errorf("expected one deployment 'db1'; got %+v", r.Deployments)
	}
}

// --- helpers ---------------------------------------------------------

func openFSAt(t *testing.T, repoURL string) storage.StoragePlugin {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	return p
}

func putKey(ctx context.Context, sp storage.StoragePlugin, key string, body []byte) error {
	_, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	return err
}
