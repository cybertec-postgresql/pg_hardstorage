package retention_test

// safety_fuzz_test.go — the data-loss invariants of retention, fuzzed
// against arbitrary manifest sets. A policy decides which backups to
// DELETE; three properties must hold for EVERY input, or retention
// deletes a backup that is still needed:
//
//  1. Partition: Keep and Delete together are exactly the input — no
//     backup invented, dropped, or duplicated.
//  2. Newest always kept: the most-recent backup is never deleted, the
//     safety net every policy documents.
//  3. Chain integrity: a kept incremental whose parent is present in the
//     input keeps that parent too — never an orphaned chain that lists
//     as restorable but refuses (chain.broken_tombstoned).

import (
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

func FuzzRetentionSafetyInvariants(f *testing.F) {
	f.Add([]byte{3, 1, 0, 2, 5, 7, 9, 4, 8, 6}, uint8(2), uint8(1), uint8(3), uint8(4))
	f.Add([]byte{10, 0, 1, 1, 2, 1, 3, 1}, uint8(0), uint8(0), uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, raw []byte, keepDaily, keepWeekly, keepMonthly, keepFulls uint8) {
		if len(raw) == 0 {
			return
		}
		base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		n := int(raw[0]%40) + 1 // 1..40 manifests
		at := func(idx int) byte { return raw[idx%len(raw)] }

		in := make([]*backup.Manifest, 0, n)
		for i := 0; i < n; i++ {
			hoursBack := int(at(i*3 + 1))
			// Distinct StoppedAt per manifest (the -i minutes) so "newest"
			// is unambiguous and the sort is stable regardless of tie-break.
			stoppedAt := base.Add(-time.Duration(hoursBack) * time.Hour).Add(-time.Duration(i) * time.Minute)
			typ := backup.BackupTypeFull
			parent := ""
			switch at(i*3+2) % 3 {
			case 1:
				typ = backup.BackupTypeIncremental
				if i > 0 {
					parent = in[int(at(i*3+3))%i].BackupID // point at an earlier manifest
				}
			case 2:
				typ = backup.BackupTypeSnapshot
			}
			in = append(in, &backup.Manifest{
				BackupID:       fmt.Sprintf("db1.%s.%03d", typ, i),
				Deployment:     "db1",
				Type:           typ,
				StoppedAt:      stoppedAt,
				ParentBackupID: parent,
			})
		}

		policies := []retention.Policy{
			retention.GFSPolicy{
				KeepDaily: int(keepDaily % 8), KeepWeekly: int(keepWeekly % 5),
				KeepMonthly: int(keepMonthly % 4), KeepYearly: 1,
			},
			retention.SimplePolicy{KeepFor: time.Duration(keepDaily) * time.Hour},
			retention.CountPolicy{KeepFulls: int(keepFulls % 12)},
		}
		for _, p := range policies {
			checkRetentionInvariants(t, p.Name(), in, p.Apply(base, in))
		}
	})
}

func checkRetentionInvariants(t *testing.T, policy string, in []*backup.Manifest, d retention.Decision) {
	t.Helper()
	inIDs := map[string]*backup.Manifest{}
	for _, m := range in {
		inIDs[m.BackupID] = m
	}
	keptIDs := map[string]struct{}{}
	for _, m := range d.Keep {
		if _, dup := keptIDs[m.BackupID]; dup {
			t.Fatalf("[%s] %s appears twice in Keep", policy, m.BackupID)
		}
		keptIDs[m.BackupID] = struct{}{}
	}
	delIDs := map[string]struct{}{}
	for _, m := range d.Delete {
		if _, dup := delIDs[m.BackupID]; dup {
			t.Fatalf("[%s] %s appears twice in Delete", policy, m.BackupID)
		}
		if _, both := keptIDs[m.BackupID]; both {
			t.Fatalf("[%s] %s is in BOTH Keep and Delete", policy, m.BackupID)
		}
		delIDs[m.BackupID] = struct{}{}
	}

	// 1. Partition: Keep ∪ Delete == input, exactly.
	if len(keptIDs)+len(delIDs) != len(inIDs) {
		t.Fatalf("[%s] partition size mismatch: keep=%d delete=%d input=%d — a backup was "+
			"invented or dropped", policy, len(keptIDs), len(delIDs), len(inIDs))
	}
	for id := range inIDs {
		_, kept := keptIDs[id]
		_, del := delIDs[id]
		if !kept && !del {
			t.Fatalf("[%s] input %s is in neither Keep nor Delete — silently lost", policy, id)
		}
	}

	// 2. Newest always kept (the documented safety net).
	newest := in[0]
	for _, m := range in {
		if m.StoppedAt.After(newest.StoppedAt) {
			newest = m
		}
	}
	if _, kept := keptIDs[newest.BackupID]; !kept {
		t.Fatalf("[%s] the NEWEST backup %s (StoppedAt=%s) was DELETED — the safety net that "+
			"guarantees a deployment never has zero backups failed", policy, newest.BackupID, newest.StoppedAt)
	}

	// 3. Chain integrity: a kept manifest whose parent is present must
	//    keep that parent, or the kept child is an orphan chain link.
	for id := range keptIDs {
		m := inIDs[id]
		if m.ParentBackupID == "" {
			continue
		}
		if _, present := inIDs[m.ParentBackupID]; !present {
			continue // parent not in this set — retention only reasons about what it sees
		}
		if _, parentKept := keptIDs[m.ParentBackupID]; !parentKept {
			t.Fatalf("[%s] kept %s references parent %s which is present but DELETED — an "+
				"orphaned chain that lists restorable but refuses at restore", policy, id, m.ParentBackupID)
		}
	}
}
