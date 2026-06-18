package repo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// plantBackupManifest writes a minimal-but-decodable backup
// manifest at manifests/<dep>/backups/<id>/manifest.json. Includes
// only the fields walprune.oldestKeptBackupFrontier reads via
// partial-decode (backup_id, start_lsn, stopped_at).
func plantBackupManifest(t *testing.T, sp storage.StoragePlugin, deployment, backupID, startLSN string, stoppedAt time.Time) {
	t.Helper()
	body := map[string]any{
		"backup_id":  backupID,
		"start_lsn":  startLSN,
		"stopped_at": stoppedAt.UTC().Format(time.RFC3339Nano),
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("manifests/%s/backups/%s/manifest.json", deployment, backupID)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc))}); err != nil {
		t.Fatalf("plant backup manifest: %v", err)
	}
}

// plantTombstone marks a backup as soft-deleted.
func plantTombstone(t *testing.T, sp storage.StoragePlugin, deployment, backupID string) {
	t.Helper()
	key := fmt.Sprintf("manifests/%s/backups/%s/manifest.json.tombstone",
		deployment, backupID)
	body := []byte(`{"backup_id":"` + backupID + `"}`)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("plant tombstone: %v", err)
	}
}

// plantWALSegManifest writes a minimal WAL segment manifest with
// the (end_lsn, created_at, chunks) fields walprune reads.
func plantWALSegManifest(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segName, endLSN string, createdAt time.Time, chunkLens []int64) {
	t.Helper()
	type chunkRef struct {
		Hash string `json:"hash"`
		Len  int64  `json:"len"`
	}
	chunks := make([]chunkRef, len(chunkLens))
	for i, ln := range chunkLens {
		chunks[i] = chunkRef{
			Hash: fmt.Sprintf("%064x", i+1),
			Len:  ln,
		}
	}
	body := map[string]any{
		"end_lsn":    endLSN,
		"created_at": createdAt.UTC().Format(time.RFC3339Nano),
		"chunks":     chunks,
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, segName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc))}); err != nil {
		t.Fatalf("plant wal manifest: %v", err)
	}
}

// segExists reports whether a wal/<dep>/<tli>/<seg>.json manifest
// is present.
func segExists(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segName string) bool {
	t.Helper()
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, segName)
	_, err := sp.Stat(context.Background(), key)
	return err == nil
}

// TestWALPrune_NoBackup_NoOp: with zero backups in the deployment,
// WALPrune is an honest no-op. Conservative: without a frontier we
// can't know what to keep.
func TestWALPrune_NoBackup_NoOp(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000005",
		"0/06000000", time.Now(), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsConsidered != 0 {
		t.Errorf("no-backup case should not look at segments; got considered=%d",
			res.SegmentsConsidered)
	}
	if res.SegmentsDeleted != 0 {
		t.Errorf("no-backup case must not delete; got deleted=%d", res.SegmentsDeleted)
	}
	// Sanity: the segment is still there.
	if !segExists(t, sp, "db1", 1, "000000010000000000000005") {
		t.Error("segment was unexpectedly deleted in the no-backup path")
	}
}

// TestWALPrune_DryRun_DeletesNothing: with --dry-run, all
// candidates are reported but no Delete is issued.
func TestWALPrune_DryRun_DeletesNothing(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	// Backup at start_lsn 0/05000000 — so segments before that LSN
	// are candidates.
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", time.Now().Add(-2*time.Hour), []int64{1024})
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000005",
		"0/06000000", time.Now(), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("dry-run should report 1 candidate; got %d", res.SegmentsDeleted)
	}
	if !res.DryRun {
		t.Error("DryRun flag should propagate to result")
	}
	// Both segments still on disk.
	if !segExists(t, sp, "db1", 1, "000000010000000000000001") {
		t.Error("dry-run unexpectedly deleted seg #1")
	}
	if !segExists(t, sp, "db1", 1, "000000010000000000000005") {
		t.Error("kept seg disappeared")
	}
}

