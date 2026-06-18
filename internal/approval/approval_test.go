package approval_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// newApprovalStore builds a fresh fs-backed store rooted at t.TempDir().
// Same pattern as the audit package's tests.
func newApprovalStore(t *testing.T) (*approval.Store, storage.StoragePlugin) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return approval.NewStore(sp), sp
}

// genKey returns a fresh ed25519 keypair plus the public-key PEM
// (the shape that goes into Request.ApproverKeys).
func genKey(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PG_HARDSTORAGE ED25519 PUBLIC KEY", Bytes: der})
	return priv, pubPEM
}

// TestCreate_Persists asserts a Request lands at approvals/<id>.json
// with the expected fields populated.
func TestCreate_Persists(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)
	_, pubB := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Initiator:    "ops@acme.example",
		Target:       "db1.full.20260427T0900Z",
		Reason:       "Old monthly retention",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.ID == "" {
		t.Error("ID should be set")
	}
	if req.Op != "backup.delete" {
		t.Errorf("Op = %q", req.Op)
	}
	if req.Threshold != 2 {
		t.Errorf("Threshold = %d", req.Threshold)
	}
	if len(req.ApproverKeys) != 2 {
		t.Errorf("ApproverKeys len = %d", len(req.ApproverKeys))
	}
	if req.ExpiresAt.Before(req.CreatedAt) || req.ExpiresAt.Equal(req.CreatedAt) {
		t.Errorf("ExpiresAt should be after CreatedAt")
	}

	// Round-trip via Get.
	got, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != req.ID || got.Op != req.Op {
		t.Errorf("Get round-trip lost data: %+v", got)
	}
}

// TestCreate_RefusesUnreachableThreshold asserts we reject a request
// whose threshold exceeds the supplied approver-key count — a request
// that no amount of approval can satisfy.
func TestCreate_RefusesUnreachableThreshold(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)
	_, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "kms.shred",
		Threshold:    3,
		ApproverKeys: [][]byte{pubA}, // only one key for a threshold of 3
	})
	if err == nil {
		t.Fatal("expected refusal for unreachable threshold")
	}
}

// TestApprove_HappyPath drives the full flow: create → A approves →
// status is still pending (threshold 2) → B approves → status is
// approved.
func TestApprove_HappyPath(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)
	privB, pubB := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Initial status = pending.
	st, err := store.StatusOf(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != approval.StatusPending {
		t.Errorf("initial status = %q, want pending", st)
	}

	// A approves.
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice@acme", "ok by me"); err != nil {
		t.Fatal(err)
	}
	st, _ = store.StatusOf(context.Background(), req.ID)
	if st != approval.StatusPending {
		t.Errorf("after A: status = %q, want still pending", st)
	}

	// B approves.
	if _, err := store.Approve(context.Background(), req.ID, privB, "bob@acme", "verified"); err != nil {
		t.Fatal(err)
	}
	st, _ = store.StatusOf(context.Background(), req.ID)
	if st != approval.StatusApproved {
		t.Errorf("after B: status = %q, want approved", st)
	}

	// Verify the count via VerifyApprovals.
	final, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	count, err := approval.VerifyApprovals(final)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("VerifyApprovals = %d, want 2", count)
	}
}

// TestApprove_DoubleApproveSameKeyIsIdempotent — same approver
// signing twice must not count as two votes.
func TestApprove_DoubleApproveSameKeyIsIdempotent(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)
	_, pubB := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A approves twice.
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", "second"); err != nil {
		t.Fatal(err)
	}

	// Status must still be pending — A's second approval doesn't
	// contribute a second vote.
	st, _ := store.StatusOf(context.Background(), req.ID)
	if st != approval.StatusPending {
		t.Errorf("status = %q, want pending (double-approve must be idempotent)", st)
	}
	final, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Approvals) != 1 {
		t.Errorf("Approvals len = %d, want 1 (de-duped on key fingerprint)", len(final.Approvals))
	}
}

// TestApprove_RefusesNonAllowlistedKey — the trust-foundation test:
// an approver who isn't in ApproverKeys cannot land an approval, even
// if their signature is structurally valid.
func TestApprove_RefusesNonAllowlistedKey(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)
	_, pubB := genKey(t)
	privUnlisted, _ := genKey(t) // NOT in the allowlist

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Approve(context.Background(), req.ID, privUnlisted, "mallory@acme", "lol")
	if !errors.Is(err, approval.ErrApproverNotAllowed) {
		t.Errorf("got %v, want ErrApproverNotAllowed", err)
	}
}

// TestApprove_RefusesExpiredRequest — past-TTL requests cannot be
// approved.
func TestApprove_RefusesExpiredRequest(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)
	_, pubB := genKey(t)

	// Create with a tiny TTL, then sleep past it.
	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	_, err = store.Approve(context.Background(), req.ID, privA, "alice", "")
	if !errors.Is(err, approval.ErrExpired) {
		t.Errorf("got %v, want ErrExpired", err)
	}
	st, _ := store.StatusOf(context.Background(), req.ID)
	if st != approval.StatusExpired {
		t.Errorf("status = %q, want expired", st)
	}
}

