package approval_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
)

// trustRoster writes pubs into a fresh operator roster directory and
// points the gate's policy at it via the documented environment
// variables. minThreshold "" leaves the default in force.
func trustRoster(t *testing.T, minThreshold string, pubs ...[]byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "approvers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, p := range pubs {
		if err := os.WriteFile(filepath.Join(dir, "k"+string(rune('a'+i))+".pem"), p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PG_HARDSTORAGE_APPROVAL_ROSTER", dir)
	t.Setenv("PG_HARDSTORAGE_APPROVAL_MIN_THRESHOLD", minThreshold)
}

// approvedRequest creates a request for op/target and collects every
// signer's vote.
func approvedRequest(t *testing.T, store *approval.Store, op approval.Op, target string, threshold int, ttl time.Duration, signers ...approverKey) *approval.Request {
	t.Helper()
	pubs := make([][]byte, 0, len(signers))
	for _, s := range signers {
		pubs = append(pubs, s.pub)
	}
	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           op,
		Target:       target,
		Threshold:    threshold,
		ApproverKeys: pubs,
		TTL:          ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range signers {
		if _, err := store.Approve(context.Background(), req.ID, s.priv, "approver-"+string(rune('a'+i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	return req
}

// TestGate_TargetlessRequestDoesNotAuthoriseATarget pins C6's first
// leg: a request created WITHOUT a target used to skip the target
// binding entirely, so one target-less approval for repo.wipe redeemed
// against any repository. The gate must demand an exact match.
func TestGate_TargetlessRequestDoesNotAuthoriseATarget(t *testing.T) {
	a := newApproverKey(t)
	trustRoster(t, "1", a.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "repo.wipe", "", 1, time.Hour, a)

	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID, Op: "repo.wipe", Target: "file:///srv/prod-repo",
	}); !errors.Is(err, approval.ErrTargetMismatch) {
		t.Fatalf("a target-less approval against a concrete target: got %v, want ErrTargetMismatch", err)
	}
}

// TestGate_ApprovalIsSingleUse pins C6's second leg: an approved
// request used to authorise the destructive op an unlimited number of
// times. The first redemption must consume it.
func TestGate_ApprovalIsSingleUse(t *testing.T) {
	a := newApproverKey(t)
	trustRoster(t, "1", a.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "backup.delete", "db1.full.x", 1, time.Hour, a)

	opts := approval.GateOptions{RequestID: req.ID, Op: "backup.delete", Target: "db1.full.x"}
	if _, err := store.Gate(context.Background(), opts); err != nil {
		t.Fatalf("first redemption must pass: %v", err)
	}
	if _, err := store.Gate(context.Background(), opts); !errors.Is(err, approval.ErrConsumed) {
		t.Fatalf("second redemption: got %v, want ErrConsumed (approvals are single-use)", err)
	}
	got, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsumedAt == nil {
		t.Error("Get does not report the consumption")
	}
}

// TestGate_SingleUseUnderRace: concurrent redemptions of one approval
// must let exactly one through — the consumption marker is written
// IfNotExists, not read-then-written.
func TestGate_SingleUseUnderRace(t *testing.T) {
	a := newApproverKey(t)
	trustRoster(t, "1", a.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "backup.delete", "db1.full.x", 1, time.Hour, a)

	const n = 8
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Gate(context.Background(), approval.GateOptions{
				RequestID: req.ID, Op: "backup.delete", Target: "db1.full.x",
			}); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := ok.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent redemptions succeeded, want exactly 1", got, n)
	}
}

// TestGate_ApprovedRequestStillExpires pins C6's third leg: quorum was
// checked before the TTL, so an approved request never expired and a
// year-old approval stayed redeemable.
func TestGate_ApprovedRequestStillExpires(t *testing.T) {
	a := newApproverKey(t)
	trustRoster(t, "1", a.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "kms.shred", "/keyring", 1, 150*time.Millisecond, a)
	time.Sleep(250 * time.Millisecond)

	if st, _ := store.StatusOf(context.Background(), req.ID); st != approval.StatusExpired {
		t.Errorf("status of an approved request past its TTL = %q, want expired", st)
	}
	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID, Op: "kms.shred", Target: "/keyring",
	}); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("approval past its TTL: got %v, want ErrExpired", err)
	}
}

// TestGate_RefusesSelfChosenRoster pins H17: the request creator picks
// the approver keys and the threshold, so anyone with repo write could
// file a 1-of-1 request naming their own key, approve it and redeem it.
// Only votes from the operator's trusted roster count.
func TestGate_RefusesSelfChosenRoster(t *testing.T) {
	trusted := newApproverKey(t)
	attacker := newApproverKey(t)
	trustRoster(t, "1", trusted.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "repo.wipe", "file:///srv/repo", 1, time.Hour, attacker)

	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID, Op: "repo.wipe", Target: "file:///srv/repo",
	}); !errors.Is(err, approval.ErrUntrustedApprovals) {
		t.Fatalf("self-approved request outside the roster: got %v, want ErrUntrustedApprovals", err)
	}
}

