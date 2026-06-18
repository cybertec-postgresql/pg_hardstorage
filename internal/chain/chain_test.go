package chain_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/chain"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type chainWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
}

func setupWorld(t *testing.T) *chainWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &chainWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
	}
}

// commitWithChunks plants a manifest whose FileEntry references the
// given chunk bodies. Different bodies → different chunk hashes.
// Same body → same chunk hash (CAS dedup).
func (w *chainWorld) commitWithChunks(t *testing.T, deployment, suffix string, idx int, parent string, btype backup.BackupType, timeline uint32, chunkBodies [][]byte) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	chunks := make([]backup.ChunkRef, 0, len(chunkBodies))
	var off int64
	for _, body := range chunkBodies {
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, backup.ChunkRef{
			Hash:   info.Hash,
			Offset: off, // contiguous — Manifest.Validate requires this
			Len:    int64(len(body)),
		})
		off += int64(len(body))
	}
	stoppedAt := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	id := deployment + "." + string(btype) + "." + suffix + "." + stoppedAt.Format("20060102T150405Z")
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
			Size:   sumBodyBytes(chunkBodies),
			Mode:   0o600,
			Chunks: chunks,
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// orphan creates a genuine orphaned incremental. Commit refuses to
// write an incremental onto a non-live parent, so we commit a real
// parent + child and then delete the parent's manifest — simulating
// the parent being hard-deleted later (GC after its tombstone grace,
// or an out-of-band rm). Returns the orphaned child's backup ID.
func (w *chainWorld) orphan(t *testing.T, deployment string) string {
	t.Helper()
	parentID := w.commitWithChunks(t, deployment, "PARENT", 0, "",
		backup.BackupTypeFull, 1, [][]byte{[]byte("p")})
	orphanID := w.commitWithChunks(t, deployment, "ORPH", 1, parentID,
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

func sumBodyBytes(bodies [][]byte) int64 {
	var n int64
	for _, b := range bodies {
		n += int64(len(b))
	}
	return n
}

// ----- BuildGraph tests -----

// TestBuildGraph_EmptyDeployment: no manifests → empty graph.
func TestBuildGraph_EmptyDeployment(t *testing.T) {
	w := setupWorld(t)
	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if g.TotalNodes != 0 || len(g.Roots) != 0 || len(g.Orphans) != 0 {
		t.Errorf("expected empty graph; got %+v", g)
	}
}

// TestBuildGraph_SingleFull: a lone full backup is a root with
// no children and depth=1.
func TestBuildGraph_SingleFull(t *testing.T) {
	w := setupWorld(t)
	id := w.commitWithChunks(t, "db1", "a", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("alpha"), []byte("bravo")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.TotalNodes != 1 || g.FullCount != 1 || g.MaxChainDepth != 1 {
		t.Errorf("counts off: %+v", g)
	}
	if len(g.Roots) != 1 || g.Roots[0].BackupID != id {
		t.Errorf("Roots = %v", g.Roots)
	}
	if g.Roots[0].Depth != 1 {
		t.Errorf("Depth = %d", g.Roots[0].Depth)
	}
}

// TestBuildGraph_FullPlusIncrementals: full + 2 incrementals →
// one chain with depth 3.
func TestBuildGraph_FullPlusIncrementals(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a"), []byte("b"), []byte("c")})
	inc1 := w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("d")}) // shares "a", new "d"
	inc2 := w.commitWithChunks(t, "db1", "I2", 3, inc1, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("b"), []byte("e")}) // shares "b", new "e"

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.TotalNodes != 3 || g.FullCount != 1 || g.IncrementalCount != 2 || g.MaxChainDepth != 3 {
		t.Errorf("counts off: %+v", g)
	}
	if len(g.Roots) != 1 || g.Roots[0].BackupID != full {
		t.Errorf("root not %q: %v", full, g.Roots)
	}
	if len(g.Roots[0].Children) != 1 || g.Roots[0].Children[0].BackupID != inc1 {
		t.Errorf("inc1 not under full: %+v", g.Roots[0].Children)
	}
	leaf := g.Roots[0].Children[0].Children
	if len(leaf) != 1 || leaf[0].BackupID != inc2 {
		t.Errorf("inc2 not under inc1: %+v", leaf)
	}
}

// TestBuildGraph_OrphanSurfaced: an incremental whose parent was
// never committed → Orphans + critical issue.
func TestBuildGraph_OrphanSurfaced(t *testing.T) {
	w := setupWorld(t)
	w.orphan(t, "db1")

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Orphans) != 1 {
		t.Errorf("Orphans = %d, want 1", len(g.Orphans))
	}
	hasOrphanIssue := false
	for _, iss := range g.Issues {
		if iss.Code == "chain.orphaned_incremental" {
			hasOrphanIssue = true
		}
	}
	if !hasOrphanIssue {
		t.Errorf("expected chain.orphaned_incremental issue: %+v", g.Issues)
	}
}

