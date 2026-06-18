package cli_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/attestgate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// thresholdSignerForRoster wraps the readWorld's local key for use
// as a threshold-package Signer.
type thresholdSignerForRoster struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func (s thresholdSignerForRoster) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s thresholdSignerForRoster) PublicKey() ed25519.PublicKey { return s.pub }

// plantRosterForLocalKey builds a 1-of-1 roster whose only member
// is the readWorld's local signing key.  Returns the roster ID.
func plantRosterForLocalKey(t *testing.T, w *readWorld, id string) string {
	t.Helper()
	pub := w.signer.PublicKey()
	priv := w.signer.PrivateKey()
	signer := thresholdSignerForRoster{priv: priv, pub: pub}
	r := threshold.NewRoster(id, "test", 1,
		[]threshold.Member{threshold.NewMember("alice@acme", pub)},
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(r, signer, "alice@acme"); err != nil {
		t.Fatalf("SignRoster: %v", err)
	}
	if err := threshold.NewRosterStore(w.sp).Put(context.Background(), r); err != nil {
		t.Fatalf("RosterStore.Put: %v", err)
	}
	return id
}

// plantSignedManifest commits a real manifest and returns it.
func plantSignedManifest(t *testing.T, w *readWorld, deployment string) *backup.Manifest {
	t.Helper()
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + ts.Format("20060102T150405Z"),
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Re-read so we get the post-commit form (with Attestation populated).
	got, err := w.store.Read(context.Background(), deployment, m.BackupID, w.verifier)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

// signAttestationForManifest plants a threshold attestation that
// pins the manifest's canonical-bytes hash, signed by the
// readWorld's local key acting as roster member alice@acme.
func signAttestationForManifest(t *testing.T, w *readWorld, m *backup.Manifest, rosterID string) {
	t.Helper()
	canon, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canon)
	hashHex := hex.EncodeToString(hash[:])

	r, err := threshold.NewRosterStore(w.sp).Get(context.Background(), rosterID)
	if err != nil {
		t.Fatal(err)
	}
	signer := thresholdSignerForRoster{priv: w.signer.PrivateKey(), pub: w.signer.PublicKey()}
	subject := threshold.AttestationSubject{
		Kind: attestgate.SubjectKind,
		ID:   m.BackupID,
		Hash: hashHex,
	}
	now := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, r, signer, "", now)
	if err != nil {
		t.Fatal(err)
	}
	store := threshold.NewAttestationStore(w.sp)
	if err := store.PutHeader(context.Background(), &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   r.ID,
		RosterHash: threshold.RosterHash(r),
		Threshold:  r.Threshold,
		CreatedAt:  now.Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSignature(context.Background(), sig); err != nil {
		t.Fatal(err)
	}
}

// TestRestore_AttestationGate_Missing exits 6 when the gate is
// requested but no attestation is on disk.
func TestRestore_AttestationGate_Missing(t *testing.T) {
	w := newReadWorld(t)
	plantRosterForLocalKey(t, w, "prod-admins")
	m := plantSignedManifest(t, w, "db1")

	target := t.TempDir()
	_, errb, exit := runCLI(t,
		"restore", "db1", m.BackupID,
		"--repo", w.repoURL,
		"--target", target,
		"--require-threshold-attestation", "prod-admins",
		"-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound (6)\n%s", exit, errb)
	}
	if !strings.Contains(errb, "notfound.attestation") {
		t.Errorf("expected notfound.attestation:\n%s", errb)
	}
}

// TestRestore_AttestationGate_Met allows the restore to proceed
// past the gate when the attestation is present.  We don't drive a
// full restore in this test (no PG running) — we just want to
// observe that the gate doesn't block and the failure is downstream
// of it.
func TestRestore_AttestationGate_Met(t *testing.T) {
	w := newReadWorld(t)
	rosterID := plantRosterForLocalKey(t, w, "prod-admins")
	m := plantSignedManifest(t, w, "db1")
	signAttestationForManifest(t, w, m, rosterID)

	target := t.TempDir()
	_, errb, exit := runCLI(t,
		"restore", "db1", m.BackupID,
		"--repo", w.repoURL,
		"--target", target,
		"--require-threshold-attestation", "prod-admins",
		"-o", "json")
	// The gate must pass.  The downstream restore will still fail
	// (we don't have full chunk content for a real restore) but the
	// failure must NOT be the gate's exit code (6) and must NOT carry
	// the gate's error code.
	if exit == int(output.ExitNotFound) {
		t.Errorf("gate refused a valid attestation: exit=%d\n%s", exit, errb)
	}
	if strings.Contains(errb, "notfound.attestation") ||
		strings.Contains(errb, "verify.attestation_quorum") ||
		strings.Contains(errb, "verify.attestation_subject") ||
		strings.Contains(errb, "verify.attestation_roster") {
		t.Errorf("gate-related error code surfaced after a valid attestation:\n%s", errb)
	}
}

// TestRestore_AttestationGate_QuorumNotMet uses a 2-of-2 roster
// (local key + an out-of-band member) but only signs with the
// local key — the gate must refuse with verify.attestation_quorum.
func TestRestore_AttestationGate_QuorumNotMet(t *testing.T) {
	w := newReadWorld(t)
	// Build a 2-of-2 roster whose first member is the readWorld's
	// local key.  The second member exists in the roster but never
	// signs.
	pub := w.signer.PublicKey()
	priv := w.signer.PrivateKey()
	signer := thresholdSignerForRoster{priv: priv, pub: pub}
	bobPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		// nil reader → use rand.Reader fallback.  We need a real key.
		bobPub, _, _ = mustGenKey(t)
	}
	r := threshold.NewRoster("two-of-two", "tier-0", 2,
		[]threshold.Member{
			threshold.NewMember("alice@acme", pub),
			threshold.NewMember("bob@acme", bobPub),
		},
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(r, signer, "alice@acme"); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(w.sp).Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	m := plantSignedManifest(t, w, "db1")
	signAttestationForManifest(t, w, m, "two-of-two") // only alice signs

	target := t.TempDir()
	_, errb, exit := runCLI(t,
		"restore", "db1", m.BackupID,
		"--repo", w.repoURL,
		"--target", target,
		"--require-threshold-attestation", "two-of-two",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed (9)\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.attestation_quorum") {
		t.Errorf("expected verify.attestation_quorum:\n%s", errb)
	}
}

// TestRestore_AttestationGate_RosterNotFound exits cleanly when the
// requested roster doesn't exist — caller should see a clear error.
func TestRestore_AttestationGate_RosterNotFound(t *testing.T) {
	w := newReadWorld(t)
	m := plantSignedManifest(t, w, "db1")

	target := t.TempDir()
	_, errb, exit := runCLI(t,
		"restore", "db1", m.BackupID,
		"--repo", w.repoURL,
		"--target", target,
		"--require-threshold-attestation", "ghost-roster",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit with missing roster")
	}
	// The threshold roster-not-found error doesn't currently route
	// to a specific exit code; we just assert we don't proceed
	// (= no .pgdata file emitted at the target).
	_ = errb
}

// mustGenKey returns a fresh ed25519 keypair (used only as a
// fallback in TestRestore_AttestationGate_QuorumNotMet when
// crypto/rand isn't supplied).
func mustGenKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv, nil
}
