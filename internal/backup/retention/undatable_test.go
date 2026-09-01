package retention

// undatable_test.go — retention must not delete a backup it cannot date.
//
// Every policy in this package dates a manifest by StoppedAt. The zero
// time is not a date; it is the absence of one. Read as a date it means
// year 1 — infinitely old — which is the worst possible reading for the
// only operation here that destroys data:
//
//	SimplePolicy   zero < any cutoff        → delete
//	CountPolicy    sorts last, past KeepFulls → delete
//
// So a backup was deleted precisely BECAUSE the tool could not tell how
// old it was. Manifest.Validate does not require StoppedAt and runs
// only at commit, so a manifest in this shape can exist in a repo and
// is never re-checked.
//
// The rest of the tree already takes the conservative side of this
// question — wal prune's keep-floor treats an unknown created_at as
// too-new-to-delete, and the recovery-window and reporting paths guard
// with IsZero. Retention was the outlier, and it is the destructive one.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func undatableFixture(now time.Time) []*backup.Manifest {
	mk := func(id string, st time.Time) *backup.Manifest {
		return &backup.Manifest{BackupID: id, StoppedAt: st, Type: backup.BackupTypeFull}
	}
	return []*backup.Manifest{
		mk("b.newest", now.Add(-30*time.Minute)),
		mk("b.hour", now.Add(-time.Hour)),
		mk("b.ancient", now.AddDate(-3, 0, 0)),
		mk("b.undatable", time.Time{}),
	}
}

func deletedIDs(d Decision) map[string]bool {
	out := map[string]bool{}
	for _, m := range d.Delete {
		out[m.BackupID] = true
	}
	return out
}

func TestPolicies_NeverDeleteAManifestTheyCannotDate(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	in := undatableFixture(now)

	policies := []Policy{
		SimplePolicy{KeepFor: 7 * 24 * time.Hour},
		CountPolicy{KeepFulls: 1},
		GFSPolicy{KeepDaily: 2, KeepWeekly: 1, KeepMonthly: 1, KeepYearly: 1},
	}
	for _, p := range policies {
		t.Run(p.Name(), func(t *testing.T) {
			d := p.Apply(now, in)
			if deletedIDs(d)["b.undatable"] {
				t.Fatalf("%s deleted a backup with no stop time — it was destroyed "+
					"BECAUSE the policy could not tell how old it was", p.Name())
			}
			reasons := d.Reasons["b.undatable"]
			var sawUndatable bool
			for _, r := range reasons {
				if r == "undatable" {
					sawUndatable = true
				}
			}
			if !sawUndatable {
				t.Errorf("kept, but the reason list %v does not say why; `rotate --dry-run` "+
					"must show the operator that this backup has no stop time", reasons)
			}
		})
	}
}

// The guard must not turn retention off. A datable backup outside the
// window is still deleted.
func TestPolicies_StillDeleteDatableBackupsOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	in := undatableFixture(now)

	d := SimplePolicy{KeepFor: 7 * 24 * time.Hour}.Apply(now, in)
	if !deletedIDs(d)["b.ancient"] {
		t.Fatal("a 3-year-old backup survived a 7-day simple policy; the undatable " +
			"guard has disabled retention")
	}

	d = CountPolicy{KeepFulls: 1}.Apply(now, in)
	if !deletedIDs(d)["b.ancient"] {
		t.Fatal("count policy with KeepFulls=1 kept the ancient backup")
	}
}

// A repo where EVERY manifest is undatable must keep everything rather
// than empty itself out.
func TestPolicies_AllUndatableKeepsEverything(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	in := []*backup.Manifest{
		{BackupID: "a", Type: backup.BackupTypeFull},
		{BackupID: "b", Type: backup.BackupTypeFull},
		{BackupID: "c", Type: backup.BackupTypeFull},
	}
	for _, p := range []Policy{
		SimplePolicy{KeepFor: time.Hour},
		CountPolicy{KeepFulls: 1},
	} {
		d := p.Apply(now, in)
		if len(d.Delete) != 0 {
			t.Errorf("%s deleted %d of 3 undatable backups", p.Name(), len(d.Delete))
		}
	}
}
