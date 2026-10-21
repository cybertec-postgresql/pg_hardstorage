package repo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

func TestCAS_SpilledAdoptionBehavesLikeTheMap(t *testing.T) {
	sp, _ := fenceWorld(t)
	c := NewCAS(sp, WithSpilledAdoption(spill.Options{MemEntries: 4, Dir: t.TempDir()}))
	defer c.CloseAdopted()
	var hs []Hash
	for i := 0; i < 50; i++ {
		h := HashOf([]byte(fmt.Sprintf("a%d", i)))
		hs = append(hs, h)
		c.markAdopted(h)
	}
	if n := c.AdoptedLen(); n != 50 {
		t.Fatalf("AdoptedLen = %d", n)
	}
	for _, h := range hs {
		if !c.WasAdopted(h) {
			t.Fatal("adopted hash not reported")
		}
	}
	c.ForgetAdopted(hs[:5]...)
	c.ForgetChunk(hs[5])
	if c.WasAdopted(hs[0]) || c.WasAdopted(hs[5]) || !c.WasAdopted(hs[6]) {
		t.Fatal("forget not honoured")
	}
	if n := c.AdoptedLen(); n != 44 {
		t.Fatalf("AdoptedLen after forgets = %d, want 44", n)
	}
	if got := len(c.AdoptedHashes()); got != 44 {
		t.Fatalf("AdoptedHashes = %d", got)
	}
	// Re-adopting a forgotten hash brings it back.
	c.markAdopted(hs[0])
	if !c.WasAdopted(hs[0]) {
		t.Fatal("re-adoption lost")
	}
	if err := c.AdoptedErr(); err != nil {
		t.Fatal(err)
	}
}

// A fence over a large adoption set writes its pin in parts and gc reads
// every part.
func TestFence_PinWrittenInPartsAndReadBackByGC(t *testing.T) {
	defer func(n int) { pinPartHashes = n }(pinPartHashes)
	pinPartHashes = 100
	sp, _ := fenceWorld(t)
	var hs []Hash
	for i := 0; i < 450; i++ {
		hs = append(hs, HashOf([]byte(fmt.Sprintf("p%d", i))))
	}
	f, err := BeginCommitFenceSource(context.Background(), sp, sliceSource(hs), FenceOptions{Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.pinAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts := 0
	for info, err := range sp.List(context.Background(), gcPinsPrefix) {
		if err != nil {
			t.Fatal(err)
		}
		_ = info
		parts++
	}
	if parts != 5 {
		t.Fatalf("pin parts = %d, want 5 (450 hashes / 100)", parts)
	}
	ps := newGCPinSet()
	defer ps.close()
	if err := ps.refresh(context.Background(), sp, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if !ps.has(h) {
			t.Fatalf("gc does not see pinned hash %s", h)
		}
	}
	if ps.has(HashOf([]byte("not pinned"))) {
		t.Fatal("unpinned hash reported pinned")
	}
	if len(f.Adopted()) != 450 {
		t.Fatalf("Adopted() = %d", len(f.Adopted()))
	}
}
