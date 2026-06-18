package retention_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

// TestRetention_NeverDeletesNewestBackup pins the safety net that
// closes data-loss path #2: even a maximally-aggressive policy
// (`rotate --keep-fulls 0`, or a time policy whose window excludes
// every backup) must NOT delete every backup. finalize always keeps
// the newest manifest, so the deployment always retains at least one
// restorable base. A refactor that drops the "newest" rule would
// silently re-open the total-wipe path — this test forbids it.
func TestRetention_NeverDeletesNewestBackup(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var in []*backup.Manifest
	for i := 0; i < 5; i++ {
		in = append(in, &backup.Manifest{
			BackupID:   fmt.Sprintf("db1.full.%02d", i),
			Deployment: "db1",
			Type:       backup.BackupTypeFull,
			StoppedAt:  now.Add(time.Duration(i) * time.Hour), // i=4 is newest
		})
	}
	newest := in[len(in)-1].BackupID

	for _, tc := range []struct {
		name   string
		policy retention.Policy
	}{
		{"count keep_fulls=0", retention.CountPolicy{KeepFulls: 0}},
		{"simple keep_for=1ns", retention.SimplePolicy{KeepFor: time.Nanosecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.policy.Apply(now.Add(24*time.Hour), in)
			if d.KeptCount() < 1 {
				t.Fatalf("%s deleted EVERY backup — no restorable base left", tc.name)
			}
			kept := false
			for _, m := range d.Keep {
				if m.BackupID == newest {
					kept = true
				}
			}
			if !kept {
				t.Errorf("the newest backup %q must always survive retention", newest)
			}
		})
	}
}

// TestRetention_NewestIncrementalKeepsItsAnchor: when the surviving
// newest backup is an incremental, its full anchor is kept too — so
// the one backup retention guarantees is actually restorable, not a
// dangling chain link.
func TestRetention_NewestIncrementalKeepsItsAnchor(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	full := &backup.Manifest{
		BackupID: "db1.full.old", Deployment: "db1",
		Type: backup.BackupTypeFull, StoppedAt: now.Add(-100 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID: "db1.inc.new", Deployment: "db1",
		Type: backup.BackupTypeIncremental, ParentBackupID: full.BackupID,
		StoppedAt: now.Add(-1 * time.Hour),
	}
	d := retention.CountPolicy{KeepFulls: 0}.Apply(now, []*backup.Manifest{full, inc})
	if d.KeptCount() != 2 {
		t.Fatalf("keep_fulls=0 with a newest incremental must keep BOTH (newest + its anchor); kept=%d delete=%d", d.KeptCount(), d.DeletedCount())
	}
	for _, m := range d.Delete {
		if m.BackupID == full.BackupID {
			t.Error("the newest incremental's full anchor must not be deleted")
		}
	}
}

// TestPromoteChainParents_KeptIncrementalKeepsItsFull verifies the
// core chain-aware retention rule: when a policy keeps an
// incremental backup, its parent_backup_id chain is also kept.
// Without this rule the kept incremental is a dangling chain link
// — restore would fail with chain.broken_tombstoned.
func TestPromoteChainParents_KeptIncrementalKeepsItsFull(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	// 8-day-old full + 1-hour-old incremental child. Simple policy
	// with KeepFor=24h would keep the incremental and try to delete
	// the full — exactly the chain-break we're protecting against.
	full := &backup.Manifest{
		BackupID:   "db1.full.20260422T120000Z",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-8 * 24 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID:       "db1.incremental_lsn.20260430T110000Z",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-1 * time.Hour),
	}

	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(now, []*backup.Manifest{full, inc})

	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (incremental + chain-anchored full); got delete=%d", d.KeptCount(), d.DeletedCount())
	}
	if !contains(d.Reasons[full.BackupID], "chain-anchor") {
		t.Errorf("full should be kept with reason 'chain-anchor'; reasons = %v", d.Reasons[full.BackupID])
	}
	for _, m := range d.Delete {
		if m.BackupID == full.BackupID {
			t.Error("full was placed in Delete despite being a chain anchor")
		}
	}
}

// TestPromoteChainParents_TransitiveChain: a kept leaf at the end of
// a 3-link chain (full → inc1 → inc2) keeps both parents.
func TestPromoteChainParents_TransitiveChain(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	full := &backup.Manifest{
		BackupID:   "db1.full.A",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-3 * 24 * time.Hour),
	}
	inc1 := &backup.Manifest{
		BackupID:       "db1.inc.B",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-2 * 24 * time.Hour),
	}
	inc2 := &backup.Manifest{
		BackupID:       "db1.inc.C",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: inc1.BackupID,
		StoppedAt:      now.Add(-1 * time.Hour), // inside the 24h window
	}

	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(now, []*backup.Manifest{full, inc1, inc2})

	if d.KeptCount() != 3 {
		t.Errorf("kept = %d, want 3 (leaf + 2 transitively-anchored ancestors)", d.KeptCount())
	}
	for _, id := range []string{full.BackupID, inc1.BackupID} {
		if !contains(d.Reasons[id], "chain-anchor") {
			t.Errorf("%s should carry chain-anchor reason; got %v", id, d.Reasons[id])
		}
	}
}

