package restore_test

// timetarget_skipped_test.go — the twin of latest_skipped_test.go.
//
// ResolveBackupForTime answers "the LATEST backup at or before the
// target", which is the seed PG replays forward from. It skips
// manifests that fail verification, counts them, and then used that
// count only to decide whether EVERY manifest had failed — so when some
// succeeded, the skip was silent, exactly as in ResolveLatest.
//
// It bites harder here. The manifest that gets skipped may be precisely
// the one that would have won: the closest seed below the target.
// Falling back to an older one is not a neutral substitution — PG then
// replays every WAL segment between that older stop_lsn and the target,
// which is a longer span, more time, and more chance of running into a
// pruned or gap-recorded stretch of archive.
//
// A skipped manifest also corrupts the other half of the answer.
// laterCount feeds "the deployment has N manifest(s), but every one was
// taken AFTER your --to target" — a claim about the DEPLOYMENT derived
// from the manifests that happened to be readable.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// commitN writes manifests at 14:00, 14:01, 14:02 UTC on 2026-04-28,
// each stopping 30s later.
const ttBase = "2026-04-28T14:00:00Z"

func ttTime(t *testing.T, offsetMinutes int, plusSeconds int) time.Time {
	t.Helper()
	base, err := time.Parse(time.RFC3339, ttBase)
	if err != nil {
		t.Fatal(err)
	}
	return base.Add(time.Duration(offsetMinutes)*time.Minute + time.Duration(plusSeconds)*time.Second)
}

func TestResolveBackupForTimeDetailed_ReportsSkippedManifests(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3) // stops at 14:00:30, 14:01:30, 14:02:30

	// Target after the newest stop: the best seed is ids[2].
	target := ttTime(t, 5, 0)

	// Corrupt ids[2] — the manifest that would have won.
	key := "manifests/db1/backups/" + ids[2] + "/manifest.json"
	if _, err := sp.Put(context.Background(), key,
		strings.NewReader("{\"schema\":\"broken\"}"), storage.PutOptions{}); err != nil {
		t.Fatalf("corrupt best seed: %v", err)
	}

	got, skipped, err := restore.ResolveBackupForTimeDetailed(context.Background(), sp, "db1", target, verifier)
	if err != nil {
		t.Fatalf("ResolveBackupForTimeDetailed: %v", err)
	}
	if got != ids[1] {
		t.Fatalf("chose %q, want the next-best seed %q", got, ids[1])
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 — without the count the caller cannot say that a "+
			"CLOSER seed existed and could not be read, so PG silently replays more WAL "+
			"than it needed to", skipped)
	}
}

// The healthy path reports zero, or the warning would fire constantly
// and mean nothing.
func TestResolveBackupForTimeDetailed_HealthyRepoReportsZero(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3)

	got, skipped, err := restore.ResolveBackupForTimeDetailed(
		context.Background(), sp, "db1", ttTime(t, 5, 0), verifier)
	if err != nil {
		t.Fatalf("ResolveBackupForTimeDetailed: %v", err)
	}
	if got != ids[2] || skipped != 0 {
		t.Fatalf("got (%q, %d), want (%q, 0)", got, skipped, ids[2])
	}
}

// "No backup old enough" must not be asserted about the DEPLOYMENT when
// part of it could not be read — the unreadable one may be the seed.
func TestNoBackupBeforeTime_CarriesSkippedCount(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3)

	// Target before every backup, and corrupt one of them so the walk
	// cannot classify it as earlier or later.
	key := "manifests/db1/backups/" + ids[0] + "/manifest.json"
	if _, err := sp.Put(context.Background(), key,
		strings.NewReader("{\"schema\":\"broken\"}"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	target := ttTime(t, -60, 0)

	_, skipped, err := restore.ResolveBackupForTimeDetailed(context.Background(), sp, "db1", target, verifier)
	var noTime *restore.NoBackupBeforeTimeError
	if !errors.As(err, &noTime) {
		t.Fatalf("got %v, want NoBackupBeforeTimeError", err)
	}
	if skipped != 1 || noTime.Skipped != 1 {
		t.Fatalf("skipped=%d, err.Skipped=%d, want 1 and 1 — the error claims every manifest "+
			"is too new, which is only true of the ones that could be read", skipped, noTime.Skipped)
	}
}

// The operator sentence must name what was chosen, the target, that
// something was skipped, and the way out.
func TestTimeTargetSkippedWarning_IsActionable(t *testing.T) {
	msg := restore.TimeTargetSkippedWarning("db1", "db1.full.X", 2, ttTime(t, 0, 0))
	for _, want := range []string{"db1", "db1.full.X", "2", "2026-04-28", "repo check", "explicit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning does not mention %q — an operator cannot act on it:\n%s", want, msg)
		}
	}
}
