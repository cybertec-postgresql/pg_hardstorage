package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// A spilled adoption set that can no longer write must fail the chunk that
// was being adopted, so a backup on a full spill volume stops at once
// instead of after the rest of the data has been read.
func TestCAS_MarkAdoptedFailsFastWhenSpillBreaks(t *testing.T) {
	sp, _ := fenceWorld(t)
	dir := t.TempDir()
	c := NewCAS(sp, WithSpilledAdoption(spill.Options{MemEntries: 4, Dir: dir}))
	defer c.CloseAdopted()
	for i := 0; i < 4; i++ { // fills the heap budget: first run written, spill dir exists
		if err := c.markAdopted(HashOf([]byte(fmt.Sprintf("ok%d", i)))); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() {
			p := filepath.Join(dir, e.Name())
			if err := os.Chmod(p, 0o500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(p, 0o700)
			if f, err := os.Create(filepath.Join(p, "probe")); err == nil {
				f.Close()
				t.Skip("directory permissions are not enforced (running as root?)")
			}
		}
	}
	var failed error
	for i := 0; i < 20 && failed == nil; i++ {
		failed = c.markAdopted(HashOf([]byte(fmt.Sprintf("late%d", i))))
	}
	if failed == nil {
		t.Fatal("markAdopted kept succeeding although the spill set cannot write")
	}
}
