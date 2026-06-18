// Package fips reports the binary's FIPS-mode posture.
//
// pg_hardstorage ships in two flavours:
//
//   - default: pure-Go crypto via the standard library's
//     `crypto/...` packages.  Runs anywhere; no FIPS claim.
//   - fips:    built with `make build-fips` (GOEXPERIMENT=
//     boringcrypto, CGO_ENABLED=1, build tag `fips`).  Every
//     crypto operation routes through Google's BoringSSL —
//     a FIPS 140-2 validated module.  Runs on linux/amd64
//     only.
//
// The runtime needs to know which variant it is so:
//
//   - `pg_hardstorage doctor` can print "FIPS: yes/no" and
//     warn if the configured KMS provider isn't FIPS-validated.
//   - `--fips-strict` mode (in+) can refuse to start
//     when Enabled() returns false.
//   - The audit log can stamp every backup with a `fips:true`
//     attribute that compliance auditors rely on.
//
// The actual mechanism lives in the per-build-tag files:
// `enabled_fips.go` (compiled with -tags=fips) and
// `enabled_default.go` (every other build).  This file holds
// only the public Enabled() symbol so callers don't have to
// know about build tags.
package fips

// Enabled reports whether the binary is the FIPS variant.
// The implementation is build-tag-gated (`enabled_fips.go`
// vs `enabled_default.go`).
func Enabled() bool { return enabled }

// Variant returns the operator-readable build flavour.  Same
// information as Enabled, in a form suitable for the JSON
// schema's `variant` field and audit-event Body.
func Variant() string {
	if enabled {
		return "fips"
	}
	return "default"
}
