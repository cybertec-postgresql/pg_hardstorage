package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestHold_Add_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, _, exit := runCLI(t, "hold", "add", "db1", "id1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
}

func TestHold_Add_RefusesUnknownBackup(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "hold", "add", "db1", "no-such-id",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound\nstderr: %s", exit, errb)
	}
	if !strings.Contains(errb, "notfound.backup") {
		t.Errorf("expected notfound.backup; got: %s", errb)
	}
}

func TestHold_Add_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	stdout, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--holder", "ops@acme",
		"--reason", "GDPR Art 17 #4421",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, out:\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"action": "added"`,
		`"holder": "ops@acme"`,
		`"reason": "GDPR Art 17 #4421"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestHold_Remove_RequiresYes(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	// First place a hold so removal has something to act on.
	_, _, _ = runCLI(t, "hold", "add", "db1", id, "--repo", w.repoURL, "-o", "json")

	_, _, exit := runCLI(t, "hold", "remove", "db1", id,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitAborted) {
		t.Errorf("remove without --yes should exit ExitAborted(5); got %d", exit)
	}
	_, _, exit = runCLI(t, "hold", "remove", "db1", id,
		"--repo", w.repoURL, "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Errorf("remove with --yes should succeed; got %d", exit)
	}
}

func TestHold_Remove_Idempotent(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	// No hold placed; removing should still succeed (idempotent).
	_, _, exit := runCLI(t, "hold", "remove", "db1", id,
		"--repo", w.repoURL, "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Errorf("remove of absent hold should be idempotent (exit 0); got %d", exit)
	}
}

func TestHold_List_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "hold", "list", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, `"count": 0`) {
		t.Errorf("empty repo should report 0 holds:\n%s", stdout)
	}
}

func TestHold_List_FleetWide(t *testing.T) {
	w := newReadWorld(t)
	id1 := commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	id2 := commitVerifiableBackup(t, w, "db2", 0, []byte("b"))
	_, _, _ = runCLI(t, "hold", "add", "db1", id1,
		"--repo", w.repoURL, "--reason", "audit-1", "-o", "json")
	_, _, _ = runCLI(t, "hold", "add", "db2", id2,
		"--repo", w.repoURL, "--reason", "audit-2", "-o", "json")

	stdout, _, exit := runCLI(t, "hold", "list", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"count": 2`,
		`"deployment": "db1"`,
		`"deployment": "db2"`,
		`"reason": "audit-1"`,
		`"reason": "audit-2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestHold_List_DeploymentScoped(t *testing.T) {
	w := newReadWorld(t)
	id1 := commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	id2 := commitVerifiableBackup(t, w, "db2", 0, []byte("b"))
	_, _, _ = runCLI(t, "hold", "add", "db1", id1, "--repo", w.repoURL, "-o", "json")
	_, _, _ = runCLI(t, "hold", "add", "db2", id2, "--repo", w.repoURL, "-o", "json")

	stdout, _, exit := runCLI(t, "hold", "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, `"count": 1`) {
		t.Errorf("scope=db1 should list exactly 1 hold:\n%s", stdout)
	}
	if strings.Contains(stdout, `"deployment": "db2"`) {
		t.Errorf("scope=db1 must NOT list db2 holds:\n%s", stdout)
	}
}

// PutHold preserves HeldAt across edits — the audit-log artefact
// of "how long has this been held" must not reset on every metadata
// touch.
func TestHold_Add_PreservesHeldAtAcrossEdits(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	// First add — captures held_at(t0).
	_, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL, "--reason", "first", "-o", "json")
	if exit != 0 {
		t.Fatalf("first add: %d", exit)
	}
	first, _, _ := runCLI(t, "hold", "list", "db1", "--repo", w.repoURL, "-o", "json")
	t0 := captureHeldAt(t, first)

	// Second add — operator updates the reason. held_at MUST be the same.
	_, _, _ = runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL, "--reason", "updated", "-o", "json")
	second, _, _ := runCLI(t, "hold", "list", "db1", "--repo", w.repoURL, "-o", "json")
	t1 := captureHeldAt(t, second)
	if t0 == "" || t1 == "" {
		t.Fatalf("could not extract held_at; t0=%q t1=%q", t0, t1)
	}
	if t0 != t1 {
		t.Errorf("HeldAt must be preserved across edits; got %q -> %q", t0, t1)
	}
}

// captureHeldAt extracts the first "held_at": "<TS>" value from
// the listing JSON.
func captureHeldAt(t *testing.T, body string) string {
	t.Helper()
	const marker = `"held_at": "`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
