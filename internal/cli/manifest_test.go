package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestManifest_IsRecognizedCommand is the regression guard for issue
// #94: the documented `pg_hardstorage manifest …` command used to fail
// with `unknown command "manifest"`.
func TestManifest_IsRecognizedCommand(t *testing.T) {
	stdout, stderr, _ := runCLI(t, "manifest", "--help")
	combined := stdout + stderr
	if strings.Contains(combined, `unknown command "manifest"`) {
		t.Fatalf("manifest is still unknown:\n%s", combined)
	}
	if !strings.Contains(combined, "show") {
		t.Errorf("manifest help should list the show subcommand:\n%s", combined)
	}
}

// TestManifestShow_RequiresRepo confirms `manifest show` runs the same
// validated path as the top-level `show` (i.e. it's really wired, not a
// stub).
func TestManifestShow_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, stderr, exit := runCLI(t, "manifest", "show", "db1", "some-id", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestManifestShow_RoundTrip commits a manifest and reads it back via
// `manifest show`, proving the documented command returns the manifest
// body.
func TestManifestShow_RoundTrip(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)

	// Discover the generated backup ID via `list`.
	listOut, _, exit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var lv struct {
		Backups []struct {
			BackupID string `json:"backup_id"`
		} `json:"backups"`
	}
	bodyOf(t, listOut, &lv)
	if len(lv.Backups) == 0 {
		t.Fatal("no backups listed")
	}
	id := lv.Backups[0].BackupID

	// `manifest show <deployment> <id>` returns that manifest.
	out, stderr, exit := runCLI(t, "manifest", "show", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("manifest show exit = %d\n%s", exit, stderr)
	}
	var mb struct {
		BackupID   string `json:"backup_id"`
		Deployment string `json:"deployment"`
	}
	bodyOf(t, out, &mb)
	if mb.BackupID != id {
		t.Errorf("backup_id = %q, want %q", mb.BackupID, id)
	}
	if mb.Deployment != "db1" {
		t.Errorf("deployment = %q, want db1", mb.Deployment)
	}
}
