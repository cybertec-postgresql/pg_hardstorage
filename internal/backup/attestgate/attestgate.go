// Package attestgate gates backup-manifest consumption on a
// threshold (k-of-n) attestation.  Tier-0 deployments configure
// "no manifest is consumable until k of n operators have signed
// off on it"; this package is the runtime check.
//
// Background: shipped the threshold-attestation primitive
// (`pg_hardstorage threshold attest sign`) but didn't gate any
// consumer on it — operators could collect the signatures, but
// nothing in the system refused a manifest that lacked a quorum.
// attestgate is the missing enforcement layer.
//
// Threat model: a regulated workload requires multiple operators
// to sign off on a backup before it can be restored (think GDPR
// data-protection sign-off, or Sarbanes-Oxley financial controls).
// The threshold attestation is the artefact; this gate is the
// rule that "no restore until the artefact exists and meets
// quorum."
//
// Contract:
//
//	Verify(ctx, sp, manifest, opts) error
//
// Returns nil iff:
//   - opts.RosterID is non-empty (the gate is opt-in per call;
//     callers that don't pass a roster get nil immediately)
//   - the threshold attestation under
//     threshold/attestations/backup_manifest/<backup_id>/
//     exists, references the supplied roster, and pins the
//     manifest's canonical-bytes SHA-256 (so an attestation
//     for a different version of the same backup_id doesn't
//     count)
//   - that attestation's signatures meet the roster's k threshold
//
// Otherwise returns one of the typed errors so callers can
// distinguish "no attestation exists yet" from "quorum not met"
// from "the attestation was for a different manifest body."
package attestgate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// SubjectKind is the canonical Kind we use for backup-manifest
// attestations.  Operators run
// `pg_hardstorage threshold attest sign backup_manifest <backup-id> ...`.
const SubjectKind = "backup_manifest"

// Sentinel errors.
var (
	// ErrAttestationMissing is returned when the gate can't find any
	// threshold attestation header for the manifest.
	ErrAttestationMissing = errors.New("attestgate: no threshold attestation for this manifest")
	// ErrSubjectHashMismatch is returned when an attestation exists
	// but pins a different manifest body — i.e. someone re-signed a
	// different version, or the attestation predates a manifest
	// rewrite (KEK rotation, etc.).
	ErrSubjectHashMismatch = errors.New("attestgate: attestation pins a different manifest body")
	// ErrRosterMismatch is returned when an attestation exists but
	// references a different roster than the one required by the
	// caller.
	ErrRosterMismatch = errors.New("attestgate: attestation references a different roster")
	// ErrQuorumNotMet is returned when an attestation exists for the
	// right manifest + roster but doesn't have enough valid
	// signatures.
	ErrQuorumNotMet = errors.New("attestgate: quorum not met")
	// ErrNoTrustAnchor is returned when the gate is engaged (RosterID
	// set) but the caller supplied no trusted keys to anchor the
	// roster's creator. Without an anchor a repo-write attacker could
	// forge a 1-of-1 roster that satisfies its own quorum, so the gate
	// refuses rather than trust a self-signed roster.
	ErrNoTrustAnchor = errors.New("attestgate: roster gate requires at least one trusted key")
	// ErrRosterUntrusted is returned when the roster's creator key is
	// not among the caller's trusted keys.
	ErrRosterUntrusted = errors.New("attestgate: roster creator key is not trusted")
)

// Options configures one Verify call.
type Options struct {
	// RosterID is the roster the caller requires.  Empty means "no
	// requirement" — Verify returns nil immediately.  Callers that
	// want strict enforcement supply the operator-configured roster
	// id.
	RosterID string

	// TrustedKeys anchors the roster's creator to an out-of-band trust
	// root — the operator keyring that also verifies manifests. REQUIRED
	// when RosterID is set: the roster supplies the member set + quorum
	// threshold the gate enforces, so a forged roster (a 1-of-1 naming
	// the attacker) would otherwise satisfy its own quorum. The gate
	// loads the roster through a trust-anchored store, so a roster whose
	// creator key is not in this set is rejected before its quorum is
	// even consulted. Empty + RosterID set → ErrNoTrustAnchor.
	TrustedKeys []ed25519.PublicKey
}

