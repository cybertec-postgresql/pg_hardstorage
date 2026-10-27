package cli

import (
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// The result keeps an exact count and a capped list, while every mismatch
// stays reachable (ascending) for --heal through the spilled set.
func TestScrubAgg_MismatchListIsCappedCountIsExactHealSeesAll(t *testing.T) {
	var agg scrubResultAgg
	defer agg.Close()
	n := maxScrubMismatchesListed + 250
	for i := 0; i < n; i++ {
		agg.noteMismatch(repo.HashOf([]byte(fmt.Sprintf("m%d", i))))
	}
	if agg.MismatchCount != n || len(agg.Mismatches) != maxScrubMismatchesListed {
		t.Fatalf("count=%d listed=%d, want %d/%d", agg.MismatchCount, len(agg.Mismatches), n, maxScrubMismatchesListed)
	}
	got := 0
	var prev repo.Hash
	if err := agg.eachMismatch(func(h repo.Hash) error {
		if got > 0 && h.Compare(prev) <= 0 {
			t.Fatalf("not ascending at %d", got)
		}
		prev = h
		got++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("heal stream saw %d of %d mismatches", got, n)
	}
}

func TestHashListForMsg_ReportsTotalBeyondSample(t *testing.T) {
	hs := []repo.Hash{repo.HashOf([]byte("a")), repo.HashOf([]byte("b"))}
	if got := hashListForMsg(hs, 2); got != hs[0].String()+", "+hs[1].String() {
		t.Errorf("got %q", got)
	}
	if got := hashListForMsg(hs, 1000); got[len(got)-len("(+998 more)"):] != "(+998 more)" {
		t.Errorf("got %q", got)
	}
}