// TestBuildGraph_TimelineAdvance_Notice: a child on a different
// timeline than its parent surfaces a notice.
func TestBuildGraph_TimelineAdvance_Notice(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 2,
		[][]byte{[]byte("a"), []byte("b")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	hasNotice := false
	for _, iss := range g.Issues {
		if iss.Code == "chain.timeline_advance" {
			hasNotice = true
		}
	}
	if !hasNotice {
		t.Errorf("expected chain.timeline_advance: %+v", g.Issues)
	}
}

// TestBuildGraph_MultipleChains: two full backups → two chains.
func TestBuildGraph_MultipleChains(t *testing.T) {
	w := setupWorld(t)
	full1 := w.commitWithChunks(t, "db1", "F1", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "I1", 2, full1, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("b")})
	w.commitWithChunks(t, "db1", "F2", 3, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("c")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Roots) != 2 {
		t.Errorf("Roots = %d, want 2", len(g.Roots))
	}
}

// TestBuildGraph_IncludeTombstoned: opt-in surfaces tombstoned.
func TestBuildGraph_IncludeTombstoned(t *testing.T) {
	w := setupWorld(t)
	a := w.commitWithChunks(t, "db1", "A", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("alive")})
	dead := w.commitWithChunks(t, "db1", "D", 2, "", backup.BackupTypeFull, 1, [][]byte{[]byte("dead")})
	if err := w.store.SoftDelete(context.Background(), "db1", dead, "test", "test"); err != nil {
		t.Fatal(err)
	}

	// Default — tombstone hidden.
	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.TotalNodes != 1 {
		t.Errorf("TotalNodes (default) = %d, want 1", g.TotalNodes)
	}

	// With opt-in.
	g, err = chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier:          w.verifier,
		IncludeTombstoned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.TotalNodes != 2 {
		t.Errorf("TotalNodes (with tombstones) = %d, want 2", g.TotalNodes)
	}
	tombstoned := 0
	for _, n := range g.AllNodes {
		if n.Tombstoned {
			tombstoned++
		}
	}
	if tombstoned != 1 {
		t.Errorf("Tombstoned = %d, want 1", tombstoned)
	}
	_ = a // silence unused
}

// TestBuildGraph_Validation: programmer-error guards.
func TestBuildGraph_Validation(t *testing.T) {
	w := setupWorld(t)
	if _, err := chain.BuildGraph(context.Background(), nil, "db1", chain.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{}); err == nil {
		t.Error("nil Verifier must error")
	}
	if _, err := chain.BuildGraph(context.Background(), w.sp, "", chain.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("empty deployment must error")
	}
}

// TestBuildGraph_ContextCancellation
func TestBuildGraph_ContextCancellation(t *testing.T) {
	w := setupWorld(t)
	w.commitWithChunks(t, "db1", "A", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := chain.BuildGraph(ctx, w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("expected ctx error")
	}
}

// ----- AnalyzeChain tests -----

// TestAnalyzeChain_RootHasNoSharedChunks: a root's TotalChunks ==
// UniqueChunks; SharedWithAncestors == 0.
func TestAnalyzeChain_RootHasNoSharedChunks(t *testing.T) {
	w := setupWorld(t)
	w.commitWithChunks(t, "db1", "A", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := g.Roots[0]
	if root.Metrics == nil {
		t.Fatal("Metrics not populated")
	}
	if root.Metrics.TotalChunks != 3 || root.Metrics.UniqueChunks != 3 ||
		root.Metrics.SharedWithAncestors != 0 {
		t.Errorf("root metrics off: %+v", root.Metrics)
	}
	if root.Metrics.DedupRatioVsChain != 1.0 {
		t.Errorf("DedupRatio = %v, want 1.0", root.Metrics.DedupRatioVsChain)
	}
}

// TestAnalyzeChain_IncrementalSharesWithAncestor: an incremental
// referencing the same chunk as the parent has SharedWithAncestors=1.
func TestAnalyzeChain_IncrementalSharesWithAncestor(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a"), []byte("b")})
	w.commitWithChunks(t, "db1", "I", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("c")}) // shares "a", new "c"

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	inc := g.Roots[0].Children[0]
	if inc.Metrics.TotalChunks != 2 {
		t.Errorf("TotalChunks = %d, want 2", inc.Metrics.TotalChunks)
	}
	if inc.Metrics.SharedWithAncestors != 1 {
		t.Errorf("SharedWithAncestors = %d, want 1", inc.Metrics.SharedWithAncestors)
	}
	if inc.Metrics.UniqueChunks != 1 {
		t.Errorf("UniqueChunks = %d, want 1", inc.Metrics.UniqueChunks)
	}
	if inc.Metrics.AncestorChunkCount != 2 {
		t.Errorf("AncestorChunkCount = %d, want 2 (full had 2 chunks)",
			inc.Metrics.AncestorChunkCount)
	}
}

// TestAnalyzeChain_DeepChain: a depth-3 chain accumulates ancestor
// chunks correctly.
func TestAnalyzeChain_DeepChain(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	inc1 := w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("b")})
	w.commitWithChunks(t, "db1", "I2", 3, inc1, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("b"), []byte("c")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf := g.Roots[0].Children[0].Children[0]
	if leaf.Metrics.TotalChunks != 3 {
		t.Errorf("TotalChunks = %d, want 3", leaf.Metrics.TotalChunks)
	}
	if leaf.Metrics.SharedWithAncestors != 2 {
		t.Errorf("SharedWithAncestors = %d, want 2 (a+b in ancestors)",
			leaf.Metrics.SharedWithAncestors)
	}
	if leaf.Metrics.UniqueChunks != 1 {
		t.Errorf("UniqueChunks = %d, want 1 (just c)", leaf.Metrics.UniqueChunks)
	}
}