// rosterStore builds the roster store the gate reads through. When
// RosterID is set, TrustedKeys must be non-empty (ErrNoTrustAnchor
// otherwise) and the store is anchored so an untrusted/forged roster
// fails to load.
func (o Options) rosterStore(sp storage.StoragePlugin) (*threshold.RosterStore, error) {
	if len(o.TrustedKeys) == 0 {
		return nil, ErrNoTrustAnchor
	}
	return threshold.NewRosterStore(sp).WithTrustedKeys(o.TrustedKeys...), nil
}

// Verify enforces the threshold-attestation requirement on m.
// Returns nil when the requirement is satisfied or when the caller
// didn't ask for one (RosterID empty).
//
// Manifest body hash: we use the canonical signing bytes (the same
// bytes the manifest's own ed25519 signature covers).  Operators
// running `threshold attest sign` pass that same hash via --hash;
// the gate's job is to confirm the attestation pins it.
//
// Lookup path: threshold/attestations/backup_manifest/<backup_id>/.
// `m.BackupID` is operator-friendly + filesystem-safe, so it's a
// fine path component without escaping.
func Verify(ctx context.Context, sp storage.StoragePlugin, m *backup.Manifest, opts Options) error {
	if opts.RosterID == "" {
		return nil
	}
	if m == nil {
		return errors.New("attestgate: nil manifest")
	}
	if m.BackupID == "" {
		return errors.New("attestgate: manifest has no BackupID")
	}

	// Compute the manifest's canonical-bytes SHA-256 — the value
	// operators pass to `threshold attest sign --hash`.  Manifest
	// signature verification is the caller's responsibility (this
	// package gates consumption, not authenticity).
	canon, err := m.Canonicalize()
	if err != nil {
		return fmt.Errorf("attestgate: canonicalize manifest: %w", err)
	}
	expected := sha256.Sum256(canon)
	expectedHex := hex.EncodeToString(expected[:])

	// Load the roster (we need its threshold value + member list)
	// through a trust-anchored store: a roster whose creator key isn't
	// in opts.TrustedKeys fails to load, so a forged roster can't
	// govern this gate.
	rosters, err := opts.rosterStore(sp)
	if err != nil {
		return err
	}
	r, err := rosters.Get(ctx, opts.RosterID)
	if err != nil {
		switch {
		case errors.Is(err, threshold.ErrRosterNotFound):
			return fmt.Errorf("attestgate: %w: %q", threshold.ErrRosterNotFound, opts.RosterID)
		case errors.Is(err, threshold.ErrRosterUntrusted):
			return fmt.Errorf("%w: %q: %v", ErrRosterUntrusted, opts.RosterID, err)
		}
		return fmt.Errorf("attestgate: load roster %q: %w", opts.RosterID, err)
	}

	// Load the attestation (header + every per-signer signature).
	attStore := threshold.NewAttestationStore(sp)
	att, err := attStore.LoadAttestation(ctx, SubjectKind, m.BackupID)
	if err != nil {
		if errors.Is(err, threshold.ErrAttestationNotFound) {
			return fmt.Errorf("%w: backup_manifest/%s", ErrAttestationMissing, m.BackupID)
		}
		return fmt.Errorf("attestgate: load attestation: %w", err)
	}

	// Cross-check identity: the attestation must pin THIS manifest.
	if att.Header.Subject.Hash != expectedHex {
		return fmt.Errorf("%w: attestation hash %s, manifest hash %s",
			ErrSubjectHashMismatch, att.Header.Subject.Hash, expectedHex)
	}
	if att.Header.RosterID != opts.RosterID {
		return fmt.Errorf("%w: attestation roster %q, requested roster %q",
			ErrRosterMismatch, att.Header.RosterID, opts.RosterID)
	}

	// Run the quorum check.
	res, err := threshold.VerifyAttestation(att.Header, att.Signatures, r)
	if err != nil {
		return fmt.Errorf("attestgate: verify attestation: %w", err)
	}
	if !res.Met {
		return fmt.Errorf("%w: %d of %d valid signatures, threshold %d",
			ErrQuorumNotMet, res.Members, len(att.Signatures), res.Threshold)
	}
	return nil
}