// TestWALPrune_HappyPath_DeletesPreFrontier: with apply (DryRun=false),
// segments whose end_lsn is < frontier are deleted; later segments
// stay.
func TestWALPrune_HappyPath_DeletesPreFrontier(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", time.Now().Add(-3*time.Hour), []int64{1024})
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000002",
		"0/03000000", time.Now().Add(-2*time.Hour), []int64{2048})
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000005",
		"0/06000000", time.Now().Add(-30*time.Minute), []int64{4096})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 2 {
		t.Errorf("expected 2 segments deleted; got %d", res.SegmentsDeleted)
	}
	if res.SegmentsKept != 1 {
		t.Errorf("expected 1 segment kept; got %d", res.SegmentsKept)
	}
	if res.BytesDeleted != 1024+2048 {
		t.Errorf("BytesDeleted=%d, want 3072", res.BytesDeleted)
	}
	if res.FrontierBackupID != "db1.full.aaa" {
		t.Errorf("FrontierBackupID=%q, want db1.full.aaa", res.FrontierBackupID)
	}
	// Old segments gone.
	if segExists(t, sp, "db1", 1, "000000010000000000000001") {
		t.Error("seg #1 should have been deleted")
	}
	if segExists(t, sp, "db1", 1, "000000010000000000000002") {
		t.Error("seg #2 should have been deleted")
	}
	// Newer segment stays.
	if !segExists(t, sp, "db1", 1, "000000010000000000000005") {
		t.Error("seg #5 (post-frontier) was deleted")
	}
}

// TestWALPrune_TombstonedBackupExcluded: a tombstoned backup
// doesn't contribute to the frontier. So its start_lsn doesn't
// constrain WAL retention.
func TestWALPrune_TombstonedBackupExcluded(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	// Tombstoned older backup at 0/01000000; live backup at 0/05000000.
	// The frontier should be the LIVE backup's start_lsn.
	plantBackupManifest(t, sp, "db1", "db1.full.tombstoned",
		"0/01000000", time.Now().Add(-2*time.Hour))
	plantTombstone(t, sp, "db1", "db1.full.tombstoned")
	plantBackupManifest(t, sp, "db1", "db1.full.live",
		"0/05000000", time.Now().Add(-1*time.Hour))
	// Segment between the tombstoned-start and live-start should be
	// candidate (it's before the LIVE frontier).
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000003",
		"0/04000000", time.Now().Add(-90*time.Minute), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
		// The tombstone was just planted (young); advance the clock well
		// past the grace so it counts as OLD and excludes its backup — the
		// scenario this test is about. The young-tombstone case (WAL kept)
		// is TestWALPrune_YoungTombstoneKeepsWAL.
		Now: time.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.live" {
		t.Errorf("frontier should be the LIVE backup; got %q", res.FrontierBackupID)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("seg before live frontier should be deleted; got %d", res.SegmentsDeleted)
	}
}

// TestWALPrune_YoungTombstoneKeepsWAL is the undelete-grace regression: a
// FRESHLY tombstoned backup still constrains the WAL frontier, so the WAL it
// needs survives and a `backup undelete` within the grace window can recover
// it — the same protection repo gc gives the chunks. Before the fix, prune
// excluded any tombstoned backup immediately and would delete that WAL,
// stranding the undeleted backup.
func TestWALPrune_YoungTombstoneKeepsWAL(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.tombstoned",
		"0/01000000", time.Now().Add(-2*time.Hour))
	plantTombstone(t, sp, "db1", "db1.full.tombstoned") // mtime ~ now → young
	plantBackupManifest(t, sp, "db1", "db1.full.live",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000003",
		"0/04000000", time.Now().Add(-90*time.Minute), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1", // default 24h grace; the tombstone is young
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.tombstoned" {
		t.Errorf("a young tombstone must still set the frontier (keep its WAL); got %q", res.FrontierBackupID)
	}
	if res.SegmentsDeleted != 0 {
		t.Errorf("WAL for an undeletable (young-tombstoned) backup must NOT be deleted; deleted %d", res.SegmentsDeleted)
	}
}

// TestWALPrune_GraceDisabledExcludesYoungTombstone: a negative grace restores
// the old immediate-exclusion behaviour for operators who want it.
func TestWALPrune_GraceDisabledExcludesYoungTombstone(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.tombstoned",
		"0/01000000", time.Now().Add(-2*time.Hour))
	plantTombstone(t, sp, "db1", "db1.full.tombstoned")
	plantBackupManifest(t, sp, "db1", "db1.full.live",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000003",
		"0/04000000", time.Now().Add(-90*time.Minute), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:     "db1",
		TombstoneGrace: -1, // disable grace
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.live" || res.SegmentsDeleted != 1 {
		t.Errorf("grace disabled: want live frontier + 1 deleted, got %q / %d",
			res.FrontierBackupID, res.SegmentsDeleted)
	}
}

