// combine_chain_edges_test.go — regression coverage for the chain
// parent-walk safety invariants. These guard against a malformed or
// hostile parent_backup_id graph turning chain resolution into an
// infinite loop, an out-of-repo read, or a silently-wrong merge.
// combine_test.go already covers the self-loop cycle, missing link,
// tombstoned ancestor, and empty leafID; these fill the rest.
package combine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/combine"
)

// A multi-node cycle (A→B→A, not just the self-loop A→A that
// combine_test already covers) must be caught by the seen-set, not
// walked forever.
func TestBuild_RejectsMultiNodeCycle(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	commitManifest(t, sp, signer, mkInc("db1.inc.A", "db1.inc.B", t0))
	commitManifest(t, sp, signer, mkInc("db1.inc.B", "db1.inc.A", t0))

	_, err := combine.Build(context.Background(), sp, "db1", "db1.inc.A", verifier)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "chain.cycle" {
		t.Fatalf("want chain.cycle for A→B→A; got %v", err)
	}
}

// A chain longer than MaxChainDepth must refuse with chain.too_deep
// rather than reading an unbounded number of manifests. We build
// MaxChainDepth+1 increments atop a full so the walk trips the cap
// before reaching the anchor.
func TestBuild_RejectsTooDeep(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	commitManifest(t, sp, signer, mkFull("db1.full.root", t0))
	prev := "db1.full.root"
	var leaf string
	for i := 0; i <= combine.MaxChainDepth; i++ { // MaxChainDepth+1 increments
		id := fmt.Sprintf("db1.inc.%04d", i)
		commitManifest(t, sp, signer, mkInc(id, prev, t0.Add(time.Duration(i+1)*time.Minute)))
		prev = id
		leaf = id
	}

	_, err := combine.Build(context.Background(), sp, "db1", leaf, verifier)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "chain.too_deep" {
		t.Fatalf("want chain.too_deep for an over-long chain; got %v", err)
	}
}

// An incremental whose parent_backup_id is empty bottoms the walk out
// at a non-full anchor — chain.no_full_anchor, not a silent attempt to
// pg_combinebackup an increment with no base.
func TestBuild_RejectsOrphanIncremental(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	commitManifest(t, sp, signer, mkInc("db1.inc.orphan", "", t0))

	_, err := combine.Build(context.Background(), sp, "db1", "db1.inc.orphan", verifier)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "chain.no_full_anchor" {
		t.Fatalf("want chain.no_full_anchor for a parentless increment; got %v", err)
	}
}

// A leafID containing path-traversal segments must NOT read a file
// outside the repo root. With no such manifest present it simply
// errors; the point is that it returns an error (no escape, no panic)
// rather than resolving the traversal. The fs storage layer rejects
// the escaping replica key and the cleaned primary key stays in-root.
func TestBuild_TraversalLeafID_NoEscape(t *testing.T) {
	sp, _, verifier := newRepo(t)
	for _, leaf := range []string{
		"../../../etc/passwd",
		"..",
		"../_replicas/x",
		"db1/../../secret",
	} {
		if _, err := combine.Build(context.Background(), sp, "db1", leaf, verifier); err == nil {
			t.Errorf("traversal leafID %q resolved without error — possible escape", leaf)
		}
	}
}
