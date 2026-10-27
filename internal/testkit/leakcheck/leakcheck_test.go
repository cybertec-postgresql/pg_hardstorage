package leakcheck

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The detector must actually detect: a leaked fd, goroutine and file-backed
// mapping each move the census, and releasing them moves it back.
func TestSnapshotSeesFDGoroutineAndMmap(t *testing.T) {
	base := Take()

	f, err := os.Create(filepath.Join(t.TempDir(), "pghs-manifest-x.canon"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 16); err != nil {
		t.Fatal(err)
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, 1<<16, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Skip("mmap unavailable")
	}
	stop := make(chan struct{})
	go func() { <-stop }()
	time.Sleep(20 * time.Millisecond)

	cur := Take()
	if cur.FDs <= base.FDs {
		t.Errorf("fd not seen: %d -> %d", base.FDs, cur.FDs)
	}
	if cur.Goroutines <= base.Goroutines {
		t.Errorf("goroutine not seen: %d -> %d", base.Goroutines, cur.Goroutines)
	}
	if cur.MapsFiles <= base.MapsFiles {
		t.Errorf("mmap not seen: %d -> %d", base.MapsFiles, cur.MapsFiles)
	}

	_ = syscall.Munmap(b)
	_ = f.Close()
	close(stop)
	AssertNoLeak(t, base, Tolerance{})
}
