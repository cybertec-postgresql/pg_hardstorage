package cli_test

// restore_to_latest_test.go — the end-of-archive recovery flag.
//
// Before --to-latest existed there was NO CLI spelling for "restore the
// backup, then replay every archived WAL segment": no --to* flag meant
// a plain restore with no recovery files at all, which boots with only
// the WAL bundled inside the backup and silently ignores everything
// archived since — while a doc comment claimed recovery files were
// always written. A fake far-future --to is not a workaround: PG 13+
// FATALs when the target outruns the WAL. The most common DR operation
// had no honest spelling.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRestoreToLatest_WritesRecoveryFilesWithoutTarget: the flag must
// produce recovery.signal + restore_command + timeline, and NO
// recovery_target_* line — that absence is what PG defines as
// replay-to-end-of-archive.
func TestRestoreToLatest_WritesRecoveryFilesWithoutTarget(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", nowMinus(t, 1))

	target := filepath.Join(t.TempDir(), "restored")
	if _, errb, exit := runCLI(t, "restore", "db1", "b1",
		"--repo", w.repoURL, "--target", target,
		"--to-latest", "--to-action", "promote", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("restore --to-latest: exit=%d\n%s", exit, errb)
	}

	if _, err := os.Stat(filepath.Join(target, "recovery.signal")); err != nil {
		t.Fatalf("recovery.signal missing: %v — without it PG boots straight up and never "+
			"runs restore_command; every archived segment after the backup is silently "+
			"ignored", err)
	}
	conf, err := os.ReadFile(filepath.Join(target, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(conf)
	for _, want := range []string{"restore_command", "recovery_target_timeline = 'latest'", "recovery_target_action = 'promote'"} {
		if !strings.Contains(got, want) {
			t.Errorf("postgresql.auto.conf missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"recovery_target_lsn", "recovery_target_time ", "recovery_target_name"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("postgresql.auto.conf contains %q — a target was set for a "+
				"no-target recovery; PG would stop early or FATAL:\n%s", forbidden, got)
		}
	}
}

// TestRestoreToLatest_ConflictsWithPointTargets: one recovery, one
// meaning. Combining end-of-archive with a point target is a
// contradiction and must be a usage error, not a silent precedence.
func TestRestoreToLatest_ConflictsWithPointTargets(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", nowMinus(t, 1))

	for _, extra := range [][]string{
		{"--to-lsn", "0/4000000"},
		{"--to", "2026-01-01 00:00 UTC"},
		{"--to-name", "point"},
	} {
		args := append([]string{"restore", "db1", "b1",
			"--repo", w.repoURL, "--target", filepath.Join(t.TempDir(), "x"),
			"--to-latest"}, extra...)
		args = append(args, "-o", "json")
		_, errb, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("--to-latest %v: exit=%d, want ExitMisuse:\n%s", extra, exit, errb)
		}
		if !strings.Contains(errb, "conflicting_targets") {
			t.Errorf("--to-latest %v: wrong code:\n%s", extra, errb)
		}
	}
}

// TestRestorePlain_StillWritesNoRecoveryFiles pins the other side: a
// plain restore stays plain. Auto-arming recovery would surprise every
// existing clone/seed workflow that boots the copy standalone.
func TestRestorePlain_StillWritesNoRecoveryFiles(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", nowMinus(t, 1))

	target := filepath.Join(t.TempDir(), "restored")
	if _, errb, exit := runCLI(t, "restore", "db1", "b1",
		"--repo", w.repoURL, "--target", target, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("plain restore: exit=%d\n%s", exit, errb)
	}
	if _, err := os.Stat(filepath.Join(target, "recovery.signal")); err == nil {
		t.Error("plain restore wrote recovery.signal; end-of-archive recovery must stay opt-in")
	}
}

// nowMinus returns a UTC instant h hours ago (fixture helper).
func nowMinus(t *testing.T, h int) time.Time {
	t.Helper()
	return time.Now().UTC().Add(-time.Duration(h) * time.Hour)
}
