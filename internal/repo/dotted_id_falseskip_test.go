package repo_test

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func probeRepo(t *testing.T) (context.Context, storage.StoragePlugin) {
	t.Helper()
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return ctx, sp
}
func pput(t *testing.T, sp storage.StoragePlugin, ctx context.Context, key, body string) {
	t.Helper()
	if _, err := sp.Put(ctx, key, bytes.NewReader([]byte(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// PROBE 1: CollectReferences must harvest a committed WAL segment manifest
// under a deployment whose name contains ".json.tmp." — else its chunks are
// excluded from the ref set and gc reaps them (data loss).
func TestCollectReferences_HarvestsSegmentUnderDottedDeployment(t *testing.T) {
	ctx, sp := probeRepo(t)
	h := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	pput(t, sp, ctx, "wal/db.json.tmp.x/1/000000010000000000000001.json",
		`{"chunks":[{"hash":"`+h+`"}]}`)
	refs, err := repo.CollectReferences(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := repo.ParseHash(h)
	if !refs.Has(hash) {
		t.Fatalf("chunk of a COMMITTED WAL segment under deployment 'db.json.tmp.x' is ABSENT from the "+
			"ref set — gc would reap it (DATA LOSS). refs.Len=%d", refs.Len())
	}
}

// PROBE 2: wal prune's frontier must include a live backup whose ID contains
// ".tmp." — else the frontier advances past it and its WAL is pruned.
func TestWALPruneFrontier_IncludesBackupWithTmpInID(t *testing.T) {
	ctx, sp := probeRepo(t)
	// The min-start_lsn backup has ".tmp." in its ID.
	pput(t, sp, ctx, "manifests/db1/backups/db1.full.tmp.abc/manifest.json",
		`{"backup_id":"db1.full.tmp.abc","start_lsn":"0/1000000"}`)
	pput(t, sp, ctx, "manifests/db1/backups/plain/manifest.json",
		`{"backup_id":"plain","start_lsn":"0/9000000"}`)
	res, err := repo.WALPrune(ctx, sp, repo.WALPruneOptions{Deployment: "db1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pglogrepl.ParseLSN("0/1000000")
	got, _ := pglogrepl.ParseLSN(res.FrontierLSN)
	if got != want {
		t.Fatalf("frontier=%s want=0/1000000 — the min-start_lsn backup 'db1.full.tmp.abc' was SKIPPED "+
			"(\".tmp.\" in ID), frontier advanced past it → its WAL would be pruned (DATA LOSS). backupID=%q",
			res.FrontierLSN, res.FrontierBackupID)
	}
}
