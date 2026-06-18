//go:build !pkcs11
// +build !pkcs11

// Stub backend: compiled into the default `pg_hardstorage`
// binary (CGO_ENABLED=0).  Registers the scheme so the
// configuration is portable across builds, but every
// operation refuses with a structured error pointing at the
// rebuild instructions.
//
// Operators wanting HSM rebuild with -tags pkcs11 (which
// pulls in the github.com/miekg/pkcs11 cgo binding).  The
// `pg-hardstorage-fips` artifact ships with that tag set.

package pkcs11

import (
	"context"
	"errors"
)

// errNotBuilt is the structured refusal every stub method
// returns.  Tests assert errors.Is detection works against
// it so callers can distinguish "build flavour wrong" from
// "PIN wrong" / "module missing" / etc.
var errNotBuilt = errors.New(
	"pkcs11: this binary was built without -tags pkcs11; " +
		"rebuild with `go build -tags pkcs11 ./cmd/pg_hardstorage` " +
		"or use the official pg-hardstorage-fips artifact",
)

// ErrNotBuilt is the public sentinel for callers that want
// errors.Is detection.
func ErrNotBuilt() error { return errNotBuilt }

// stubClient implements Client and refuses every call with
// errNotBuilt.
type stubClient struct{}

// Encrypt implements Client; the stub build always returns errNotBuilt.
func (stubClient) Encrypt(context.Context, Mechanism, string, []byte, []byte) ([]byte, error) {
	return nil, errNotBuilt
}

// Decrypt implements Client; the stub build always returns errNotBuilt.
func (stubClient) Decrypt(context.Context, Mechanism, string, []byte, []byte) ([]byte, error) {
	return nil, errNotBuilt
}

// DestroyKey implements Client; the stub build always returns errNotBuilt.
func (stubClient) DestroyKey(context.Context, string) error { return errNotBuilt }

// DescribeKey implements Client; the stub build always returns errNotBuilt.
func (stubClient) DescribeKey(context.Context, string) (map[string]any, error) {
	return nil, errNotBuilt
}

// Close implements Client. There is nothing to release in the stub build.
func (stubClient) Close() error { return nil }

// newRealClient is the stub-build entry point — refuses to
// open at all so misconfigured deployments fail at startup
// with a clear remediation, not deep inside a wrap call.
func newRealClient(_ context.Context, _ realClientConfig) (Client, error) {
	return nil, errNotBuilt
}

// Built reports whether this binary was built with the
// `pkcs11` tag (i.e. has the cgo backend).  Tests + doctor
// surface this so operators can confirm their flavour.
func Built() bool { return false }
