package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitChainBackup plants a manifest with chunks built from the
// supplied bodies. Used by graph CLI tests to construct
// reproducible chain topologies.
func commitChainBackup(t *testing.T, w *readWorld, deployment, suffix string, idx int, parent string, btype backup.BackupType, timeline uint32, bodies [][]byte) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	chunks := make([]backup.ChunkRef, 0, len(bodies))
	for _, b := range bodies {
		info, err := cas.PutChunk(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, backup.ChunkRef{
			Hash: info.Hash,
			// Offset is filled in below once we know the running
			// total — Validate requires chunks to be contiguous.
			Len: int64(len(b)),
		})
	}
	stoppedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(time.Duration(idx) * time.Minute)
	id := deployment + "." + string(btype) + "." + suffix + "." + stoppedAt.Format("20060102T150405Z")
	var totalSize int64
	for i := range chunks {
		chunks[i].Offset = totalSize
		totalSize += chunks[i].Len
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             btype,
		ParentBackupID:   parent,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         timeline,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path:   "data/" + id,
			Size:   totalSize,
			Mode:   0o600,
			Chunks: chunks,
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// orphanIncremental creates a genuine orphaned incremental. Commit
// now refuses to write an incremental whose parent is not live, so an
// orphan can't be fabricated directly: we commit a real parent +
// child, then delete the parent's manifest to simulate it being
// hard-deleted later (GC after its tombstone grace, or an out-of-band
// `rm`). Returns the orphaned child's backup ID.
func orphanIncremental(t *testing.T, w *readWorld, deployment string) string {
	t.Helper()
	parentID := commitChainBackup(t, w, deployment, "PARENT", 0, "",
		backup.BackupTypeFull, 1, [][]byte{[]byte("p")})
	orphanID := commitChainBackup(t, w, deployment, "ORPH", 1, parentID,
		backup.BackupTypeIncremental, 1, [][]byte{[]byte("x")})
	for _, k := range []string{
		backup.PrimaryPath(deployment, parentID),
		backup.ReplicaPath(parentID),
	} {
		if err := w.sp.Delete(context.Background(), k); err != nil {
			t.Fatalf("delete parent manifest %s: %v", k, err)
		}
	}
	return orphanID
}

// graphView is the JSON shape we assert on. Mirrors chain.Graph.
type graphView struct {
	Schema           string `json:"schema"`
	URL              string `json:"url"`
	Deployment       string `json:"deployment"`
	TotalNodes       int    `json:"total_nodes"`
	FullCount        int    `json:"full_count"`
	IncrementalCount int    `json:"incremental_count"`
	OrphanCount      int    `json:"orphan_count"`
	MaxChainDepth    int    `json:"max_chain_depth"`
	Roots            []struct {
		BackupID string `json:"backup_id"`
		Type     string `json:"type"`
		Depth    int    `json:"depth"`
		Children []struct {
			BackupID string `json:"backup_id"`
			Depth    int    `json:"depth"`
			Metrics  *struct {
				TotalChunks         int `json:"total_chunks"`
				UniqueChunks        int `json:"unique_chunks"`
				SharedWithAncestors int `json:"shared_with_ancestors"`
			} `json:"metrics"`
		} `json:"children"`
		Metrics *struct {
			TotalChunks  int     `json:"total_chunks"`
			UniqueChunks int     `json:"unique_chunks"`
			DedupRatio   float64 `json:"dedup_ratio_vs_chain"`
		} `json:"metrics"`
	} `json:"roots"`
	Orphans []struct {
		BackupID       string `json:"backup_id"`
		ParentBackupID string `json:"parent_backup_id"`
	} `json:"orphans"`
	Issues []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	} `json:"issues"`
}

// TestBackupGraph_RequiresRepo: --repo is mandatory.
func TestBackupGraph_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "backup", "graph", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestBackupGraph_BadFormat: --format must be json/markdown/dot/tree.
func TestBackupGraph_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestBackupGraph_EmptyDeployment: a deployment with no manifests
// → empty graph, exit 0.
func TestBackupGraph_EmptyDeployment(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if view.TotalNodes != 0 || len(view.Roots) != 0 {
		t.Errorf("expected empty: %+v", view)
	}
	if view.Schema != "pg_hardstorage.backup_chain.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
}

