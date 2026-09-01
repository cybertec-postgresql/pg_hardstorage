package chain_test

// unreadable_manifest_test.go — an incomplete graph must say it is
// incomplete, because an incomplete graph manufactures findings.
//
// BuildGraph skips manifests that fail signature verification or cannot
// be read. Skipping is right: one corrupt manifest must not hide the
// rest of the topology, and `backup graph` is often the first thing an
// operator runs when something looks wrong.
//
// But it was silent, and silence here does not merely omit — it
// FABRICATES. A child whose parent was skipped gets no parent link, so
// IsOrphan() is true and the graph reports it as an orphan: "parent
// deleted / never existed". The parent is in fact sitting right there,
// merely unverifiable. The operator is sent looking for a missing
// manifest instead of a corrupt one, which is the wrong repair and the
// wrong urgency — a signature failure is potential tampering.
//
// So the count is surfaced, and the finding is recorded BEFORE the
// orphan findings it explains.

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/chain"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestBuildGraph_CorruptParentIsReportedNotJustOrphaned(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "f", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	inc := w.commitWithChunks(t, "db1", "i", 2, full, backup.BackupTypeIncremental, 1, [][]byte{[]byte("b")})

	// Corrupt the PARENT. Its child is now unparented.
	key := "manifests/db1/backups/" + full + "/manifest.json"
	if _, err := w.sp.Put(context.Background(), key,
		strings.NewReader("{\"schema\":\"broken\"}"), storage.PutOptions{}); err != nil {
		t.Fatalf("corrupt parent manifest: %v", err)
	}

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{Verifier: w.verifier})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}

	if g.UnreadableCount != 1 {
		t.Fatalf("UnreadableCount = %d, want 1 — the graph is incomplete and does not say so, "+
			"so every finding derived from it is unaccountable", g.UnreadableCount)
	}

	// The finding must exist, be critical, and point at the real cause.
	var found *chain.GraphIssue
	for i := range g.Issues {
		if g.Issues[i].Code == "chain.manifests_unreadable" {
			found = &g.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no chain.manifests_unreadable issue; issues = %+v", g.Issues)
	}
	if found.Severity != "critical" {
		t.Errorf("severity = %q, want critical — a manifest that fails signature verification "+
			"is potential tampering", found.Severity)
	}
	if !strings.Contains(found.Suggestion, "repo check") {
		t.Errorf("suggestion does not point at repo check: %q", found.Suggestion)
	}

	// And it must come BEFORE the orphan finding it explains, or the
	// operator reads the manufactured symptom first.
	unreadableAt, orphanAt := -1, -1
	for i, is := range g.Issues {
		if is.Code == "chain.manifests_unreadable" && unreadableAt < 0 {
			unreadableAt = i
		}
		if strings.Contains(is.Code, "orphan") && orphanAt < 0 {
			orphanAt = i
		}
	}
	if orphanAt >= 0 && unreadableAt > orphanAt {
		t.Errorf("the unreadable-manifest finding (index %d) comes after the orphan finding "+
			"(index %d) it explains", unreadableAt, orphanAt)
	}
	_ = inc
}

// A healthy deployment must report zero, or the critical issue fires
// on every run and stops meaning anything.
func TestBuildGraph_HealthyDeploymentReportsNoUnreadable(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "f", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "i", 2, full, backup.BackupTypeIncremental, 1, [][]byte{[]byte("b")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{Verifier: w.verifier})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if g.UnreadableCount != 0 {
		t.Errorf("UnreadableCount = %d on a healthy deployment", g.UnreadableCount)
	}
	for _, is := range g.Issues {
		if is.Code == "chain.manifests_unreadable" {
			t.Errorf("healthy deployment raised %s", is.Code)
		}
	}
	if g.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", g.TotalNodes)
	}
}
