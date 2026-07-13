package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// repoAuditView mirrors the v1 contract's top-level shape. We only
// pull in the fields the tests assert on.
type repoAuditView struct {
	Schema string `json:"schema"`
	URL    string `json:"url"`
	Repo   *struct {
		ID       string `json:"id"`
		Mode     string `json:"mode"`
		WORMMode string `json:"worm_mode"`
	} `json:"repo"`
	DeploymentFilter string `json:"deployment_filter"`
	Deployments      []struct {
		Name              string   `json:"name"`
		Active            int      `json:"active"`
		Tombstoned        int      `json:"tombstoned"`
		Held              int      `json:"held"`
		HeldExpired       int      `json:"held_expired"`
		SignatureFailed   int      `json:"signature_failed"`
		EncryptionPosture string   `json:"encryption_posture"`
		KEKRefs           []string `json:"kek_refs"`
		Latest            *struct {
			BackupID string `json:"backup_id"`
		} `json:"latest"`
	} `json:"deployments"`
	KEKRefs []struct {
		KEKRef        string `json:"kek_ref"`
		ManifestCount int    `json:"manifest_count"`
	} `json:"kek_refs"`
	SchemaVersions []struct {
		Schema        string `json:"schema"`
		ManifestCount int    `json:"manifest_count"`
	} `json:"schema_versions"`
	Storage *struct {
		TotalObjects int64 `json:"total_objects"`
		TotalBytes   int64 `json:"total_bytes"`
	} `json:"storage"`
	Chain *struct {
		EventCount        int  `json:"event_count"`
		HeadHashAvailable bool `json:"head_hash_available"`
	} `json:"chain"`
	Approvals *struct {
		Total int `json:"total"`
	} `json:"approvals"`
}

// TestRepoAudit_RequiresRepo: --repo or positional URL is required.
func TestRepoAudit_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "audit", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestRepoAudit_PositionalAndFlagConflict: both repo URL forms
// disagreeing → usage.repo_conflict.
func TestRepoAudit_PositionalAndFlagConflict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "audit",
		w.repoURL+"-other", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict:\n%s", errb)
	}
}

// TestRepoAudit_EmptyRepo: a fresh repo produces a clean report
// with zero deployments and zero rollups.
func TestRepoAudit_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "repo", "audit", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if view.Schema != "pg_hardstorage.repo_audit.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
	if len(view.Deployments) != 0 {
		t.Errorf("Deployments = %d, want 0", len(view.Deployments))
	}
	if view.Repo == nil || view.Repo.ID == "" {
		t.Errorf("Repo summary missing")
	}
	// Storage section default-on; an empty repo still has the
	// HSREPO file under no recognised category, so total may be 0.
	if view.Storage == nil {
		t.Errorf("Storage section should be present by default")
	}
}

// TestRepoAudit_SingleDeployment: per-deployment counters surface
// correctly for a one-deployment repo.
func TestRepoAudit_SingleDeployment(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w, "db1", 1, []byte("b"))

	stdout, _, exit := runCLI(t, "repo", "audit", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("Deployments = %d, want 1", len(view.Deployments))
	}
	d := view.Deployments[0]
	if d.Name != "db1" {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Active != 2 {
		t.Errorf("Active = %d, want 2", d.Active)
	}
	if d.EncryptionPosture != "plaintext" {
		t.Errorf("EncryptionPosture = %q", d.EncryptionPosture)
	}
}

// TestRepoAudit_DeploymentFilter: --deployment scopes the
// per-deployment slice without affecting fleet rollups.
func TestRepoAudit_DeploymentFilter(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w, "db2", 1, []byte("b"))

	stdout, _, exit := runCLI(t, "repo", "audit",
		"--repo", w.repoURL, "--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if view.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", view.DeploymentFilter)
	}
	if len(view.Deployments) != 1 {
		t.Errorf("Deployments = %d, want 1 (filtered)", len(view.Deployments))
	}
	// Fleet rollups still cover both deployments.
	totalKEKCount := 0
	for _, k := range view.KEKRefs {
		totalKEKCount += k.ManifestCount
	}
	if totalKEKCount != 2 {
		t.Errorf("KEK rollup total = %d, want 2 (filter must not affect rollup)", totalKEKCount)
	}
}

// TestRepoAudit_SkipFlags: opt-outs suppress the optional sections.
func TestRepoAudit_SkipFlags(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "repo", "audit",
		"--repo", w.repoURL, "--no-storage", "--no-chain", "--no-approvals",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if view.Storage != nil {
		t.Errorf("Storage = %+v, want nil with --no-storage", view.Storage)
	}
	if view.Chain != nil {
		t.Errorf("Chain = %+v, want nil with --no-chain", view.Chain)
	}
	if view.Approvals != nil {
		t.Errorf("Approvals = %+v, want nil with --no-approvals", view.Approvals)
	}
}

// TestRepoAudit_PositionalURL: positional <url> works without --repo.
func TestRepoAudit_PositionalURL(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "repo", "audit", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
}

// TestRepoAudit_TextOutput: -o text includes the section headings
// and per-deployment row.
func TestRepoAudit_TextOutput(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "repo", "audit",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"repo audit",
		"Deployments:",
		"db1",
		"Manifest schema distribution",
		"KEK ref breakdown",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoAudit_HelpDiscoverable: parent shows audit; audit --help
// surfaces the flags.
func TestRepoAudit_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "repo", "--help")
	if !strings.Contains(stdout, "audit") {
		t.Errorf("repo --help missing audit:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "repo", "audit", "--help")
	for _, want := range []string{
		"--deployment",
		"--no-storage",
		"--no-chain",
		"--no-approvals",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("repo audit --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoAudit_HeldDeployment: an active hold marker shows up in
// the per-deployment Held counter.
func TestRepoAudit_HeldDeployment(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	// Use the hold add CLI to plant the marker so we exercise the
	// real path the operator sees.
	_, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL, "--reason", "test", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("hold add exit = %d", exit)
	}

	stdout, _, exit := runCLI(t, "repo", "audit",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit exit = %d", exit)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("Deployments = %d", len(view.Deployments))
	}
	if view.Deployments[0].Held != 1 {
		t.Errorf("Held = %d, want 1", view.Deployments[0].Held)
	}
}

// TestRepoAudit_TombstonedDeployment: a soft-deleted manifest
// shows up in Tombstoned, NOT Active.
func TestRepoAudit_TombstonedDeployment(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("alive"))
	dead := commitVerifiableBackup(t, w, "db1", 1, []byte("dead"))

	_, _, exit := runCLI(t, "backup", "delete", "db1", dead, "--yes",
		"--repo", w.repoURL, "--reason", "test", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("backup delete exit = %d", exit)
	}

	stdout, _, exit := runCLI(t, "repo", "audit",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit exit = %d\n%s", exit, stdout)
	}
	var view repoAuditView
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("Deployments = %d", len(view.Deployments))
	}
	d := view.Deployments[0]
	if d.Active != 1 {
		t.Errorf("Active = %d, want 1", d.Active)
	}
	if d.Tombstoned != 1 {
		t.Errorf("Tombstoned = %d, want 1", d.Tombstoned)
	}
}
