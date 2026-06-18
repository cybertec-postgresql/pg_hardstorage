//go:build !fips

package fips

// enabled is false for non-FIPS builds.  Compiled into every
// build that doesn't set `-tags=fips`.
const enabled = false
