# verify/

The sandbox-PG verifier: spin up an isolated Postgres against a candidate
restore and run `pg_verifybackup` + `pg_amcheck` before declaring a backup good.

## What lives here

The outer subsystem that wires `internal/restore` to a throwaway Postgres
instance, runs the official PG verification tools against it, and reports a
signed verdict back to the manifest store. The sandbox is pluggable — Docker
today, with Kubernetes Job and Firecracker microVM backends behind the same
interface.

## Key files / subdirs

- `sandbox/` — backend abstraction over the throwaway runtime
  - `sandbox.go` — `Backend` interface, lifecycle (`Provision`, `Start`,
    `Exec`, `Stop`, `Destroy`)
  - `backend_docker.go` — real backend; container per verification, ephemeral
    data volume
  - `backend_firecracker_real.go` / `backend_firecracker_stub.go` — microVM
    backend (build-tagged: `firecracker` for the real impl, stub by default)
  - `firecracker_common.go` — shared Firecracker plumbing (jailer config,
    vsock)
  - `sandbox_integration_test.go` — end-to-end against Docker
  - `testing_exports.go` — unexported handles for in-package tests

## Verification steps (per backup)

1. Restore the candidate into the sandbox data directory.
2. Boot Postgres in single-user / standalone mode.
3. Run `pg_verifybackup` against the manifest.
4. Run `pg_amcheck --all` for heap + B-tree corruption.
5. Emit an `Event` (`verify.passed` or `verify.failed`) and record the verdict
   in the manifest store.

## Read next

- `../backup/verifybackup/` — the inner caller
- `../restore/README.md` — supplies the candidate restore
- `../audit/` — verification verdicts are also journaled here

## Don't put X here

- Production restores — the sandbox is for verification only; destroy on exit.
- pg_amcheck / pg_verifybackup wrappers — invoke the official binaries via
  `Exec`, don't re-implement.
- k8s Job backend yet — planned; tracking issue gates the wire-up.
