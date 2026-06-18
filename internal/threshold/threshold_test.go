package threshold_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// signerFromKey is a tiny test signer wrapping a raw ed25519 keypair.
// Production code uses backup.Signer; the threshold package only
// needs the {Sign, PublicKey} interface.
type signerFromKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s signerFromKey) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s signerFromKey) PublicKey() ed25519.PublicKey { return s.pub }

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func mustMember(t *testing.T, signer string) (threshold.Member, signerFromKey) {
	t.Helper()
	pub, priv := mustKeypair(t)
	return threshold.NewMember(signer, pub), signerFromKey{pub: pub, priv: priv}
}

// makeRoster wires up alice/bob/charlie and admin-signs the roster.
// Returns the roster and the three signers.
func makeRoster(t *testing.T, threshold_ int) (
	*threshold.Roster,
	signerFromKey, signerFromKey, signerFromKey,
) {
	t.Helper()
	alice, aliceSigner := mustMember(t, "alice@acme.example")
	bob, bobSigner := mustMember(t, "bob@acme.example")
	charlie, charlieSigner := mustMember(t, "charlie@acme.example")

	r := threshold.NewRoster("prod-admins", "Production cluster admins",
		threshold_,
		[]threshold.Member{alice, bob, charlie},
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	)
	if err := threshold.SignRoster(r, aliceSigner, "alice@acme.example"); err != nil {
		t.Fatalf("SignRoster: %v", err)
	}
	return r, aliceSigner, bobSigner, charlieSigner
}

// makeStorage spins up a file-backed storage plugin pointed at a fresh
// repo so the tests exercise the real Stat/Get/Put/RenameIfNotExists
// codepaths.
func makeStorage(t *testing.T) storage.StoragePlugin {
	t.Helper()
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	u, _ := url.Parse(repoURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("storage open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// ----- ValidateRoster + signing -----

func TestValidateRoster_Empty(t *testing.T) {
	r := &threshold.Roster{ID: "x"}
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidMembers) {
		t.Errorf("err = %v, want ErrInvalidMembers", err)
	}
}

func TestValidateRoster_BadID(t *testing.T) {
	pub, _ := mustKeypair(t)
	r := threshold.NewRoster("UPPER_CASE", "", 1,
		[]threshold.Member{threshold.NewMember("alice", pub)},
		time.Now())
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
}

func TestValidateRoster_ThresholdOutOfRange(t *testing.T) {
	pub, _ := mustKeypair(t)
	r := threshold.NewRoster("x", "", 5,
		[]threshold.Member{threshold.NewMember("alice", pub)},
		time.Now())
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidThreshold) {
		t.Errorf("err = %v, want ErrInvalidThreshold", err)
	}
	r.Threshold = 0
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidThreshold) {
		t.Errorf("zero threshold err = %v, want ErrInvalidThreshold", err)
	}
}

func TestValidateRoster_DuplicateFingerprint(t *testing.T) {
	pub, _ := mustKeypair(t)
	r := threshold.NewRoster("x", "", 1, []threshold.Member{
		threshold.NewMember("alice", pub),
		threshold.NewMember("alice-alias", pub),
	}, time.Now())
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidMembers) {
		t.Errorf("err = %v, want ErrInvalidMembers", err)
	}
}

func TestValidateRoster_FingerprintPubkeyMismatch(t *testing.T) {
	pub, _ := mustKeypair(t)
	other, _ := mustKeypair(t)
	r := threshold.NewRoster("x", "", 1, []threshold.Member{
		{Signer: "alice",
			PublicKey:            base64.StdEncoding.EncodeToString(pub),
			PublicKeyFingerprint: threshold.PublicKeyFingerprint(other),
		},
	}, time.Now())
	if err := threshold.ValidateRoster(r); !errors.Is(err, threshold.ErrInvalidMember) {
		t.Errorf("err = %v, want ErrInvalidMember", err)
	}
}

func TestSignRoster_RoundTrip(t *testing.T) {
	r, _, _, _ := makeRoster(t, 2)
	if r.Signature == "" {
		t.Errorf("Signature empty after SignRoster")
	}
	if err := threshold.VerifyRoster(r); err != nil {
		t.Errorf("VerifyRoster: %v", err)
	}
}