// TestWALPrune_KeepFloorTime_OverridesLSNRule: a segment that's a
// candidate per the LSN rule is preserved when its CreatedAt is
// past KeepFloorTime. Operators set this for "I want at least 14
// days of WAL even if I don't strictly need it for PITR."
func TestWALPrune_KeepFloorTime_OverridesLSNRule(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", now.Add(-1*time.Hour))
	// Segment older than the frontier (LSN-wise), but young in
	// time — should be kept by the floor.
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", now.Add(-1*time.Hour), []int64{1024})
	// Segment older than both rules — should be deleted.
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000002",
		"0/03000000", now.Add(-72*time.Hour), []int64{2048})

	floor := now.Add(-24 * time.Hour) // "keep last 24h regardless"
	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:    "db1",
		KeepFloorTime: floor,
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsKeptByFloor != 1 {
		t.Errorf("expected 1 segment kept-by-floor; got %d", res.SegmentsKeptByFloor)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("expected 1 segment deleted; got %d", res.SegmentsDeleted)
	}
	if !segExists(t, sp, "db1", 1, "000000010000000000000001") {
		t.Error("floor-protected segment was deleted")
	}
	if segExists(t, sp, "db1", 1, "000000010000000000000002") {
		t.Error("very-old segment should have been deleted")
	}
}

// TestWALPrune_TimelineHistoryFilesPreserved: segments under
// wal/<dep>/timelines/ are NEVER touched by WALPrune. Those are
// timeline history files (TLI bookkeeping) and are needed across
// versions.
func TestWALPrune_TimelineHistoryFilesPreserved(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	// History file at a deliberately weird path.
	hist := []byte(`{"timeline":2}`)
	if _, err := sp.Put(context.Background(),
		"wal/db1/timelines/2.history",
		bytes.NewReader(hist),
		storage.PutOptions{ContentLength: int64(len(hist))}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
	}); err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	// History file still there.
	if _, err := sp.Stat(context.Background(),
		"wal/db1/timelines/2.history"); err != nil {
		t.Errorf("timeline history file was disturbed: %v", err)
	}
}

// TestWALPrune_OnProgressCallback fires per segment with the
// classification outcome.
func TestWALPrune_OnProgressCallback(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", time.Now().Add(-3*time.Hour), []int64{1024})
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000005",
		"0/06000000", time.Now(), []int64{1024})

	var outcomes []string
	if _, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
		OnProgress: func(p repo.WALPruneProgress) {
			outcomes = append(outcomes, p.Outcome)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("expected 2 progress events; got %d", len(outcomes))
	}
	// Outcomes should be: one "deleted" (or "would_delete" in dry-run),
	// one "kept".
	var saw string
	for _, o := range outcomes {
		saw += o + " "
	}
	if !contains(saw, "deleted") || !contains(saw, "kept") {
		t.Errorf("expected both deleted + kept outcomes; got: %s", saw)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// TestWALPrune_NilStoragePlugin is the validation guard.
func TestWALPrune_NilStoragePlugin(t *testing.T) {
	if _, err := repo.WALPrune(context.Background(), nil, repo.WALPruneOptions{
		Deployment: "db1",
	}); err == nil {
		t.Error("expected error for nil StoragePlugin")
	}
}

// TestWALPrune_RequiresDeployment surfaces the obvious missing-arg
// error.
func TestWALPrune_RequiresDeployment(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	if _, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{}); err == nil {
		t.Error("expected error for missing Deployment")
	}
}

// TestWALPrune_FailsClosedOnUndecodableManifest pins the fix for
// data-loss path #1: when a LIVE (non-tombstoned) backup's manifest
// cannot be decoded, the frontier walk must FAIL CLOSED (abort the
// prune) instead of silently skipping that backup. Skipping it would
// drop it from the min(start_lsn) frontier, advancing the frontier
// past a backup that still needs older WAL and pruning that WAL away
// — a silent, unrecoverable PITR gap.
//
// Scenario: bkpOld (early start_lsn) anchors the frontier and a WAL
// segment lies in the range only it needs; bkpNew has a later
// start_lsn. Corrupt bkpOld's manifest. Pre-fix, the walk skipped
// bkpOld so the frontier became bkpNew.start and the segment looked
// prunable (SegmentsDeleted=1). Post-fix, WALPrune returns an error
// and deletes nothing.
func TestWALPrune_FailsClosedOnUndecodableManifest(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	plantBackupManifest(t, sp, "db1", "bkpOld", "0/01000000", base.Add(1*time.Hour))
	plantBackupManifest(t, sp, "db1", "bkpNew", "0/05000000", base.Add(2*time.Hour))

	// A segment bkpOld still needs (end >= bkpOld.start, < bkpNew.start).
	const seg = "000000010000000000000002"
	plantWALSegManifest(t, sp, "db1", 1, seg, "0/03000000", base.Add(90*time.Minute), []int64{1024})

	// Corrupt bkpOld's manifest with non-JSON bytes (bit-rot / a
	// partial write that still registered an object).
	bad := []byte("}{ this is not valid json")
	key := fmt.Sprintf("manifests/%s/backups/%s/manifest.json", "db1", "bkpOld")
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(bad),
		storage.PutOptions{ContentLength: int64(len(bad))}); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
		DryRun:     true,
	})
	if err == nil {
		t.Fatalf("WALPrune must FAIL CLOSED when a live backup's manifest is undecodable; got nil error (res=%+v). Silently skipping it would advance the frontier and prune WAL bkpOld still needs.", res)
	}
}

