package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// plantSegmentManifest writes a minimal committed WAL segment manifest
// stamped with sysID, the way the streamer would.
func plantSegmentManifest(t *testing.T, sp storage.StoragePlugin, deployment, sysID string, tli uint32, seg uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, seg, walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: sysID,
		Timeline:         tli,
		SegmentNumber:    seg,
		SegmentName:      name,
		StartLSN:         "0/1000000",
		EndLSN:           "0/2000000",
		SegmentSize:      16 << 20,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal segment manifest: %v", err)
	}
	key := walsink.SegmentPath(deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatalf("plant segment manifest: %v", err)
	}
}

// TestGuardSystemIdentifier pins the pg_upgrade / clone / restore guard:
// streaming into a deployment whose archived WAL carries a DIFFERENT
// pg_control system identifier is refused (preflight, exit 4); a matching
// identifier, the override flag, an empty live id, and a fresh deployment
// with no WAL all pass.
func TestGuardSystemIdentifier(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()

	const oldSys = "7000000000000000001"
	const newSys = "8000000000000000002" // post-pg_upgrade identifier
	plantSegmentManifest(t, sp, "db1", oldSys, 1, 5)

	// Same cluster (normal resume, or a promotion that keeps the sysid).
	if err := guardSystemIdentifier(ctx, sp, "db1", oldSys, false); err != nil {
		t.Errorf("matching system identifier must pass: %v", err)
	}

	// Different cluster (pg_upgrade) → hard refuse.
	err = guardSystemIdentifier(ctx, sp, "db1", newSys, false)
	if err == nil {
		t.Fatal("a changed system identifier must be refused")
	}
	if !strings.Contains(err.Error(), "preflight.system_identifier_changed") {
		t.Errorf("want preflight.system_identifier_changed code, got: %v", err)
	}
	if got := output.ExitCodeFor(err); got != output.ExitPreflight {
		t.Errorf("exit = %d, want ExitPreflight(%d)", got, output.ExitPreflight)
	}
	if !strings.Contains(err.Error(), oldSys) || !strings.Contains(err.Error(), newSys) {
		t.Errorf("error should name both identifiers: %v", err)
	}

	// Deliberate override.
	if err := guardSystemIdentifier(ctx, sp, "db1", newSys, true); err != nil {
		t.Errorf("--allow-system-identifier-change must pass: %v", err)
	}

	// Empty live identifier (couldn't probe) → conservative skip.
	if err := guardSystemIdentifier(ctx, sp, "db1", "", false); err != nil {
		t.Errorf("empty live identifier must skip: %v", err)
	}

	// Fresh deployment with no archived WAL → first stream establishes
	// the baseline, nothing to compare against.
	if err := guardSystemIdentifier(ctx, sp, "fresh", newSys, false); err != nil {
		t.Errorf("a deployment with no WAL must skip: %v", err)
	}
}

// TestDeploymentRecordedSysID_IgnoresHistoryAndTmp ensures the lookup
// reads only real per-segment manifests — not history aux files (no
// system_identifier) or in-flight .tmp staging files.
func TestDeploymentRecordedSysID_IgnoresHistoryAndTmp(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()

	// No WAL yet.
	if _, found, err := deploymentRecordedSysID(ctx, sp, "db1"); err != nil || found {
		t.Fatalf("empty deployment: found=%v err=%v", found, err)
	}

	// A history aux file and a tmp staging file must NOT satisfy the lookup.
	put := func(key string, body []byte) {
		if _, err := sp.Put(ctx, key, bytes.NewReader(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}
	put("wal/db1/history/00000002.history", []byte("1\t0/3000028\tno sysid here\n"))
	put("wal/db1/00000001/000000010000000000000003.json.tmp.deadbeef", []byte(`{"schema":"x"}`))
	if _, found, err := deploymentRecordedSysID(ctx, sp, "db1"); err != nil || found {
		t.Fatalf("history+tmp only must not resolve: found=%v err=%v", found, err)
	}

	// A real segment manifest resolves.
	plantSegmentManifest(t, sp, "db1", "7000000000000000001", 1, 3)
	got, found, err := deploymentRecordedSysID(ctx, sp, "db1")
	if err != nil || !found {
		t.Fatalf("real manifest must resolve: found=%v err=%v", found, err)
	}
	if got != "7000000000000000001" {
		t.Errorf("sysid = %q, want 7000000000000000001", got)
	}
}