// TestPromoteChainParents_NoOpWhenAllPolicyKept: if every chain
// link is independently selected by the policy, no extra reasons
// are added — the chain promotion is purely additive, never
// substitutive.
func TestPromoteChainParents_NoOpWhenAllPolicyKept(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	full := &backup.Manifest{
		BackupID:   "db1.full.A",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-2 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID:       "db1.inc.B",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-1 * time.Hour),
	}

	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(now, []*backup.Manifest{full, inc})

	if d.KeptCount() != 2 {
		t.Fatalf("kept = %d, want 2", d.KeptCount())
	}
	// Full was already kept by the policy bucket; should NOT have
	// chain-anchor added (that's a "I'm only kept because of chain"
	// signal).
	if contains(d.Reasons[full.BackupID], "chain-anchor") {
		t.Errorf("full was kept by policy; chain-anchor should not be added: reasons = %v", d.Reasons[full.BackupID])
	}
}

// TestPromoteChainParents_ChainAnchorOnGFS: the GFS policy with
// KeepDaily=1 picks one daily backup and ages out the rest. When
// the picked daily is an incremental, the chain promotion must
// promote its full anchor and any intervening incrementals.
func TestPromoteChainParents_ChainAnchorOnGFS(t *testing.T) {
	now := time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)
	// Older full, then an incremental from today (which GFS picks
	// as daily-1).
	full := &backup.Manifest{
		BackupID:   "db1.full.20260425T120000Z",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-5 * 24 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID:       "db1.incremental_lsn.20260430T120000Z",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-2 * time.Hour),
	}

	d := retention.GFSPolicy{KeepDaily: 1}.Apply(now, []*backup.Manifest{full, inc})

	// Both kept: leaf as daily-1, full as chain-anchor.
	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (daily + chain-anchor)", d.KeptCount())
	}
	if !contains(d.Reasons[full.BackupID], "chain-anchor") {
		t.Errorf("full should carry chain-anchor reason; got %v", d.Reasons[full.BackupID])
	}
	if !contains(d.Reasons[inc.BackupID], "daily-1") {
		t.Errorf("incremental should carry daily-1; got %v", d.Reasons[inc.BackupID])
	}
}

// TestPromoteChainParents_StopsAtMissingParent: a kept manifest
// whose parent_backup_id is not in the input slice is left alone
// (retention can't repair what it can't see).
func TestPromoteChainParents_StopsAtMissingParent(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	orphan := &backup.Manifest{
		BackupID:       "db1.inc.orphan",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: "db1.full.not-in-slice",
		StoppedAt:      now.Add(-1 * time.Hour),
	}

	// Should not panic, should not synthesise a fake parent.
	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(now, []*backup.Manifest{orphan})

	if d.KeptCount() != 1 {
		t.Errorf("kept = %d, want 1", d.KeptCount())
	}
	// No chain-anchor reason synthesized for the absent parent.
	for id := range d.Reasons {
		if id == "db1.full.not-in-slice" {
			t.Error("synthesized reason for absent parent")
		}
	}
}

// TestPromoteChainParents_CountPolicyChainProtection: the count
// policy's full-only filter would normally delete an incremental;
// the safety-net "newest" rule keeps the newest, then chain
// promotion must keep its parent. Validates the interaction
// between count's full-bias and chain-aware promotion.
func TestPromoteChainParents_CountPolicyChainProtection(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	full := &backup.Manifest{
		BackupID:   "db1.full.A",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-2 * 24 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID:       "db1.inc.B",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-1 * time.Hour),
	}

	// keep_full_count=0 → only safety-net "newest" applies.
	d := retention.CountPolicy{KeepFulls: 0}.Apply(now, []*backup.Manifest{full, inc})

	// Newest is the incremental (saved by safety-net), then chain
	// promotion saves the full anchor.
	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (newest=inc + chain-anchor=full)", d.KeptCount())
	}
}

// TestPromoteChainParents_ChainAnchorAppearsInReasonsString: tests
// that reason emission is human-readable and the chain-anchor
// label fits the existing "X-N space-separated" output convention.
func TestPromoteChainParents_ChainAnchorAppearsInReasonsString(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	full := &backup.Manifest{
		BackupID:   "db1.full.A",
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  now.Add(-3 * 24 * time.Hour),
	}
	inc := &backup.Manifest{
		BackupID:       "db1.inc.B",
		Deployment:     "db1",
		Type:           backup.BackupTypeIncremental,
		ParentBackupID: full.BackupID,
		StoppedAt:      now.Add(-1 * time.Hour),
	}

	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(now, []*backup.Manifest{full, inc})

	got := strings.Join(d.Reasons[full.BackupID], " ")
	if !strings.Contains(got, "chain-anchor") {
		t.Errorf("expected chain-anchor in joined reasons; got %q", got)
	}
}
