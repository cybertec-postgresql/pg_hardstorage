package backup_test

// retention_store_compose_fuzz_test.go — the retention-POLICY × tombstone-
// STORE seam, fuzzed. The policy-only fuzz (retention/safety_fuzz_test.go)
// proves a policy's Decision partitions correctly and never orphans a kept
// chain link IN MEMORY. The tombstone state-machine fuzz
// (tombstone_statemachine_fuzz_test.go) proves the store never reaches a
// live-child-on-dead-parent state under arbitrary op sequences. Neither
// drives a policy's Delete set THROUGH the real store — which is exactly
// what `rotate` does (rotate.go: policy.Apply → filterHeld →
// store.SoftDeleteBatch).
//
// The load-bearing claim in rotate.go is "the policy's finalize guarantees
// the set is chain-safe, so the batch's chain check normally passes." If a
// policy ever emits a Delete set that SoftDeleteBatch REFUSES with
// ErrChainHasLiveDescendants (a deleted parent whose live child the policy
// kept), rotate aborts atomically and the operator re-runs it forever — the
// repo grows without bound on the unattended path, the exact failure shape
// of the agent-rotation wedge (2bbece5) and the rotate-undelete churn (#17).
//
// This fuzz commits a random chain to the REAL ManifestStore, runs a random
// policy, and applies decision.Delete via SoftDeleteBatch — the production
// call. It asserts the batch is NEVER chain-refused (the policy/store chain
// reasoning must AGREE — the policy reasons over in-memory ParentBackupID,
// the store over the committed manifest bytes via loadChainSnapshot; a
// divergence there is a real wedge), the NEWEST backup survives the store
// layer, and no live backup is left on a tombstoned parent.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

func FuzzRetentionApplyThroughStoreNeverWedges(f *testing.F) {
	f.Add([]byte{7, 2, 0, 1, 3, 0, 5, 1, 2, 9, 4, 0, 6, 1, 1}, uint8(1), uint8(2))
	f.Add([]byte{4, 0, 1, 1, 0, 1, 2, 1, 3}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, raw []byte, sel, keepN uint8) {
		if len(raw) < 3 {
			return
		}
		store, _, signer, _ := newStore(t)
		ctx := context.Background()
		base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		at := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return raw[i%len(raw)]
		}

		// Commit a random chain, all live. Distinct StoppedAt per backup
		// (minutes offset) so "newest" is unambiguous. Empty Files keeps
		// the commit cheap; the chain shape is what matters here.
		n := int(raw[0]%20) + 1
		var in []*backup.Manifest
		var ids []string
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("bk%03d", i)
			typ := backup.BackupTypeFull
			parent := ""
			if i > 0 && at(i*2+1)%2 == 0 {
				typ = backup.BackupTypeIncremental
				parent = ids[int(at(i*2+2))%i]
			}
			hoursBack := int(at(i*2 + 1))
			stoppedAt := base.Add(-time.Duration(hoursBack) * time.Hour).Add(-time.Duration(i) * time.Minute)

			m := sampleManifest()
			m.BackupID = id
			m.Deployment = "db1"
			m.Type = typ
			m.ParentBackupID = parent
			m.StoppedAt = stoppedAt
			m.Files = []backup.FileEntry{}
			if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit %s (type=%s parent=%q): %v", id, typ, parent, err)
			}
			in = append(in, m)
			ids = append(ids, id)
		}

		// Pick ONE policy per exec (applying a policy mutates the store, so
		// testing several would cross-contaminate; the selector byte lets
		// the fuzzer explore all three).
		policies := []retention.Policy{
			retention.GFSPolicy{
				KeepDaily: int(keepN % 6), KeepWeekly: int(sel % 4),
				KeepMonthly: int(keepN % 3), KeepYearly: 1,
			},
			retention.SimplePolicy{KeepFor: time.Duration(keepN) * time.Hour},
			retention.CountPolicy{KeepFulls: int(keepN%10) + 1},
		}
		p := policies[int(sel)%len(policies)]

		decision := p.Apply(base, in)
		delIDs := make([]string, 0, len(decision.Delete))
		for _, m := range decision.Delete {
			delIDs = append(delIDs, m.BackupID)
		}

		// THE production call rotate makes. The policy PROMISES this set is
		// chain-safe; the store ENFORCES it. They must agree.
		_, err := store.SoftDeleteBatch(ctx, "db1", delIDs, decision.PolicyName, "fuzz")
		if err != nil {
			if errors.Is(err, backup.ErrChainHasLiveDescendants) {
				t.Fatalf("policy %s emitted a Delete set (%v) that SoftDeleteBatch REFUSED as "+
					"chain-unsafe — rotate would abort atomically and re-run forever, growing the "+
					"repo without bound (the agent-rotation wedge class). Policy/store chain "+
					"reasoning DISAGREE. chain=%s", decision.PolicyName, delIDs, chainDump(in))
			}
			t.Fatalf("policy %s: SoftDeleteBatch(%v): unexpected error: %v", decision.PolicyName, delIDs, err)
		}

		// The NEWEST backup must survive the store layer — retention's
		// documented safety net, verified end-to-end rather than in-memory.
		newest := in[0]
		for _, m := range in {
			if m.StoppedAt.After(newest.StoppedAt) {
				newest = m
			}
		}
		if dead, derr := store.IsTombstoned(ctx, "db1", newest.BackupID); derr != nil {
			t.Fatalf("IsTombstoned(newest %s): %v", newest.BackupID, derr)
		} else if dead {
			t.Fatalf("policy %s tombstoned the NEWEST backup %s through the store — a deployment "+
				"could be left with zero live backups", decision.PolicyName, newest.BackupID)
		}

		// No live backup may sit on a tombstoned parent after the real
		// batch runs (the failover-restore hazard, verified through the
		// store rather than asserted on the Decision).
		parentOf := map[string]string{}
		for _, m := range in {
			parentOf[m.BackupID] = m.ParentBackupID
		}
		for _, m := range in {
			pid := parentOf[m.BackupID]
			if pid == "" {
				continue
			}
			childDead, e1 := store.IsTombstoned(ctx, "db1", m.BackupID)
			if e1 != nil {
				t.Fatalf("IsTombstoned(%s): %v", m.BackupID, e1)
			}
			if childDead {
				continue
			}
			parentDead, e2 := store.IsTombstoned(ctx, "db1", pid)
			if e2 != nil {
				t.Fatalf("IsTombstoned(%s): %v", pid, e2)
			}
			if parentDead {
				t.Fatalf("policy %s left LIVE child %s on TOMBSTONED parent %s through the store",
					decision.PolicyName, m.BackupID, pid)
			}
		}
	})
}