func TestVerifyRoster_TamperedMembers(t *testing.T) {
	r, _, _, _ := makeRoster(t, 2)
	// Swap one member's signer ID — this changes canonical bytes.
	r.Members[0].Signer = "mallory@acme.example"
	if err := threshold.VerifyRoster(r); !errors.Is(err, threshold.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerifyRoster_TamperedThreshold(t *testing.T) {
	r, _, _, _ := makeRoster(t, 2)
	r.Threshold = 1
	if err := threshold.VerifyRoster(r); !errors.Is(err, threshold.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// TestRosterHash_StableAcrossOrder asserts member-list reordering
// doesn't change the canonical hash (members are sorted internally).
func TestRosterHash_StableAcrossOrder(t *testing.T) {
	r1, _, _, _ := makeRoster(t, 2)
	r2 := *r1
	r2.Members = []threshold.Member{r1.Members[2], r1.Members[0], r1.Members[1]}
	if threshold.RosterHash(&r2) != threshold.RosterHash(r1) {
		t.Errorf("hash differs after reorder")
	}
}

// ----- attestation signing + verification -----

func TestSignAttestation_NoMatch(t *testing.T) {
	r, _, _, _ := makeRoster(t, 2)
	pub, priv := mustKeypair(t) // not in roster
	mallory := signerFromKey{pub: pub, priv: priv}
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	_, err := threshold.SignAttestation(subject, r, mallory, "", time.Now())
	if !errors.Is(err, threshold.ErrLocalKeyDoesNotMatchMember) {
		t.Errorf("err = %v, want ErrLocalKeyDoesNotMatchMember", err)
	}
}

func TestSignAttestation_AsMismatch(t *testing.T) {
	r, _, bobSigner, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	// bob's local key but claims to be alice.
	_, err := threshold.SignAttestation(subject, r, bobSigner, "alice@acme.example", time.Now())
	if !errors.Is(err, threshold.ErrLocalKeyDoesNotMatchMember) {
		t.Errorf("err = %v, want ErrLocalKeyDoesNotMatchMember", err)
	}
}

func TestSignAttestation_HappyPath(t *testing.T) {
	r, aliceSigner, _, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig, err := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	if err != nil {
		t.Fatalf("SignAttestation: %v", err)
	}
	if sig.Signer != "alice@acme.example" {
		t.Errorf("Signer = %q, want alice", sig.Signer)
	}
	if sig.Subject.Hash != "abc123" {
		t.Errorf("Subject.Hash = %q", sig.Subject.Hash)
	}
	if err := threshold.VerifySignature(sig, r); err != nil {
		t.Errorf("VerifySignature: %v", err)
	}
}

func TestVerifySignature_TamperedSubject(t *testing.T) {
	r, aliceSigner, _, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig, err := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sig.Subject.Hash = "tampered"
	if err := threshold.VerifySignature(sig, r); !errors.Is(err, threshold.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerifySignature_TamperedRosterHash(t *testing.T) {
	r, aliceSigner, _, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig, err := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sig.RosterHash = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := threshold.VerifySignature(sig, r); !errors.Is(err, threshold.ErrRosterHashMismatch) {
		t.Errorf("err = %v, want ErrRosterHashMismatch", err)
	}
}

func TestVerifyAttestation_QuorumNotMet(t *testing.T) {
	r, aliceSigner, _, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig, _ := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	header := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   r.ID,
		RosterHash: threshold.RosterHash(r),
		Threshold:  r.Threshold,
		CreatedAt:  time.Now().UTC(),
	}
	res, err := threshold.VerifyAttestation(header, []*threshold.AttestationSignature{sig}, r)
	if err != nil {
		t.Fatalf("VerifyAttestation: %v", err)
	}
	if res.Met {
		t.Errorf("Met = true with 1 sig and threshold 2")
	}
	if res.Members != 1 {
		t.Errorf("Members = %d, want 1", res.Members)
	}
}

func TestVerifyAttestation_QuorumMet(t *testing.T) {
	r, aliceSigner, bobSigner, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig1, _ := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	sig2, _ := threshold.SignAttestation(subject, r, bobSigner, "", time.Now())
	header := &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: r.ID, RosterHash: threshold.RosterHash(r),
		Threshold: r.Threshold, CreatedAt: time.Now().UTC(),
	}
	res, err := threshold.VerifyAttestation(header,
		[]*threshold.AttestationSignature{sig1, sig2}, r)
	if err != nil {
		t.Fatalf("VerifyAttestation: %v", err)
	}
	if !res.Met {
		t.Errorf("Met = false with 2 sigs and threshold 2")
	}
	if res.Members != 2 {
		t.Errorf("Members = %d, want 2", res.Members)
	}
}

// TestVerifyAttestation_DuplicateSigner counts a member only once
// even if they signed twice.
func TestVerifyAttestation_DuplicateSigner(t *testing.T) {
	r, aliceSigner, _, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "abc123"}
	sig1, _ := threshold.SignAttestation(subject, r, aliceSigner, "",
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	sig2, _ := threshold.SignAttestation(subject, r, aliceSigner, "",
		time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC))
	header := &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: r.ID, RosterHash: threshold.RosterHash(r),
		Threshold: r.Threshold, CreatedAt: time.Now().UTC(),
	}
	res, _ := threshold.VerifyAttestation(header,
		[]*threshold.AttestationSignature{sig1, sig2}, r)
	if res.Met {
		t.Errorf("Met = true with same member counted twice")
	}
	if res.Members != 1 {
		t.Errorf("Members = %d, want 1", res.Members)
	}
}

// TestVerifyAttestation_SubjectMismatch flags a sig that belongs to a
// different subject as invalid.
func TestVerifyAttestation_SubjectMismatch(t *testing.T) {
	r, aliceSigner, bobSigner, _ := makeRoster(t, 2)
	subjectA := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.a", Hash: "aaa"}
	subjectB := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.b", Hash: "bbb"}
	sig1, _ := threshold.SignAttestation(subjectA, r, aliceSigner, "", time.Now())
	sig2, _ := threshold.SignAttestation(subjectB, r, bobSigner, "", time.Now())
	header := &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subjectA,
		RosterID: r.ID, RosterHash: threshold.RosterHash(r),
		Threshold: r.Threshold, CreatedAt: time.Now().UTC(),
	}
	res, _ := threshold.VerifyAttestation(header,
		[]*threshold.AttestationSignature{sig1, sig2}, r)
	if res.Met {
		t.Errorf("Met = true with mismatched subject")
	}
	if len(res.InvalidSignatures) != 1 {
		t.Errorf("InvalidSignatures = %d, want 1", len(res.InvalidSignatures))
	}
}

// TestVerifyAttestation_HeaderRosterHashMismatch surfaces post-hoc
// roster tampering.
func TestVerifyAttestation_HeaderRosterHashMismatch(t *testing.T) {
	r, aliceSigner, bobSigner, _ := makeRoster(t, 2)
	subject := threshold.AttestationSubject{Kind: "x", ID: "y", Hash: "z"}
	sig1, _ := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	sig2, _ := threshold.SignAttestation(subject, r, bobSigner, "", time.Now())
	header := &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: r.ID, RosterHash: "0000000000000000",
		Threshold: r.Threshold, CreatedAt: time.Now().UTC(),
	}
	_, err := threshold.VerifyAttestation(header,
		[]*threshold.AttestationSignature{sig1, sig2}, r)
	if !errors.Is(err, threshold.ErrRosterHashMismatch) {
		t.Errorf("err = %v, want ErrRosterHashMismatch", err)
	}
}

