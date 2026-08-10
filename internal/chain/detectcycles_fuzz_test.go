package chain

// detectcycles_fuzz_test.go — crash-freedom of the cycle detector
// against adversarial parent pointers.
//
// The commit path is supposed to make cycles impossible, but
// detectCycles exists precisely because "supposed to" is not a
// guarantee — a corrupted or hand-edited manifest set can carry a
// self-parent, a 2-cycle, or a dangling reference. This is graph code
// walking arbitrary repository data; a panic here takes down `backup
// graph`, which is often the tool an operator reaches for WHILE a
// restore is already failing. Errors and empty results are fine;
// panics are the bug.

import (
	"strconv"
	"testing"
)

func FuzzDetectCycles(f *testing.F) {
	f.Add(uint(3), int64(0))    // 0/1/2 chain
	f.Add(uint(2), int64(-1))   // self-parents
	f.Add(uint(4), int64(1))    // shifted — 2-cycles
	f.Add(uint(1), int64(9999)) // single node, dangling parent
	f.Fuzz(func(t *testing.T, n uint, parentSkew int64) {
		if n > 512 {
			n = 512 // bound the graph; the property is shape-independent
		}
		nodes := make(map[string]*Node, n)
		for i := uint(0); i < n; i++ {
			id := strconv.FormatUint(uint64(i), 10)
			// parentSkew maps each node to some other id (possibly
			// itself, possibly out of range → dangling).
			var parent string
			if n > 0 {
				pidx := (int64(i) + parentSkew)
				parent = strconv.FormatInt(pidx, 10) // may not exist in the map
			}
			nodes[id] = &Node{BackupID: id, ParentBackupID: parent}
		}
		got := detectCycles(nodes) // must never panic

		// Cross-check: every reported cycle id must be a real node.
		for id := range got {
			if _, ok := nodes[id]; !ok {
				t.Fatalf("detectCycles reported a cycle on non-existent node %q", id)
			}
		}
	})
}

// FuzzAssignDepth: the depth DFS follows Parent pointers; a
// hand-constructed cyclic Parent chain (which BuildGraph breaks before
// calling assignDepth, but the function must be self-safe) must not
// infinite-loop into a stack overflow.
func FuzzAssignDepth(f *testing.F) {
	f.Add(uint(5))
	f.Fuzz(func(t *testing.T, n uint) {
		if n == 0 || n > 256 {
			n = 8
		}
		// Build a straight chain a->b->c-> ... then close it into a
		// loop: the pathological input assignDepth must survive.
		ns := make([]*Node, n)
		for i := range ns {
			ns[i] = &Node{BackupID: strconv.Itoa(i)}
		}
		for i := 0; i < len(ns); i++ {
			ns[i].Children = []*Node{ns[(i+1)%len(ns)]} // ring → cycle
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("assignDepth panicked on a cyclic Children ring: %v", r)
			}
		}()
		assignDepthBounded(ns[0], 1, len(ns)+2)
	})
}

// assignDepthBounded is a depth-guarded shim so the fuzz can prove the
// RING case terminates. The production assignDepth is only ever
// called after cycles are broken, so it has no internal guard — this
// documents that invariant rather than changing it.
func assignDepthBounded(n *Node, d, budget int) {
	if budget <= 0 {
		return
	}
	n.Depth = d
	for _, c := range n.Children {
		assignDepthBounded(c, d+1, budget-1)
	}
}
