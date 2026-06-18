// Package mutation implements the testkit's mutation-testing
// harness.  Closes the SPEC commitment for "mutation testing.
// Build-tagged fault injection in our own code (e.g.,
// `//go:build mutation_chunker_off_by_one`) — re-run scenarios;
// assertions must catch the mutation.  If they don't, we have a
// coverage gap."
//
// Pattern: each target function is split into two implementations
// guarded by mutually-exclusive build tags:
//
//   - <name>.go               with `//go:build !mutation_<tag>`
//     contains the real implementation
//   - <name>_mutation_<tag>.go with `//go:build mutation_<tag>`
//     contains a deliberately-broken variant
//
// The harness runs `go test -tags=mutation_<tag>` against the
// affected packages and asserts the test suite FAILS — i.e. our
// existing tests catch the regression.  A mutation that doesn't
// trigger any failure is a coverage gap and surfaces as a hard
// failure in this harness's own run.
//
// To add a new mutation:
//  1. Pick a target function whose correctness matters.
//  2. Move it to its own file with `!mutation_<tag>` constraint.
//  3. Add a sibling `<name>_mutation_<tag>.go` with the mutated
//     version under `mutation_<tag>`.
//  4. Append a Mutation entry to Registry below naming the tag,
//     the package(s) where breakage is expected, and a brief
//     description.
//  5. Run `go test -tags=mutation_runner ./internal/testkit/mutation`
//     to confirm the mutation is caught.
package mutation

// Mutation describes one mutation-testing entry.  The harness
// loops over Registry and runs `go test -tags=<Tag>` against each
// package in Packages, expecting the test invocation to fail.
type Mutation struct {
	// Tag is the build tag the mutated source file is guarded by
	// (e.g. "mutation_chunkkey_no_suffix").  The harness passes
	// this through `go test -tags=...`.
	Tag string

	// Description is a one-line operator-readable note explaining
	// what the mutation breaks and which test class catches it.
	Description string

	// Packages is the set of import paths the harness runs `go
	// test` against under the mutation tag.  Each package's test
	// suite must fail for the mutation to be considered "caught".
	// Listing one package per mutation keeps wallclock reasonable;
	// the harness exits as soon as it sees a non-zero exit from
	// any of them.
	Packages []string
}

// Registry is the canonical list of mutations the harness runs.
// Adding an entry requires a corresponding pair of source files
// in the named package — see the package doc.
var Registry = []Mutation{
	{
		Tag: "mutation_chunkkey_no_suffix",
		Description: "repo.ChunkKey drops the .chk suffix; round-trip + " +
			"ParseChunkKey tests must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/repo",
		},
	},
	{
		Tag: "mutation_audit_hash_zeroed",
		Description: "audit.ComputeHash returns a constant zero hash; " +
			"chain-walking + Append-genesis tests must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/audit",
		},
	},
	{
		Tag: "mutation_threshold_off_by_one",
		Description: "threshold.quorumMet uses > instead of >=; " +
			"members==threshold no longer passes; QuorumMet test must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold",
		},
	},
	{
		Tag: "mutation_lsn_shape_loose",
		Description: "restore.LooksLikeLSN drops the 'exactly one slash' " +
			"check, re-introducing the silent multi-slash regression " +
			"(0//0 sneaks through).  Property tests in " +
			"recovery_property_test.go must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_target_reachable_off_by_one",
		Description: "restore.targetReachable drops the exclusive-stop " +
			"strictness and uses >= for both modes, re-introducing the " +
			"issue-#99 bug where `--to-lsn <stop> --to-exclusive` is " +
			"silently accepted (recovery would run to end-of-WAL).  The " +
			"exclusive-equality case in plan_reachability_test.go must " +
			"catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_identifier_no_length_cap",
		Description: "pg.ValidIdentifier drops the 1..63 byte length cap, " +
			"accepting arbitrarily-long (and empty) identifiers.  Property " +
			"tests in identifier_property_test.go must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/pg",
		},
	},
}