// ----- RosterStore round-trip -----

func TestRosterStore_PutGet(t *testing.T) {
	sp := makeStorage(t)
	store := threshold.NewRosterStore(sp)
	r, _, _, _ := makeRoster(t, 2)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != r.ID || got.Threshold != r.Threshold || len(got.Members) != len(r.Members) {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, r)
	}
}

func TestRosterStore_PutTwice(t *testing.T) {
	sp := makeStorage(t)
	store := threshold.NewRosterStore(sp)
	r, _, _, _ := makeRoster(t, 2)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), r); !errors.Is(err, threshold.ErrRosterAlreadyExists) {
		t.Errorf("err = %v, want ErrRosterAlreadyExists", err)
	}
}

// tmpRecordingSP wraps a StoragePlugin and records every Put key so a test
// can assert RosterStore.Put stages through a randomised key+".tmp.<rand>"
// rather than a fixed key+".tmp" two concurrent first-writers would tear.
type tmpRecordingSP struct {
	storage.StoragePlugin
	mu          sync.Mutex
	putKeys     []string
	retainCalls []retainCall
}

type retainCall struct {
	key   string
	until time.Time
	mode  storage.WORMMode
}

func (w *tmpRecordingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	w.mu.Lock()
	w.putKeys = append(w.putKeys, key)
	w.mu.Unlock()
	return w.StoragePlugin.Put(ctx, key, r, opts)
}

