package chain

// The cycle breaker claims a "deterministic break point". It was not
// one: detectCycles picked its DFS entry points by ranging a map, so
// which member of a cycle turned gray first — and therefore which one
// came back as the cycle's representative — was a coin flip. Measured
// on a 3-cycle before the fix: 200 runs over identical input named "a"
// 88 times, "b" 62 and "c" 50.
//
// Everything downstream inherited it. The representative decides which
// back-edge BuildGraph drops, which decides which node becomes a root,
// which decides every descendant's Depth, which decides the dot and
// markdown renderings. And the chain.cycle_detected issue names a
// backup ID in its remediation text, so two operators running `backup
// graph` on the same corrupt repository were told to inspect two
// different backups. An audit artefact that changes between runs on
// unchanged data cannot be reproduced or diffed.
//
// removeChild is exercised here too — it was unwitnessed, and it is
// how the back-edge actually gets dropped.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

func cyclicNodes() map[string]*Node {
	n := map[string]*Node{
		"a": {BackupID: "a", ParentBackupID: "b"},
		"b": {BackupID: "b", ParentBackupID: "c"},
		"c": {BackupID: "c", ParentBackupID: "a"},
	}
	// Acyclic noise, so the map is big enough for Go's iteration
	// randomisation to bite.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("x%02d", i)
		n[id] = &Node{BackupID: id}
	}
	return n
}

func cycleKey(got map[string]struct{}) string {
	ids := make([]string, 0, len(got))
	for id := range got {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestDetectCycles_RepresentativeIsDeterministic(t *testing.T) {
	first := cycleKey(detectCycles(cyclicNodes()))
	for i := 0; i < 300; i++ {
		if got := cycleKey(detectCycles(cyclicNodes())); got != first {
			t.Fatalf("run %d returned %q, the first run returned %q — the cycle representative depends "+
				"on map iteration order, so `backup graph` renders differently on identical "+
				"data and its remediation text names a different backup each time", i, got, first)
		}
	}
	// Lowest ID wins, because the DFS now enters in sorted order and
	// the entry node is the one found gray when the cycle closes.
	if first != "a" {
		t.Errorf("representative = %q, want %q (the sorted-first member of the cycle)", first, "a")
	}
}

// Two independent cycles must each be reported, and always the same
// two — a single-representative-per-cycle contract the break loop
// relies on.
func TestDetectCycles_MultipleCyclesEachReportedDeterministically(t *testing.T) {
	build := func() map[string]*Node {
		return map[string]*Node{
			"a": {BackupID: "a", ParentBackupID: "b"},
			"b": {BackupID: "b", ParentBackupID: "a"},
			"m": {BackupID: "m", ParentBackupID: "n"},
			"n": {BackupID: "n", ParentBackupID: "o"},
			"o": {BackupID: "o", ParentBackupID: "m"},
			"z": {BackupID: "z"},
		}
	}
	first := cycleKey(detectCycles(build()))
	for i := 0; i < 200; i++ {
		if got := cycleKey(detectCycles(build())); got != first {
			t.Fatalf("run %d: %q != %q", i, got, first)
		}
	}
	if first != "a,m" {
		t.Errorf("representatives = %q, want %q (one per cycle, sorted-first member of each)",
			first, "a,m")
	}
}

// A self-parent is the degenerate cycle a corrupt manifest is most
// likely to carry.
func TestDetectCycles_SelfParent(t *testing.T) {
	got := detectCycles(map[string]*Node{
		"solo": {BackupID: "solo", ParentBackupID: "solo"},
	})
	if _, ok := got["solo"]; !ok || len(got) != 1 {
		t.Errorf("self-parent not detected as a cycle: %v", got)
	}
}

// An acyclic graph must report nothing, or every healthy repository
// gets a critical finding.
func TestDetectCycles_AcyclicAndDanglingParentsAreClean(t *testing.T) {
	got := detectCycles(map[string]*Node{
		"root":    {BackupID: "root"},
		"child":   {BackupID: "child", ParentBackupID: "root"},
		"gchild":  {BackupID: "gchild", ParentBackupID: "child"},
		"orphan":  {BackupID: "orphan", ParentBackupID: "not-in-set"},
		"orphan2": {BackupID: "orphan2", ParentBackupID: "also-absent"},
	})
	if len(got) != 0 {
		t.Errorf("acyclic graph reported cycles %v — a healthy repository would get a "+
			"critical chain.cycle_detected finding", got)
	}
}

func TestRemoveChild(t *testing.T) {
	mk := func(ids ...string) []*Node {
		out := make([]*Node, len(ids))
		for i, id := range ids {
			out[i] = &Node{BackupID: id, StoppedAt: time.Unix(int64(i), 0)}
		}
		return out
	}

	kids := mk("a", "b", "c")
	got := removeChild(kids, kids[1])
	if len(got) != 2 || got[0].BackupID != "a" || got[1].BackupID != "c" {
		t.Fatalf("removeChild dropped the wrong element: %v", ids(got))
	}

	// Removing every duplicate reference, not just the first.
	dup := mk("a", "b", "c")
	dup = append(dup, dup[1])
	if got := removeChild(dup, dup[1]); len(got) != 2 {
		t.Errorf("removeChild left a duplicate reference behind: %v", ids(got))
	}

	// A target that isn't present must leave the list intact — a
	// removeChild that dropped something here would silently detach a
	// backup from its parent and orphan its whole subtree.
	same := mk("a", "b")
	if got := removeChild(same, &Node{BackupID: "ghost"}); len(got) != 2 {
		t.Errorf("removing an absent target changed the list: %v", ids(got))
	}

	if got := removeChild(nil, &Node{}); len(got) != 0 {
		t.Errorf("removeChild(nil) = %v, want empty", ids(got))
	}
}

func ids(ns []*Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.BackupID
	}
	return out
}
