# bisect

Scenario-aware `git bisect` driver — given a known-bad and known-good commit
and a scenario, walks the commit range and reports the regression-introducing
commit.

## What lives here

`pg_hardstorage_testkit scenario bisect --bad HEAD --good v0.1.0 --scenario
X.scenario.yaml` lists the range via `git log --pretty=%H bad..good^`,
binary-searches it, and at each midpoint rebuilds the binary and re-runs the
scenario harness.

We deliberately do not shell out to `git bisect run`: that command interleaves
the bisect-controller's stdout with the harness's, and we want clean structured
output for dashboards. Instead the driver:

1. Lists the commit range.
2. Binary-searches it.
3. Calls a pluggable `Runner` func at each midpoint.
4. Returns the first commit whose `Runner` exit is non-zero.

The pluggable `Runner` is the key affordance for unit tests: tests pass an
in-memory func that maps commit hashes to pass/fail, so the bisect logic itself
can be tested without spawning subprocesses or touching git.

## Key files / subdirs

- `bisect.go` — commit-range walker, binary-search driver, pluggable `Runner`
- `bisect_test.go` — unit tests using an in-memory `Runner`

## Read next

- `../runner/README.md` — the harness real runs invoke at each bisect step
- `cmd/pg_hardstorage_testkit/` — the `scenario bisect` subcommand wiring

## Don't put X here

No scenario semantics or assertion knowledge — this package only knows "the
harness passed or failed at commit X". No git-plumbing beyond `git log` and `git
checkout`; if a scenario needs a custom build, the caller's `Runner` invokes it.
