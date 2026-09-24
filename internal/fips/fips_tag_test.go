//go:build fips && !goexperiment.boringcrypto

package fips_test

import (
	"crypto/fips140"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fips"
)

// TestEnabled_TagAloneIsNotFIPS pins the LOW finding: Enabled()
// trusted the `fips` build tag alone, so `go build -tags fips` WITHOUT
// the BoringCrypto experiment (or the Go FIPS 140-3 module switched
// on) claimed FIPS — in doctor, in --fips-strict, and in the
// `fips:true` stamp auditors rely on — while every crypto call ran
// through the ordinary, unvalidated implementation.
//
// Run with `go test -tags fips ./internal/fips/`.
func TestEnabled_TagAloneIsNotFIPS(t *testing.T) {
	if got, want := fips.Enabled(), fips140.Enabled(); got != want {
		t.Errorf("Enabled() = %v under -tags fips without BoringCrypto; the validated module is active: %v", got, want)
	}
}