// zeroTombstoneModTime wraps a StoragePlugin so List reports a ZERO
// ModTime for every .tombstone object, reproducing S3/azblob behaviour
// when the SDK returns a nil LastModified. Everything else passes
// through unchanged.
type zeroTombstoneModTime struct {
	storage.StoragePlugin
}

func (z zeroTombstoneModTime) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	inner := z.StoragePlugin.List(ctx, prefix)
	return func(yield func(storage.ObjectInfo, error) bool) {
		inner(func(info storage.ObjectInfo, err error) bool {
			if err == nil && strings.HasSuffix(info.Key, "/manifest.json.tombstone") {
				info.ModTime = time.Time{}
			}
			return yield(info, err)
		})
	}
}

// TestWALPrune_ZeroModTimeTombstoneKeepsWAL pins the gc/walprune grace
// parity bug: on a backend whose List leaves a tombstone's ModTime zero,
// repo gc treats the tombstone as YOUNG and keeps the backup's chunks for
// undelete (gc.go, audit). walprune must mirror that — otherwise it
// reads the zero time as "older than any cutoff", prunes the WAL, and
// strands an undeletable backup with chunks but no WAL.
func TestWALPrune_ZeroModTimeTombstoneKeepsWAL(t *testing.T) {
	_, base := newTestRepo(t)
	defer base.Close()
	sp := zeroTombstoneModTime{base}

	// Tombstoned backup at the earliest LSN; planted "long ago" so a
	// real ModTime would put it well outside the 24h grace — proving the
	// keep comes from the zero-ModTime rule, not a young marker.
	plantBackupManifest(t, sp, "db1", "db1.full.tombstoned",
		"0/01000000", time.Now().Add(-72*time.Hour))
	plantTombstone(t, sp, "db1", "db1.full.tombstoned")
	plantBackupManifest(t, sp, "db1", "db1.full.live",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000003",
		"0/04000000", time.Now().Add(-90*time.Minute), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1", // default 24h grace active
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.tombstoned" {
		t.Errorf("zero-ModTime tombstone under active grace must hold the frontier (keep its WAL); got %q", res.FrontierBackupID)
	}
	if res.SegmentsDeleted != 0 {
		t.Errorf("WAL of an undeletable (zero-ModTime tombstoned) backup must NOT be pruned; deleted %d", res.SegmentsDeleted)
	}
}

// TestWALPrune_ZeroModTimeTombstoneGraceDisabled confirms the mirror is
// exact: with grace disabled (TombstoneGrace<0), an unknown-age tombstone
// is excluded just like gc collects its chunks — the operator opted out
// of undelete safety.
func TestWALPrune_ZeroModTimeTombstoneGraceDisabled(t *testing.T) {
	_, base := newTestRepo(t)
	defer base.Close()
	sp := zeroTombstoneModTime{base}

	plantBackupManifest(t, sp, "db1", "db1.full.tombstoned",
		"0/01000000", time.Now().Add(-72*time.Hour))
	plantTombstone(t, sp, "db1", "db1.full.tombstoned")
	plantBackupManifest(t, sp, "db1", "db1.full.live",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000003",
		"0/04000000", time.Now().Add(-90*time.Minute), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:     "db1",
		TombstoneGrace: -1, // disable grace
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.live" || res.SegmentsDeleted != 1 {
		t.Errorf("grace disabled: want live frontier + 1 deleted, got %q / %d",
			res.FrontierBackupID, res.SegmentsDeleted)
	}
}
