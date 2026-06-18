package timeline_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// newStore wires a temp file:// repo + a Store.
func newStore(t *testing.T) (*timeline.Store, storage.StoragePlugin) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("fs open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return timeline.New(sp), sp
}

// TestPath is a regression guard on the canonical key. Several
// downstream packages (doctor / repair / fleet search) will look
// for this exact prefix shape; renaming the layout silently is a
// 24-month-compat violation.
func TestPath(t *testing.T) {
	want := "wal/db1/timelines/2.history"
	if got := timeline.Path("db1", 2); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// TestPut_FirstWriteCommits: the first write for a (deployment,
// tli) pair lands at the canonical path with the verbatim bytes.
func TestPut_FirstWriteCommits(t *testing.T) {
	s, sp := newStore(t)
	body := []byte("1\t0/15A2B388\tno recovery target specified\n")

	if err := s.Put(context.Background(), "db1", 2, body); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Round-trip via the store's Get.
	got, err := s.Get(context.Background(), "db1", 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip mismatch:\n  got:  %q\n  want: %q", got, body)
	}

	// Direct repo lookup: confirm the on-disk key.
	if _, err := sp.Stat(context.Background(), "wal/db1/timelines/2.history"); err != nil {
		t.Errorf("expected committed key wal/db1/timelines/2.history; stat err = %v", err)
	}
}

// TestPut_IdempotentReWrite: re-putting byte-identical content is
// a no-op (no error). This is the common case for the
// leader-follow loop reconnecting to a previously-known leader
// after a transient blip.
func TestPut_IdempotentReWrite(t *testing.T) {
	s, _ := newStore(t)
	body := []byte("1\t0/15A2B388\tno recovery target\n")
	if err := s.Put(context.Background(), "db1", 2, body); err != nil {
		t.Fatal(err)
	}
	// Same bytes, second time. Must not error, must not corrupt.
	if err := s.Put(context.Background(), "db1", 2, body); err != nil {
		t.Errorf("idempotent re-put: %v", err)
	}
	got, err := s.Get(context.Background(), "db1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("post-re-put bytes drifted: %q", got)
	}
}

// TestPut_MismatchSurfaces: a different bytestream for an already-
// committed (deployment, tli) is the rare-but-documented forced-
// rebuild scenario. We refuse and surface ErrHistoryMismatch so
// the operator drives an explicit repair.
func TestPut_MismatchSurfaces(t *testing.T) {
	s, _ := newStore(t)
	original := []byte("1\t0/15A2B388\tfailover at lsn A\n")
	conflicting := []byte("1\t0/9999BEEF\tfailover at lsn B\n")

	if err := s.Put(context.Background(), "db1", 2, original); err != nil {
		t.Fatal(err)
	}
	err := s.Put(context.Background(), "db1", 2, conflicting)
	if err == nil {
		t.Fatal("expected ErrHistoryMismatch")
	}
	if !errors.Is(err, timeline.ErrHistoryMismatch) {
		t.Errorf("expected errors.Is(ErrHistoryMismatch); got %v", err)
	}
	var mErr *timeline.MismatchError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected *timeline.MismatchError; got %T", err)
	}
	if mErr.Timeline != 2 || mErr.Deployment != "db1" {
		t.Errorf("subject mismatch: %+v", mErr)
	}
	// Existing committed bytes must be preserved (mismatch
	// detection MUST NOT mutate state).
	got, err := s.Get(context.Background(), "db1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("post-mismatch existing bytes drifted: %q (want %q)", got, original)
	}
	if !strings.Contains(mErr.Error(), "db1") {
		t.Errorf("error string should name deployment: %v", mErr)
	}
}

// TestPut_RejectsZeroTLI: TLI 0 is a misuse — PG numbers
// timelines starting at 1.
func TestPut_RejectsZeroTLI(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(context.Background(), "db1", 0, []byte("x")); err == nil {
		t.Error("expected error for zero TLI")
	}
}

// TestPut_RejectsEmptyDeployment: an empty deployment name is a
// misuse.
func TestPut_RejectsEmptyDeployment(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(context.Background(), "", 2, []byte("x")); err == nil {
		t.Error("expected error for empty deployment")
	}
}

// TestGet_NotFound: an unwritten path surfaces storage.ErrNotFound.
func TestGet_NotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(context.Background(), "db1", 99)
	if err == nil {
		t.Fatal("expected ErrNotFound")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected storage.ErrNotFound; got %v", err)
	}
}

// TestPut_ConcurrentSameContent_NoTruncation is the redundant-agent
// regression: two leader-follow loops can capture the SAME timeline's
// .history simultaneously. With a shared (fixed) tmp key one agent's
// truncate-then-write could leave the file partial at the instant the
// other renames it, installing a TRUNCATED .history. The randomized
// per-writer tmp suffix makes that impossible. Hammer concurrent Puts
// of identical content and assert the committed history is byte-exact —
// never short.
func TestPut_ConcurrentSameContent_NoTruncation(t *testing.T) {
	store, _ := newStore(t)
	// A realistic, non-trivial history body so a truncation would be
	// visible as a short read.
	var b strings.Builder
	for i := 0; i < 256; i++ {
		b.WriteString("1\t0/3000028\tno significant maintenance window\n")
	}
	content := []byte(b.String())

	const writers = 12
	var wg sync.WaitGroup
	errs := make([]error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = store.Put(context.Background(), "db1", 2, content)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	got, err := store.Get(context.Background(), "db1", 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("committed .history is corrupt/truncated: got %d bytes, want %d", len(got), len(content))
	}
}
