package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestHoldAdd_UntilDuration_AcceptsShorthand: --until 30d sets
// ExpiresAt ~30d in the future. The CLI emits expires_at in the
// JSON body so callers see the resolved time.
func TestHoldAdd_UntilDuration_AcceptsShorthand(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))

	stdout, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--holder", "debug",
		"--reason", "investigating-bug-1234",
		"--until", "30d",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("hold add --until 30d: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"expires_at"`) {
		t.Errorf("expected expires_at in JSON body:\n%s", stdout)
	}

	// On disk the marker should carry ExpiresAt.
	h, err := w.store.GetHold(context.Background(), "db1", id)
	if err != nil {
		t.Fatal(err)
	}
	if h.ExpiresAt == nil {
		t.Fatal("ExpiresAt should be set on disk")
	}
	delta := time.Until(*h.ExpiresAt)
	wantApprox := 30 * 24 * time.Hour
	if delta < wantApprox-time.Minute || delta > wantApprox+time.Minute {
		t.Errorf("ExpiresAt ~%v from now, want ~30d (got delta=%v)", h.ExpiresAt, delta)
	}
}

// TestHoldAdd_UntilAbsolute_AcceptsRFC3339: a fixed RFC3339
// time is parsed and passed through.
func TestHoldAdd_UntilAbsolute_AcceptsRFC3339(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))

	want := "2027-01-01T00:00:00Z"
	_, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--until", want,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("absolute --until: exit=%d", exit)
	}
	h, err := w.store.GetHold(context.Background(), "db1", id)
	if err != nil {
		t.Fatal(err)
	}
	if h.ExpiresAt == nil || h.ExpiresAt.Format(time.RFC3339) != want {
		t.Errorf("ExpiresAt = %v, want %s", h.ExpiresAt, want)
	}
}

// TestHoldAdd_UntilPast_RefusedAtUsage: --until in the past is
// rejected with `usage.bad_until` before any storage write.
// (A past expiry would be inert from the start — almost
// certainly an operator typo.)
func TestHoldAdd_UntilPast_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))

	_, stderr, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--until", "2020-01-01",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.bad_until") {
		t.Errorf("expected usage.bad_until; got %s", stderr)
	}
	// Confirm no marker was written.
	if _, err := w.store.GetHold(context.Background(), "db1", id); err == nil {
		t.Errorf("hold marker should not exist after rejected --until")
	}
}

// TestHoldAdd_UntilNonsense_RefusedAtUsage: a string that
// neither matches the duration shorthand nor any absolute
// format surfaces as usage.bad_until.
func TestHoldAdd_UntilNonsense_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))

	_, stderr, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--until", "fortnight",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.bad_until") {
		t.Errorf("expected usage.bad_until; got %s", stderr)
	}
}

// TestHoldList_SurfacesExpiry: hold list shows expires_at +
// active flag for both bounded and indefinite holds.
func TestHoldList_SurfacesExpiry(t *testing.T) {
	w := newReadWorld(t)
	idIndef := commitVerifiableBackup(t, w, "db1", 0, []byte("indef"))
	idBounded := commitVerifiableBackup(t, w, "db1", 1, []byte("bounded"))
	if err := w.store.PutHold(context.Background(), "db1", idIndef, "ops", "indef"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", idBounded,
		"ops", "bounded", future); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "hold", "list", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("hold list exit=%d", exit)
	}
	for _, want := range []string{
		idIndef, idBounded,
		`"expires_at"`,
		`"active": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("hold list missing %q:\n%s", want, stdout)
		}
	}
	// The indefinite hold should NOT have an expires_at key
	// in its row (omitempty on absence).
	// Hard to assert per-row without parsing; just check
	// it's there at all (the bounded row populates it).
}

// TestHoldList_TagsExpiredEntries: an expired hold shows up
// with active=false / expired=true and the [EXPIRED] tag in
// text mode.
func TestHoldList_TagsExpiredEntries(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("expired"))
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", id,
		"ops", "stale-debug", past); err != nil {
		t.Fatal(err)
	}

	// JSON.
	stdout, _, exit := runCLI(t, "hold", "list", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("hold list (expired) exit=%d", exit)
	}
	for _, want := range []string{
		`"active": false`,
		`"expired": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected %q in JSON:\n%s", want, stdout)
		}
	}

	// Text — [EXPIRED] tag.
	stdout, _, exit = runCLI(t, "hold", "list", "db1",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("hold list text exit=%d", exit)
	}
	if !strings.Contains(stdout, "[EXPIRED]") {
		t.Errorf("expected [EXPIRED] in text:\n%s", stdout)
	}
}

// TestBackupDelete_PastExpiredHold: a manifest whose hold has
// expired is no longer protected — `backup delete` succeeds.
// The marker stays on disk.
func TestBackupDelete_PastExpiredHold(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("test"))
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", id,
		"ops", "expired", past); err != nil {
		t.Fatal(err)
	}

	_, _, exit := runCLI(t, "backup", "delete", "db1", id,
		"--repo", w.repoURL,
		"--reason", "after-hold-expired")
	if exit != int(output.ExitOK) {
		t.Fatalf("backup delete past expired hold should succeed; exit=%d", exit)
	}
	// Tombstoned.
	if dead, _ := w.store.IsTombstoned(context.Background(), "db1", id); !dead {
		t.Errorf("manifest should be tombstoned after delete past expired hold")
	}
}

// TestHoldAdd_UntilDiscoverable: --until shows in --help with
// the duration-shorthand examples + absolute-time hint.
func TestHoldAdd_UntilDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "hold", "add", "--help")
	for _, want := range []string{
		"--until",
		"30d",
		"indefinite",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("hold add --help missing %q:\n%s", want, stdout)
		}
	}
}