func (w *tmpRecordingSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	w.mu.Lock()
	w.retainCalls = append(w.retainCalls, retainCall{key, until, mode})
	w.mu.Unlock()
	return w.StoragePlugin.SetRetention(ctx, key, until, mode)
}

// TestVerifyRosterTrusted covers the trust anchor in isolation: a roster
// passes only when its creator key is in the trusted set; an empty set is
// a misconfiguration; a non-matching set is a refusal.
func TestVerifyRosterTrusted(t *testing.T) {
	r, aliceSigner, bobSigner, _ := makeRoster(t, 2) // alice is creator

	if err := threshold.VerifyRosterTrusted(r, aliceSigner.PublicKey()); err != nil {
		t.Errorf("creator key trusted: want nil, got %v", err)
	}
	if err := threshold.VerifyRosterTrusted(r); !errors.Is(err, threshold.ErrNoTrustAnchor) {
		t.Errorf("no anchor: want ErrNoTrustAnchor, got %v", err)
	}
	// bob is a roster MEMBER but not the creator — being a member must
	// not make a key a valid trust anchor.
	if err := threshold.VerifyRosterTrusted(r, bobSigner.PublicKey()); !errors.Is(err, threshold.ErrRosterUntrusted) {
		t.Errorf("member-but-not-creator: want ErrRosterUntrusted, got %v", err)
	}
	// Anchor that includes the creator among several keys still passes.
	if err := threshold.VerifyRosterTrusted(r, bobSigner.PublicKey(), aliceSigner.PublicKey()); err != nil {
		t.Errorf("creator among trusted set: want nil, got %v", err)
	}
}

// TestRosterStore_TrustAnchoredGet proves a trust-anchored store refuses
// to load a roster whose creator key it doesn't trust, even though the
// roster is internally self-consistent (was accepted by an unanchored Put).
func TestRosterStore_TrustAnchoredGet(t *testing.T) {
	sp := makeStorage(t)
	r, aliceSigner, _, _ := makeRoster(t, 2)
	if err := threshold.NewRosterStore(sp).Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Trusted: creator key anchored → loads.
	if _, err := threshold.NewRosterStore(sp).
		WithTrustedKeys(aliceSigner.PublicKey()).
		Get(context.Background(), r.ID); err != nil {
		t.Errorf("anchored Get with creator key: %v", err)
	}

	// Untrusted: anchor a stranger's key → refuses.
	strangerPub, _ := mustKeypair(t)
	_, err := threshold.NewRosterStore(sp).
		WithTrustedKeys(strangerPub).
		Get(context.Background(), r.ID)
	if !errors.Is(err, threshold.ErrRosterUntrusted) {
		t.Errorf("anchored Get with stranger key: want ErrRosterUntrusted, got %v", err)
	}
}

// TestRosterStore_PutAppliesRetention proves a WORM-configured store
// re-locks the committed roster (SetRetention on the final key, not the
// staging tmp) so a compliance repo's quorum policy can't be deleted.
func TestRosterStore_PutAppliesRetention(t *testing.T) {
	rec := &tmpRecordingSP{StoragePlugin: makeStorage(t)}
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	store := threshold.NewRosterStore(rec).WithRetention(until, storage.WORMCompliance)
	r, _, _, _ := makeRoster(t, 2)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(rec.retainCalls) != 1 {
		t.Fatalf("want exactly 1 SetRetention call, got %d: %+v", len(rec.retainCalls), rec.retainCalls)
	}
	got := rec.retainCalls[0]
	if strings.Contains(got.key, ".tmp") {
		t.Errorf("retention applied to staging tmp %q, not the committed key", got.key)
	}
	if !got.until.Equal(until) || got.mode != storage.WORMCompliance {
		t.Errorf("retention call = %+v, want until=%v mode=compliance", got, until)
	}
}