// VerifyResult is a richer report-style return for callers that
// want to surface counts (e.g. an HTTP API, a doctor check).
// VerifyDetailed runs the same check as Verify but returns the
// quorum statistics regardless of pass/fail; callers consult
// res.Met.
type VerifyResult struct {
	RosterID         string
	Threshold        int
	ValidSignatures  int
	TotalSignatures  int
	Met              bool
	SubjectHashOK    bool
	AttestationFound bool
}

// VerifyDetailed mirrors Verify but returns the structured result
// alongside the error.  When the gate is satisfied, err is nil and
// res.Met is true.  When the gate fails, err is one of the typed
// sentinels; res carries whatever counters it could collect before
// the failure.
func VerifyDetailed(ctx context.Context, sp storage.StoragePlugin, m *backup.Manifest, opts Options) (VerifyResult, error) {
	res := VerifyResult{RosterID: opts.RosterID}
	if opts.RosterID == "" {
		return res, nil
	}
	if m == nil {
		return res, errors.New("attestgate: nil manifest")
	}
	if m.BackupID == "" {
		return res, errors.New("attestgate: manifest has no BackupID")
	}

	canon, err := m.Canonicalize()
	if err != nil {
		return res, fmt.Errorf("attestgate: canonicalize manifest: %w", err)
	}
	expected := sha256.Sum256(canon)
	expectedHex := hex.EncodeToString(expected[:])

	rosters, err := opts.rosterStore(sp)
	if err != nil {
		return res, err
	}
	r, err := rosters.Get(ctx, opts.RosterID)
	if err != nil {
		if errors.Is(err, threshold.ErrRosterUntrusted) {
			return res, fmt.Errorf("%w: %q: %v", ErrRosterUntrusted, opts.RosterID, err)
		}
		return res, fmt.Errorf("attestgate: load roster: %w", err)
	}
	res.Threshold = r.Threshold

	attStore := threshold.NewAttestationStore(sp)
	att, err := attStore.LoadAttestation(ctx, SubjectKind, m.BackupID)
	if err != nil {
		if errors.Is(err, threshold.ErrAttestationNotFound) {
			return res, fmt.Errorf("%w: backup_manifest/%s", ErrAttestationMissing, m.BackupID)
		}
		return res, fmt.Errorf("attestgate: load attestation: %w", err)
	}
	res.AttestationFound = true
	res.TotalSignatures = len(att.Signatures)

	if att.Header.Subject.Hash != expectedHex {
		return res, fmt.Errorf("%w: attestation hash %s, manifest hash %s",
			ErrSubjectHashMismatch, att.Header.Subject.Hash, expectedHex)
	}
	res.SubjectHashOK = true

	if att.Header.RosterID != opts.RosterID {
		return res, fmt.Errorf("%w: attestation roster %q, requested roster %q",
			ErrRosterMismatch, att.Header.RosterID, opts.RosterID)
	}

	vr, err := threshold.VerifyAttestation(att.Header, att.Signatures, r)
	if err != nil {
		return res, fmt.Errorf("attestgate: verify attestation: %w", err)
	}
	res.ValidSignatures = vr.Members
	res.Met = vr.Met
	if !vr.Met {
		return res, fmt.Errorf("%w: %d of %d valid signatures, threshold %d",
			ErrQuorumNotMet, vr.Members, len(att.Signatures), vr.Threshold)
	}
	return res, nil
}
