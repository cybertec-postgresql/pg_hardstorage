package cli_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// vmHWM reads the process's peak resident set size from /proc (Linux).
func vmHWM(t testing.TB) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmHWM:") {
			f := strings.Fields(ln)
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	t.Skip("no VmHWM")
	return 0
}

// scaleCLIFixture commits, through the spool, the manifests the scale run
// measures: a full backup of nChunks chunk references with an incremental
// child (1/3 the size, half overlapping the parent), and a "many files"
// deployment of nFiles one-chunk files backed by real chunk objects, with a
// second backup that changed 5% of the files. It returns the IDs.
type scaleIDs struct{ bigFull, bigIncr, manyA, manyB string }

func scaleCLIFixture(t *testing.T, w *readWorld, nChunks, nFiles int) scaleIDs {
	t.Helper()
	ctx := context.Background()
	cas := casdefault.New(w.sp)
	chunkHash := func(i int) repo.Hash {
		var b [9]byte
		b[0] = 'c'
		for k := 0; k < 8; k++ {
			b[1+k] = byte(i >> (8 * k))
		}
		return repo.HashOf(b[:])
	}
	now := time.Now().UTC().Truncate(time.Second)
	hdr := func(dep, kind string, seq int, parent string, ago time.Duration) *backup.Manifest {
		t0 := now.Add(-ago)
		id := fmt.Sprintf("%s.%s.%s.%04d", dep, kind, t0.Format("20060102T150405Z"), seq)
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: dep, Tenant: "default",
			Type: backup.BackupTypeFull, PGVersion: 180000, SystemIdentifier: "7000000000000000001",
			StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: t0, StoppedAt: t0.Add(time.Minute),
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		}
		if parent != "" {
			m.Type, m.ParentBackupID = backup.BackupTypeIncremental, parent
			m.PGBackupManifest = []byte(`{"PostgreSQL-Backup-Manifest-Version":1}`)
		}
		return m
	}
	commit := func(h *backup.Manifest, gen func(emit func(backup.FileEntry))) {
		t.Helper()
		spool, err := backup.NewManifestSpool(t.TempDir(), 250_000)
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
		gen(func(fe backup.FileEntry) {
			if err := spool.Add(&fe); err != nil {
				t.Fatal(err)
			}
		})
		if err := w.store.CommitSpooled(ctx, h, spool, w.signer, backup.CommitOptions{SkipReplica: true}); err != nil {
			t.Fatalf("CommitSpooled %s: %v", h.BackupID, err)
		}
	}
	chunkRun := func(lo, hi int) func(func(backup.FileEntry)) {
		return func(emit func(backup.FileEntry)) {
			for f, i := 0, lo; i < hi; f++ {
				c := min(16384, hi-i)
				fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/%d", 100000+lo/16384+f), Mode: 0o600, Chunks: make([]backup.ChunkRef, c)}
				var off int64
				for k := range fe.Chunks {
					fe.Chunks[k] = backup.ChunkRef{Hash: chunkHash(i + k), Offset: off, Len: 65536}
					off += 65536
				}
				fe.Size = off
				i += c
				emit(fe)
			}
		}
	}
	var ids scaleIDs
	h := hdr("big", "full", 1, "", 48*time.Hour)
	ids.bigFull = h.BackupID
	commit(h, chunkRun(0, nChunks))
	h = hdr("big", "incr", 2, ids.bigFull, 24*time.Hour)
	ids.bigIncr = h.BackupID
	commit(h, chunkRun(nChunks-nChunks/6, nChunks+nChunks/6))
	w.plantArchivedWAL(t, "big", 1)

	bodies := make([]repo.ChunkInfo, 1000)
	for i := range bodies {
		ci, err := cas.PutChunk(ctx, []byte(fmt.Sprintf("real chunk body %d %0100d", i, i)))
		if err != nil {
			t.Fatal(err)
		}
		bodies[i] = ci
	}
	manyRun := func(changed bool) func(func(backup.FileEntry)) {
		return func(emit func(backup.FileEntry)) {
			for i := 0; i < nFiles; i++ {
				ci := bodies[i%len(bodies)]
				if changed && i%20 == 0 {
					ci = bodies[(i+1)%len(bodies)]
				}
				emit(backup.FileEntry{
					Path: fmt.Sprintf("base/16384/t%07d/f%d", i/100, i), Mode: 0o600, Size: int64(ci.Size),
					Chunks: []backup.ChunkRef{{Hash: ci.Hash, Offset: 0, Len: int64(ci.Size)}},
				})
			}
		}
	}
	h = hdr("many", "full", 1, "", 3*time.Hour)
	ids.manyA = h.BackupID
	commit(h, manyRun(false))
	h = hdr("many", "full", 2, "", time.Hour)
	ids.manyB = h.BackupID
	commit(h, manyRun(true))
	w.plantArchivedWAL(t, "many", 1)
	return ids
}

