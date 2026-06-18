//go:build fips

package fips

// enabled is true for the FIPS variant.  Compiled only when
// the binary is built with `-tags=fips` (see Makefile's
// build-fips target).  In production this build also sets
// `GOEXPERIMENT=boringcrypto` + `CGO_ENABLED=1` so every
// crypto call routes through BoringSSL; the build tag is the
// runtime-visible signal that those compile-time settings
// were applied.
const enabled = true
