package attestgate_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/attestgate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// signerFromKey is the test-side Signer (mirrors threshold/integrity tests).
type signerFromKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s signerFromKey) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s signerFromKey) PublicKey() ed25519.PublicKey { return s.pub }

// fixture spins up a fresh repo + builds a roster + plants a
// signed manifest.  Helper functions on it produce signed
// attestations so individual tests stay legible.
type fixture struct {
	sp         storage.StoragePlugin
	manifest   *backup.Manifest
	roster     *threshold.Roster
	signers    map[string]signerFromKey // signer id → key
	creatorKey ed25519.PublicKey        // the roster's creator/admin key (trust anchor)
}

// trustOpts builds gate options anchored to this fixture's roster
// creator key — the realistic case where the operator trusts the key
// that created the roster.
func (f *fixture) trustOpts(rosterID string) attestgate.Options {
	return attestgate.Options{
		RosterID:    rosterID,
		TrustedKeys: []ed25519.PublicKey{f.creatorKey},
	}
}

func newFixture(t *testing.T, threshold_ int, memberIDs ...string) *fixture {
	t.Helper()
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(repoURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	// Build the roster.
	signers := make(map[string]signerFromKey, len(memberIDs))
	members := make([]threshold.Member, 0, len(memberIDs))
	for _, id := range memberIDs {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signers[id] = signerFromKey{pub: pub, priv: priv}
		members = append(members, threshold.NewMember(id, pub))
	}
	first := memberIDs[0]
	r := threshold.NewRoster("prod-admins", "tier-0 governance",
		threshold_, members, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(r, signers[first], first); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(sp).Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	// Plant a manifest (unsigned — attestgate doesn't care about the
	// manifest's own ed25519 signature, only the canonical bytes).
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260601T120000Z",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		StoppedAt:        time.Date(2026, 6, 1, 12, 0, 30, 0, time.UTC),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
		},
	}
	return &fixture{sp: sp, manifest: m, roster: r, signers: signers, creatorKey: signers[first].pub}
}

// canonicalHash returns the manifest's canonical-bytes SHA-256 in hex.
// Tests hand this string to threshold.SignAttestation as the
// subject hash.
func (f *fixture) canonicalHash(t *testing.T) string {
	t.Helper()
	canon, err := f.manifest.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// attestAs makes one signature on the manifest's body, persists it
// (alongside the header on first call), and returns no error on
// happy path.
func (f *fixture) attestAs(t *testing.T, memberID string, hashOverride string) {
	t.Helper()
	subjectHash := hashOverride
	if subjectHash == "" {
		subjectHash = f.canonicalHash(t)
	}
	subject := threshold.AttestationSubject{
		Kind: attestgate.SubjectKind,
		ID:   f.manifest.BackupID,
		Hash: subjectHash,
	}
	now := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, f.roster, f.signers[memberID], "", now)
	if err != nil {
		t.Fatalf("SignAttestation as %s: %v", memberID, err)
	}
	store := threshold.NewAttestationStore(f.sp)
	header := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   f.roster.ID,
		RosterHash: threshold.RosterHash(f.roster),
		Threshold:  f.roster.Threshold,
		CreatedAt:  now.Truncate(time.Second),
	}
	if err := store.PutHeader(context.Background(), header); err != nil {
		t.Fatalf("PutHeader: %v", err)
	}
	if err := store.PutSignature(context.Background(), sig); err != nil {
		t.Fatalf("PutSignature as %s: %v", memberID, err)
	}
}

// ----- Verify -----

func TestVerify_NoRosterRequired_NoOp(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob")
	if err := attestgate.Verify(context.Background(), f.sp, f.manifest, attestgate.Options{}); err != nil {
		t.Errorf("empty RosterID should be a no-op; got %v", err)
	}
}

func TestVerify_AttestationMissing(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob")
	err := attestgate.Verify(context.Background(), f.sp, f.manifest, f.trustOpts("prod-admins"))
	if !errors.Is(err, attestgate.ErrAttestationMissing) {
		t.Errorf("err = %v, want ErrAttestationMissing", err)
	}
}

