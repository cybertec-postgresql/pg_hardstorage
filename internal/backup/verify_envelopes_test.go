package backup_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// constResolver returns the same KEK regardless of ref. Useful for
// "this resolver knows exactly one key" tests.
func constResolver(k [encryption.KeyLen]byte) func(string) ([encryption.KeyLen]byte, error) {
	return func(string) ([encryption.KeyLen]byte, error) { return k, nil }
}

// refResolver maps explicit refs to keys; any other ref errors. This
// is the realistic v0.5+ resolver shape (multi-tenant keyring).
func refResolver(refs map[string][encryption.KeyLen]byte) func(string) ([encryption.KeyLen]byte, error) {
	return func(ref string) ([encryption.KeyLen]byte, error) {
		k, ok := refs[ref]
		if !ok {
			return [encryption.KeyLen]byte{}, fmt.Errorf("unknown ref %q", ref)
		}
		return k, nil
	}
}

// TestVerifyEnvelopes_AllOK: every encrypted manifest unwraps
// cleanly with the shared KEK → OK count == considered, no failures.
func TestVerifyEnvelopes_AllOK(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.A", kek, "test:v1", 1)
	w.commitEncrypted(t, "db1", "db1.full.B", kek, "test:v1", 2)
	w.commitEncrypted(t, "db2", "db2.full.C", kek, "test:v1", 3)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(kek),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Considered != 3 {
		t.Errorf("Considered = %d, want 3", res.Considered)
	}
	if res.OK != 3 {
		t.Errorf("OK = %d, want 3", res.OK)
	}
	if res.AnyBroken() {
		t.Errorf("AnyBroken = true, want false; failures=%v", res.Failures)
	}
	if res.Schema != backup.VerifyEnvelopesSchema {
		t.Errorf("Schema = %q", res.Schema)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS = %d", res.DurationMS)
	}
}

// TestVerifyEnvelopes_UnwrapFailed: a manifest wrapped with KEK_A
// surveyed by a resolver returning KEK_B → unwrap_failed. The bad
// finding lands in Failures with the KEKRef so the operator can
// triage.
func TestVerifyEnvelopes_UnwrapFailed(t *testing.T) {
	w := setupRotateWorld(t)
	realKEK := mkKEK(t)
	wrongKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.bad", realKEK, "test:v1", 1)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(wrongKEK),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.UnwrapFailed != 1 {
		t.Errorf("UnwrapFailed = %d, want 1", res.UnwrapFailed)
	}
	if res.OK != 0 {
		t.Errorf("OK = %d, want 0", res.OK)
	}
	if !res.AnyBroken() {
		t.Errorf("AnyBroken = false, want true")
	}
	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1", len(res.Failures))
	}
	if got := res.Failures[0]; got.Status != backup.EnvelopeStatusUnwrapFailed ||
		got.BackupID != "db1.full.bad" || got.KEKRef != "test:v1" {
		t.Errorf("Failure = %+v", got)
	}
}

// TestVerifyEnvelopes_KEKUnknown: resolver returns an error for the
// manifest's ref → kek_unknown. Reason carries the resolver's
// message.
func TestVerifyEnvelopes_KEKUnknown(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.unknown-ref", kek, "tenant-a:v3", 1)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier: w.verifier,
		// resolver only knows tenant-b
		KEKResolver: refResolver(map[string][encryption.KeyLen]byte{
			"tenant-b:v1": kek,
		}),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.KEKUnknown != 1 {
		t.Errorf("KEKUnknown = %d, want 1", res.KEKUnknown)
	}
	if !res.AnyBroken() {
		t.Errorf("AnyBroken = false")
	}
	if len(res.Failures) != 1 || res.Failures[0].Status != backup.EnvelopeStatusKEKUnknown {
		t.Errorf("Failures[0] = %+v", res.Failures)
	}
}

