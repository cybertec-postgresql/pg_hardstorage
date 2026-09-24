package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// buildSyntheticGCRepo writes a repository shaped like a shared repo of
// several heavy deployments straight onto disk: `deployments` × `backups`
// backup manifests of `chunksPerBackup` chunks each, `walSegments` WAL
// segment manifests of 4 chunks each, plus `orphans` unreferenced chunks.
// Chunk bodies are irrelevant to gc (it never reads them), so they are a
// few bytes each; mtimes are aged past every floor.
func buildSyntheticGCRepo(tb testing.TB, deployments, backups, chunksPerBackup, walSegments, orphans int) string {
	tb.Helper()
	root := tb.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		tb.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	n := 0
	chunk := func() repo.Hash {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		n++
		h := repo.Hash(sha256.Sum256(b[:]))
		p := filepath.Join(root, filepath.FromSlash(repo.ChunkKey(h)))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, b[:], 0o644); err != nil {
			tb.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
		return h
	}
	write := func(key, body string) {
		p := filepath.Join(root, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			tb.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	for d := 0; d < deployments; d++ {
		dep := "db" + strconv.Itoa(d)
		for b := 0; b < backups; b++ {
			var sb strings.Builder
			sb.WriteString(`{"files":[{"chunks":[`)
			for c := 0; c < chunksPerBackup; c++ {
				if c > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"hash":"%s"}`, chunk())
			}
			sb.WriteString(`]}]}`)
			write(fmt.Sprintf("manifests/%s/backups/%s.full.%06d/manifest.json", dep, dep, b), sb.String())
		}
		for s := 0; s < walSegments; s++ {
			var sb strings.Builder
			sb.WriteString(`{"chunks":[`)
			for c := 0; c < 4; c++ {
				if c > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"hash":"%s"}`, chunk())
			}
			sb.WriteString(`]}`)
			write(fmt.Sprintf("wal/%s/00000001/%024X.json", dep, s+1), sb.String())
		}
	}
	for o := 0; o < orphans; o++ {
		chunk()
	}
	return repoURL
}

// TestRepoGC_SyntheticRepoTiming measures a full `repo gc --apply` on a
// synthetic shared repository (M28). Opt-in: set PG_HARDSTORAGE_GC_BENCH=1.
// Sizes: 4 deployments × 500 backups × 40 chunks + 4 × 2500 WAL segments
// × 4 chunks = 120,000 referenced chunks, 12,000 manifests, 20,000 orphans.
func TestRepoGC_SyntheticRepoTiming(t *testing.T) {
	if os.Getenv("PG_HARDSTORAGE_GC_BENCH") == "" {
		t.Skip("set PG_HARDSTORAGE_GC_BENCH=1 to run the gc timing benchmark")
	}
	scale := 1
	if v, err := strconv.Atoi(os.Getenv("PG_HARDSTORAGE_GC_BENCH")); err == nil && v > 1 {
		scale = v
	}
	repoURL := buildSyntheticGCRepo(t, 4, 500*scale, 40, 2500*scale, 20000*scale)

	start := time.Now()
	stdout, stderr, exit := runCmd(t, "repo", "gc", repoURL, "-o", "json")
	dry := time.Since(start)
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	start = time.Now()
	stdout, stderr, exit = runCmd(t, "repo", "gc", repoURL, "--apply", "-o", "json")
	apply := time.Since(start)
	if exit != int(output.ExitOK) {
		t.Fatalf("apply exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf(`"applied": %d`, 20000*scale)) {
		t.Fatalf("expected %d orphans deleted:\n%s", 20000*scale, stdout)
	}
	t.Logf("gc dry-run: %v   gc --apply: %v", dry, apply)
}