// TestSoftDeleteBatchRefusesOrphaningDelete pins the store-enforcement
// PRECONDITION the fuzz above relies on: a batch that deletes a parent while
// leaving its child live MUST be refused with ErrChainHasLiveDescendants.
// Without this the fuzz's central assertion would be vacuous — it would pass
// simply because the store never refuses anything. This is the "guard" the
// fuzz "trusts"; pinning both is the recurring win-shape of this campaign.
func TestSoftDeleteBatchRefusesOrphaningDelete(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()

	full := sampleManifest()
	full.BackupID = "F"
	full.Deployment = "db1"
	full.Type = backup.BackupTypeFull
	full.Files = []backup.FileEntry{}
	if err := store.Commit(ctx, full, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit F: %v", err)
	}
	inc := sampleManifest()
	inc.BackupID = "I"
	inc.Deployment = "db1"
	inc.Type = backup.BackupTypeIncremental
	inc.ParentBackupID = "F"
	inc.Files = []backup.FileEntry{}
	if err := store.Commit(ctx, inc, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit I: %v", err)
	}

	// Delete the PARENT while the child stays live → must be refused.
	_, err := store.SoftDeleteBatch(ctx, "db1", []string{"F"}, "test", "orphan-probe")
	if !errors.Is(err, backup.ErrChainHasLiveDescendants) {
		t.Fatalf("SoftDeleteBatch([F]) with live child I: got err=%v, want ErrChainHasLiveDescendants "+
			"— if the store no longer refuses orphaning deletes, the compose fuzz is vacuous", err)
	}
	// And F must remain LIVE (the refusal rolled back / never installed).
	if dead, derr := store.IsTombstoned(ctx, "db1", "F"); derr != nil {
		t.Fatalf("IsTombstoned(F): %v", derr)
	} else if dead {
		t.Fatalf("F was tombstoned despite the refusal — orphaning delete leaked through")
	}
}

// chainDump renders the committed chain compactly for a failure message so
// a discovered wedge is reproducible without re-deriving the shape.
func chainDump(in []*backup.Manifest) string {
	s := ""
	for _, m := range in {
		s += fmt.Sprintf("%s(%s<-%q)@%s ", m.BackupID, m.Type, m.ParentBackupID,
			m.StoppedAt.Format("15:04"))
	}
	return s
}
