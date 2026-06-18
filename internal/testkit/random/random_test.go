package random_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/random"
)

func TestPick_Deterministic(t *testing.T) {
	c, _ := catalog.Default()
	a, err := random.Pick(c, random.Options{Count: 5, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	b, err := random.Pick(c, random.Options{Count: 5, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Entries) != len(b.Entries) {
		t.Fatalf("length differs: %d vs %d", len(a.Entries), len(b.Entries))
	}
	for i := range a.Entries {
		if a.Entries[i] != b.Entries[i] {
			t.Errorf("entry %d differs:\n  a=%+v\n  b=%+v", i, a.Entries[i], b.Entries[i])
		}
	}
}

func TestPick_DifferentSeedsDiffer(t *testing.T) {
	c, _ := catalog.Default()
	a, _ := random.Pick(c, random.Options{Count: 5, Seed: 1})
	b, _ := random.Pick(c, random.Options{Count: 5, Seed: 2})
	same := 0
	for i := 0; i < len(a.Entries) && i < len(b.Entries); i++ {
		if a.Entries[i] == b.Entries[i] {
			same++
		}
	}
	if same == len(a.Entries) {
		t.Errorf("different seeds produced identical fleets — randomness broken")
	}
}

func TestPick_DiversityBias(t *testing.T) {
	c, _ := catalog.Default()
	f, err := random.Pick(c, random.Options{Count: 6, Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	// At 6 cells the picker should hit ≥ 2 distinct OS
	// families and ≥ 2 distinct PG majors.  The catalog has
	// 3 families and 3-4 PG majors — anything less means
	// the bias isn't working.
	families := map[string]bool{}
	pgs := map[string]bool{}
	for _, e := range f.Entries {
		os_, _ := c.FindOS(e.OS)
		families[os_.Family] = true
		pgs[e.PG] = true
	}
	if len(families) < 2 {
		t.Errorf("expected ≥ 2 OS families in 6-cell fleet; got %v", families)
	}
	if len(pgs) < 2 {
		t.Errorf("expected ≥ 2 PG majors in 6-cell fleet; got %v", pgs)
	}
}

// TestPick_PreferPatroni_CurrentlyIgnored locks in the
// no-Patroni-in-soak-fleet behaviour.  The soak runner's
// compose path doesn't yet emit etcd / install Patroni / wire
// configs for role:patroni-cluster entries; producing them
// gave operators compose-up failures (run-20260506-102722).
//
// Real Patroni testing lives in the patroni-local-docker
// topology consumed by L4 scenarios; that path is fully
// supported.  When the soak runner grows equivalent
// support, this test flips to assert at-least-one-Patroni.
func TestPick_PreferPatroni_CurrentlyIgnored(t *testing.T) {
	c, _ := catalog.Default()
	f, err := random.Pick(c, random.Options{Count: 6, Seed: 99, PreferPatroni: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Entries {
		if e.Role == "patroni-cluster" {
			t.Errorf("soak fleet must not produce role:patroni-cluster (compose path unsupported); got %+v", e)
		}
	}
}

func TestPick_PatroniSuppressedWhenSmall(t *testing.T) {
	c, _ := catalog.Default()
	f, err := random.Pick(c, random.Options{Count: 3, Seed: 99, PreferPatroni: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Entries {
		if e.Role == "patroni-cluster" {
			t.Errorf("3-cell fleet shouldn't get a Patroni cluster: %+v", e)
		}
	}
}

func TestPick_PreferArm64(t *testing.T) {
	c, _ := catalog.Default()
	f, err := random.Pick(c, random.Options{Count: 4, Seed: 7, PreferArm64: true})
	if err != nil {
		t.Fatal(err)
	}
	hasArm := false
	for _, e := range f.Entries {
		if e.Arch == "arm64" {
			hasArm = true
		}
	}
	if !hasArm {
		t.Errorf("expected at least one arm64 cell with PreferArm64; got %v", f.Entries)
	}
}

func TestPick_FilesystemPool(t *testing.T) {
	c, _ := catalog.Default()
	f, err := random.Pick(c, random.Options{
		Count: 8, Seed: 11,
		FilesystemPool: []string{"ext4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Entries {
		if e.Filesystem != "ext4" {
			t.Errorf("FilesystemPool=[ext4] should pin all cells to ext4; got %v", e)
		}
	}
}

func TestPick_ValidatesAgainstCatalog(t *testing.T) {
	c, _ := catalog.Default()
	for seed := int64(0); seed < 10; seed++ {
		f, err := random.Pick(c, random.Options{Count: 8, Seed: seed})
		if err != nil {
			t.Fatalf("seed=%d: %v", seed, err)
		}
		if err := f.Validate(c); err != nil {
			t.Errorf("seed=%d produced invalid fleet: %v", seed, err)
		}
	}
}

func TestPick_RejectsZeroCount(t *testing.T) {
	c, _ := catalog.Default()
	if _, err := random.Pick(c, random.Options{Count: 0, Seed: 1}); err == nil {
		t.Errorf("expected error for count=0")
	}
}

func TestPick_UniqueNames(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := random.Pick(c, random.Options{Count: 12, Seed: 5})
	seen := map[string]bool{}
	for _, e := range f.Entries {
		if seen[e.Name] {
			t.Errorf("duplicate name %q", e.Name)
		}
		seen[e.Name] = true
	}
}