// TestRosterStore_RandomisedTmp pins the torn-overwrite fix: the staging key
// must be a randomised key+".tmp.<rand>", never the fixed key+".tmp".
func TestRosterStore_RandomisedTmp(t *testing.T) {
	rec := &tmpRecordingSP{StoragePlugin: makeStorage(t)}
	store := threshold.NewRosterStore(rec)
	r, _, _, _ := makeRoster(t, 2)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var tmp string
	for _, k := range rec.putKeys {
		if strings.Contains(k, ".tmp") {
			tmp = k
		}
	}
	if tmp == "" {
		t.Fatalf("no .tmp staging Put observed; keys=%v", rec.putKeys)
	}
	final := strings.SplitN(tmp, ".tmp", 2)[0]
	if tmp == final+".tmp" {
		t.Errorf("staging key is fixed %q — torn-overwrite race on concurrent same-ID writes", tmp)
	}
	if !strings.HasPrefix(tmp, final+".tmp.") {
		t.Errorf("staging key %q is not the expected randomised %q.tmp.<rand> shape", tmp, final)
	}
}

func TestRosterStore_GetMissing(t *testing.T) {
	sp := makeStorage(t)
	store := threshold.NewRosterStore(sp)
	_, err := store.Get(context.Background(), "ghost")
	if !errors.Is(err, threshold.ErrRosterNotFound) {
		t.Errorf("err = %v, want ErrRosterNotFound", err)
	}
}

func TestRosterStore_PutValidationFailure(t *testing.T) {
	sp := makeStorage(t)
	store := threshold.NewRosterStore(sp)
	r := threshold.NewRoster("x", "", 5,
		[]threshold.Member{}, time.Now())
	if err := store.Put(context.Background(), r); err == nil {
		t.Errorf("expected validation error")
	}
}

func TestRosterStore_List(t *testing.T) {
	sp := makeStorage(t)
	store := threshold.NewRosterStore(sp)
	for _, id := range []string{"prod", "staging", "dev"} {
		r, signer, _, _ := makeRoster(t, 1)
		r.ID = id
		// Re-sign because ID change invalidates the canonical hash.
		_ = threshold.SignRoster(r, signer, r.CreatedBy)
		if err := store.Put(context.Background(), r); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	rs, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("len = %d, want 3", len(rs))
	}
	if rs[0].ID != "dev" || rs[1].ID != "prod" || rs[2].ID != "staging" {
		t.Errorf("not sorted: %v %v %v", rs[0].ID, rs[1].ID, rs[2].ID)
	}
}

// ----- AttestationStore round-trip -----

