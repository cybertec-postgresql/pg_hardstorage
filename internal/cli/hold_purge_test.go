package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestHoldPurgeExpired_RemovesExpiredOnly_AuditEmits:
// end-to-end CLI happy path. Three holds (indefinite,
// active-bounded, expired-bounded); --yes purges only the
// expired one; an audit event is emitted per removal.
func TestHoldPurgeExpired_RemovesExpiredOnly_AuditEmits(t *testing.T) {
	w := newReadWorld(t)
	idIndef := commitVerifiableBackup(t, w, "db1", 0, []byte("indef"))
	idActive := commitVerifiableBackup(t, w, "db1", 1, []byte("active"))
	idExpired := commitVerifiableBackup(t, w, "db1", 2, []byte("expired"))
	if err := w.store.PutHold(context.Background(), "db1", idIndef, "compliance", "GDPR"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", idActive, "ops", "active", future); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", idExpired,
		"old-debug", "stale-debug", past); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "hold", "purge-expired", "db1",
		"--repo", w.repoURL, "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("purge --yes: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"count": 1`,
		idExpired,
		`"holder": "old-debug"`,
		`"reason": "stale-debug"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
	// On disk: only expired marker should be gone.
	for _, c := range []struct {
		id        string
		stillHeld bool
	}{
		{idIndef, true},
		{idActive, true},
		{idExpired, false},
	} {
		held, err := w.store.IsHeld(context.Background(), "db1", c.id)
		if err != nil {
			t.Fatal(err)
		}
		if held != c.stillHeld {
			t.Errorf("%s: held=%v, want %v", c.id, held, c.stillHeld)
		}
	}

	// Audit chain has the per-marker event.
	stdoutAudit, _, exit := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "hold.purge_expired",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search: exit=%d", exit)
	}
	for _, want := range []string{
		`"count": 1`,
		`"action": "hold.purge_expired"`,
		idExpired,
	} {
		if !strings.Contains(stdoutAudit, want) {
			t.Errorf("audit chain missing %q:\n%s", want, stdoutAudit)
		}
	}
}

// TestHoldPurgeExpired_DryRun_NoMutation: --dry-run identifies
// expired markers without removing them. No audit emit.
func TestHoldPurgeExpired_DryRun_NoMutation(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("expired"))
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", id,
		"ops", "stale", past); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "hold", "purge-expired", "db1",
		"--repo", w.repoURL, "--dry-run", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Errorf("expected dry_run=true:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"count": 1`) {
		t.Errorf("expected count=1 in preview:\n%s", stdout)
	}
	// Marker still on disk.
	held, err := w.store.IsHeld(context.Background(), "db1", id)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Errorf("dry-run should not remove the marker")
	}
	// No audit event was emitted — check the chain has zero
	// hold.purge_expired entries.
	auditOut, _, _ := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "hold.purge_expired",
		"--output", "json")
	if !strings.Contains(auditOut, `"count": 0`) {
		t.Errorf("dry-run should emit no audit events:\n%s", auditOut)
	}
}

// TestHoldPurgeExpired_RequiresYesOrDryRun: bare invocation
// (neither --yes nor --dry-run) refuses.
func TestHoldPurgeExpired_RequiresYesOrDryRun(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t, "hold", "purge-expired", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("bare invocation should refuse")
	}
	if !strings.Contains(stderr, "aborted.confirmation_required") {
		t.Errorf("expected aborted.confirmation_required:\n%s", stderr)
	}
}

// TestHoldPurgeExpired_NoExpired_FriendlyEmpty: a clean
// deployment returns count=0 cleanly with a friendly text
// message.
func TestHoldPurgeExpired_NoExpired_FriendlyEmpty(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("indef"))
	if err := w.store.PutHold(context.Background(), "db1", id, "ops", "indef"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "hold", "purge-expired", "db1",
		"--repo", w.repoURL, "--yes", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, "no expired hold markers") {
		t.Errorf("expected friendly empty message:\n%s", stdout)
	}
}

// TestHoldPurgeExpired_FleetWide: no positional → walks every
// deployment.
func TestHoldPurgeExpired_FleetWide(t *testing.T) {
	w := newReadWorld(t)
	id1 := commitVerifiableBackup(t, w, "db1", 0, []byte("d1"))
	id2 := commitVerifiableBackup(t, w, "db2", 0, []byte("d2"))
	past := time.Now().UTC().Add(-time.Hour)
	for _, c := range []struct{ dep, id string }{
		{"db1", id1}, {"db2", id2},
	} {
		if err := w.store.PutHoldUntil(context.Background(), c.dep, c.id, "ops", "stale", past); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _, exit := runCLI(t, "hold", "purge-expired",
		"--repo", w.repoURL, "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("fleet-wide exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"count": 2`) {
		t.Errorf("fleet-wide should reap 2; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"deployment": "db1"`) ||
		!strings.Contains(stdout, `"deployment": "db2"`) {
		t.Errorf("expected both db1 and db2 in result:\n%s", stdout)
	}
}

// TestHoldPurgeExpired_TextRendering: text mode renders a
// table with counts + the markers' metadata, plus a "re-run
// with --yes" hint after dry-run.
func TestHoldPurgeExpired_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("text"))
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", id,
		"ops", "stale", past); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := runCLI(t, "hold", "purge-expired", "db1",
		"--repo", w.repoURL, "--dry-run", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run text exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"Would remove 1 expired hold",
		"DEPLOYMENT",
		"db1",
		"Re-run with --yes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestHoldPurgeExpired_DiscoverableFromHelp: subcommand shows
// in `hold --help` and its own help advertises both modes.
func TestHoldPurgeExpired_DiscoverableFromHelp(t *testing.T) {
	stdout, _, _ := runCLI(t, "hold", "--help")
	if !strings.Contains(stdout, "purge-expired") {
		t.Errorf("hold --help missing purge-expired:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "hold", "purge-expired", "--help")
	for _, want := range []string{"--dry-run", "--yes", "fleet-wide", "audit-emitted"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("hold purge-expired --help missing %q:\n%s", want, stdout)
		}
	}
}
