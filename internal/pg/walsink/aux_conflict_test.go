package walsink_test

// aux_conflict_test.go — auxiliary archive files get the same
// split-brain treatment segment manifests already get.
//
// `.history` and `.backup` names are derived from the timeline and
// segment number, so two clusters that share a deployment name (a
// cloned datadir, a restored copy still running archive_command)
// generate the SAME key with DIFFERENT bodies. Treating the collision
// as an idempotent retry tells PG "archived" while the repo holds the
// other cluster's copy — and the divergence stays invisible until a
// restore reads the wrong timeline history and picks the wrong parent.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// TestPushAuxiliaryFile_ConflictingContentRefused: same key, different
// bytes → a splitbrain error, not a silent success.
func TestPushAuxiliaryFile_ConflictingContentRefused(t *testing.T) {
	sp, _ := openFSRepo(t, "file://"+t.TempDir())
	defer sp.Close()

	const base = "00000002.history"
	ours := []byte("1\t13/FE000170\tno recovery target specified\n")
	theirs := []byte("1\t42/AB000000\tafter LSN 42/AB000000\n")

	first := filepath.Join(t.TempDir(), base)
	if err := os.WriteFile(first, ours, 0o600); err != nil {
		t.Fatal(err)
	}
	key, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, first,
		walsink.PushOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("first push: %v", err)
	}

	// The doppelgänger cluster archives its own timeline history under
	// the identical name.
	second := filepath.Join(t.TempDir(), base)
	if err := os.WriteFile(second, theirs, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = walsink.PushAuxiliaryFile(context.Background(), sp, second,
		walsink.PushOptions{Deployment: "db1"})
	if err == nil {
		t.Fatal("conflicting auxiliary file reported success; PG would believe its history is archived")
	}
	if !strings.Contains(err.Error(), "splitbrain.content_mismatch") {
		t.Errorf("want splitbrain.content_mismatch; got %v", err)
	}

	// The first writer's bytes must survive — refusing the second push
	// is only correct if it also leaves the repo untouched.
	rc, gerr := sp.Get(context.Background(), key)
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer rc.Close()
	got, rerr := readAll(rc)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytesEqual(got, ours) {
		t.Errorf("stored body changed: got %q, want %q", got, ours)
	}
}

// TestPushAuxiliaryFile_IdenticalRepushSucceeds: the archive_command
// retry path stays idempotent — identical bytes at the same key are a
// success, which is what PG's contract requires.
func TestPushAuxiliaryFile_IdenticalRepushSucceeds(t *testing.T) {
	sp, _ := openFSRepo(t, "file://"+t.TempDir())
	defer sp.Close()

	const base = "0000000100000013000000FE.000000D8.backup"
	body := []byte("START WAL LOCATION: 13/FE000098\nSTOP WAL LOCATION: 13/FE000170\n")
	src := filepath.Join(t.TempDir(), base)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, src,
			walsink.PushOptions{Deployment: "db1"}); err != nil {
			t.Fatalf("push %d: %v", i+1, err)
		}
	}
}