// scaleCLIPass runs every manifest-touching command that can run against
// the fixture, through the real CLI, and returns the process's peak RSS.
func scaleCLIPass(t *testing.T, w *readWorld, ids scaleIDs) uint64 {
	t.Helper()
	R := w.repoURL
	cmds := [][]string{
		{"list", "big", "--repo", R, "-o", "json"},
		{"list", "many", "--repo", R, "-o", "json"},
		{"show", "big", ids.bigFull, "--repo", R, "-o", "json"},
		{"manifest", "show", "many", ids.manyA, "--repo", R, "-o", "json"},
		{"status", "big", "--repo", R, "-o", "json"},
		{"verify", "big", ids.bigFull, "--repo", R, "-o", "json"},
		{"verify", "many", ids.manyA, "--full", "--repo", R, "-o", "json"},
		{"backup", "compare", "big", ids.bigFull, ids.bigIncr, "--repo", R, "-o", "json"},
		{"backup", "compare", "many", ids.manyA, ids.manyB, "--repo", R, "-o", "json"},
		{"backup", "graph", "big", "--repo", R, "-o", "json"},
		{"repo", "check", R, "-o", "json"},
		{"repo", "audit", R, "-o", "json"},
		{"repo", "usage", R, "-o", "json"},
		{"repo", "gc", R, "-o", "json"},
		{"rotate", "big", "--repo", R, "-o", "json"},
		{"wal", "prune", "big", "--repo", R, "-o", "json"},
		{"recovery", "readiness", "big", "--repo", R, "-o", "json"},
		{"recovery", "windows", "many", "--repo", R, "-o", "json"},
		{"integrity", "run", "--repo", R, "--strategy", "presence", "--skip-sign", "-o", "json"},
		{"compliance", "report", R, "-o", "json"},
	}
	for _, args := range cmds {
		start := time.Now()
		_, stderr, exit := runCLI(t, args...)
		t.Logf("%-60s exit=%d %6.1fs peak RSS so far %d MiB", strings.Join(args[:min(len(args), 4)], " "), exit,
			time.Since(start).Seconds(), vmHWM(t)>>20)
		if strings.Contains(stderr, "out of memory") {
			t.Errorf("%v: %s", args, stderr)
		}
	}
	// Bundle export of the many-file deployment, then import into a new repo.
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if _, se, exit := runCLI(t, "repo", "bundle", "export", "--deployment", "many", "--repo", R, "--out", bundle, "-o", "json"); exit != 0 {
		t.Errorf("bundle export exit %d: %s", exit, se)
	}
	imp := "file://" + t.TempDir()
	if _, _, exit := runCLI(t, "repo", "init", imp); exit != 0 {
		t.Fatalf("repo init")
	}
	if _, se, exit := runCLI(t, "repo", "bundle", "import", "--in", bundle, "--to", imp, "-o", "json"); exit != 0 {
		t.Errorf("bundle import exit %d: %s", exit, se)
	}
	_ = os.Remove(bundle)
	// Restore the many-file backup (resume log + per-file verify).
	target := filepath.Join(t.TempDir(), "restored")
	if _, se, exit := runCLI(t, "restore", "many", ids.manyB, "--repo", R, "--target", target, "-o", "json"); exit != 0 {
		t.Errorf("restore exit %d: %s", exit, se)
	}
	_ = os.RemoveAll(target)
	return vmHWM(t)
}

// TestScale_CLIPeakRSS runs the real CLI over manifests of growing size
// under a soft memory limit and asserts that peak RSS does not grow with
// them: the larger pass may not exceed the smaller by more than a fixed
// allowance, and both stay under an absolute ceiling. Opt-in: PGHS_SCALE=1.
//
//	PGHS_SCALE_CHUNKS  chunk references in the large pass (default 4M; the
//	                   small pass uses a quarter)
//	PGHS_SCALE_FILES   files in the many-files deployment, large pass
//	                   (default 400k; small pass a quarter)
func TestScale_CLIPeakRSS(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the CLI peak-RSS proof")
	}
	nBig, nFiles := 4_000_000, 400_000
	if v := os.Getenv("PGHS_SCALE_CHUNKS"); v != "" {
		nBig, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("PGHS_SCALE_FILES"); v != "" {
		nFiles, _ = strconv.Atoi(v)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		if need := int64(nBig) * 235 * 3; need > free/5 {
			t.Skipf("would need ~%d GiB of %d GiB free; lower PGHS_SCALE_CHUNKS", need>>30, free>>30)
		}
	}
	debug.SetMemoryLimit(512 << 20)
	defer debug.SetMemoryLimit(-1)

	var small, large uint64
	t.Run("small", func(t *testing.T) {
		w := newReadWorld(t)
		ids := scaleCLIFixture(t, w, nBig/4, nFiles/4)
		small = scaleCLIPass(t, w, ids)
		t.Logf("small pass (%d chunk refs, %d files): peak RSS %d MiB", nBig/4, nFiles/4, small>>20)
	})
	t.Run("large", func(t *testing.T) {
		w := newReadWorld(t)
		ids := scaleCLIFixture(t, w, nBig, nFiles)
		large = scaleCLIPass(t, w, ids)
		t.Logf("large pass (%d chunk refs, %d files): peak RSS %d MiB", nBig, nFiles, large>>20)
	})
	// 4x the data may not cost more than a fixed allowance over the 1x run
	// (a linear structure would add >= several hundred MiB per million
	// chunk references), and the absolute peak stays bounded.
	if large > small+384<<20 {
		t.Errorf("peak RSS grew with manifest size: %d MiB at 1x -> %d MiB at 4x", small>>20, large>>20)
	}
	if large > 2<<30 {
		t.Errorf("peak RSS %d MiB over the 2 GiB ceiling", large>>20)
	}
}
