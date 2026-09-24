package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// A batch undelete that failed on a later ID returned before the audit
// append, so resurrections it had ALREADY applied were never recorded —
// backups came back to life with no trace in the audit chain. Every
// applied resurrection must be audited, failure or not.
func TestBackupUndelete_MidBatchFailureStillAuditsApplied(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	bodyB := []byte("only-B-references-this-chunk")
	idB := commitVerifiableBackup(t, w, "db1", 5, bodyB)
	for _, id := range []string{"db1.full.A", idB} {
		if err := w.store.SoftDelete(ctx, "db1", id, "manual", "t"); err != nil {
			t.Fatal(err)
		}
	}
	// B's chunk was reaped: its undelete fails closed (chunks_missing).
	if err := w.sp.Delete(ctx, repo.ChunkKey(repo.HashOf(bodyB))); err != nil {
		t.Fatal(err)
	}

	_, _, exit := runCLI(t, "backup", "undelete", "db1", "db1.full.A", idB,
		"--repo", w.repoURL, "--reason", "t", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("undelete of a backup with missing chunks exited 0")
	}
	if dead, err := w.store.IsTombstoned(ctx, "db1", "db1.full.A"); err != nil || dead {
		t.Fatalf("precondition: A should have been resurrected before B failed (dead=%v err=%v)", dead, err)
	}

	out, _, _ := runCLI(t, "audit", "search", "--repo", w.repoURL, "--action", "backup.undelete", "-o", "json")
	if !strings.Contains(out, `"count": 1`) || !strings.Contains(out, "db1.full.A") {
		t.Fatalf("resurrection of db1.full.A happened but left no audit record:\n%s", out)
	}
}
