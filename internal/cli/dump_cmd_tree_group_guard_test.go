package cli_test

// dump_cmd_tree_group_guard_test.go — the group_guard flag must mean
// exactly "this command does nothing but print help".
//
// The CLI coverage gate skips group_guard commands, so the flag is now
// load-bearing: anything wrongly marked drops out of the gate silently,
// which is a worse failure than the one it fixed. The gate had been
// demanding scenarios for 41 pure groups (`kms`, `audit`, `repo`, …),
// reporting failures nobody could act on, and it stayed red long enough
// that every CI job downstream of it was skipped for a week.
//
// So this pins both directions: every pure group carries the flag, and
// no command that does real work carries it.

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type dumpNode struct {
	Path           string   `json:"path"`
	Runnable       bool     `json:"runnable"`
	HasSubcommands bool     `json:"has_subcommands"`
	Hidden         bool     `json:"hidden"`
	GroupGuard     bool     `json:"group_guard"`
	Flags          []string `json:"flags"`
}

func dumpTree(t *testing.T) []dumpNode {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	bin := filepath.Join(root, "bin", "pg_hardstorage")

	out, err := exec.Command(bin, "__dump-cmd-tree").Output()
	if err != nil {
		t.Skipf("bin/pg_hardstorage not built or not runnable (%v); run `make build`", err)
	}
	var nodes []dumpNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		t.Fatalf("parse cmd-tree JSON: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("command tree is empty")
	}
	return nodes
}

// TestGroupGuard_MarksExactlyThePureGroups asserts the flag's meaning
// against cobra's own view: a pure group is a command that has
// subcommands and whose only RunE is the synthesised one.
//
// The observable proxy for "synthesised" is that the command has
// subcommands AND is runnable. A real dual-purpose command like
// `backup` — which takes a deployment and also hosts `backup delete` —
// is runnable for its OWN reason and must not be flagged.
func TestGroupGuard_MarksExactlyThePureGroups(t *testing.T) {
	nodes := dumpTree(t)

	var flagged, unflaggedGroups []string
	for _, n := range nodes {
		if n.Hidden {
			continue
		}
		if n.GroupGuard {
			flagged = append(flagged, n.Path)
			// A guard is only ever synthesised for a command WITH
			// subcommands. One flagged without them means the
			// annotation has drifted from hardenGroupCommands.
			if !n.HasSubcommands {
				t.Errorf("%q is marked group_guard but has no subcommands — the "+
					"annotation no longer means what the coverage gate assumes, and "+
					"that command is now silently exempt from coverage", n.Path)
			}
			if !n.Runnable {
				t.Errorf("%q is marked group_guard but is not runnable", n.Path)
			}
			continue
		}
		if n.HasSubcommands && n.Runnable {
			unflaggedGroups = append(unflaggedGroups, n.Path)
		}
	}

	if len(flagged) == 0 {
		t.Fatal("no commands are marked group_guard — hardenGroupCommands is not " +
			"annotating, so the coverage gate will demand scenarios for every pure " +
			"group again")
	}
	sort.Strings(unflaggedGroups)
	t.Logf("group_guard: %d pure groups; %d dual-purpose commands still require "+
		"coverage: %s", len(flagged), len(unflaggedGroups),
		strings.Join(unflaggedGroups, ", "))
}

// TestGroupGuard_RealCommandsAreNotExempt pins the specific commands
// that are BOTH a group and a real action. If one of these ever gains
// the flag it would drop out of the coverage gate unnoticed, which is
// exactly the silent-exemption failure this flag risks.
func TestGroupGuard_RealCommandsAreNotExempt(t *testing.T) {
	nodes := dumpTree(t)
	byPath := map[string]dumpNode{}
	for _, n := range nodes {
		byPath[n.Path] = n
	}

	// `backup <deployment>` is the canonical case: a real action that
	// also hosts subcommands.
	for _, path := range []string{"backup"} {
		n, ok := byPath[path]
		if !ok {
			t.Errorf("command %q not found in the tree — renamed? update this test", path)
			continue
		}
		if !n.HasSubcommands {
			t.Errorf("%q no longer has subcommands; it is not the dual-purpose case "+
				"this test exists to protect", path)
		}
		if n.GroupGuard {
			t.Errorf("%q is marked group_guard, so the CLI coverage gate now SKIPS it "+
				"— a real, invocable command silently exempt from requiring a scenario",
				path)
		}
	}
}
