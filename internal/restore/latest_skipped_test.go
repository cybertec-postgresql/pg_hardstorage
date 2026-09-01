package restore_test

// latest_skipped_test.go — "the latest backup" vs "the latest backup I
// could read".
//
// ResolveLatest ranks manifests by StoppedAt and skips any that fail
// signature verification or cannot be fetched. Skipping is deliberate
// and stays: one corrupt old manifest must not stop an operator
// finding a newer good backup, least of all during a recovery.
//
// What was missing is that the skip was SILENT. A manifest that fails
// to verify yields no StoppedAt, so it cannot be ranked — which means
// it may have been NEWER than the winner. `restore <dep> latest` then
// restores an older backup while the operator believes they asked for
// the newest, and nothing anywhere says otherwise. That is a silently
// wrong recovery, and it is worst in exactly the case that produced it:
// the newest manifest being the corrupt one.
//
// The count cannot be narrowed to "only the ones that might have been
// newer" — establishing that would mean reading the manifest that just
// failed to read. So the count is reported and the caller decides:
// `restore` warns and proceeds (recovery must not be blocked),
// `standby create`, the agent restore executor, and
// `backup --incremental-from=latest` refuse (none is an emergency
// path, and all three would otherwise bake the wrong choice in).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// storagePutOpts is an overwrite Put with no WORM retention.
func storagePutOpts() storage.PutOptions { return storage.PutOptions{} }

func TestResolveLatestDetailed_ReportsSkippedManifests(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3)

	// Corrupt the NEWEST manifest's bytes. It still lists, still has a
	// key, but no longer verifies — so it drops out of the ranking and
	// the answer silently becomes the second-newest.
	key := "manifests/db1/backups/" + ids[2] + "/manifest.json"
	if _, err := sp.Put(context.Background(), key, strings.NewReader("{\"schema\":\"broken\"}"),
		storagePutOpts()); err != nil {
		t.Fatalf("corrupt newest manifest: %v", err)
	}

	got, skipped, err := restore.ResolveLatestDetailed(context.Background(), sp, "db1", verifier)
	if err != nil {
		t.Fatalf("ResolveLatestDetailed: %v", err)
	}
	if got != ids[1] {
		t.Fatalf("chose %q, want the second-newest %q", got, ids[1])
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 — the caller cannot warn about what it is not told; "+
			"without this count `restore latest` hands back an OLDER backup and says nothing", skipped)
	}
}

// A healthy repo must report zero, or every caller would warn (or
// refuse) all the time and the signal would be worthless.
func TestResolveLatestDetailed_HealthyRepoReportsZeroSkipped(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3)

	got, skipped, err := restore.ResolveLatestDetailed(context.Background(), sp, "db1", verifier)
	if err != nil {
		t.Fatalf("ResolveLatestDetailed: %v", err)
	}
	if got != ids[2] || skipped != 0 {
		t.Fatalf("got (%q, skipped=%d), want (%q, 0)", got, skipped, ids[2])
	}
}

// The old signature must keep behaving exactly as before, so the
// change is additive for anything still calling it.
func TestResolveLatest_UnchangedBehaviour(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 2)

	got, err := restore.ResolveLatest(context.Background(), sp, "db1", verifier)
	if err != nil || got != ids[1] {
		t.Fatalf("got (%q, %v), want (%q, nil)", got, err, ids[1])
	}
	if _, err := restore.ResolveLatest(context.Background(), sp, "nope", verifier); !errors.Is(err, restore.ErrNoBackupsFound) {
		t.Fatalf("empty deployment: got %v, want ErrNoBackupsFound", err)
	}
}

// The operator-facing sentence has to name the three things they need:
// what was chosen, that something was skipped, and the way out.
func TestLatestSkippedWarning_IsActionable(t *testing.T) {
	msg := restore.LatestSkippedWarning("db1", "db1.full.X", 2)
	for _, want := range []string{"db1", "db1.full.X", "2", "repo check", "explicit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning does not mention %q — an operator cannot act on it:\n%s", want, msg)
		}
	}
}