// TestVerify_NoTrustAnchor pins the secure default: engaging the gate
// (RosterID set) without supplying a trust anchor is a configuration
// error, not a silent pass — a caller that forgot to anchor would
// otherwise trust any self-signed roster.
func TestVerify_NoTrustAnchor(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob")
	f.attestAs(t, "alice", "")
	f.attestAs(t, "bob", "")
	err := attestgate.Verify(context.Background(), f.sp, f.manifest, attestgate.Options{
		RosterID: "prod-admins", // no TrustedKeys
	})
	if !errors.Is(err, attestgate.ErrNoTrustAnchor) {
		t.Errorf("err = %v, want ErrNoTrustAnchor", err)
	}
}

// TestVerify_ForgedRosterRejected is the core regression for the
// roster trust-anchor fix. An attacker with repo write forges a 1-of-1
// roster naming their own key as sole member + creator, signs it
// (self-consistent), and plants a complete, quorum-meeting attestation
// under it. Before the fix the gate would load that roster (it verifies
// against its own embedded key) and wave the restore through. With the
// operator's key as the trust anchor, the forged roster's creator is
// untrusted, so the gate refuses before even consulting the quorum.
func TestVerify_ForgedRosterRejected(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob") // legit operator roster

	// Attacker forges a roster under a key the operator does NOT trust.
	atkPub, atkPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	atk := signerFromKey{pub: atkPub, priv: atkPriv}
	forged := threshold.NewRoster("attacker-roster", "forged",
		1, []threshold.Member{threshold.NewMember("mallory", atkPub)},
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(forged, atk, "mallory"); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(f.sp).Put(context.Background(), forged); err != nil {
		t.Fatal(err) // unanchored store accepts it — the forgery is on-disk
	}

	// Attacker signs a full quorum attestation under the forged roster.
	subject := threshold.AttestationSubject{
		Kind: attestgate.SubjectKind, ID: f.manifest.BackupID, Hash: f.canonicalHash(t),
	}
	now := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, forged, atk, "", now)
	if err != nil {
		t.Fatal(err)
	}
	as := threshold.NewAttestationStore(f.sp)
	if err := as.PutHeader(context.Background(), &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: forged.ID, RosterHash: threshold.RosterHash(forged),
		Threshold: forged.Threshold, CreatedAt: now.Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := as.PutSignature(context.Background(), sig); err != nil {
		t.Fatal(err)
	}

	// The operator restores anchored to THEIR key, requiring the forged
	// roster. Quorum is met on paper, but the roster is untrusted.
	err = attestgate.Verify(context.Background(), f.sp, f.manifest, attestgate.Options{
		RosterID:    "attacker-roster",
		TrustedKeys: []ed25519.PublicKey{f.creatorKey},
	})
	if !errors.Is(err, attestgate.ErrRosterUntrusted) {
		t.Errorf("err = %v, want ErrRosterUntrusted (forged roster must not gate a restore)", err)
	}
}

func TestVerify_QuorumMet(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob", "charlie")
	f.attestAs(t, "alice", "")
	f.attestAs(t, "bob", "")

	if err := attestgate.Verify(context.Background(), f.sp, f.manifest, f.trustOpts("prod-admins")); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerify_QuorumNotMet(t *testing.T) {
	f := newFixture(t, 3, "alice", "bob", "charlie")
	// Only one signer; threshold is 3.
	f.attestAs(t, "alice", "")
	err := attestgate.Verify(context.Background(), f.sp, f.manifest, f.trustOpts("prod-admins"))
	if !errors.Is(err, attestgate.ErrQuorumNotMet) {
		t.Errorf("err = %v, want ErrQuorumNotMet", err)
	}
}

// TestVerify_SubjectHashMismatch covers the case where the
// attestation pins a *different* manifest body — e.g. operators
// signed a draft and then the manifest was rewritten (KEK rotation,
// re-encryption, etc.).  The gate must refuse.
func TestVerify_SubjectHashMismatch(t *testing.T) {
	f := newFixture(t, 1, "alice")
	f.attestAs(t, "alice", "deadbeef00000000000000000000000000000000000000000000000000000000")
	err := attestgate.Verify(context.Background(), f.sp, f.manifest, f.trustOpts("prod-admins"))
	if !errors.Is(err, attestgate.ErrSubjectHashMismatch) {
		t.Errorf("err = %v, want ErrSubjectHashMismatch", err)
	}
}

// TestVerify_RosterMismatch covers the case where an attestation
// exists, satisfies the quorum, but references a roster that ISN'T
// the one the caller required.  An attestation signed under
// staging-admins doesn't satisfy a prod-admins requirement.
func TestVerify_RosterMismatch(t *testing.T) {
	f := newFixture(t, 1, "alice")
	f.attestAs(t, "alice", "")

	// Create + sign a SECOND roster with a different ID.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	staging := threshold.NewRoster("staging-admins", "different",
		1,
		[]threshold.Member{
			{Signer: "x@acme",
				PublicKey:            base64.StdEncoding.EncodeToString(pub),
				PublicKeyFingerprint: threshold.PublicKeyFingerprint(pub)},
		},
		time.Now())
	if err := threshold.SignRoster(staging, signerFromKey{pub: pub, priv: priv}, "x@acme"); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(f.sp).Put(context.Background(), staging); err != nil {
		t.Fatal(err)
	}

	// Caller asks for staging-admins, but the planted attestation is
	// for prod-admins. Both rosters' creators are trusted, so the gate
	// gets past the trust anchor and fails purely on the roster-ID
	// mismatch — the behaviour under test.
	err = attestgate.Verify(context.Background(), f.sp, f.manifest, attestgate.Options{
		RosterID:    "staging-admins",
		TrustedKeys: []ed25519.PublicKey{f.creatorKey, pub},
	})
	if !errors.Is(err, attestgate.ErrRosterMismatch) {
		t.Errorf("err = %v, want ErrRosterMismatch", err)
	}
}

func TestVerify_RosterNotFound(t *testing.T) {
	f := newFixture(t, 1, "alice")
	err := attestgate.Verify(context.Background(), f.sp, f.manifest, f.trustOpts("ghost"))
	if err == nil {
		t.Errorf("expected error for missing roster")
	}
	if !errors.Is(err, threshold.ErrRosterNotFound) {
		t.Errorf("err = %v, want ErrRosterNotFound", err)
	}
}

func TestVerify_NilManifest(t *testing.T) {
	f := newFixture(t, 1, "alice")
	err := attestgate.Verify(context.Background(), f.sp, nil, f.trustOpts("prod-admins"))
	if err == nil {
		t.Errorf("expected error for nil manifest")
	}
}

// ----- VerifyDetailed -----

func TestVerifyDetailed_QuorumMet_PopulatesCounts(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob")
	f.attestAs(t, "alice", "")
	f.attestAs(t, "bob", "")

	res, err := attestgate.VerifyDetailed(context.Background(), f.sp, f.manifest,
		f.trustOpts("prod-admins"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !res.Met {
		t.Errorf("Met = false")
	}
	if res.Threshold != 2 || res.ValidSignatures != 2 || res.TotalSignatures != 2 {
		t.Errorf("counts: %+v", res)
	}
	if !res.AttestationFound || !res.SubjectHashOK {
		t.Errorf("flags: %+v", res)
	}
}

func TestVerifyDetailed_QuorumNotMet_StillReportsCounts(t *testing.T) {
	f := newFixture(t, 3, "alice", "bob", "charlie")
	f.attestAs(t, "alice", "")
	res, err := attestgate.VerifyDetailed(context.Background(), f.sp, f.manifest,
		f.trustOpts("prod-admins"))
	if !errors.Is(err, attestgate.ErrQuorumNotMet) {
		t.Errorf("err = %v, want ErrQuorumNotMet", err)
	}
	if res.Threshold != 3 || res.ValidSignatures != 1 {
		t.Errorf("counts: %+v", res)
	}
	if res.Met {
		t.Errorf("Met should be false")
	}
}

func TestVerifyDetailed_NoRosterRequired_EmptyResult(t *testing.T) {
	f := newFixture(t, 2, "alice", "bob")
	res, err := attestgate.VerifyDetailed(context.Background(), f.sp, f.manifest,
		attestgate.Options{})
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if res.RosterID != "" || res.AttestationFound {
		t.Errorf("res = %+v", res)
	}
}