func TestAttestationStore_FullCycle(t *testing.T) {
	sp := makeStorage(t)
	rs := threshold.NewRosterStore(sp)
	as := threshold.NewAttestationStore(sp)

	r, aliceSigner, bobSigner, _ := makeRoster(t, 2)
	if err := rs.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	subject := threshold.AttestationSubject{
		Kind: "backup_manifest", ID: "db1.full.20260501T120000Z", Hash: "deadbeef",
	}
	header := &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: r.ID, RosterHash: threshold.RosterHash(r),
		Threshold: r.Threshold,
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := as.PutHeader(context.Background(), header); err != nil {
		t.Fatalf("PutHeader: %v", err)
	}
	// Idempotent re-put.
	if err := as.PutHeader(context.Background(), header); err != nil {
		t.Errorf("idempotent re-put: %v", err)
	}

	sig1, _ := threshold.SignAttestation(subject, r, aliceSigner, "", time.Now())
	if err := as.PutSignature(context.Background(), sig1); err != nil {
		t.Fatalf("PutSignature alice: %v", err)
	}
	sig2, _ := threshold.SignAttestation(subject, r, bobSigner, "", time.Now())
	if err := as.PutSignature(context.Background(), sig2); err != nil {
		t.Fatalf("PutSignature bob: %v", err)
	}

	att, err := as.LoadAttestation(context.Background(), subject.Kind, subject.ID)
	if err != nil {
		t.Fatalf("LoadAttestation: %v", err)
	}
	if len(att.Signatures) != 2 {
		t.Fatalf("len(Signatures) = %d, want 2", len(att.Signatures))
	}
	res, err := threshold.VerifyAttestation(att.Header, att.Signatures, r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Met {
		t.Errorf("Met = false, want true")
	}
}

// TestAttestationStore_HeaderConflict asserts a re-put with different
// content is refused (subject_hash drift would invalidate signatures).
func TestAttestationStore_HeaderConflict(t *testing.T) {
	sp := makeStorage(t)
	as := threshold.NewAttestationStore(sp)
	header := &threshold.AttestationHeader{
		Schema:   threshold.SchemaAttestationHeader,
		Subject:  threshold.AttestationSubject{Kind: "k", ID: "i", Hash: "v1"},
		RosterID: "r", RosterHash: "h", Threshold: 2,
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := as.PutHeader(context.Background(), header); err != nil {
		t.Fatal(err)
	}
	header.Subject.Hash = "v2"
	if err := as.PutHeader(context.Background(), header); err == nil {
		t.Errorf("expected conflict")
	}
}

// TestAttestationStore_SignatureConflict asserts a member's second
// signing with different bytes is refused (sign-with-changed-content
// is an attack vector).  Identical-bytes re-put is accepted.
func TestAttestationStore_SignatureConflict(t *testing.T) {
	sp := makeStorage(t)
	rs := threshold.NewRosterStore(sp)
	as := threshold.NewAttestationStore(sp)
	r, aliceSigner, _, _ := makeRoster(t, 2)
	if err := rs.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	subject := threshold.AttestationSubject{Kind: "k", ID: "i", Hash: "v"}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, r, aliceSigner, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := as.PutSignature(context.Background(), sig); err != nil {
		t.Fatal(err)
	}
	// Identical bytes — accepted.
	if err := as.PutSignature(context.Background(), sig); err != nil {
		t.Errorf("identical re-put: %v", err)
	}
	// Different timestamp — different bytes — refused.
	sig2, _ := threshold.SignAttestation(subject, r, aliceSigner, "",
		now.Add(time.Hour))
	if err := as.PutSignature(context.Background(), sig2); !errors.Is(err, threshold.ErrSubjectAlreadySigned) {
		t.Errorf("err = %v, want ErrSubjectAlreadySigned", err)
	}
}

func TestAttestationStore_LoadMissing(t *testing.T) {
	sp := makeStorage(t)
	as := threshold.NewAttestationStore(sp)
	_, err := as.LoadAttestation(context.Background(), "k", "ghost")
	if !errors.Is(err, threshold.ErrAttestationNotFound) {
		t.Errorf("err = %v, want ErrAttestationNotFound", err)
	}
}

// TestAttestationStore_HeaderCreatedAtDriftIdempotent is the
// deterministic regression test for the v27-audit flake.  Two
// sequential PutHeaders for the same subject + roster but with
// different CreatedAt values must be accepted as logically
// equivalent.  Pre-fix: byte-equality dup-check returned conflict;
// post-fix: logical equality (Subject + RosterID + RosterHash +
// Threshold + Schema) accepts the second as idempotent.
//
// Without this fix, TestThresholdAttestSign_DupNoOp in
// internal/cli/threshold_test.go flaked when the two CLI invocations
// straddled a wallclock-second boundary.
func TestAttestationStore_HeaderCreatedAtDriftIdempotent(t *testing.T) {
	sp := makeStorage(t)
	as := threshold.NewAttestationStore(sp)
	subject := threshold.AttestationSubject{Kind: "k", ID: "drift-test", Hash: "v"}
	first := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   "r",
		RosterHash: "h",
		Threshold:  2,
		CreatedAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := as.PutHeader(context.Background(), first); err != nil {
		t.Fatalf("first PutHeader: %v", err)
	}
	// Logically equivalent header with a SECOND CreatedAt — the
	// kind of drift that produced the flake when two CLI signs
	// straddled a second boundary.
	second := *first
	second.CreatedAt = first.CreatedAt.Add(7 * time.Second)
	if err := as.PutHeader(context.Background(), &second); err != nil {
		t.Errorf("second PutHeader (CreatedAt drift): %v — should be idempotent", err)
	}
	// Sanity: the on-disk header is still the FIRST one (the
	// existing-header path took precedence).
	stored, err := as.GetHeader(context.Background(), subject.Kind, subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("stored.CreatedAt = %v, want %v (existing should win)", stored.CreatedAt, first.CreatedAt)
	}
}

// TestAttestationStore_HeaderRosterMismatchRefused asserts the
// security-relevant comparison still fires: a second PutHeader with
// a DIFFERENT roster (or subject hash) is rejected, regardless of
// CreatedAt.  This is the property the byte-equality check was
// trying to provide; the logical-equality check provides it
// directly.
func TestAttestationStore_HeaderRosterMismatchRefused(t *testing.T) {
	sp := makeStorage(t)
	as := threshold.NewAttestationStore(sp)
	subject := threshold.AttestationSubject{Kind: "k", ID: "roster-mismatch", Hash: "v"}
	first := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   "alpha",
		RosterHash: "h-alpha",
		Threshold:  2,
		CreatedAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := as.PutHeader(context.Background(), first); err != nil {
		t.Fatalf("first PutHeader: %v", err)
	}
	// Different roster — must be refused.
	second := *first
	second.RosterID = "beta"
	second.RosterHash = "h-beta"
	if err := as.PutHeader(context.Background(), &second); err == nil {
		t.Error("PutHeader with different roster should be refused")
	}
	// Different subject hash — must be refused.
	third := *first
	third.Subject.Hash = "v-tampered"
	if err := as.PutHeader(context.Background(), &third); err == nil {
		t.Error("PutHeader with different subject hash should be refused")
	}
}

// TestAttestationStore_HeaderConcurrentFirstWriters: two goroutines
// race to PutHeader on the same subject + roster.  Headers are
// deterministic so both write byte-equal bodies; both should succeed
// (the loser's RenameIfNotExists fails but the helper re-checks the
// destination's bytes and treats byte-equal as idempotent).  Without
// the fix, one goroutine would surface a generic rename error.
func TestAttestationStore_HeaderConcurrentFirstWriters(t *testing.T) {
	sp := makeStorage(t)
	as := threshold.NewAttestationStore(sp)
	header := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    threshold.AttestationSubject{Kind: "k", ID: "id1", Hash: "v"},
		RosterID:   "r",
		RosterHash: "h",
		Threshold:  2,
		CreatedAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}

	const N = 32
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := *header
			errs <- as.PutHeader(context.Background(), &h)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent PutHeader err = %v, want nil (byte-equal idempotent)", err)
		}
	}
}

