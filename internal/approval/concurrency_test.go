package approval_test

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
)

// TestApprove_ConcurrentApproversAllLand exercises the
// race-condition fix: two approvers run Approve concurrently
// against the same request. Before the fix, each Approve did a
// read-modify-write against `approvals/<id>.json`; the second
// Put would overwrite the first approver's signature. With the
// per-approver-key layout each approver writes a distinct key
// (`approvals/<id>/approvers/<fp>.json`) with IfNotExists, so
// neither vote can lose.
//
// We launch N goroutines each calling Approve with a distinct
// approver key, wait for all to finish, then assert every vote
// is present.
func TestApprove_ConcurrentApproversAllLand(t *testing.T) {
	store, _ := newApprovalStore(t)

	const N = 8
	privs := make([]ed25519.PrivateKey, N)
	pubPEMs := make([][]byte, N)
	for i := 0; i < N; i++ {
		privs[i], pubPEMs[i] = genKey(t)
	}

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Initiator:    "ops@acme.example",
		Target:       "db1.full.race",
		Threshold:    N,
		ApproverKeys: pubPEMs,
		TTL:          1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := store.Approve(context.Background(), req.ID, privs[i],
				"approver-"+string(rune('A'+i)), "concurrent-test")
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Approve returned err: %v", err)
	}

	got, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Approvals) != N {
		t.Fatalf("got %d approvals, want %d (signatures lost to the read-modify-write race)",
			len(got.Approvals), N)
	}
	// Every fingerprint must be unique — sanity that each
	// approver's vote is present exactly once.
	seen := map[string]struct{}{}
	for _, a := range got.Approvals {
		if _, dup := seen[a.KeyFingerprint]; dup {
			t.Errorf("duplicate fingerprint in Approvals: %s", a.KeyFingerprint)
		}
		seen[a.KeyFingerprint] = struct{}{}
	}
	if len(seen) != N {
		t.Errorf("unique fingerprints = %d, want %d", len(seen), N)
	}
}

// TestApprove_SameApproverIdempotentUnderRace: the same approver
// fires Approve twice concurrently. Both calls succeed (no error
// surface to the operator), the post-state has exactly one vote
// for that fingerprint. Validates the IfNotExists-loses-race →
// "already approved" idempotency mapping.
func TestApprove_SameApproverIdempotentUnderRace(t *testing.T) {
	store, _ := newApprovalStore(t)
	priv, pub := genKey(t)
	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Initiator:    "ops",
		Target:       "x",
		Threshold:    1,
		ApproverKeys: [][]byte{pub},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.Approve(context.Background(), req.ID, priv, "alice", "race"); err != nil {
				t.Errorf("Approve: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Approvals) != 1 {
		t.Errorf("got %d approvals, want 1 (idempotent re-approval)", len(got.Approvals))
	}
}

// TestApprove_PerApproverKeyShape: a successful Approve writes
// one file at `approvals/<id>/approvers/<fp>.json`. Pins the
// on-disk layout so a regression that reverts to the in-line
// Approvals slice surfaces as a test failure here.
func TestApprove_PerApproverKeyShape(t *testing.T) {
	store, sp := newApprovalStore(t)
	priv, pub := genKey(t)
	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "kms.shred",
		Initiator:    "ops",
		Threshold:    1,
		ApproverKeys: [][]byte{pub},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, priv, "alice", "ok"); err != nil {
		t.Fatal(err)
	}

	prefix := "approvals/" + req.ID + "/approvers/"
	count := 0
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatal(err)
		}
		if info.Key == "" {
			continue
		}
		count++
	}
	if count != 1 {
		t.Errorf("expected exactly 1 per-approver-key file under %s, got %d", prefix, count)
	}
}
