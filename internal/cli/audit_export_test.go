package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// auditExportView mirrors the v1 contract's BundleResult shape.
type auditExportView struct {
	Schema       string `json:"schema"`
	Path         string `json:"path"`
	EventCount   int    `json:"event_count"`
	AnchorCount  int    `json:"anchor_count"`
	BundleBytes  int64  `json:"bundle_bytes"`
	SHA256       string `json:"sha256"`
	HeadHash     string `json:"head_hash"`
	HeadSequence int64  `json:"head_sequence"`
	Manifest     *struct {
		Schema               string   `json:"schema"`
		Operator             string   `json:"operator"`
		SourceURL            string   `json:"source_url"`
		EventCount           int      `json:"event_count"`
		PublicKeyFingerprint string   `json:"public_key_fingerprint"`
		SignedFiles          []string `json:"signed_files"`
	} `json:"manifest"`
}

// appendAuditEvent plants one event in the readWorld's audit
// chain.  Helper used by export-bundle CLI tests.
func appendAuditEvent(t *testing.T, w *readWorld, action, deployment string, at time.Time) {
	t.Helper()
	store := audit.NewStore(w.sp)
	if err := store.Append(context.Background(), &audit.Event{
		Action:    action,
		Subject:   audit.Subject{Deployment: deployment},
		Timestamp: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAuditExportBundle_RequiresFlags
func TestAuditExportBundle_RequiresFlags(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "audit", "export-bundle", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--repo missing: exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	w := newReadWorld(t)
	_, errb, exit = runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--out missing: exit = %d, want ExitMisuse", exit)
	}
}

// TestAuditExportBundle_BadSince
func TestAuditExportBundle_BadSince(t *testing.T) {
	w := newReadWorld(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, errb, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--since", "yesterday-ish", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestAuditExportBundle_OverwriteRefused: an existing --out
// path is refused with conflict.file_exists.
func TestAuditExportBundle_OverwriteRefused(t *testing.T) {
	w := newReadWorld(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(out, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit; got 0")
	}
	if !strings.Contains(errb, "conflict.file_exists") {
		t.Errorf("expected conflict.file_exists:\n%s", errb)
	}
}

// TestAuditExportBundle_HappyPath: events + happy export +
// SHA256 populated + bundle file exists.
func TestAuditExportBundle_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	appendAuditEvent(t, w, "backup.create", "db1", now.Add(-2*time.Hour))
	appendAuditEvent(t, w, "backup.delete", "db2", now.Add(-1*time.Hour))

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--operator", "alice@example.com", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2", view.EventCount)
	}
	if view.SHA256 == "" {
		t.Errorf("SHA256 empty")
	}
	if view.BundleBytes <= 0 {
		t.Errorf("BundleBytes = %d", view.BundleBytes)
	}
	if view.Path != out {
		t.Errorf("Path = %q, want %q", view.Path, out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("bundle file not created: %v", err)
	}
	if view.Manifest == nil || view.Manifest.Operator != "alice@example.com" {
		t.Errorf("manifest operator = %+v", view.Manifest)
	}
}

// TestAuditExportBundle_FilterByActionPrefix
func TestAuditExportBundle_FilterByActionPrefix(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	appendAuditEvent(t, w, "backup.create", "db1", now.Add(-2*time.Hour))
	appendAuditEvent(t, w, "kms.rotate", "", now.Add(-1*time.Hour))
	appendAuditEvent(t, w, "backup.delete", "db1", now.Add(-30*time.Minute))

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--action-prefix", "backup.", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2 (backup.* only)", view.EventCount)
	}
}

// TestAuditExportBundle_TimeWindow
func TestAuditExportBundle_TimeWindow(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	appendAuditEvent(t, w, "x.event", "db1", now.Add(-3*time.Hour))
	appendAuditEvent(t, w, "x.event", "db1", now.Add(-1*time.Hour))
	appendAuditEvent(t, w, "x.event", "db1", now.Add(-30*time.Minute))

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--since", "2h", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.EventCount != 2 {
		t.Errorf("windowed EventCount = %d, want 2", view.EventCount)
	}
}

// TestAuditExportBundle_IncludeAnchors
func TestAuditExportBundle_IncludeAnchors(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())
	// Run audit anchor to plant an anchor.
	_, _, exit := runCLI(t, "audit", "anchor", "--repo", w.repoURL,
		"--publisher", "test", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit anchor exit = %d", exit)
	}

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--include-anchors", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.AnchorCount != 1 {
		t.Errorf("AnchorCount = %d, want 1", view.AnchorCount)
	}
}

// TestAuditExportBundle_TextFormat
func TestAuditExportBundle_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"audit export-bundle",
		"Path:",
		"SHA256:",
		"Events:",
		"verify-bundle",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestAuditVerifyBundle_HappyPath: export → verify round-trip.
func TestAuditVerifyBundle_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("export exit = %d", exit)
	}
	stdout, _, exit := runCLI(t, "audit", "verify-bundle", out, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit = %d\n%s", exit, stdout)
	}
}

// TestAuditVerifyBundle_RejectsTampered: a flipped byte fails
// verification.
func TestAuditVerifyBundle_RejectsTampered(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("export exit = %d", exit)
	}
	// Flip a byte in the middle of the file.
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0xFF
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, exit := runCLI(t, "audit", "verify-bundle", out, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("tampered bundle verified clean")
	}
	if !strings.Contains(errb, "verify.bundle_invalid") {
		t.Errorf("expected verify.bundle_invalid:\n%s", errb)
	}
}

// TestAuditVerifyBundle_NotFound: nonexistent path → notfound.bundle.
func TestAuditVerifyBundle_NotFound(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "audit", "verify-bundle",
		"/does/not/exist.tar.gz", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Error("expected non-zero exit")
	}
	if !strings.Contains(errb, "notfound.bundle") {
		t.Errorf("expected notfound.bundle:\n%s", errb)
	}
}

// TestAuditExportBundle_HelpDiscoverable
func TestAuditExportBundle_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "audit", "--help")
	for _, want := range []string{"export-bundle", "verify-bundle"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "audit", "export-bundle", "--help")
	for _, want := range []string{
		"--repo", "--out", "--since", "--until",
		"--action-prefix", "--include-anchors", "--operator",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit export-bundle --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestAuditExportBundle_FilterByDeployment
func TestAuditExportBundle_FilterByDeployment(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	appendAuditEvent(t, w, "x.event", "db1", now.Add(-1*time.Hour))
	appendAuditEvent(t, w, "x.event", "db2", now.Add(-30*time.Minute))

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out,
		"--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1 (db1 only)", view.EventCount)
	}
}

// TestAuditExportBundle_BundleSchemaStable
func TestAuditExportBundle_BundleSchemaStable(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	stdout, _, exit := runCLI(t, "audit", "export-bundle",
		"--repo", w.repoURL, "--out", out, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view auditExportView
	bodyOf(t, stdout, &view)
	if view.Schema != "pg_hardstorage.audit.export_bundle_result.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
	if view.Manifest == nil ||
		view.Manifest.Schema != "pg_hardstorage.audit.evidence_bundle.v1" {
		t.Errorf("Manifest schema off: %+v", view.Manifest)
	}
}
