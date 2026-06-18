package fips_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fips"
)

func TestVariant_DefaultBuild(t *testing.T) {
	// Standard test invocation runs without -tags=fips, so
	// Enabled() must report false.  The fips-build path is
	// covered by a CI matrix cell that sets the tag.
	if fips.Enabled() {
		t.Errorf("default build should report Enabled()=false; got true")
	}
	if fips.Variant() != "default" {
		t.Errorf("Variant() = %q, want \"default\"", fips.Variant())
	}
}
