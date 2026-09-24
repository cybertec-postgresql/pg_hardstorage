package compliance_test

import (
	"context"
	"crypto/rand"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/compliance"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// controlsFor returns the controls mapped onto section.
func controlsFor(t *testing.T, r *compliance.Report, section string) []compliance.Control {
	t.Helper()
	if r.Controls == nil {
		t.Fatal("report has no Controls")
	}
	var out []compliance.Control
	for _, c := range r.Controls.Controls {
		if c.Section == section {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no controls for section %q", section)
	}
	return out
}

// TestAssessControls_Verification_FailedRuns is the regression for
// the verification control passing on run COUNT alone: three runs
// that all failed read as "3 verification run(s) recorded" → PASS.
// A failed verify is the finding the control exists to surface, and
// a window with no successful run has not demonstrated restorability.
func TestAssessControls_Verification_FailedRuns(t *testing.T) {
	cases := []struct {
		name      string
		byOutcome map[string]int
		want      compliance.ControlStatus
	}{
		{"all failed", map[string]int{"failed": 3}, compliance.StatusFail},
		{"some failed", map[string]int{"ok": 5, "failed": 1}, compliance.StatusFail},
		{"only skipped", map[string]int{"skipped": 2}, compliance.StatusFail},
		{"all ok", map[string]int{"ok": 4}, compliance.StatusPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total := 0
			for _, n := range tc.byOutcome {
				total += n
			}
			r := &compliance.Report{Verification: &compliance.VerificationSection{
				TotalRuns: total, ByOutcome: tc.byOutcome,
			}}
			r.Controls = compliance.AssessControls(r)
			for _, c := range controlsFor(t, r, "verification") {
				if c.Status != tc.want {
					t.Errorf("%s/%s = %s (%s), want %s", c.Framework, c.ControlID, c.Status, c.Evidence, tc.want)
				}
				if c.Status == compliance.StatusFail && c.Remediation == "" {
					t.Errorf("%s/%s: failed control without remediation", c.Framework, c.ControlID)
				}
			}
		})
	}
}

// TestGenerate_Verification_FailedRunsFailControl drives the same
// regression end to end from verify.run audit events.
func TestGenerate_Verification_FailedRunsFailControl(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		w.appendAudit(t, "verify.run", "db1", now.Add(-time.Duration(i)*time.Hour),
			map[string]any{"outcome": "failed", "mode": "fast"})
	}
	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range controlsFor(t, rep, "verification") {
		if c.Status != compliance.StatusFail {
			t.Errorf("%s/%s = %s (%s) with every verify run failed", c.Framework, c.ControlID, c.Status, c.Evidence)
		}
	}
}

// TestGenerate_Verification_DeploymentFilterScopesTotals: the per-
// deployment rows honoured --deployment but TotalRuns / ByOutcome
// counted every deployment, so a filtered report mixed another
// deployment's failures (or successes) into its verdict.
func TestGenerate_Verification_DeploymentFilterScopesTotals(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "verify.run", "db1", now.Add(-time.Hour), map[string]any{"outcome": "ok"})
	w.appendAudit(t, "verify.run", "db2", now.Add(-time.Hour), map[string]any{"outcome": "failed"})
	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier, DeploymentFilter: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification.TotalRuns != 1 || rep.Verification.ByOutcome["failed"] != 0 {
		t.Errorf("filtered verification = %+v, want only db1's one ok run", rep.Verification)
	}
	for _, c := range controlsFor(t, rep, "verification") {
		if c.Status != compliance.StatusPass {
			t.Errorf("%s/%s = %s (%s); db1's only run passed", c.Framework, c.ControlID, c.Status, c.Evidence)
		}
	}
}

// TestGenerate_SignatureFailedManifestsReported is the regression for
// manifests that fail signature verification being dropped silently:
// the Options contract promises a signature_failed entry, and the
// coverage controls cannot pass when part of the population is
// unverifiable — a forged or tampered manifest is exactly what an
// auditor must see.
func TestGenerate_SignatureFailedManifestsReported(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "good", now.Add(-2*time.Hour), true, backup.BackupTypeFull)

	// Plant a manifest signed by a key the report's verifier does
	// not trust.
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rogue, _ := backup.LoadSigner(priv)
	trusted := w.signer
	w.signer = rogue
	w.commitBackup(t, "db1", "forged", now.Add(-time.Hour), true, backup.BackupTypeFull)
	w.signer = trusted

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SignatureFailed) != 1 {
		t.Fatalf("SignatureFailed = %+v, want one entry", rep.SignatureFailed)
	}
	if sf := rep.SignatureFailed[0]; sf.Deployment != "db1" || sf.Error == "" {
		t.Errorf("SignatureFailed[0] = %+v", sf)
	}
	for _, section := range []string{"encryption", "replicas"} {
		for _, c := range controlsFor(t, rep, section) {
			if c.Status != compliance.StatusFail {
				t.Errorf("%s/%s = %s (%s) with an unverifiable manifest", c.Framework, c.ControlID, c.Status, c.Evidence)
			}
		}
	}
	var md strings.Builder
	if err := compliance.RenderMarkdown(&md, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "failed signature verification") {
		t.Errorf("Markdown does not surface the signature failure:\n%s", md.String())
	}
}

// failingAuditPlugin makes every audit-shard listing fail, the way a
// permission error or a flaky backend would.
type failingAuditPlugin struct {
	storage.StoragePlugin
}

func (p failingAuditPlugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if strings.HasPrefix(prefix, "audit/shards/") {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, errors.New("injected: access denied"))
		}
	}
	return p.StoragePlugin.List(ctx, prefix)
}

// TestGenerate_AuditReadErrorFailsControls is the regression for
// audit Search errors being swallowed: the approvals section came
// back with DestructiveOps = 0, which assessApprovals reads as "no
// destructive operations executed" → PASS. Missing evidence is not
// evidence of absence; the error must be surfaced and the controls
// built on it must not pass.
func TestGenerate_AuditReadErrorFailsControls(t *testing.T) {
	w := setupWorld(t)
	rep, err := compliance.Generate(context.Background(), failingAuditPlugin{w.sp}, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"approvals", "verification"} {
		found := false
		for _, se := range rep.SectionErrors {
			if se.Section == section && strings.Contains(se.Error, "access denied") {
				found = true
			}
		}
		if !found {
			t.Errorf("SectionErrors = %+v, want an entry for %s", rep.SectionErrors, section)
		}
		for _, c := range controlsFor(t, rep, section) {
			if c.Status != compliance.StatusFail {
				t.Errorf("%s/%s = %s (%s) on unreadable audit log", c.Framework, c.ControlID, c.Status, c.Evidence)
			}
			if !strings.Contains(c.Evidence, "access denied") {
				t.Errorf("%s/%s evidence does not carry the read error: %q", c.Framework, c.ControlID, c.Evidence)
			}
		}
	}
}
