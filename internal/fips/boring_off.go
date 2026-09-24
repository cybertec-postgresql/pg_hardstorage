//go:build fips && !goexperiment.boringcrypto

package fips

// boringEnabled is false: this fips-tagged build was made without
// GOEXPERIMENT=boringcrypto, so BoringCrypto is not linked in.
func boringEnabled() bool { return false }