// TestVerifyEnvelopes_Unencrypted: a manifest without an encryption
// block → unencrypted counter, NOT a failure (operator policy
// decides whether unencrypted is acceptable).
func TestVerifyEnvelopes_Unencrypted(t *testing.T) {
	w := setupRotateWorld(t)
	w.commitUnencrypted(t, "db1", "db1.full.plain", 1)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(mkKEK(t)),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Unencrypted != 1 {
		t.Errorf("Unencrypted = %d, want 1", res.Unencrypted)
	}
	if res.AnyBroken() {
		t.Errorf("Unencrypted should NOT be a 'break': %+v", res)
	}
}

// TestVerifyEnvelopes_DeploymentFilter: only the named deployment is
// considered. db2's manifests are invisible.
func TestVerifyEnvelopes_DeploymentFilter(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.A", kek, "test:v1", 1)
	w.commitEncrypted(t, "db2", "db2.full.B", kek, "test:v1", 2)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:         w.verifier,
		KEKResolver:      constResolver(kek),
		DeploymentFilter: "db1",
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Considered != 1 {
		t.Errorf("Considered = %d, want 1", res.Considered)
	}
	if res.OK != 1 {
		t.Errorf("OK = %d, want 1", res.OK)
	}
}

// TestVerifyEnvelopes_KEKRefFilter: only manifests with the
// requested ref are checked; everything else lands in Skipped (NOT
// a failure). Useful for post-rotation audits.
func TestVerifyEnvelopes_KEKRefFilter(t *testing.T) {
	w := setupRotateWorld(t)
	kekOld := mkKEK(t)
	kekNew := mkKEK(t)
	// One manifest still on the old ref, one on the new ref, one
	// unencrypted (also Skipped by the filter — no kek_ref).
	w.commitEncrypted(t, "db1", "db1.full.old", kekOld, "tenant:old", 1)
	w.commitEncrypted(t, "db1", "db1.full.new", kekNew, "tenant:new", 2)
	w.commitUnencrypted(t, "db1", "db1.full.plain", 3)

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier: w.verifier,
		KEKResolver: refResolver(map[string][encryption.KeyLen]byte{
			"tenant:old": kekOld,
		}),
		KEKRefFilter: "tenant:old",
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Considered != 3 {
		t.Errorf("Considered = %d, want 3", res.Considered)
	}
	if res.OK != 1 {
		t.Errorf("OK = %d, want 1 (only tenant:old should pass)", res.OK)
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (the new ref + the plain manifest)", res.Skipped)
	}
	if res.AnyBroken() {
		t.Errorf("AnyBroken = true; want false (filter excludes shouldn't be findings)")
	}
}

// TestVerifyEnvelopes_OnProgress: the callback fires once per
// classified manifest, in deployment-then-ID order.
func TestVerifyEnvelopes_OnProgress(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.A", kek, "test:v1", 1)
	w.commitEncrypted(t, "db1", "db1.full.B", kek, "test:v1", 2)
	w.commitEncrypted(t, "db2", "db2.full.C", kek, "test:v1", 3)

	var seen []backup.VerifyEnvelopeFinding
	_, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(kek),
		OnProgress:  func(f backup.VerifyEnvelopeFinding) { seen = append(seen, f) },
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("OnProgress fired %d times, want 3", len(seen))
	}
	wantIDs := []string{"db1.full.A", "db1.full.B", "db2.full.C"}
	for i, want := range wantIDs {
		if seen[i].BackupID != want {
			t.Errorf("seen[%d].BackupID = %q, want %q", i, seen[i].BackupID, want)
		}
	}
}

// TestVerifyEnvelopes_ValidationErrors: nil sp / Verifier / resolver
// surface clear errors. Programmer-error guard.
func TestVerifyEnvelopes_ValidationErrors(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)

	if _, err := backup.VerifyEnvelopes(context.Background(), nil, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(kek),
	}); err == nil {
		t.Error("nil sp should error")
	}

	if _, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		KEKResolver: constResolver(kek),
	}); err == nil {
		t.Error("nil Verifier should error")
	}

	if _, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil KEKResolver should error")
	}
}

