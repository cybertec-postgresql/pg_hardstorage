package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRestore_LatestWithTimeTarget_AutoResolvesEarlierBackup:
// `restore db1 latest --to "..."` against a deployment with
// multiple backups, where the target is BEFORE the most-recent
// backup's StoppedAt, must auto-resolve to an EARLIER backup.
//
// Two backups taken; target is between them; the resolver must
// pick the earlier one (whose stop_time ≤ target). The
// `--preview` mode is the cheapest way to assert resolution
// without actually performing a restore.
func TestRestore_LatestWithTimeTarget_AutoResolvesEarlierBackup(t *testing.T) {
	w := newReadWorld(t)
	idA := commitVerifiableBackup(t, w, "db1", 0, []byte("backup-A"))
	_ = commitVerifiableBackup(t, w, "db1", 5, []byte("backup-B"))

	// Get backupA's StoppedAt for the target. commitVerifiableBackup's
	// fixture: ts is now-1h + idx*minute, StoppedAt = ts+30s.
	ma, err := w.store.Read(context.Background(), "db1", idA, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	target := ma.StoppedAt.Add(30 * time.Second).UTC().Format(time.RFC3339)

	stdout, _, exit := runRestore(t,
		"db1", "latest",
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", target,
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\n%s", exit, stdout)
	}
	// Preview body's BackupID is the resolved one — must be A,
	// not B. (B was 5 min later, AFTER the target.)
	if !strings.Contains(stdout, idA) {
		t.Errorf("expected resolution to %s; got:\n%s", idA, stdout)
	}
}

// TestRestore_ExplicitBackupTooNew_RejectedUpFront: explicit
// backup-id whose StoppedAt > --to target surfaces a
// structured `conflict.backup_after_target` error before any
// restore work begins. The Suggestion mentions the auto-
// resolve path so the operator knows the fix.
func TestRestore_ExplicitBackupTooNew_RejectedUpFront(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("backup-A"))
	idB := commitVerifiableBackup(t, w, "db1", 5, []byte("backup-B"))

	mb, err := w.store.Read(context.Background(), "db1", idB, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	// Target is 1 minute BEFORE backup B's stop_time —
	// pinning B explicitly should refuse.
	target := mb.StoppedAt.Add(-time.Minute).UTC().Format(time.RFC3339)

	_, stderr, exit := runRestore(t,
		"db1", idB,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", target,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal; got OK\nstderr=%s", stderr)
	}
	if exit != int(output.ExitConflict) {
		t.Errorf("exit=%d, want ExitConflict", exit)
	}
	for _, want := range []string{
		"conflict.backup_after_target",
		"AFTER --to target",
		"latest", // Suggestion mentions the auto-resolve via latest
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestRestore_TimeTarget_NoBackupBeforeTarget: target is
// before every backup → structured
// `notfound.backup_before_time` with a Suggestion explaining
// the constraint.
func TestRestore_TimeTarget_NoBackupBeforeTarget(t *testing.T) {
	w := newReadWorld(t)
	// Commit one backup. commitVerifiableBackup uses now-1h as
	// the base, so anything older than ~now-1h is "before all
	// backups".
	commitVerifiableBackup(t, w, "db1", 0, []byte("backup-A"))
	target := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)

	_, stderr, exit := runRestore(t,
		"db1", "latest",
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", target,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal\n%s", stderr)
	}
	for _, want := range []string{
		"notfound.backup_before_time",
		"PITR replays forward",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestRestore_LatestWithoutTimeTarget_StillPicksLatest: no
// --to means the auto-resolver should still default to
// "latest" semantics. Regression guard: the new
// time-aware path must not break the simple
// `restore db1 latest` flow.
func TestRestore_LatestWithoutTimeTarget_StillPicksLatest(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("backup-A"))
	idLatest := commitVerifiableBackup(t, w, "db1", 5, []byte("backup-B"))

	stdout, _, exit := runRestore(t,
		"db1", "latest",
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, idLatest) {
		t.Errorf("expected latest %q in preview body:\n%s", idLatest, stdout)
	}
}

// TestRestore_BadTimeTarget_StructuredUsageError: --to with
// a value naturaltime can't parse surfaces
// usage.bad_time before any storage round-trip.
func TestRestore_BadTimeTarget_StructuredUsageError(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("backup-A"))
	_, stderr, exit := runRestore(t,
		"db1", "latest",
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", "not-a-time-expression",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.bad_time") {
		t.Errorf("expected usage.bad_time:\n%s", stderr)
	}
}