// TestRevoke_TerminalIsApproved — once a request is revoked it
// cannot be approved any further; status flips to revoked even with
// existing approvals.
func TestRevoke(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)
	_, pubB := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    2,
		ApproverKeys: [][]byte{pubA, pubB},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(context.Background(), req.ID, "ops-lead@acme", "wrong target"); err != nil {
		t.Fatal(err)
	}

	st, _ := store.StatusOf(context.Background(), req.ID)
	if st != approval.StatusRevoked {
		t.Errorf("status = %q, want revoked", st)
	}
	// Idempotent.
	if _, err := store.Revoke(context.Background(), req.ID, "ops-lead@acme", "again"); err != nil {
		t.Errorf("re-revoke should be idempotent; got %v", err)
	}
}

// TestList_FiltersByOpAndStatus exercises the listing surface.
func TestList_FiltersByOpAndStatus(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)

	// Create three: two backup.delete, one kms.shred.
	for i := 0; i < 2; i++ {
		if _, err := store.Create(context.Background(), approval.CreateOptions{
			Op:           "backup.delete",
			Threshold:    1,
			ApproverKeys: [][]byte{pubA},
			TTL:          time.Hour,
		}); err != nil {
			t.Fatal(err)
		}
		// IDs are second-precision; sleep so two creations don't
		// produce the same prefix.
		time.Sleep(1100 * time.Millisecond)
	}
	if _, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "kms.shred",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	}); err != nil {
		t.Fatal(err)
	}

	// Filter by op.
	got, err := store.List(context.Background(), approval.ListFilters{Op: "backup.delete"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("op filter returned %d, want 2", len(got))
	}
	got, err = store.List(context.Background(), approval.ListFilters{Op: "kms.shred"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("kms filter returned %d, want 1", len(got))
	}
}

// TestApprove_TamperedRequestBytesInvalidatesSignature is the
// trust-foundation property: if anything in the request changes
// after creation, the approver's signature won't verify and the
// approval doesn't count.
func TestApprove_TamperedSignatureDoesNotCount(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}

	// Tamper: directly mutate the in-memory Request and verify.
	final, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	final.Op = "kms.shred" // forge the op
	count, err := approval.VerifyApprovals(final)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("VerifyApprovals on tampered request = %d, want 0 (signature must invalidate)", count)
	}
}

// TestGet_NotFound returns the sentinel.
func TestGet_NotFound(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, err := store.Get(context.Background(), "appr-doesnotexist")
	if !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestGate_Approved is the happy path: a fully-approved request
// matching the op + target lets the gate through.
func TestGate_Approved(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "repo.set_mode",
		Target:       "file:///srv/repo",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}

	got, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID,
		Op:        "repo.set_mode",
		Target:    "file:///srv/repo",
	})
	if err != nil {
		t.Fatalf("Gate should pass on approved + matched request: %v", err)
	}
	if got.ID != req.ID {
		t.Errorf("Gate returned wrong request: %q vs %q", got.ID, req.ID)
	}
}

// TestGate_RefusesOpMismatch is the trust-foundation property: an
// approval for "backup.delete" must NOT redeem against "kms.shred".
func TestGate_RefusesOpMismatch(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Target:       "db1.full.x",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}

	// Try to redeem the backup.delete approval for kms.shred.
	_, err = store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID,
		Op:        "kms.shred",
		Target:    "db1.full.x",
	})
	if !errors.Is(err, approval.ErrOpMismatch) {
		t.Errorf("got %v, want ErrOpMismatch", err)
	}
}

// TestGate_RefusesTargetMismatch — same posture: approval for db1
// must not redeem against db2.
func TestGate_RefusesTargetMismatch(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Target:       "db1.full.x",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}

	_, err = store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID,
		Op:        "backup.delete",
		Target:    "db2.full.y",
	})
	if !errors.Is(err, approval.ErrTargetMismatch) {
		t.Errorf("got %v, want ErrTargetMismatch", err)
	}
}

// TestGate_RefusesPending — an unapproved request must not pass the
// gate.
func TestGate_RefusesPending(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID,
		Op:        "backup.delete",
	})
	if !errors.Is(err, approval.ErrThresholdNotMet) {
		t.Errorf("got %v, want ErrThresholdNotMet", err)
	}
}

// TestGate_RefusesRevoked — a revoked request, even if previously
// approved, must not pass the gate.
func TestGate_RefusesRevoked(t *testing.T) {
	store, _ := newApprovalStore(t)
	privA, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), req.ID, privA, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(context.Background(), req.ID, "ops-lead", "wrong target"); err != nil {
		t.Fatal(err)
	}

	_, err = store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID,
		Op:        "backup.delete",
	})
	if !errors.Is(err, approval.ErrRevoked) {
		t.Errorf("got %v, want ErrRevoked", err)
	}
}
