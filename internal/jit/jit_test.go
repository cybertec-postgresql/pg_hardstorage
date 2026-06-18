package jit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/jit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// jitWorld is a self-contained fixture: an init'd repo + a
// test signer + a SingleKeyResolver.
type jitWorld struct {
	sp       storage.StoragePlugin
	store    *jit.Store
	signer   testSigner
	resolver *jit.SingleKeyResolver
	repoURL  string
}

// testSigner satisfies jit.Signer with a hand-rolled ed25519
// keypair so tests don't import internal/backup.
type testSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func (s testSigner) Sign(p []byte) []byte         { return ed25519.Sign(s.priv, p) }
func (s testSigner) PublicKey() ed25519.PublicKey { return s.pub }

func setupJITWorld(t *testing.T) *jitWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &jitWorld{
		sp:       sp,
		store:    jit.NewStore(sp),
		signer:   testSigner{priv: priv, pub: pub},
		resolver: &jit.SingleKeyResolver{Key: pub},
		repoURL:  repoURL,
	}
}

// TestIssue_Validation: every required field + bound check.
func TestIssue_Validation(t *testing.T) {
	w := setupJITWorld(t)

	cases := []struct {
		name string
		opts jit.IssueOptions
		want error
	}{
		{"empty principal",
			jit.IssueOptions{Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour},
			jit.ErrPrincipalRequired},
		{"empty reason",
			jit.IssueOptions{Principal: "p", Scope: []string{"x.y"}, Duration: time.Hour},
			jit.ErrMissingReason},
		{"reason too long",
			jit.IssueOptions{Principal: "p", Scope: []string{"x.y"},
				Reason: strings.Repeat("x", jit.MaxReasonLength+1), Duration: time.Hour},
			jit.ErrReasonTooLong},
		{"empty scope",
			jit.IssueOptions{Principal: "p", Scope: nil, Reason: "r", Duration: time.Hour},
			jit.ErrInvalidScope},
		{"scope too long",
			jit.IssueOptions{Principal: "p", Scope: makeScope(jit.MaxScopes + 1),
				Reason: "r", Duration: time.Hour},
			jit.ErrInvalidScope},
		{"duration too short",
			jit.IssueOptions{Principal: "p", Scope: []string{"x.y"},
				Reason: "r", Duration: time.Second},
			jit.ErrInvalidDuration},
		{"duration too long",
			jit.IssueOptions{Principal: "p", Scope: []string{"x.y"},
				Reason: "r", Duration: 1000 * time.Hour},
			jit.ErrInvalidDuration},
		{"scope contains invalid char",
			jit.IssueOptions{Principal: "p", Scope: []string{"x y"},
				Reason: "r", Duration: time.Hour},
			jit.ErrInvalidScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := jit.Issue(w.signer, c.opts)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func makeScope(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "x.y"
	}
	return out
}

// TestIssue_NilSigner
func TestIssue_NilSigner(t *testing.T) {
	if _, err := jit.Issue(nil, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	}); err == nil {
		t.Error("nil signer should error")
	}
}

// TestIssue_HappyPath
func TestIssue_HappyPath(t *testing.T) {
	w := setupJITWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tok, err := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "alice@acme.example",
		Scope:     []string{"kms.shred"},
		Reason:    "GDPR ART17 #4421",
		Duration:  1 * time.Hour,
		IssuedBy:  "bob@acme.example",
		Now:       now,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.ID == "" {
		t.Errorf("ID empty")
	}
	if !tok.IssuedAt.Equal(now) {
		t.Errorf("IssuedAt = %v, want %v", tok.IssuedAt, now)
	}
	if !tok.ExpiresAt.Equal(now.Add(1 * time.Hour)) {
		t.Errorf("ExpiresAt off: %v", tok.ExpiresAt)
	}
	if tok.Signature == "" {
		t.Errorf("Signature empty")
	}
	if tok.PublicKeyFingerprint == "" {
		t.Errorf("Fingerprint empty")
	}
}

// TestIssue_DedupesScope
func TestIssue_DedupesScope(t *testing.T) {
	w := setupJITWorld(t)
	tok, err := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Reason: "r", Duration: time.Hour,
		Scope: []string{"a.b", "a.b", "c.d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Scope) != 2 {
		t.Errorf("Scope = %v, want deduped to 2", tok.Scope)
	}
}

// TestVerify_HappyPath: signature round-trips.
func TestVerify_HappyPath(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := jit.Verify(tok, w.resolver); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestVerify_TamperedSignatureRejected
func TestVerify_TamperedSignatureRejected(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	tok.Reason = "MUTATED"
	if err := jit.Verify(tok, w.resolver); !errors.Is(err, jit.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// TestVerify_DifferentSignerKeyRejected
func TestVerify_DifferentSignerKeyRejected(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	// Resolve via a different key.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	otherResolver := &jit.SingleKeyResolver{Key: otherPub}
	if err := jit.Verify(tok, otherResolver); !errors.Is(err, jit.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// TestEncodeDecode_RoundTrip
func TestEncodeDecode_RoundTrip(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	enc, err := jit.Encode(tok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "/") || strings.Contains(enc, "+") || strings.Contains(enc, "=") {
		t.Errorf("expected base64url (no /, +, =); got %q", enc)
	}
	tok2, err := jit.Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if tok2.ID != tok.ID || tok2.Signature != tok.Signature {
		t.Errorf("round-trip mismatch: %+v vs %+v", tok2, tok)
	}
	// And the decoded token still verifies.
	if err := jit.Verify(tok2, w.resolver); err != nil {
		t.Errorf("decoded token doesn't verify: %v", err)
	}
}

// TestDecode_BadInput
func TestDecode_BadInput(t *testing.T) {
	if _, err := jit.Decode("$$$"); err == nil {
		t.Error("malformed base64 should error")
	}
	if _, err := jit.Decode("aGVsbG8"); err == nil {
		t.Error("non-JSON should error")
	}
}

// TestStore_PutGetRoundTrip
func TestStore_PutGetRoundTrip(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := w.store.Put(context.Background(), tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := w.store.Get(context.Background(), tok.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != tok.ID || got.Reason != tok.Reason {
		t.Errorf("round-trip diff: %+v vs %+v", got, tok)
	}
}

// TestStore_PutDuplicateRefused
func TestStore_PutDuplicateRefused(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := w.store.Put(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Put(context.Background(), tok); err == nil {
		t.Error("duplicate Put should error")
	}
}

// TestStore_GetNotFound
func TestStore_GetNotFound(t *testing.T) {
	w := setupJITWorld(t)
	_, err := w.store.Get(context.Background(), "nonexistent")
	if !errors.Is(err, jit.ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

// TestStore_RevokeRoundTrip
func TestStore_RevokeRoundTrip(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := w.store.Put(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := w.store.Revoke(context.Background(), tok.ID, "alice", "issued by mistake", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked, err := w.store.IsRevoked(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("IsRevoked = false; want true")
	}
	r, err := w.store.GetRevocation(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.RevokedBy != "alice" || r.Reason != "issued by mistake" {
		t.Errorf("revocation body off: %+v", r)
	}
}

// TestStore_DoubleRevokeRefused
func TestStore_DoubleRevokeRefused(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := w.store.Put(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := w.store.Revoke(context.Background(), tok.ID, "alice", "r1", now); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Revoke(context.Background(), tok.ID, "bob", "r2", now); !errors.Is(err, jit.ErrAlreadyRevoked) {
		t.Errorf("err = %v, want ErrAlreadyRevoked", err)
	}
}

// TestStore_RevokeNonexistent
func TestStore_RevokeNonexistent(t *testing.T) {
	w := setupJITWorld(t)
	err := w.store.Revoke(context.Background(), "nonexistent",
		"alice", "r", time.Now().UTC())
	if !errors.Is(err, jit.ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

// TestVerifyAt_HappyPath: signature ok + active + scope match
// + no revocation → success.
func TestVerifyAt_HappyPath(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour,
	})
	_ = w.store.Put(context.Background(), tok)
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "kms.shred",
	})
	if err != nil {
		t.Errorf("VerifyAt: %v", err)
	}
}

// TestVerifyAt_Expired
func TestVerifyAt_Expired(t *testing.T) {
	w := setupJITWorld(t)
	now := time.Now().UTC()
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour, Now: now.Add(-2 * time.Hour),
	})
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "kms.shred",
		Now:       now,
	})
	if !errors.Is(err, jit.ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

// TestVerifyAt_NotYetActive
func TestVerifyAt_NotYetActive(t *testing.T) {
	w := setupJITWorld(t)
	now := time.Now().UTC()
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour, Now: now,
		NotBefore: now.Add(30 * time.Minute),
	})
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "kms.shred",
		Now:       now,
	})
	if !errors.Is(err, jit.ErrTokenNotYetActive) {
		t.Errorf("err = %v, want ErrTokenNotYetActive", err)
	}
}

// TestVerifyAt_Revoked
func TestVerifyAt_Revoked(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour,
	})
	_ = w.store.Put(context.Background(), tok)
	_ = w.store.Revoke(context.Background(), tok.ID, "alice", "test", time.Now().UTC())
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "kms.shred",
	})
	if !errors.Is(err, jit.ErrTokenRevoked) {
		t.Errorf("err = %v, want ErrTokenRevoked", err)
	}
}

// TestVerifyAt_ScopeMismatch
func TestVerifyAt_ScopeMismatch(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour,
	})
	_ = w.store.Put(context.Background(), tok)
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "repo.gc", // not in scope
	})
	if !errors.Is(err, jit.ErrScopeNotMatched) {
		t.Errorf("err = %v, want ErrScopeNotMatched", err)
	}
}

