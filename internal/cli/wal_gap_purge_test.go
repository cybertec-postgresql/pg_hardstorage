package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// commitBackupOnTLI commits a minimal verifiable backup whose manifest
// records the given timeline (commitVerifiableBackup is hardcoded to
// TLI 1). Used by the gap-purge tests that need a live backup on a
// higher timeline so a lower timeline becomes a genuine orphan.
func commitBackupOnTLI(t *testing.T, w *readWorld, deployment string, tli uint32, body []byte) string {
	t.Helper()
	return commitBackupOnTLIWithID(t, w, deployment, deployment+".tli.20260502T120000Z", tli, body)
}

// commitBackupOnTLIWithID is commitBackupOnTLI with a caller-chosen
// backup ID, so a test can commit MORE than one backup (distinct IDs)
// on different timelines.
func commitBackupOnTLIWithID(t *testing.T, w *readWorld, deployment, id string, tli uint32, body []byte) string {
	t.Helper()
	cas := repo.NewCAS(w.sp)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        180000,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         tli,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit backup on TLI %d: %v", tli, err)
	}
	return id
}

// seedGap plants one gap record at (deployment, tli) for the
// supplied detection time.
func seedGap(t *testing.T, w *readWorld, deployment string, tli uint32, at time.Time) {
	t.Helper()
	rec := gapstate.Record{
		Deployment:  deployment,
		SlotName:    "pg_hardstorage_" + deployment,
		Timeline:    tli,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "0/30001A0",
		GapBytes:    420,
		DetectedAt:  at,
	}
	if _, err := gapstate.New(w.sp).Put(context.Background(), rec); err != nil {
		t.Fatalf("seed gap: %v", err)
	}
}

// TestWalGapPurge_Orphans_HappyPath: with a live backup on TLI 5,
// --orphans removes a gap on TLI 2 (below the live backup, unreachable
// by any forward PITR) but KEEPS gaps on TLI 5 and TLI 7 — the latter is
// the forward-PITR fix: a gap on a NEWER timeline than the backup is
// still crossed by that backup's recovery_target_timeline='latest'.
func TestWalGapPurge_Orphans_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	commitBackupOnTLI(t, w, "db1", 5, []byte("live"))
	at := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	seedGap(t, w, "db1", 2, at)                    // below min live (5) → orphan
	seedGap(t, w, "db1", 5, at.Add(time.Minute))   // live → keep
	seedGap(t, w, "db1", 7, at.Add(2*time.Minute)) // above live, but reachable → keep

	stdout, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("gap-purge --orphans exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"mode": "orphans"`,
		`"count": 1`,
		`"timeline": 2`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
	// On disk: TLI 5 and TLI 7 remain (TLI 2 reaped).
	all, _ := gapstate.New(w.sp).List(context.Background(), "db1")
	if len(all) != 2 {
		t.Fatalf("expected 2 records (TLI 5 + 7); got %v", all)
	}
	for _, r := range all {
		if r.Timeline == 2 {
			t.Errorf("orphan TLI 2 survived")
		}
	}
}

// TestWalGapPurge_Orphans_NoLiveManifests_Refused: a
// deployment with no live manifests + --orphans refuses with a
// structured `conflict.no_live_manifests` and a Suggestion to
// pass --all explicitly.
func TestWalGapPurge_Orphans_NoLiveManifests_Refused(t *testing.T) {
	w := newReadWorld(t)
	seedGap(t, w, "db1", 7, time.Now())

	_, stderr, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--yes", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal; got OK")
	}
	for _, want := range []string{
		"conflict.no_live_manifests",
		"--all",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestWalGapPurge_All_RemovesEverything: --all wipes every gap
// record for the deployment regardless of TLI membership.
func TestWalGapPurge_All_RemovesEverything(t *testing.T) {
	w := newReadWorld(t)
	at := time.Now()
	for i, tli := range []uint32{3, 5, 7} {
		seedGap(t, w, "db1", tli, at.Add(time.Duration(i)*time.Minute))
	}

	stdout, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--all", "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("--all exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"count": 3`) {
		t.Errorf("expected count=3; got:\n%s", stdout)
	}
	all, _ := gapstate.New(w.sp).List(context.Background(), "db1")
	if len(all) != 0 {
		t.Errorf("--all should leave nothing; got %d", len(all))
	}
}

// TestWalGapPurge_DryRun_NoMutation: --dry-run identifies
// targets without removing them. Subsequent --yes finishes.
func TestWalGapPurge_DryRun_NoMutation(t *testing.T) {
	w := newReadWorld(t)
	// Live backup on TLI 5; TLI-2 gap is a real orphan the dry-run must
	// identify (count 1) without removing it.
	commitBackupOnTLI(t, w, "db1", 5, []byte("live"))
	seedGap(t, w, "db1", 2, time.Now())

	stdout, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--dry-run", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"dry_run": true`) || !strings.Contains(stdout, `"count": 1`) {
		t.Errorf("expected dry_run=true and count=1:\n%s", stdout)
	}
	// On disk: record still there.
	all, _ := gapstate.New(w.sp).List(context.Background(), "db1")
	if len(all) != 1 {
		t.Errorf("dry-run mutated; %d records left", len(all))
	}
}

// TestWalGapPurge_OrphansAndAll_MutuallyExclusive:
// usage.bad_flag for both modes set or neither set.
func TestWalGapPurge_OrphansAndAll_MutuallyExclusive(t *testing.T) {
	w := newReadWorld(t)
	// Both set.
	_, stderr, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--all", "--yes", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("both modes: exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", stderr)
	}
	// Neither set.
	_, stderr, exit = runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--yes", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("neither mode: exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", stderr)
	}
}

