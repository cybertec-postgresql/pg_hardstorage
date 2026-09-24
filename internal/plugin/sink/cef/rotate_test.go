package cef_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// logrotate's default move-and-create renames the live file and
// creates a fresh one at the same path. Writes after the rotation must
// land in the new file, not keep flowing into the renamed one (which
// the forwarder has stopped reading and logrotate will compress).
func TestCEF_ReopensAfterMoveAndCreateRotation(t *testing.T) {
	s, path := newSink(t, nil)
	defer s.Close()
	ctx := context.Background()

	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "c", "before")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil { // "create"
		t.Fatal(err)
	}
	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "c", "after")); err != nil {
		t.Fatal(err)
	}

	if old := readLines(t, path+".1"); len(old) != 1 || !strings.Contains(old[0], "|before|") {
		t.Errorf("rotated file = %q; want only the pre-rotation line", old)
	}
	if cur := readLines(t, path); len(cur) != 1 || !strings.Contains(cur[0], "|after|") {
		t.Errorf("live file = %q; want the post-rotation line", cur)
	}
}

// Rotation that only moves the file (no create — e.g. `create` absent
// and the forwarder expected to recreate) must also be survived: the
// sink recreates the file at the configured path.
func TestCEF_RecreatesAfterMoveWithoutCreate(t *testing.T) {
	s, path := newSink(t, nil)
	defer s.Close()
	ctx := context.Background()

	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "c", "before")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "c", "after")); err != nil {
		t.Fatal(err)
	}
	if cur := readLines(t, path); len(cur) != 1 || !strings.Contains(cur[0], "|after|") {
		t.Errorf("live file = %q; want the post-rotation line", cur)
	}
}