// TestBackupGraph_SingleFull: one full → one root, depth 1, no
// children, dedup ratio 1.0.
func TestBackupGraph_SingleFull(t *testing.T) {
	w := newReadWorld(t)
	commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a"), []byte("b")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if view.TotalNodes != 1 || view.FullCount != 1 || view.MaxChainDepth != 1 {
		t.Errorf("counts off: %+v", view)
	}
	if len(view.Roots) != 1 || view.Roots[0].Type != "full" {
		t.Errorf("Roots = %v", view.Roots)
	}
	if view.Roots[0].Metrics == nil ||
		view.Roots[0].Metrics.UniqueChunks != 2 ||
		view.Roots[0].Metrics.DedupRatio != 1.0 {
		t.Errorf("Metrics off: %+v", view.Roots[0].Metrics)
	}
}

// TestBackupGraph_FullPlusIncrementals: chain depth 3 with shared
// chunks → SharedWithAncestors > 0 on incrementals.
func TestBackupGraph_FullPlusIncrementals(t *testing.T) {
	w := newReadWorld(t)
	full := commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a"), []byte("b")})
	commitChainBackup(t, w, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("c")}) // shares "a"

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if view.MaxChainDepth != 2 {
		t.Errorf("MaxChainDepth = %d, want 2", view.MaxChainDepth)
	}
	root := view.Roots[0]
	if len(root.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(root.Children))
	}
	inc := root.Children[0]
	if inc.Metrics == nil {
		t.Fatal("Metrics missing on incremental")
	}
	if inc.Metrics.SharedWithAncestors != 1 {
		t.Errorf("SharedWithAncestors = %d, want 1",
			inc.Metrics.SharedWithAncestors)
	}
	if inc.Metrics.UniqueChunks != 1 {
		t.Errorf("UniqueChunks = %d, want 1", inc.Metrics.UniqueChunks)
	}
}

// TestBackupGraph_OrphanedIncremental: incremental whose parent
// is missing → Orphans + critical issue.
func TestBackupGraph_OrphanedIncremental(t *testing.T) {
	w := newReadWorld(t)
	orphanIncremental(t, w, "db1")

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if view.OrphanCount != 1 {
		t.Errorf("OrphanCount = %d, want 1", view.OrphanCount)
	}
	hasOrphanIssue := false
	for _, iss := range view.Issues {
		if iss.Code == "chain.orphaned_incremental" {
			hasOrphanIssue = true
		}
	}
	if !hasOrphanIssue {
		t.Errorf("expected chain.orphaned_incremental issue: %+v", view.Issues)
	}
}