// TestVerifyEnvelopes_Mixed: a realistic fleet has a bit of
// everything. The result body's per-class counters tell the right
// story.
func TestVerifyEnvelopes_Mixed(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	wrongKEK := mkKEK(t)

	w.commitEncrypted(t, "db1", "db1.full.A", kek, "test:v1", 1)       // OK
	w.commitEncrypted(t, "db1", "db1.full.B", wrongKEK, "test:v1", 2)  // unwrap_failed (wrong key under same ref)
	w.commitEncrypted(t, "db2", "db2.full.C", kek, "test:v1", 3)       // OK
	w.commitEncrypted(t, "db2", "db2.full.D", kek, "tenant-x:lost", 4) // kek_unknown
	w.commitUnencrypted(t, "db3", "db3.full.E", 5)                     // unencrypted

	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier: w.verifier,
		KEKResolver: refResolver(map[string][encryption.KeyLen]byte{
			"test:v1": kek,
		}),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Considered != 5 {
		t.Errorf("Considered = %d, want 5", res.Considered)
	}
	if res.OK != 2 {
		t.Errorf("OK = %d, want 2", res.OK)
	}
	if res.UnwrapFailed != 1 {
		t.Errorf("UnwrapFailed = %d, want 1", res.UnwrapFailed)
	}
	if res.KEKUnknown != 1 {
		t.Errorf("KEKUnknown = %d, want 1", res.KEKUnknown)
	}
	if res.Unencrypted != 1 {
		t.Errorf("Unencrypted = %d, want 1", res.Unencrypted)
	}
	if !res.AnyBroken() {
		t.Errorf("AnyBroken = false; want true (have unwrap_failed and kek_unknown)")
	}
	if len(res.Failures) != 2 {
		t.Errorf("Failures = %d, want 2 (unwrap_failed + kek_unknown only)", len(res.Failures))
	}
}

// TestVerifyEnvelopes_FailuresCap: very large fleets with many
// broken manifests don't blow out the result body. The Failures
// slice is capped; counters stay accurate.
func TestVerifyEnvelopes_FailuresCap(t *testing.T) {
	w := setupRotateWorld(t)
	wrongKEK := mkKEK(t)
	realKEK := mkKEK(t)

	// Plant 110 broken manifests (one over the cap of 100).
	const n = 110
	for i := 0; i < n; i++ {
		w.commitEncrypted(t, "db1", fmt.Sprintf("db1.full.%03d", i), realKEK, "test:v1", i)
	}
	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(wrongKEK),
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.UnwrapFailed != n {
		t.Errorf("UnwrapFailed = %d, want %d", res.UnwrapFailed, n)
	}
	// Capped at 100.
	if got := len(res.Failures); got != 100 {
		t.Errorf("Failures = %d, want 100 (capped)", got)
	}
}

// TestVerifyEnvelopes_DeploymentFilter_Empty: a deployment that has
// no committed backups returns considered=0 cleanly. No error.
func TestVerifyEnvelopes_DeploymentFilter_Empty(t *testing.T) {
	w := setupRotateWorld(t)
	res, err := backup.VerifyEnvelopes(context.Background(), w.sp, backup.VerifyEnvelopesOptions{
		Verifier:         w.verifier,
		KEKResolver:      constResolver(mkKEK(t)),
		DeploymentFilter: "no-such-deployment",
	})
	if err != nil {
		t.Fatalf("VerifyEnvelopes: %v", err)
	}
	if res.Considered != 0 {
		t.Errorf("Considered = %d, want 0", res.Considered)
	}
	if res.AnyBroken() {
		t.Errorf("AnyBroken on empty fleet")
	}
}

// TestVerifyEnvelopes_ContextCancellation: a cancelled context
// returns context.Canceled and does NOT inflate counts. The
// partial walk's Considered may be nonzero but the result body is
// internally consistent.
func TestVerifyEnvelopes_ContextCancellation(t *testing.T) {
	w := setupRotateWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.A", kek, "test:v1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call so no work happens

	res, err := backup.VerifyEnvelopes(ctx, w.sp, backup.VerifyEnvelopesOptions{
		Verifier:    w.verifier,
		KEKResolver: constResolver(kek),
	})
	if err == nil {
		t.Errorf("expected ctx.Err, got nil; res=%+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
