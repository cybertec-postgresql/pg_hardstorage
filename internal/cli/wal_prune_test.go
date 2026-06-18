package cli_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// plantBackupAtCLI plants a minimal backup manifest at
// manifests/<dep>/backups/<id>/manifest.json — only the fields
// walprune's partial-decode reads (backup_id, start_lsn,
// stopped_at). The CLI's loadVerifier never sees this manifest
// (wal prune doesn't read manifests through the signed path).
func plantBackupAtCLI(t *testing.T, repoURL, deployment, backupID, startLSN string, stoppedAt time.Time) {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	body := map[string]any{
		"backup_id":  backupID,
		"start_lsn":  startLSN,
		"stopped_at": stoppedAt.UTC().Format(time.RFC3339Nano),
	}
	enc, _ := stdjson.Marshal(body)
	key := fmt.Sprintf("manifests/%s/backups/%s/manifest.json", deployment, backupID)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc))}); err != nil {
		t.Fatal(err)
	}
}

// plantWALSegmentAtCLI plants a minimal WAL segment manifest with
// the (end_lsn, created_at, chunks) fields walprune reads.
func plantWALSegmentAtCLI(t *testing.T, repoURL, deployment string, tli uint32, segName, endLSN string, createdAt time.Time) {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	body := map[string]any{
		"end_lsn":    endLSN,
		"created_at": createdAt.UTC().Format(time.RFC3339Nano),
		"chunks":     []map[string]any{{"hash": fmt.Sprintf("%064x", 1), "len": 1024}},
	}
	enc, _ := stdjson.Marshal(body)
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, segName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc))}); err != nil {
		t.Fatal(err)
	}
}

// TestWalPrune_RequiresRepo: cobra-level missing-flag.
func TestWalPrune_RequiresRepo(t *testing.T) {
	_, stderr, exit := runCLI(t, "wal", "prune", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestWalPrune_NoBackup_NoOp: a deployment with no backups is a
// clean no-op (no frontier → conservative).
func TestWalPrune_NoBackup_NoOp(t *testing.T) {
	repoURL := initRepoForTest(t)
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000005", "0/06000000", time.Now())

	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("no-backup case should exit OK; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"segments_considered": 0`) {
		t.Errorf("expected considered=0:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segments_deleted": 0`) {
		t.Errorf("expected deleted=0:\n%s", stdout)
	}
}

// TestWalPrune_DryRunDefault: without --apply, the command is
// dry-run by default (matches every other destructive op's posture).
func TestWalPrune_DryRunDefault(t *testing.T) {
	repoURL := initRepoForTest(t)
	plantBackupAtCLI(t, repoURL, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000001", "0/02000000",
		time.Now().Add(-3*time.Hour))

	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run default: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Errorf("expected dry_run=true (default):\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segments_deleted": 1`) {
		t.Errorf("expected 1 candidate reported:\n%s", stdout)
	}
}

// TestWalPrune_ApplyDeletesSegments: --apply actually deletes the
// pre-frontier segments.
func TestWalPrune_ApplyDeletesSegments(t *testing.T) {
	repoURL := initRepoForTest(t)
	plantBackupAtCLI(t, repoURL, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000001", "0/02000000",
		time.Now().Add(-3*time.Hour))
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000005", "0/06000000",
		time.Now().Add(-30*time.Minute))

	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"--apply",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("apply: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"dry_run": false`) {
		t.Errorf("dry_run should be false with --apply:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segments_deleted": 1`) {
		t.Errorf("expected 1 segment deleted:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segments_kept": 1`) {
		t.Errorf("expected 1 segment kept:\n%s", stdout)
	}

	// Verify on disk: pre-frontier segment gone, post-frontier kept.
	u, _ := url.Parse(repoURL)
	sp := &fs.Plugin{}
	sp.Open(context.Background(), storage.StorageConfig{URL: u})
	defer sp.Close()
	if _, err := sp.Stat(context.Background(),
		"wal/db1/00000001/000000010000000000000001.json"); err == nil {
		t.Error("pre-frontier WAL manifest should have been deleted")
	}
	if _, err := sp.Stat(context.Background(),
		"wal/db1/00000001/000000010000000000000005.json"); err != nil {
		t.Errorf("post-frontier WAL manifest disappeared: %v", err)
	}
}

// TestWalPrune_KeepSinceFloor: --keep-since N keeps young segments
// even when their LSN is below the frontier.
func TestWalPrune_KeepSinceFloor(t *testing.T) {
	repoURL := initRepoForTest(t)
	now := time.Now().UTC()
	plantBackupAtCLI(t, repoURL, "db1", "db1.full.aaa",
		"0/05000000", now.Add(-1*time.Hour))
	// Young segment (1 hour ago) but pre-frontier LSN.
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000001", "0/02000000",
		now.Add(-1*time.Hour))

	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"--apply",
		"--keep-since", "24h",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("keep-since: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"segments_kept_by_floor": 1`) {
		t.Errorf("expected 1 segment kept by floor:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segments_deleted": 0`) {
		t.Errorf("expected no deletes (floor protects):\n%s", stdout)
	}
}

// TestWalPrune_RejectsNegativeKeepSince: --keep-since must be >= 0.
func TestWalPrune_RejectsNegativeKeepSince(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, stderr, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"--keep-since", "-5h",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("negative --keep-since should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestWalPrune_TextRender: the operator-friendly text body has the
// frontier line and a "next step" hint mentioning repo gc.
func TestWalPrune_TextRender(t *testing.T) {
	repoURL := initRepoForTest(t)
	plantBackupAtCLI(t, repoURL, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegmentAtCLI(t, repoURL, "db1", 1,
		"000000010000000000000001", "0/02000000",
		time.Now().Add(-3*time.Hour))

	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"--apply",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("text apply: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"wal prune — db1",
		"Frontier:",
		"db1.full.aaa",
		"Segments:",
		"repo gc --apply",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestWalPrune_NoBackup_TextRendersHint: text-mode body for the
// no-backup case offers an actionable next step.
func TestWalPrune_NoBackup_TextRendersHint(t *testing.T) {
	repoURL := initRepoForTest(t)
	stdout, _, exit := runCLI(t,
		"wal", "prune", "db1",
		"--repo", repoURL,
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "No non-tombstoned backup") {
		t.Errorf("expected no-frontier message:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Take a backup first") {
		t.Errorf("expected actionable hint:\n%s", stdout)
	}
}
