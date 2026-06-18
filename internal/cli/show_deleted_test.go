package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestShow_TombstonedDefault_HelpfulRefusal: `show <id>` of a
// tombstoned manifest (without --include-deleted) returns a
// structured `notfound.backup_tombstoned` error pointing at the
// `--include-deleted` flag and the `backup undelete` command.
// Operators chasing "where did my backup go?" land here and see
// the recovery path immediately.
func TestShow_TombstonedDefault_HelpfulRefusal(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	if err := w.store.SoftDelete(context.Background(), "db1", "db1.full.A", "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runCLI(t,
		"show", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("show on tombstoned should refuse; exit=%d\nstdout=%s", exit, stdout)
	}
	for _, want := range []string{
		"notfound.backup_tombstoned",
		"--include-deleted",
		"backup undelete",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestShow_IncludeDeleted_SurfacesBodyAndTombstoneMeta: with
// --include-deleted, a tombstoned manifest's full body shows up
// in the result, with tombstoned=true plus
// deleted_at/delete_reason/delete_policy from the marker.
func TestShow_IncludeDeleted_SurfacesBodyAndTombstoneMeta(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	if err := w.store.SoftDelete(context.Background(), "db1", "db1.full.A", "manual", "operator-error"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t,
		"show", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--include-deleted",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("show --include-deleted: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"backup_id": "db1.full.A"`,
		`"tombstoned": true`,
		`"deleted_at"`,
		`"delete_reason": "operator-error"`,
		`"delete_policy": "manual"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in JSON:\n%s", want, stdout)
		}
	}
}

// TestShow_IncludeDeleted_LiveStillWorks: --include-deleted is
// the SUPERSET of default — a live manifest still surfaces with
// no tombstone metadata. So scripts that always pass
// --include-deleted (e.g. for forensic dumps) don't see false
// tombstone signals on live manifests.
func TestShow_IncludeDeleted_LiveStillWorks(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	stdout, _, exit := runCLI(t,
		"show", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--include-deleted",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("show --include-deleted on live: exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, `"tombstoned": true`) {
		t.Errorf("live manifest should NOT carry tombstoned=true:\n%s", stdout)
	}
	if strings.Contains(stdout, `"deleted_at"`) {
		t.Errorf("live manifest should NOT have deleted_at:\n%s", stdout)
	}
	if strings.Contains(stdout, `"delete_reason"`) {
		t.Errorf("live manifest should NOT have delete_reason:\n%s", stdout)
	}
}

// TestShow_DefaultBodyShape_Unchanged: regression — the default
// `show` body shape (no --include-deleted) must not contain
// any of the new tombstone keys, preserving the 24-month
// JSON-compat commitment.
func TestShow_DefaultBodyShape_Unchanged(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	stdout, _, exit := runCLI(t,
		"show", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("default show: exit=%d\n%s", exit, stdout)
	}
	for _, banned := range []string{
		`"tombstoned"`,
		`"deleted_at"`,
		`"delete_reason"`,
		`"delete_policy"`,
	} {
		if strings.Contains(stdout, banned) {
			t.Errorf("default show body should not include %s:\n%s", banned, stdout)
		}
	}
}

// TestShow_IncludeDeleted_Text: text mode tags the header with
// [DELETED], shows tombstone metadata, and includes a recovery
// hint pointing at `backup undelete`.
func TestShow_IncludeDeleted_Text(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	if err := w.store.SoftDelete(context.Background(), "db1", "db1.full.A", "manual", "GDPR-#42"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t,
		"show", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--include-deleted",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("text show --include-deleted: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"[DELETED]",
		"DELETED (tombstoned)",
		"manual",   // policy
		"GDPR-#42", // reason
		"backup undelete",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestShow_IncludeDeletedFlagDiscoverable: `show --help` shows
// --include-deleted + a hint about tombstoned manifests.
func TestShow_IncludeDeletedFlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "show", "--help")
	for _, want := range []string{"--include-deleted", "tombstoned"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show --help missing %q:\n%s", want, stdout)
		}
	}
}
