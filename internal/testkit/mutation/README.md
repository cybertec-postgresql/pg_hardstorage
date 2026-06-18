# mutation

The testkit's mutation-testing harness — deliberately-broken variants of
production code, guarded by build tags, that the scenario suite is required to
catch.

## What lives here

Each target function is split into two implementations under mutually-exclusive
build tags:

- `<name>.go` (`//go:build !mutation_<tag>`) — the real implementation
- `<name>_mutation_<tag>.go` (`//go:build mutation_<tag>`) — a
  deliberately-broken variant

The harness runs `go test -tags=mutation_<tag>` against the affected packages
and asserts the test suite **fails** — i.e. our existing tests catch the
regression. A mutation that triggers no failure is a coverage gap and surfaces
as a hard error in this harness's own run. Closes the SPEC commitment "mutation
testing: if scenarios don't catch the mutation, we have a coverage gap".

To add a new mutation:

1. Pick a target function whose correctness matters.
2. Move it to its own file with `!mutation_<tag>` constraint.
3. Add a sibling `<name>_mutation_<tag>.go` under `mutation_<tag>` with the
   broken variant.
4. Append a `Mutation` entry to `Registry` naming the tag, the affected
   packages, and a brief description.
5. Run `go test -tags=mutation_runner ./internal/testkit/mutation` — the suite
   must fail under the mutation.

## Key files / subdirs

- `mutation.go` — registry of mutations (tag, target packages, description) +
  driver that runs `go test` per tag
- `runner_test.go` — meta-test that verifies the harness itself

## Read next

- `internal/audit/computehash_mutation_audit_hash_zeroed.go` — example:
  audit-hash zeroing mutation
- `internal/repo/chunkkey_mutation_chunkkey_no_suffix.go` — example:
  chunkkey-suffix mutation
- `internal/threshold/quorum_mutation_threshold_off_by_one.go` — example:
  threshold off-by-one mutation

## Don't put X here

No real mutations themselves — those live next to the function they break
(e.g. `internal/backup/chunker_*_mutation_*.go`). This package is just the
harness that runs them.
