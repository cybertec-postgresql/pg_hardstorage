package backup_test

// tombstone_statemachine_fuzz_test.go — the tombstone state machine's
// core failover invariant, fuzzed over arbitrary operation sequences.
//
// A LIVE incremental must NEVER sit on a TOMBSTONED parent. That state is
// this campaign's signature hazard: `backup graph --include-deleted`
// would show the two connected and stay quiet while `restore` refuses the
// leaf (chain.broken_tombstoned). Three lower-layer guards make it
// unconstructible — leaf-first soft-delete (#15), undelete refusing a
// child under a tombstoned parent (#16), and cascade tombstoning
// leaf-first. The example tests pin specific shapes; this drives a random
// chain through a random sequence of SoftDelete / SoftDeleteCascade /
// Undelete and asserts the invariant after EVERY operation, so no
// sequence the guards didn't consider can reach the hazardous state.

import (
	"context"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func FuzzTombstoneNoLiveChildOnDeadParent(f *testing.F) {
	f.Add([]byte{5, 0, 0, 2, 0, 1, 4, 0, 6, 1, 3, 2, 8, 7, 9, 11, 10, 1, 5})
	f.Add([]byte{3, 0, 1, 1, 2, 1, 0, 1, 4, 2, 3, 5, 6, 7})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 4 {
			return
		}
		store, _, signer, _ := newStore(t)
		ctx := context.Background()
		b := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return raw[i%len(raw)]
		}

		// 1. Commit a random forest of live backups: each is a full or an
		//    incremental pointing at an EARLIER backup (so parents exist and
		//    are live at commit time). Empty Files so a later Undelete's
		//    chunk-existence check is trivially satisfied.
		n := int(raw[0]%10) + 2 // 2..11
		var ids []string
		parentOf := map[string]string{}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("bk%02d", i)
			typ := backup.BackupTypeFull
			parent := ""
			if i > 0 && b(i*2+1)%2 == 0 {
				typ = backup.BackupTypeIncremental
				parent = ids[int(b(i*2+2))%i]
			}
			m := sampleManifest()
			m.BackupID = id
			m.Deployment = "db1"
			m.Type = typ
			m.ParentBackupID = parent
			m.Files = []backup.FileEntry{}
			if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit %s (type=%s parent=%q): %v", id, typ, parent, err)
			}
			ids = append(ids, id)
			parentOf[id] = parent
		}

		invariant := func(where string) {
			for _, id := range ids {
				p := parentOf[id]
				if p == "" {
					continue
				}
				childDead, err := store.IsTombstoned(ctx, "db1", id)
				if err != nil {
					t.Fatalf("%s: IsTombstoned(%s): %v", where, id, err)
				}
				if childDead {
					continue // a tombstoned child on a tombstoned parent is fine
				}
				parentDead, err := store.IsTombstoned(ctx, "db1", p)
				if err != nil {
					t.Fatalf("%s: IsTombstoned(%s): %v", where, p, err)
				}
				if parentDead {
					t.Fatalf("%s: LIVE child %s sits on TOMBSTONED parent %s — the failover "+
						"data-loss hazard the leaf-first / undelete guards must prevent", where, id, p)
				}
			}
		}
		invariant("after commit")

		// 2. Apply a bounded random sequence of operations; the invariant
		//    must hold after each, whether the operation succeeded or the
		//    store refused it.
		maxOps := 48
		for k := 0; k < maxOps; k++ {
			opByte := b(n*2 + k*2 + 3)
			tgt := ids[int(b(n*2+k*2+4))%len(ids)]
			switch opByte % 3 {
			case 0:
				_ = store.SoftDelete(ctx, "db1", tgt, "manual", "fuzz")
			case 1:
				_, _ = store.SoftDeleteCascade(ctx, "db1", tgt, "manual", "fuzz")
			case 2:
				_, _ = store.Undelete(ctx, "db1", tgt)
			}
			invariant(fmt.Sprintf("after op #%d (kind=%d) on %s", k, opByte%3, tgt))
		}
	})
}