// TestWalGapPurge_RequiresYesOrDryRun: bare invocation refuses.
func TestWalGapPurge_RequiresYesOrDryRun(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("bare invocation should refuse")
	}
	if !strings.Contains(stderr, "aborted.confirmation_required") {
		t.Errorf("expected aborted.confirmation_required:\n%s", stderr)
	}
}

// TestWalGapPurge_AuditEmits: a real run emits one audit event
// per removed record.
func TestWalGapPurge_AuditEmits(t *testing.T) {
	w := newReadWorld(t)
	// Live backup on TLI 5 so the TLI-2 gap is a genuine orphan (below
	// the lowest live timeline) and actually gets purged.
	commitBackupOnTLI(t, w, "db1", 5, []byte("live"))
	seedGap(t, w, "db1", 2, time.Now())

	if _, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--yes"); exit != int(output.ExitOK) {
		t.Fatalf("purge exit=%d", exit)
	}

	stdoutAudit, _, exit := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "wal.gap_purged",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search: exit=%d", exit)
	}
	for _, want := range []string{
		`"count": 1`,
		`"action": "wal.gap_purged"`,
		`"deployment": "db1"`,
	} {
		if !strings.Contains(stdoutAudit, want) {
			t.Errorf("audit chain missing %q:\n%s", want, stdoutAudit)
		}
	}
}

// TestWalGapPurge_HelpDiscoverable: the new subcommand shows
// in `wal --help` and its own --help advertises both modes.
func TestWalGapPurge_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "wal", "--help")
	if !strings.Contains(stdout, "gap-purge") {
		t.Errorf("wal --help missing gap-purge:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "wal", "gap-purge", "--help")
	for _, want := range []string{"--orphans", "--all", "--dry-run", "--yes", "deployment-wipe"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("wal gap-purge --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestWalGapPurge_Orphans_KeepsGapForWithinGraceTombstone pins the
// tombstone-grace fix. `wal gap-purge --orphans` used to build its
// live-timeline set from non-tombstoned manifests only. A soft-deleted
// backup within the GC grace window is still restorable via `backup
// undelete`, and its recovery_target_timeline='latest' PITR crosses
// gaps on LATER timelines — so a gap between that backup's timeline and
// the live backups' must survive. Excluding it let --orphans reap that
// gap; a later undelete + unbounded recovery would then stop short at
// the missing WAL and promote with no warning, the PITR refusal gone.
//
// Setup: a within-grace tombstoned backup on TLI 1, a live backup on
// TLI 3, and a gap on TLI 2 between them. The TLI-2 gap must NOT be
// reaped.
func TestWalGapPurge_Orphans_KeepsGapForWithinGraceTombstone(t *testing.T) {
	w := newReadWorld(t)
	idA := commitBackupOnTLIWithID(t, w, "db1", "db1.a.20260101T000000Z", 1, []byte("older"))
	commitBackupOnTLI(t, w, "db1", 3, []byte("live"))
	// Soft-delete A now → its tombstone is well within the GC grace.
	if err := w.store.SoftDelete(context.Background(), "db1", idA, "manual", "oops-restore-me-later"); err != nil {
		t.Fatalf("soft-delete A: %v", err)
	}
	seedGap(t, w, "db1", 2, time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))

	stdout, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("gap-purge --orphans exit=%d\n%s", exit, stdout)
	}
	all, _ := gapstate.New(w.sp).List(context.Background(), "db1")
	kept := false
	for _, r := range all {
		if r.Timeline == 2 {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("gap on TLI 2 was reaped while a within-grace tombstoned backup on TLI 1 "+
			"can still be undeleted and cross it — restore silently loses the PITR refusal.\n"+
			"surviving records: %+v", all)
	}
}

// TestWalGapPurge_Orphans_ReapsGapForPastGraceTombstone is the other
// side: once a tombstone ages PAST the grace window the backup is
// genuinely dead (gc reclaimed its chunks, wal-prune its WAL), so its
// timeline must stop pinning gaps — routine cleanup keeps working.
//
// A tombstone marker backdated well beyond the grace stands in for
// "aged out"; the TLI-2 gap below the only live backup (TLI 3) must be
// reaped.
func TestWalGapPurge_Orphans_ReapsGapForPastGraceTombstone(t *testing.T) {
	w := newReadWorld(t)
	idA := commitBackupOnTLIWithID(t, w, "db1", "db1.a.20260101T000000Z", 1, []byte("older"))
	commitBackupOnTLI(t, w, "db1", 3, []byte("live"))

	// Plant a tombstone marker for A whose TombstonedAt is far past the
	// grace window — the "aged out, chunks+WAL already reclaimed" state.
	old := time.Now().UTC().Add(-repo.DefaultTombstoneGracePeriod - 72*time.Hour)
	tomb := backup.Tombstone{
		Schema: backup.TombstoneSchema, BackupID: idA, Deployment: "db1",
		TombstonedAt: old, Policy: "manual",
	}
	body, err := json.Marshal(&tomb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.sp.Put(context.Background(), backup.TombstonePath("db1", idA),
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("plant backdated tombstone: %v", err)
	}
	seedGap(t, w, "db1", 2, time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))

	stdout, _, exit := runCLI(t, "wal", "gap-purge", "db1",
		"--repo", w.repoURL, "--orphans", "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("gap-purge --orphans exit=%d\n%s", exit, stdout)
	}
	all, _ := gapstate.New(w.sp).List(context.Background(), "db1")
	for _, r := range all {
		if r.Timeline == 2 {
			t.Fatalf("gap on TLI 2 survived even though the only backup at/below it is "+
				"tombstoned PAST grace (dead) — routine cleanup would never make progress.\n"+
				"records: %+v", all)
		}
	}
}