// TestBackupGraph_TreeFormat: --format tree emits the box-drawing tree.
func TestBackupGraph_TreeFormat(t *testing.T) {
	w := newReadWorld(t)
	full := commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	commitChainBackup(t, w, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("b")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--format", "tree", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"backup chain",
		"●",
		"└─",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tree output missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupGraph_DotFormat: --format dot emits a digraph.
func TestBackupGraph_DotFormat(t *testing.T) {
	w := newReadWorld(t)
	full := commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	commitChainBackup(t, w, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("b")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--format", "dot", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"digraph backup_chain",
		"shape=doublecircle",
		"shape=box",
		" -> ",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("DOT output missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupGraph_MarkdownFormat: --format markdown emits the
// forensics-grade GFM document.
func TestBackupGraph_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	full := commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	commitChainBackup(t, w, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("b")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"# pg_hardstorage backup chain",
		"## Summary",
		"## Chains",
		"### `db1.full.F",
		"Per-node detail",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupGraph_NoAnalysis: --no-analysis suppresses Metrics.
func TestBackupGraph_NoAnalysis(t *testing.T) {
	w := newReadWorld(t)
	commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--no-analysis", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if len(view.Roots) != 1 {
		t.Fatalf("Roots = %d", len(view.Roots))
	}
	if view.Roots[0].Metrics != nil {
		t.Errorf("Metrics should be nil with --no-analysis: %+v", view.Roots[0].Metrics)
	}
}

// TestBackupGraph_IncludeTombstoned: --include-tombstoned surfaces
// soft-deleted manifests.
func TestBackupGraph_IncludeTombstoned(t *testing.T) {
	w := newReadWorld(t)
	commitChainBackup(t, w, "db1", "ALIVE", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	dead := commitChainBackup(t, w, "db1", "DEAD", 2, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("b")})

	// Soft-delete via the existing CLI to exercise the real path.
	_, _, exit := runCLI(t, "backup", "delete", "db1", dead,
		"--repo", w.repoURL, "--reason", "test", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("backup delete exit = %d", exit)
	}

	// Default: tombstoned excluded.
	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if view.TotalNodes != 1 {
		t.Errorf("default TotalNodes = %d, want 1", view.TotalNodes)
	}

	// With opt-in.
	stdout, _, exit = runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--include-tombstoned", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	bodyOf(t, stdout, &view)
	if view.TotalNodes != 2 {
		t.Errorf("include-tombstoned TotalNodes = %d, want 2", view.TotalNodes)
	}
}

// TestBackupGraph_DefaultText: -o text without --format → tree.
func TestBackupGraph_DefaultText(t *testing.T) {
	w := newReadWorld(t)
	commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, "●") || !strings.Contains(stdout, "backup chain") {
		t.Errorf("default text format should be tree:\n%s", stdout)
	}
}

// TestBackupGraph_HelpDiscoverable
func TestBackupGraph_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "backup", "graph", "--help")
	for _, want := range []string{
		"--format",
		"--include-tombstoned",
		"--no-analysis",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backup graph --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "backup", "--help")
	if !strings.Contains(stdout, "graph") {
		t.Errorf("backup --help missing graph subcommand:\n%s", stdout)
	}
}

// TestBackupGraph_TimelineAdvance_Notice: timeline change surfaces
// in Issues.
func TestBackupGraph_TimelineAdvance_Notice(t *testing.T) {
	w := newReadWorld(t)
	full := commitChainBackup(t, w, "db1", "F", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	commitChainBackup(t, w, "db1", "I1", 2, full, backup.BackupTypeIncremental, 2,
		[][]byte{[]byte("a"), []byte("b")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	hasNotice := false
	for _, iss := range view.Issues {
		if iss.Code == "chain.timeline_advance" && iss.Severity == "notice" {
			hasNotice = true
		}
	}
	if !hasNotice {
		t.Errorf("expected chain.timeline_advance notice: %+v", view.Issues)
	}
}

// TestBackupGraph_FleetWithMultipleChains: two fulls + their
// incrementals → 2 roots.
func TestBackupGraph_FleetWithMultipleChains(t *testing.T) {
	w := newReadWorld(t)
	f1 := commitChainBackup(t, w, "db1", "F1", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	commitChainBackup(t, w, "db1", "I1a", 2, f1, backup.BackupTypeIncremental, 1, [][]byte{[]byte("b")})
	commitChainBackup(t, w, "db1", "F2", 5, "", backup.BackupTypeFull, 1, [][]byte{[]byte("c")})

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view graphView
	bodyOf(t, stdout, &view)
	if len(view.Roots) != 2 {
		t.Errorf("Roots = %d, want 2", len(view.Roots))
	}
	if view.TotalNodes != 3 {
		t.Errorf("TotalNodes = %d, want 3", view.TotalNodes)
	}
}

// TestBackupGraph_DOT_OrphansClustered: DOT output for an
// orphaned chain produces the orphans cluster.
func TestBackupGraph_DOT_OrphansClustered(t *testing.T) {
	w := newReadWorld(t)
	orphanIncremental(t, w, "db1")

	stdout, _, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", w.repoURL, "--format", "dot", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, "cluster_orphans") {
		t.Errorf("expected cluster_orphans:\n%s", stdout)
	}
}

// TestBackupGraph_RepoOpenError_Surfaced: pointing at a
// non-existent repo URL surfaces a clean structured error.
func TestBackupGraph_RepoOpenError_Surfaced(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "backup", "graph", "db1",
		"--repo", "file:///does/not/exist", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit; got %d (%s)", exit, errb)
	}
	if !strings.Contains(errb, "notfound.repo") &&
		!strings.Contains(errb, "open") {
		t.Errorf("expected structured error code:\n%s", errb)
	}
}

// silence unused (repo, casdefault imports) on test files that
// don't need them outside helpers — kept here to anchor.
var _ = repo.SchemaRepo
var _ = casdefault.New
