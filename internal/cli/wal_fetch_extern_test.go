package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// initRepoForTest sets up a usable file:// repo at root and returns
// the URL.
func initRepoForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	return repoURL
}

func TestWalFetch_NotFound_ReturnsExitNotFound(t *testing.T) {
	repoURL := initRepoForTest(t)
	target := filepath.Join(t.TempDir(), "out.wal")
	_, _, exit := runCmd(t,
		"wal", "fetch", "db1",
		walsink.SegmentFileName(1, 42, walsink.SegmentSize),
		target,
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 6 {
		t.Errorf("exit = %d, want 6 (ExitNotFound)", exit)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target file should not exist on miss; got err %v", err)
	}
}

func TestWalFetch_MalformedSegmentName_NotFound(t *testing.T) {
	repoURL := initRepoForTest(t)
	target := filepath.Join(t.TempDir(), "out.wal")
	_, _, exit := runCmd(t,
		"wal", "fetch", "db1",
		"not-a-segment",
		target,
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 6 {
		t.Errorf("exit = %d, want 6 (history files / garbage names map to NotFound)", exit)
	}
}

// TestWalFetch_HistoryFile_FromFollowerTimelineStore is the
// timeline-history-store regression: the streaming-HA follower captures
// `.history` files into wal/<dep>/timelines/<decimal-tli>.history (a store
// separate from the archive_command aux path). `wal fetch` must serve them
// during recovery — a request for "00000002.history" (8-hex) must resolve to
// the follower's timelines/2.history — or PITR across a failover timeline
// switch can't get the history file PG needs.
func TestWalFetch_HistoryFile_FromFollowerTimelineStore(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	content := []byte("1\t0/3000028\tafter failover\n")
	if err := timeline.New(sp).Put(context.Background(), "db1", 2, content); err != nil {
		t.Fatalf("plant follower .history: %v", err)
	}

	target := filepath.Join(t.TempDir(), "out.history")
	stdout, stderr, exit := runCmd(t,
		"wal", "fetch", "db1",
		"00000002.history", // 8-hex TLI; must map to the follower's timelines/2.history
		target,
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (served from the follower timeline store)\nstdout:%s\nstderr:%s", exit, stdout, stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read fetched history: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("fetched history = %q, want %q", got, content)
	}
}

func TestWalFetch_HistoryFile_ReportsNotFound(t *testing.T) {
	// PG asks for "<TLI>.history" files when navigating timelines. When no
	// history is stored ANYWHERE (neither the archive aux path nor the
	// follower's timeline store), the request MUST surface as NotFound —
	// the cue PG uses to conclude "no such timeline."
	repoURL := initRepoForTest(t)
	target := filepath.Join(t.TempDir(), "out.history")
	stdout, stderr, exit := runCmd(t,
		"wal", "fetch", "db1",
		"00000003.history",
		target,
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 6 {
		t.Errorf("exit = %d, want 6 (history file → NotFound)\nstdout: %s\nstderr: %s",
			exit, stdout, stderr)
	}
	// Errors land on stderr per Unix convention; the structured-error
	// code namespace must be "notfound." (so monitoring tooling can
	// distinguish it from a real fetch error).
	// Match the substring tolerantly — the JSON renderer pretty-prints
	// with whitespace between the colon and the value.
	if !strings.Contains(stderr, "notfound.") {
		t.Errorf("error code should be in notfound.* namespace; got stderr:\n%s", stderr)
	}
}

// TestWalFetch_TraversalNames_NotFoundNoEscape: a segment or aux name
// carrying path-traversal segments must surface as NotFound (exit 6)
// — the recovery-natural "no such WAL" signal — and must NOT read or
// write any file outside the repo. PG never sends such names; this
// pins that a hand-crafted (or buggy) request can't escape. Aux names
// (.history/.backup) take the less-validated AuxiliaryFilePath route,
// so they're the important case.
func TestWalFetch_TraversalNames_NotFoundNoEscape(t *testing.T) {
	repoURL := initRepoForTest(t)
	cases := []string{
		"../../../etc/passwd",
		"../../../../etc/shadow.history",
		"..%2f..%2fx.backup",
		"foo/../../bar.history",
		`..\..\win.history`,
		"00000003/../../../x.history",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "out.wal")
			_, stderr, exit := runCmd(t,
				"wal", "fetch", "db1", name, target,
				"--repo", repoURL, "--output", "json",
			)
			if exit != 6 {
				t.Errorf("exit = %d, want 6 (NotFound) for traversal name %q\nstderr: %s", exit, name, stderr)
			}
			if !strings.Contains(stderr, "notfound.") {
				t.Errorf("want notfound.* code for %q; got:\n%s", name, stderr)
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("target written for traversal name %q (err=%v)", name, err)
			}
		})
	}
}

func TestWalFetch_RequiresRepoFlag(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.wal")
	_, _, exit := runCmd(t,
		"wal", "fetch", "db1",
		walsink.SegmentFileName(1, 0, walsink.SegmentSize),
		target,
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (ExitMisuse — missing --repo)", exit)
	}
}
