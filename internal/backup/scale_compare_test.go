package backup_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// rssAnonSampler tracks the peak anonymous resident set (heap, stacks,
// anonymous mappings) of this process. File-backed pages (the mmap'd
// canonical manifest a signature check reads, page cache) are excluded:
// they are clean, reclaimable and not what a memory-limited container
// OOMs on.
type rssAnonSampler struct {
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

func readRssAnon() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "RssAnon:") {
			f := strings.Fields(ln)
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func startRssSampler(t *testing.T) *rssAnonSampler {
	t.Helper()
	if readRssAnon() == 0 {
		t.Skip("no RssAnon in /proc/self/status")
	}
	s := &rssAnonSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			if v := readRssAnon(); v > s.peak.Load() {
				s.peak.Store(v)
			}
			select {
			case <-s.stop:
				return
			case <-tk.C:
			}
		}
	}()
	return s
}

func (s *rssAnonSampler) Stop() uint64 {
	close(s.stop)
	<-s.done
	return s.peak.Load()
}

// TestScale_CompareManyFiles proves `backup compare` holds no per-file
// state: two manifests of PGHS_SCALE_FILES files (default 2M, one chunk
// each, 5% changed between them) are compared, and the peak anonymous RSS
// during the compare stays under a fixed bound that does not depend on the
// file count. Before the external-sort join, the two per-side path maps cost
// ~200 bytes per file. Opt-in: PGHS_SCALE=1.
func TestScale_CompareManyFiles(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the compare scale proof")
	}
	nf := 2_000_000
	if v := os.Getenv("PGHS_SCALE_FILES"); v != "" {
		nf, _ = strconv.Atoi(v)
	}
	debug.SetMemoryLimit(512 << 20)
	defer debug.SetMemoryLimit(-1)

	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store, _ := storeAt(t)
	ctx := context.Background()

	commit := func(id string, changed bool) {
		spool, err := backup.NewManifestSpool(t.TempDir(), 250_000)
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
		for i := 0; i < nf; i++ {
			h := repo.HashOf([]byte(fmt.Sprintf("c%d", i%5000)))
			if changed && i%20 == 0 {
				h = repo.HashOf([]byte(fmt.Sprintf("changed%d", i%5000)))
			}
			fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/t%07d/file-%d", i/100, i), Mode: 0o600, Size: 100,
				Chunks: []backup.ChunkRef{{Hash: h, Offset: 0, Len: 100}}}
			if err := spool.Add(&fe); err != nil {
				t.Fatal(err)
			}
		}
		hd := validManifest()
		hd.Files = nil
		hd.BackupID = id
		if err := store.CommitSpooled(ctx, hd, spool, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
			t.Fatal(err)
		}
	}
	const dep = "db1"
	idA, idB := "db1.full.20260506T120000Z.0001", "db1.full.20260506T130000Z.0002"
	commit(idA, false)
	commit(idB, true)
	a, err := store.ReadStreaming(ctx, dep, idA, verifier)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.ReadStreaming(ctx, dep, idB, verifier)
	if err != nil {
		t.Fatal(err)
	}
	debug.FreeOSMemory()
	base := readRssAnon()
	sm := startRssSampler(t)
	res, err := backup.CompareContext(ctx, a, b, backup.CompareOptions{})
	peak := sm.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if res.FileCounts.InBoth != nf || res.FileCounts.Changed != nf/20 {
		t.Fatalf("compare: in_both=%d changed=%d, want %d / %d", res.FileCounts.InBoth, res.FileCounts.Changed, nf, nf/20)
	}
	t.Logf("compare of 2 x %d files: anon RSS %d MiB before, peak %d MiB during", nf, base>>20, peak>>20)
	if limit := base + 192<<20; peak > limit {
		t.Errorf("compare raised anonymous RSS to %d MiB (baseline %d MiB, bound %d MiB) over %d files", peak>>20, base>>20, limit>>20, nf)
	}
}
