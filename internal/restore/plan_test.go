package restore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestPlan_HappyPath(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"

	p, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.BackupID == "" || p.Deployment != "db1" {
		t.Errorf("plan missing identity: %+v", p)
	}
	if p.FileCount != len(fx.files) {
		t.Errorf("FileCount = %d, want %d", p.FileCount, len(fx.files))
	}
	// TotalBytes is sum of file sizes; the fixture set has one
	// 200_000-byte file plus 8192 plus a 3-byte plus an empty.
	want := int64(200_000 + 8192 + 3 + 0)
	if p.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", p.TotalBytes, want)
	}
	if p.UniqueChunkCount == 0 {
		t.Error("UniqueChunkCount should be > 0")
	}
	if p.PreflightOK != true {
		t.Errorf("PreflightOK should be true for non-existent target; issues=%v", p.PreflightIssues)
	}
	if p.EstimatedRTO <= 0 {
		t.Errorf("EstimatedRTO should be positive; got %v", p.EstimatedRTO)
	}
	if p.AssumedThroughput != restore.EstimateThroughput {
		t.Errorf("AssumedThroughput = %d, want %d", p.AssumedThroughput, restore.EstimateThroughput)
	}
	if p.BackupLabelSize == 0 {
		t.Errorf("BackupLabelSize should be > 0 (fixture sets a label)")
	}
}

func TestPlan_PreflightFlagsNonEmptyTarget(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err != nil {
		t.Fatalf("Plan should succeed (it reports issues, not refuses): %v", err)
	}
	if p.PreflightOK {
		t.Error("PreflightOK should be false for non-empty target")
	}
	if len(p.PreflightIssues) == 0 {
		t.Error("PreflightIssues should be populated")
	}
	hasNonEmpty := false
	for _, msg := range p.PreflightIssues {
		if contains(msg, "not empty") {
			hasNonEmpty = true
		}
	}
	if !hasNonEmpty {
		t.Errorf("issue should mention non-empty: %v", p.PreflightIssues)
	}
}

func TestPlan_PreflightFlagsTargetAsFile(t *testing.T) {
	fx := newFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, "regfile")
	if err := os.WriteFile(target, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.PreflightOK {
		t.Error("PreflightOK should be false when target is a regular file")
	}
}

func TestPlan_BackupNotFound(t *testing.T) {
	fx := newFixture(t)
	_, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "missing",
		TargetDir:  t.TempDir(),
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected error for missing backup")
	}
}

func TestPlan_RejectsForeignVerifier(t *testing.T) {
	fx := newFixture(t)
	// Use a fresh fixture's verifier (different keypair).
	fx2 := newFixture(t)

	_, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  t.TempDir(),
		Verifier:   fx2.verifier,
	})
	if err == nil {
		t.Fatal("Plan with foreign verifier must fail")
	}
}

func TestPlan_ValidateOptions(t *testing.T) {
	cases := []struct {
		name string
		opts restore.PlanOptions
		want string
	}{
		{"missing RepoURL", restore.PlanOptions{Deployment: "d", BackupID: "b", TargetDir: "/t"}, "RepoURL"},
		{"missing Deployment", restore.PlanOptions{RepoURL: "x", BackupID: "b", TargetDir: "/t"}, "Deployment"},
		{"missing BackupID", restore.PlanOptions{RepoURL: "x", Deployment: "d", TargetDir: "/t"}, "BackupID"},
		{"missing TargetDir", restore.PlanOptions{RepoURL: "x", Deployment: "d", BackupID: "b"}, "TargetDir"},
		{"missing Verifier", restore.PlanOptions{RepoURL: "x", Deployment: "d", BackupID: "b", TargetDir: "/t"}, "Verifier"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := restore.Preview(context.Background(), c.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q", c.want)
			}
			if !errors.Is(err, output.ErrUsage) {
				t.Errorf("validation error should wrap ErrUsage; got %v", err)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
