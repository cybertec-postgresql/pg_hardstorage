package recovery

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// This file uses Go's `export_test.go` convention: it is part of
// `package recovery` (not `recovery_test`) so it can read +
// modify unexported fields, but it is only compiled during
// `go test` of this package — not into the production binary.
//
// External tests (`recovery_test` package) call these helpers
// to inject stubs for the heavy dependencies (restore, sandbox)
// without making the production API surface broader than the
// shipped contract.

// DrillOptionsWithStubs returns a DrillOptions with the (test-
// only) restoreFn + verifyFn callbacks wired in.  Production
// callers can't reach these fields; tests use this helper to
// compose drill orchestration without paying for real chunk
// fetches or Docker.
func DrillOptionsWithStubs(
	base DrillOptions,
	restoreFn func(ctx context.Context, opts restore.Options) (*restore.Result, error),
	verifyFn func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error),
) DrillOptions {
	out := base
	out.restoreFn = restoreFn
	out.verifyFn = verifyFn
	return out
}

// DrillTargetCleanupForTest exposes the internal cleanup helper
// for direct invocation in tests.  Asserts the absolute-path
// safeguard.
func DrillTargetCleanupForTest(dir string) error {
	return drillTargetCleanup(dir)
}