// TestVerifyAt_WildcardScope
func TestVerifyAt_WildcardScope(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.*"}, Reason: "r",
		Duration: time.Hour,
	})
	_ = w.store.Put(context.Background(), tok)
	for _, op := range []string{"kms.shred", "kms.rotate", "kms.verify"} {
		err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
			Operation: op,
		})
		if err != nil {
			t.Errorf("op %q: %v", op, err)
		}
	}
	// Different namespace - should not match.
	if err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "backup.delete",
	}); !errors.Is(err, jit.ErrScopeNotMatched) {
		t.Errorf("backup.delete should not match kms.*; got %v", err)
	}
}

// TestVerifyAt_TenantMismatch
func TestVerifyAt_TenantMismatch(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour, Tenant: "tenant-a",
	})
	_ = w.store.Put(context.Background(), tok)
	err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
		Operation: "kms.shred",
		Tenant:    "tenant-b",
	})
	if !errors.Is(err, jit.ErrTenantMismatch) {
		t.Errorf("err = %v, want ErrTenantMismatch", err)
	}
}

// TestVerifyAt_TenantAgnosticToken
func TestVerifyAt_TenantAgnosticToken(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"kms.shred"}, Reason: "r",
		Duration: time.Hour, // no Tenant
	})
	_ = w.store.Put(context.Background(), tok)
	// Tenant-agnostic token works against any tenant.
	for _, tn := range []string{"", "tenant-a", "tenant-b"} {
		if err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{
			Operation: "kms.shred", Tenant: tn,
		}); err != nil {
			t.Errorf("tenant %q: %v", tn, err)
		}
	}
}

