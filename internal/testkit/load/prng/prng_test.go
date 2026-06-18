package prng_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load/prng"
)

func TestNew_Deterministic(t *testing.T) {
	a := prng.New(0xC0FFEE42)
	b := prng.New(0xC0FFEE42)
	for i := 0; i < 100; i++ {
		if av, bv := a.Uint64(), b.Uint64(); av != bv {
			t.Errorf("iteration %d: %d != %d", i, av, bv)
			return
		}
	}
}

func TestNew_DifferentSeedsDiffer(t *testing.T) {
	a := prng.New(0xC0FFEE42)
	b := prng.New(0xC0FFEE43)
	same := 0
	for i := 0; i < 100; i++ {
		if a.Uint64() == b.Uint64() {
			same++
		}
	}
	// In 100 random uint64 draws, we'd expect approximately zero
	// collisions; >5 indicates the seeds aren't actually decorrelating.
	if same > 5 {
		t.Errorf("seeds 0xC0FFEE42 and 0xC0FFEE43 produced %d/100 identical draws", same)
	}
}

func TestDerive_SameLabelSameStream(t *testing.T) {
	a := prng.Derive(42, "users-table")
	b := prng.Derive(42, "users-table")
	for i := 0; i < 100; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatal("same parent + label should produce identical streams")
		}
	}
}

func TestDerive_DifferentLabelsDiffer(t *testing.T) {
	a := prng.Derive(42, "users-table")
	b := prng.Derive(42, "orders-table")
	same := 0
	for i := 0; i < 100; i++ {
		if a.Uint64() == b.Uint64() {
			same++
		}
	}
	if same > 5 {
		t.Errorf("different labels under same parent produced %d/100 identical draws", same)
	}
}
