//go:build fips

package fips

import "crypto/fips140"

// enabled reports FIPS mode for the `-tags=fips` variant — but only
// when a validated module is actually doing the crypto.
//
// The build tag alone used to be the answer. `make build-fips` also
// sets GOEXPERIMENT=boringcrypto + CGO_ENABLED=1, but nothing checked
// that it had: a plain `go build -tags fips` produced a binary that
// claimed FIPS in doctor, passed --fips-strict and stamped `fips:true`
// on every backup, while all crypto ran through the ordinary Go
// implementation. Now the claim requires BoringCrypto to be linked in
// and active, or the Go FIPS 140-3 module to be enabled
// (GODEBUG=fips140=on).
func enabled() bool { return fips140.Enabled() || boringEnabled() }