// TestAttestationStore_SignatureConcurrentFirstWriters: same posture
// for PutSignature.  Two operators with the same local key (the
// pathological retry case, e.g. an automation script invoked twice
// in parallel) produce byte-equal signatures only when SignedAt is
// equal; we use a fixed timestamp here to model that retry pattern.
func TestAttestationStore_SignatureConcurrentFirstWriters(t *testing.T) {
	sp := makeStorage(t)
	rs := threshold.NewRosterStore(sp)
	as := threshold.NewAttestationStore(sp)
	r, aliceSigner, _, _ := makeRoster(t, 2)
	if err := rs.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	subject := threshold.AttestationSubject{Kind: "k", ID: "id2", Hash: "v"}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, r, aliceSigner, "", now)
	if err != nil {
		t.Fatal(err)
	}

	const N = 32
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := *sig
			errs <- as.PutSignature(context.Background(), &s)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent PutSignature err = %v, want nil (byte-equal idempotent)", err)
		}
	}
}

// ----- ParseMemberSpec -----

func TestParseMemberSpec_Happy(t *testing.T) {
	pub, _ := mustKeypair(t)
	spec := fmt.Sprintf("alice@acme.example:%s", base64.StdEncoding.EncodeToString(pub))
	m, err := threshold.ParseMemberSpec(spec)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if m.Signer != "alice@acme.example" {
		t.Errorf("Signer = %q", m.Signer)
	}
	if m.PublicKeyFingerprint != threshold.PublicKeyFingerprint(pub) {
		t.Errorf("Fingerprint mismatch")
	}
}

func TestParseMemberSpec_BadFormat(t *testing.T) {
	cases := []string{
		"missing-colon",
		":empty-signer-base64",
		"alice@example:not-base64",
		"alice@example:" + base64.StdEncoding.EncodeToString([]byte("short")),
	}
	for _, c := range cases {
		if _, err := threshold.ParseMemberSpec(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// TestPublicKeyFingerprint_StableAcrossRuns is a sanity check on the
// 16-hex-char output shape.
func TestPublicKeyFingerprint_StableAcrossRuns(t *testing.T) {
	pub, _ := mustKeypair(t)
	fp := threshold.PublicKeyFingerprint(pub)
	if len(fp) != 16 {
		t.Errorf("len = %d, want 16", len(fp))
	}
	if !strings.ContainsAny(fp, "0123456789abcdef") {
		t.Errorf("not hex: %q", fp)
	}
}
