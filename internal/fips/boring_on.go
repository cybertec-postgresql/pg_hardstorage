//go:build fips && goexperiment.boringcrypto

package fips

import "crypto/boring"

// boringEnabled reports whether BoringCrypto is handling crypto
// operations (built with GOEXPERIMENT=boringcrypto and cgo).
func boringEnabled() bool { return boring.Enabled() }