// TestGate_RefusesBelowConfiguredMinimum: with the default policy
// (minimum two approvals) a 1-of-1 request signed by a trusted key is
// still not enough.
func TestGate_RefusesBelowConfiguredMinimum(t *testing.T) {
	a, b := newApproverKey(t), newApproverKey(t)
	trustRoster(t, "", a.pub, b.pub)
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "repo.gc", "file:///srv/repo", 1, time.Hour, a)

	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID, Op: "repo.gc", Target: "file:///srv/repo",
	}); !errors.Is(err, approval.ErrUntrustedApprovals) {
		t.Fatalf("1-of-1 under a minimum of 2: got %v, want ErrUntrustedApprovals", err)
	}

	ok := approvedRequest(t, store, "repo.gc", "file:///srv/repo", 2, time.Hour, a, b)
	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: ok.ID, Op: "repo.gc", Target: "file:///srv/repo",
	}); err != nil {
		t.Fatalf("a 2-of-2 approval from trusted keys must pass: %v", err)
	}
}

// TestGate_NoRosterFailsClosed: without an operator roster there is no
// trust anchor, and the gate must refuse rather than fall back to the
// request's self-declared keys.
func TestGate_NoRosterFailsClosed(t *testing.T) {
	a := newApproverKey(t)
	t.Setenv("PG_HARDSTORAGE_APPROVAL_ROSTER", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PG_HARDSTORAGE_APPROVAL_MIN_THRESHOLD", "1")
	store, _ := newApprovalStore(t)
	req := approvedRequest(t, store, "backup.delete", "db1.full.x", 1, time.Hour, a)

	if _, err := store.Gate(context.Background(), approval.GateOptions{
		RequestID: req.ID, Op: "backup.delete", Target: "db1.full.x",
	}); !errors.Is(err, approval.ErrNoTrustedRoster) {
		t.Fatalf("no roster configured: got %v, want ErrNoTrustedRoster", err)
	}
}

// TestCreate_WithPolicyRefusesDoomedRequests: a store carrying the
// operator policy refuses, at request time, what Gate would refuse at
// redemption — untrusted approver keys and sub-minimum thresholds.
func TestCreate_WithPolicyRefusesDoomedRequests(t *testing.T) {
	trusted, stranger := newApproverKey(t), newApproverKey(t)
	pol, err := approval.NewPolicy(2, trusted.pub)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := newApprovalStore(t)
	store.WithPolicy(pol)

	_, err = store.Create(context.Background(), approval.CreateOptions{
		Op: "repo.wipe", Target: "x", Threshold: 2, ApproverKeys: [][]byte{trusted.pub, stranger.pub},
	})
	if !errors.Is(err, approval.ErrUntrustedApprover) {
		t.Errorf("untrusted approver key: got %v, want ErrUntrustedApprover", err)
	}
	_, err = store.Create(context.Background(), approval.CreateOptions{
		Op: "repo.wipe", Target: "x", Threshold: 1, ApproverKeys: [][]byte{trusted.pub},
	})
	if !errors.Is(err, approval.ErrBelowMinThreshold) {
		t.Errorf("threshold below minimum: got %v, want ErrBelowMinThreshold", err)
	}
}

// TestLoadPolicy_RosterFileAndBadMinimum: the roster may be a single
// file of concatenated PEM blocks; a malformed minimum is an error,
// not a silent fallback to the default.
func TestLoadPolicy_RosterFileAndBadMinimum(t *testing.T) {
	a, b := newApproverKey(t), newApproverKey(t)
	f := filepath.Join(t.TempDir(), "roster.pem")
	if err := os.WriteFile(f, append(append([]byte{}, a.pub...), b.pub...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_HARDSTORAGE_APPROVAL_ROSTER", f)
	t.Setenv("PG_HARDSTORAGE_APPROVAL_MIN_THRESHOLD", "")
	pol, err := approval.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.TrustedKeys) != 2 || pol.MinThreshold != approval.DefaultMinThreshold {
		t.Errorf("policy = %d keys, min %d; want 2 keys, min %d", len(pol.TrustedKeys), pol.MinThreshold, approval.DefaultMinThreshold)
	}
	t.Setenv("PG_HARDSTORAGE_APPROVAL_MIN_THRESHOLD", "0")
	if _, err := approval.LoadPolicy(); err == nil {
		t.Error("a minimum of 0 must be refused")
	}
}

type approverKey struct {
	priv ed25519.PrivateKey
	pub  []byte
}

func newApproverKey(t *testing.T) approverKey {
	priv, pub := genKey(t)
	return approverKey{priv: priv, pub: pub}
}