// TestVerifyAt_RequiresOperation
func TestVerifyAt_RequiresOperation(t *testing.T) {
	w := setupJITWorld(t)
	tok, _ := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "p", Scope: []string{"x.y"}, Reason: "r", Duration: time.Hour,
	})
	if err := jit.VerifyAt(context.Background(), w.store, w.resolver, tok, jit.CheckOptions{}); err == nil {
		t.Error("empty operation must error")
	}
}

// TestList_Filtering: returns only the matching tokens.
func TestList_Filtering(t *testing.T) {
	w := setupJITWorld(t)
	now := time.Now().UTC()
	plant := func(principal string, dur time.Duration, when time.Time, revoke bool) string {
		tok, err := jit.Issue(w.signer, jit.IssueOptions{
			Principal: principal, Scope: []string{"x.y"},
			Reason: "r", Duration: dur, Now: when,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.store.Put(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
		if revoke {
			if err := w.store.Revoke(context.Background(), tok.ID, "x", "x", now); err != nil {
				t.Fatal(err)
			}
		}
		return tok.ID
	}
	plant("alice", time.Hour, now, false)                   // active
	plant("alice", time.Hour, now.Add(-2*time.Hour), false) // expired
	plant("bob", time.Hour, now, true)                      // revoked

	all, err := w.store.List(context.Background(), jit.ListFilter{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("List(all) = %d, want 3", len(all))
	}

	active, _ := w.store.List(context.Background(), jit.ListFilter{
		Status: jit.StatusActive, Now: now,
	})
	if len(active) != 1 {
		t.Errorf("List(active) = %d, want 1", len(active))
	}

	expired, _ := w.store.List(context.Background(), jit.ListFilter{
		Status: jit.StatusExpired, Now: now,
	})
	if len(expired) != 1 {
		t.Errorf("List(expired) = %d, want 1", len(expired))
	}

	revoked, _ := w.store.List(context.Background(), jit.ListFilter{
		Status: jit.StatusRevoked, Now: now,
	})
	if len(revoked) != 1 {
		t.Errorf("List(revoked) = %d, want 1", len(revoked))
	}

	byPrincipal, _ := w.store.List(context.Background(), jit.ListFilter{
		Principal: "bob", Now: now,
	})
	if len(byPrincipal) != 1 {
		t.Errorf("List(bob) = %d, want 1", len(byPrincipal))
	}
}

// TestList_NewestFirst: results sort by IssuedAt descending.
func TestList_NewestFirst(t *testing.T) {
	w := setupJITWorld(t)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		tok, _ := jit.Issue(w.signer, jit.IssueOptions{
			Principal: "p", Scope: []string{"x.y"}, Reason: "r",
			Duration: time.Hour, Now: now.Add(-time.Duration(i) * time.Hour),
		})
		_ = w.store.Put(context.Background(), tok)
	}
	out, _ := w.store.List(context.Background(), jit.ListFilter{Now: now})
	if len(out) != 4 {
		t.Fatalf("count = %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Token.IssuedAt.After(out[i-1].Token.IssuedAt) {
			t.Errorf("not newest-first at %d", i)
		}
	}
}

// TestStatus_Transitions: not_yet → active → expired.
func TestStatus_Transitions(t *testing.T) {
	now := time.Now().UTC()
	tok := &jit.Token{
		IssuedAt:  now,
		NotBefore: now.Add(1 * time.Hour),
		ExpiresAt: now.Add(2 * time.Hour),
	}
	if got := tok.Status(now); got != jit.StatusNotYetActive {
		t.Errorf("at issuance: %q", got)
	}
	if got := tok.Status(now.Add(90 * time.Minute)); got != jit.StatusActive {
		t.Errorf("during active window: %q", got)
	}
	if got := tok.Status(now.Add(3 * time.Hour)); got != jit.StatusExpired {
		t.Errorf("after expiry: %q", got)
	}
}

// TestMatchesScope: exact + wildcard.
func TestMatchesScope(t *testing.T) {
	t1 := &jit.Token{Scope: []string{"backup.delete"}}
	if !t1.MatchesScope("backup.delete") {
		t.Error("exact match")
	}
	if t1.MatchesScope("backup.create") {
		t.Error("non-match should fail")
	}

	t2 := &jit.Token{Scope: []string{"kms.*"}}
	for _, op := range []string{"kms.shred", "kms.rotate"} {
		if !t2.MatchesScope(op) {
			t.Errorf("kms.* should match %q", op)
		}
	}
	if t2.MatchesScope("backup.delete") {
		t.Error("kms.* should not match backup.delete")
	}

	t3 := &jit.Token{Scope: []string{"*"}}
	if !t3.MatchesScope("anything.at.all") {
		t.Error("* should match anything")
	}
}
