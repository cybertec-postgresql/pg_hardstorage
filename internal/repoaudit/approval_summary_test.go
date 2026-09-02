package repoaudit_test

// The approval rollup must count the lifecycle states it claims to
// count.
//
// summarizeApprovals documented itself as classifying pending /
// approved / expired / revoked, and did not: it counted keys and
// returned Total alone. Because the four state fields are omitempty
// they did not even render as zero -- they vanished from the report, so
// a repository full of unresolved approvals audited as a bare
// {"total":N}, and any consumer reading the struct saw "0 pending".
// This is a compliance-evidence report; "0 pending approvals" is a claim
// that every privileged operation has been resolved.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repoaudit"
)

func approvalKey(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pem.EncodeToMemory(&pem.Block{
		Type: "PG_HARDSTORAGE ED25519 PUBLIC KEY", Bytes: der,
	})
}

// approvalWorld is a repo with an approval store wired to it.
func approvalWorld(t *testing.T) (storage.StoragePlugin, *approval.Store, *repo.Metadata, string, *backup.Verifier) {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	res, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL})
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	_, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sp, approval.NewStore(sp), &res.Metadata, repoURL, ver
}

func TestAudit_ApprovalSummary_CountsEveryLifecycleState(t *testing.T) {
	ctx := context.Background()
	sp, store, meta, repoURL, ver := approvalWorld(t)
	priv, pubPEM := approvalKey(t)

	mk := func(ttl time.Duration) *approval.Request {
		t.Helper()
		r, err := store.Create(ctx, approval.CreateOptions{
			Op:           "backup.delete",
			Initiator:    "op@example.com",
			Target:       "db1",
			Threshold:    1,
			ApproverKeys: [][]byte{pubPEM},
			TTL:          ttl,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return r
	}

	// pending: live, unvoted.
	mk(24 * time.Hour)
	// approved: quorum reached.
	app := mk(24 * time.Hour)
	if _, err := store.Approve(ctx, app.ID, priv, "approver-1", "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// revoked: explicitly cancelled.
	rev := mk(24 * time.Hour)
	if _, err := store.Revoke(ctx, rev.ID, "sec@example.com", "no longer needed"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// expired: a TTL so short it is behind us by the time we audit.
	// (Create defaults a non-positive TTL to 24h, so it has to be a
	// real positive duration.)
	mk(time.Nanosecond)

	rep, err := repoaudit.Audit(ctx, sp, meta, repoURL, repoaudit.Options{Verifier: ver})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if rep.Approvals == nil {
		t.Fatal("audit produced no approvals section for a repo with four approval requests")
	}
	got := rep.Approvals

	if got.Total != 4 {
		t.Errorf("Total = %d, want 4", got.Total)
	}
	for _, c := range []struct {
		name string
		got  int
	}{
		{"Pending", got.Pending},
		{"Approved", got.Approved},
		{"Revoked", got.Revoked},
		{"Expired", got.Expired},
	} {
		if c.got != 1 {
			t.Errorf("%s = %d, want 1.\n\nfull summary: %+v\n\n"+
				"A lifecycle counter that never counts is worse than an absent one: "+
				"these fields are omitempty, so a zero renders as silence and the "+
				"report reads as though nothing is outstanding.", c.name, c.got, *got)
		}
	}
	if got.Unreadable != 0 {
		t.Errorf("Unreadable = %d, want 0 (every body was written by the store itself)", got.Unreadable)
	}
	if got.Incomplete {
		t.Errorf("Incomplete set on a clean walk: %q", got.Error)
	}

	// The invariant that makes the section self-checking.
	sum := got.Pending + got.Approved + got.Expired + got.Revoked + got.Unreadable
	if sum != got.Total {
		t.Errorf("states sum to %d but Total = %d; every listed body must land in exactly "+
			"one bucket or the report loses requests without saying so", sum, got.Total)
	}
}

// A body that will not load must be COUNTED, not dropped. Silently
// skipping it makes an unreadable approval indistinguishable from one
// that was never created -- the failure mode this whole audit keeps
// finding: a check that could not run, reported as a check that passed.
func TestAudit_ApprovalSummary_CountsUnreadableBodies(t *testing.T) {
	ctx := context.Background()
	sp, store, meta, repoURL, ver := approvalWorld(t)
	_, pubPEM := approvalKey(t)

	r, err := store.Create(ctx, approval.CreateOptions{
		Op:           "backup.delete",
		Initiator:    "op@example.com",
		Target:       "db1",
		Threshold:    1,
		ApproverKeys: [][]byte{pubPEM},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Corrupt the body in place, leaving the key listed.
	key := "approvals/" + r.ID + ".json"
	if _, err := sp.Put(ctx, key, bytes.NewReader([]byte("{ this is not json")), storage.PutOptions{}); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	rep, err := repoaudit.Audit(ctx, sp, meta, repoURL, repoaudit.Options{Verifier: ver})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if rep.Approvals == nil {
		t.Fatal("no approvals section")
	}
	got := rep.Approvals
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1", got.Total)
	}
	if got.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1.\n\nsummary: %+v\n\n"+
			"An approval whose body will not parse has an UNKNOWN lifecycle position. "+
			"Dropping it lets a corrupt or tampered request disappear from the "+
			"compliance record entirely.", got.Unreadable, *got)
	}
	if got.Pending != 0 {
		t.Errorf("Pending = %d; an unloadable body must not be classified as pending, "+
			"which would assert a lifecycle position the repository cannot support", got.Pending)
	}
	if sum := got.Pending + got.Approved + got.Expired + got.Revoked + got.Unreadable; sum != got.Total {
		t.Errorf("states sum to %d, Total = %d", sum, got.Total)
	}
}
