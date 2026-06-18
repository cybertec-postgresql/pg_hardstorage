//go:build !firecracker
// +build !firecracker

// Stub Firecracker backend: compiled into the default
// `pg_hardstorage` binary.  Registers the "firecracker"
// backend name so configuration is portable across builds,
// but Verify refuses with a structured error pointing at the
// rebuild instructions.
//
// Operators wanting microVM isolation rebuild with
// `-tags firecracker`, which pulls in
// `github.com/firecracker-microvm/firecracker-go-sdk` and
// activates the real backend in
// backend_firecracker_real.go.

package sandbox

import (
	"context"
	"errors"
	"time"
)

func init() {
	register(firecrackerStubBackend{})
}

// firecrackerBuilt reports whether this binary was built with
// `-tags firecracker`.  The doctor surface reads this to tell
// operators which sandbox flavours are available.  In stub
// builds it's always false; the real backend file overrides
// it via FirecrackerBuilt().
func firecrackerBuilt() bool { return false }

// FirecrackerBuilt is the public predicate.
func FirecrackerBuilt() bool { return firecrackerBuilt() }

// errFirecrackerNotBuilt is the structured refusal stub
// builds return when an operator sets Backend == "firecracker"
// without rebuilding.  errors.Is detection works against
// it so callers can distinguish "wrong build flavour" from
// "kernel image missing" / "KVM denied".
var errFirecrackerNotBuilt = errors.New(
	"sandbox/firecracker: this binary was built without -tags firecracker; " +
		"rebuild with `go build -tags firecracker ./cmd/pg_hardstorage` or " +
		"keep Backend=\"docker\" (the default)",
)

// ErrFirecrackerNotBuilt is the public sentinel.
func ErrFirecrackerNotBuilt() error { return errFirecrackerNotBuilt }

type firecrackerStubBackend struct{}

// Name returns the registered backend identifier.
func (firecrackerStubBackend) Name() string { return BackendFirecracker }

// Verify always returns ErrFirecrackerNotBuilt along with a partial
// Result; built into the binary when the firecracker build tag is
// absent.
func (firecrackerStubBackend) Verify(_ context.Context, opts Options) (*Result, error) {
	res := &Result{
		Schema:    SchemaResult,
		Backend:   BackendFirecracker,
		PGMajor:   opts.PGMajor,
		Tool:      "pg_verifybackup",
		StartedAt: time.Now().UTC(),
	}
	res.StoppedAt = time.Now().UTC()
	res.Duration = res.StoppedAt.Sub(res.StartedAt)
	return res, errFirecrackerNotBuilt
}