// TestAnalyzeChain_SkipAnalysis: SkipAnalysis leaves Metrics nil.
func TestAnalyzeChain_SkipAnalysis(t *testing.T) {
	w := setupWorld(t)
	w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1, [][]byte{[]byte("a")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier:     w.verifier,
		SkipAnalysis: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Roots[0].Metrics != nil {
		t.Errorf("Metrics should be nil with SkipAnalysis: %+v", g.Roots[0].Metrics)
	}
}

// TestSummarizeChain: chain summary aggregates correctly.
func TestSummarizeChain(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("aa")})
	w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("bb"), []byte("aa")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := chain.SummarizeChain(g.Roots[0])
	if s.NodeCount != 2 || s.MaxDepth != 2 || s.LeafCount != 1 {
		t.Errorf("summary off: %+v", s)
	}
	if s.UniqueChunkBytesSum != int64(len("aa")+len("bb")) {
		t.Errorf("UniqueChunkBytesSum = %d, want %d",
			s.UniqueChunkBytesSum, len("aa")+len("bb"))
	}
}

// ----- Renderer tests -----

// TestRenderTree_HappyPath: single-chain tree includes every node.
func TestRenderTree_HappyPath(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("b")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := chain.RenderTree(&sb, g); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"backup chain",
		"db1",
		"●",
		"└─",
		"full",
		"incremental_lsn",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderTree_Empty: no backups → "(no backups for this deployment)".
func TestRenderTree_Empty(t *testing.T) {
	w := setupWorld(t)
	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := chain.RenderTree(&sb, g); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "no backups") {
		t.Errorf("expected empty placeholder")
	}
}

// TestRenderTree_OrphanSurfaced: orphan section appears.
func TestRenderTree_OrphanSurfaced(t *testing.T) {
	w := setupWorld(t)
	w.orphan(t, "db1")

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	_ = chain.RenderTree(&sb, g)
	if !strings.Contains(sb.String(), "Orphans") {
		t.Errorf("expected Orphans section")
	}
}

// TestRenderDOT_HappyPath: DOT output includes the digraph header
// + per-chain subgraph + parent→child edges.
func TestRenderDOT_HappyPath(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("b")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := chain.RenderDOT(&sb, g); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"digraph backup_chain",
		"subgraph cluster_chain_0",
		"shape=doublecircle", // full
		"shape=box",          // incremental
		" -> ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDOT_OrphansClustered: an orphan goes into the orphans
// cluster.
func TestRenderDOT_OrphansClustered(t *testing.T) {
	w := setupWorld(t)
	w.orphan(t, "db1")

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	_ = chain.RenderDOT(&sb, g)
	out := sb.String()
	if !strings.Contains(out, "cluster_orphans") {
		t.Errorf("expected cluster_orphans:\n%s", out)
	}
}

// TestRenderMarkdown_HappyPath
func TestRenderMarkdown_HappyPath(t *testing.T) {
	w := setupWorld(t)
	full := w.commitWithChunks(t, "db1", "F", 1, "", backup.BackupTypeFull, 1,
		[][]byte{[]byte("a")})
	w.commitWithChunks(t, "db1", "I1", 2, full, backup.BackupTypeIncremental, 1,
		[][]byte{[]byte("a"), []byte("b")})

	g, err := chain.BuildGraph(context.Background(), w.sp, "db1", chain.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := chain.RenderMarkdown(&sb, g); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage backup chain",
		"## Summary",
		"## Chains",
		"db1",
		"Per-node detail",
		"Tree",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderMarkdown_NilErrors
func TestRenderMarkdown_NilErrors(t *testing.T) {
	var sb strings.Builder
	if err := chain.RenderMarkdown(&sb, nil); err == nil {
		t.Error("expected error for nil graph")
	}
	if err := chain.RenderTree(&sb, nil); err == nil {
		t.Error("expected error for nil graph (tree)")
	}
	if err := chain.RenderDOT(&sb, nil); err == nil {
		t.Error("expected error for nil graph (dot)")
	}
}
