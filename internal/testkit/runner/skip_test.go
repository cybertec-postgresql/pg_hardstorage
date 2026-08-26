//go:build integration

package runner_test

// skip_test.go — a scenario this build or host cannot run must SKIP,
// not fail.
//
// Four scenarios in the corpus can never pass here: three target the
// `kind` topology, which v0.1 does not ship, and one drives `partial
// dump`, which shells out to local PostgreSQL server binaries. Both
// used to be reported as failures, which is wrong in a way that
// compounds: a suite with a permanently-red baseline teaches everyone
// reading it that red is normal, and the next real regression lands in
// a result nobody looks at twice.
//
// A skip must not be mistaken for a pass either. Pass stays false on a
// skipped run, so nothing downstream can count "we could not check
// this" as "we checked this and it was fine".

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// A missing host tool skips before anything is brought up.
func TestRunner_SkipsOnMissingHostTool(t *testing.T) {
	sc := &scenario.Scenario{
		Schema:   scenario.SchemaScenario,
		Name:     "skip-host-tool",
		Tier:     "L1",
		Requires: scenario.Requires{HostTools: []string{"definitely-not-a-real-binary-xyzzy"}},
		Topology: scenario.Topology{Provider: "local-docker"},
	}
	res, err := runner.Run(context.Background(), sc, runner.RunOptions{Out: testWriter{t}})
	if err != nil {
		t.Fatalf("a skip must not be an error: %v", err)
	}
	if !res.Skipped {
		t.Fatal("scenario ran despite a missing required host tool")
	}
	if res.Pass {
		t.Error("Skipped must not imply Pass — a skip is the absence of evidence, " +
			"not evidence of correctness")
	}
	if !strings.Contains(res.SkipReason, "definitely-not-a-real-binary-xyzzy") {
		t.Errorf("skip reason does not name the missing tool: %q", res.SkipReason)
	}
}

// A topology this build does not ship skips rather than failing.
func TestRunner_SkipsOnUnimplementedTopology(t *testing.T) {
	sc := &scenario.Scenario{
		Schema:   scenario.SchemaScenario,
		Name:     "skip-kind",
		Tier:     "L5",
		Topology: scenario.Topology{Provider: "kind"},
	}
	res, err := runner.Run(context.Background(), sc, runner.RunOptions{Out: testWriter{t}})
	if err != nil {
		t.Fatalf("a skip must not be an error: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("scenario targeting the unimplemented %q provider was not skipped "+
			"(failure=%q)", sc.Topology.Provider, res.Failure)
	}
	if res.Pass {
		t.Error("Skipped must not imply Pass")
	}
	if !strings.Contains(res.SkipReason, "kind") {
		t.Errorf("skip reason does not name the provider: %q", res.SkipReason)
	}
}

// A tool that IS present must not skip — otherwise the mechanism would
// quietly disable scenarios on hosts that can run them.
func TestRunner_PresentHostToolDoesNotSkip(t *testing.T) {
	sc := &scenario.Scenario{
		Schema:   scenario.SchemaScenario,
		Name:     "no-skip",
		Tier:     "L1",
		Requires: scenario.Requires{HostTools: []string{"sh"}},
		Topology: scenario.Topology{Provider: "kind"}, // skips later, for a different reason
	}
	res, err := runner.Run(context.Background(), sc, runner.RunOptions{Out: testWriter{t}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// It DOES skip — on the topology — but must not have skipped on the
	// tool. Match the host-tool phrasing rather than the tool name: "sh"
	// is a substring of the topology reason's "ships", which is exactly
	// the kind of accidental match that makes an assertion pass or fail
	// for the wrong reason.
	if strings.Contains(res.SkipReason, "on PATH") {
		t.Errorf("skipped on a tool that is present: %q", res.SkipReason)
	}
	if !strings.Contains(res.SkipReason, "topology") {
		t.Errorf("expected the topology skip to be the one that fired, got %q", res.SkipReason)
	}
}
