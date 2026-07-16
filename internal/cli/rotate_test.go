package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

func TestBuildPolicy_GFS(t *testing.T) {
	p, err := buildPolicy(rotateOpts{
		policy: "gfs", keepDaily: 7, keepWeekly: 4, keepMonthly: 12, keepYearly: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	gfs, ok := p.(retention.GFSPolicy)
	if !ok {
		t.Fatalf("policy = %T, want retention.GFSPolicy", p)
	}
	if gfs.KeepDaily != 7 || gfs.KeepWeekly != 4 || gfs.KeepMonthly != 12 || gfs.KeepYearly != 5 {
		t.Errorf("policy fields not propagated: %+v", gfs)
	}
}

func TestBuildPolicy_Simple(t *testing.T) {
	p, err := buildPolicy(rotateOpts{policy: "simple", keepFor: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(retention.SimplePolicy); !ok {
		t.Fatalf("policy = %T, want SimplePolicy", p)
	}
}

func TestBuildPolicy_Count(t *testing.T) {
	p, err := buildPolicy(rotateOpts{policy: "count", keepFulls: 14})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(retention.CountPolicy); !ok {
		t.Fatalf("policy = %T, want CountPolicy", p)
	}
}

func TestBuildPolicy_RejectsUnknown(t *testing.T) {
	_, err := buildPolicy(rotateOpts{policy: "fortnightly"})
	if err == nil {
		t.Fatal("expected unknown-policy error")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention 'unknown'; got %v", err)
	}
}

func TestBuildPolicy_SimpleRejectsNonPositiveKeepFor(t *testing.T) {
	_, err := buildPolicy(rotateOpts{policy: "simple", keepFor: 0})
	if err == nil {
		t.Fatal("expected error on zero keep-for")
	}
}

func TestBuildPolicy_CountRejectsNegativeKeepFulls(t *testing.T) {
	_, err := buildPolicy(rotateOpts{policy: "count", keepFulls: -1})
	if err == nil {
		t.Fatal("expected error on negative keep-fulls")
	}
}

func TestShapeDecisions_KeepBeforeDeleteWhenSameStoppedAt(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	keep := &backup.Manifest{BackupID: "k1", StoppedAt: now}
	del := &backup.Manifest{BackupID: "d1", StoppedAt: now.Add(-time.Hour)}
	d := retention.Decision{
		PolicyName: "gfs",
		Keep:       []*backup.Manifest{keep},
		Delete:     []*backup.Manifest{del},
		Reasons:    map[string][]string{"k1": {"newest"}},
	}
	got := shapeDecisions(d, nil)
	if len(got) != 2 {
		t.Fatalf("got %d decisions, want 2", len(got))
	}
	if got[0].BackupID != "k1" || got[0].Action != "keep" {
		t.Errorf("first decision: got %+v, want keep k1", got[0])
	}
	if got[1].BackupID != "d1" || got[1].Action != "delete" {
		t.Errorf("second decision: got %+v, want delete d1", got[1])
	}
}

func TestRotateResultBody_WriteText_DryRun(t *testing.T) {
	body := rotateResultBody{
		DryRun:     true,
		PolicyName: "gfs",
		Deployments: []rotationPerDeployment{
			{
				Deployment: "db1",
				Policy:     "gfs",
				Kept:       2,
				Deleted:    1,
				Decisions: []rotationDecision{
					{BackupID: "db1.full.20260428T1200Z", Action: "keep", StoppedAt: "2026-04-28T12:00:00Z", Reasons: []string{"daily-1", "newest"}},
					{BackupID: "db1.full.20260427T1200Z", Action: "delete", StoppedAt: "2026-04-27T12:00:00Z"},
				},
			},
		},
	}
	var sb strings.Builder
	if err := body.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"dry-run", "Policy: gfs", "db1", "keep:    2", "delete:  1",
		"daily-1,newest", "[del ]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRotateResultBody_WriteText_Applied(t *testing.T) {
	body := rotateResultBody{
		DryRun:     false,
		PolicyName: "simple",
		Deployments: []rotationPerDeployment{
			{Deployment: "db1", Kept: 5, Deleted: 3, Applied: 3},
		},
	}
	var sb strings.Builder
	if err := body.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "Rotation applied") {
		t.Errorf("expected 'applied' in non-dry-run; got %s", sb.String())
	}
	if !strings.Contains(sb.String(), "applied: 3") {
		t.Errorf("expected applied count; got %s", sb.String())
	}
}

// Regression: a held backup that the policy chose to delete must render
// action "held", not "delete", so the per-backup listing agrees with the
// summary's held line instead of stamping a legal-hold manifest [del ].
func TestShapeDecisions_HeldNotDelete(t *testing.T) {
	keep := &backup.Manifest{BackupID: "k1", StoppedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)}
	del := &backup.Manifest{BackupID: "d1", StoppedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	held := &backup.Manifest{BackupID: "h1", StoppedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	d := retention.Decision{Keep: []*backup.Manifest{keep}, Delete: []*backup.Manifest{del, held}}
	got := shapeDecisions(d, []string{"h1"})
	byID := map[string]string{}
	for _, r := range got {
		byID[r.BackupID] = r.Action
	}
	if byID["h1"] != "held" {
		t.Errorf("held backup h1 action = %q, want held", byID["h1"])
	}
	if byID["d1"] != "delete" {
		t.Errorf("d1 action = %q, want delete", byID["d1"])
	}
}
