// Package threshold implements k-of-n attestations: a roster of n
// trusted signers, and an attestation that a quorum of k members has
// vouched for some subject (a backup manifest, an audit anchor, a KEK
// rotation, etc.).
//
// This is the SPEC commitment "threshold signing (k-of-n) for
// backup attestations — Multi-party signing for highest-assurance
// manifests."  We deliberately use *multi-signature aggregation*
// (each member signs independently) rather than true threshold
// cryptography:
//
//   - Each individual signature is independently verifiable; an
//     auditor doesn't need to reconstruct the threshold key.
//   - Membership changes (a signer leaves) don't invalidate prior
//     attestations — the roster is content-addressed and prior
//     attestations pin the roster_hash they were signed under.
//   - No share-distribution ceremony is required at roster creation;
//     every signer keeps their existing operator keypair.
//
// Storage layout (all under the repository root):
//
//	threshold/rosters/<id>.json
//	    Canonical roster body, admin-signed.  Carries the list of
//	    members + the threshold k.
//
//	threshold/attestations/<kind>/<id>/header.json
//	    Header for one attestation: subject (kind, id, hash),
//	    roster reference (id, hash), threshold (informational),
//	    creation timestamp.  Idempotent put — concurrent first-
//	    signers race-write the same canonical bytes.
//
//	threshold/attestations/<kind>/<id>/sig.<fingerprint>.json
//	    One per-member signature.  Filename uses the public-key
//	    fingerprint so a member's signature is naturally unique
//	    per attestation (no concurrent-write collision).
//
// The verifier:
//  1. Reads header.
//  2. Fetches the roster, recomputes its canonical hash, asserts
//     equality with header.RosterHash.  (Tampering with the roster
//     after signing is detected here.)
//  3. Lists signatures, decodes each, validates the ed25519 signature
//     against the roster member with that fingerprint.  (Each
//     signature commits to the subject_hash + roster_hash + signer ID
//     + timestamp, so partial tampering is detected.)
//  4. Counts distinct valid roster members; compares to roster.Threshold.
//
// Scope at+: primitive + CLI.  Integration into the backup-commit
// path so manifests can require k-of-n attestation comes in a follow-on
// commit.
package threshold

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema strings carry the v1 contract; bump on any breaking change
// (24-month backward-compat window).
const (
	SchemaRoster            = "pg_hardstorage.threshold.roster.v1"
	SchemaAttestationHeader = "pg_hardstorage.threshold.attestation.v1"
	SchemaAttestationSig    = "pg_hardstorage.threshold.signature.v1"
	canonicalSigPreamble    = "pg_hardstorage.threshold.sig.canon.v1"
	canonicalRosterPreamble = "pg_hardstorage.threshold.roster.canon.v1"
)

// Limits.  Tight enough to keep the attestation cheap to verify, loose
// enough for any realistic governance posture.
const (
	MinThreshold   = 1
	MaxMembers     = 64
	MaxIDLength    = 256
	MaxDescription = 1024
)

// Sentinel errors.  Callers errors.Is against these for control flow.
var (
	ErrRosterNotFound             = errors.New("threshold: roster not found")
	ErrRosterAlreadyExists        = errors.New("threshold: roster already exists")
	ErrRosterUntrusted            = errors.New("threshold: roster creator key is not in the trusted set")
	ErrNoTrustAnchor              = errors.New("threshold: no trusted keys supplied to anchor roster")
	ErrInvalidThreshold           = errors.New("threshold: invalid threshold (k must be 1..n)")
	ErrInvalidMembers             = errors.New("threshold: invalid member list (need 1..MaxMembers, unique)")
	ErrInvalidMember              = errors.New("threshold: invalid member entry")
	ErrInvalidID                  = errors.New("threshold: invalid ID (1..MaxIDLength, [a-z0-9._-])")
	ErrDescriptionTooLong         = errors.New("threshold: description exceeds MaxDescription")
	ErrSignatureInvalid           = errors.New("threshold: signature does not validate")
	ErrRosterHashMismatch         = errors.New("threshold: roster hash mismatch (roster modified after attestation)")
	ErrSubjectHashMismatch        = errors.New("threshold: subject hash mismatch in signature")
	ErrSignerNotInRoster          = errors.New("threshold: signer is not a roster member")
	ErrThresholdNotMet            = errors.New("threshold: not enough valid signatures to meet threshold")
	ErrAttestationNotFound        = errors.New("threshold: attestation not found")
	ErrSubjectAlreadySigned       = errors.New("threshold: this member has already signed the attestation")
	ErrLocalKeyDoesNotMatchMember = errors.New("threshold: local signing key does not match the named roster member")
)

// Signer is the minimal interface this package needs from the host's
// signing keypair.  backup.Signer satisfies it; tests use a struct
// literal.  The package never reads the private key directly.
type Signer interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
}

// Member is one entry in a roster.
type Member struct {
	Signer               string `json:"signer"`                 // operator-friendly identity (email, etc.)
	PublicKeyFingerprint string `json:"public_key_fingerprint"` // 16-hex SHA-256 prefix
	PublicKey            string `json:"public_key"`             // base64 of raw 32-byte ed25519 pubkey
}

// Roster captures a quorum policy: any k of these n members vouching
// for a subject is sufficient.  Admin-signed at creation time.
type Roster struct {
	Schema                      string    `json:"schema"`
	ID                          string    `json:"id"`
	Description                 string    `json:"description,omitempty"`
	Threshold                   int       `json:"threshold"`
	Members                     []Member  `json:"members"`
	CreatedAt                   time.Time `json:"created_at"`
	CreatedBy                   string    `json:"created_by"`
	CreatorPublicKeyFingerprint string    `json:"creator_public_key_fingerprint"`
	Signature                   string    `json:"signature"`
}

// MemberByFingerprint returns the member with the given fingerprint
// or nil if absent.
func (r *Roster) MemberByFingerprint(fp string) *Member {
	for i := range r.Members {
		if r.Members[i].PublicKeyFingerprint == fp {
			return &r.Members[i]
		}
	}
	return nil
}

// MemberBySigner returns the member with the given signer ID or nil.
func (r *Roster) MemberBySigner(signer string) *Member {
	for i := range r.Members {
		if r.Members[i].Signer == signer {
			return &r.Members[i]
		}
	}
	return nil
}

// canonicalRosterBytes returns the deterministic byte sequence the
// admin signs.  Members are sorted by PublicKeyFingerprint so the
// canonical form doesn't depend on argv order.
func canonicalRosterBytes(r *Roster) []byte {
	members := append([]Member(nil), r.Members...)
	sort.Slice(members, func(i, j int) bool {
		return members[i].PublicKeyFingerprint < members[j].PublicKeyFingerprint
	})
	var buf strings.Builder
	buf.WriteString(canonicalRosterPreamble)
	buf.WriteByte(0)
	buf.WriteString(r.ID)
	buf.WriteByte(0)
	buf.WriteString(r.Description)
	buf.WriteByte(0)
	binary.Write(&buf, binary.BigEndian, int64(r.Threshold))
	binary.Write(&buf, binary.BigEndian, int64(len(members)))
	for _, m := range members {
		buf.WriteString(m.Signer)
		buf.WriteByte(0)
		buf.WriteString(m.PublicKeyFingerprint)
		buf.WriteByte(0)
		buf.WriteString(m.PublicKey)
		buf.WriteByte(0)
	}
	binary.Write(&buf, binary.BigEndian, r.CreatedAt.UTC().UnixNano())
	buf.WriteString(r.CreatedBy)
	buf.WriteByte(0)
	buf.WriteString(r.CreatorPublicKeyFingerprint)
	return []byte(buf.String())
}

// RosterHash returns the SHA-256 hex of the canonical roster bytes
// (excluding the signature).  Attestation signatures pin this hash so
// post-hoc roster tampering is detectable.
func RosterHash(r *Roster) string {
	sum := sha256.Sum256(canonicalRosterBytes(r))
	return fmt.Sprintf("%x", sum[:])
}

// SignRoster admin-signs the roster.  Mutates r.CreatedBy +
// r.CreatorPublicKeyFingerprint + r.Signature.  The CreatedBy is
// supplied (operators name themselves; the local key's fingerprint
// is recorded for later cross-check).
func SignRoster(r *Roster, signer Signer, createdBy string) error {
	if signer == nil {
		return errors.New("threshold: nil signer for roster")
	}
	r.Schema = SchemaRoster
	r.CreatedBy = createdBy
	r.CreatorPublicKeyFingerprint = PublicKeyFingerprint(signer.PublicKey())
	canon := canonicalRosterBytes(r)
	r.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// VerifyRoster validates the admin signature.  No identity check on
// the creator — the verifier trusts the roster ID is the right one
// (configured out-of-band).
func VerifyRoster(r *Roster) error {
	if r == nil {
		return errors.New("threshold: nil roster")
	}
	if r.Signature == "" {
		return ErrSignatureInvalid
	}
	pubBytes, err := base64.StdEncoding.DecodeString(memberPublicKeyForFingerprint(r, r.CreatorPublicKeyFingerprint))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		// Creator may not be in members — fall back to looking up by
		// CreatorPublicKeyFingerprint elsewhere.  But the simplest
		// model is: creator's pub key must be in the members list (so
		// the roster is self-contained).  This is what the CLI
		// enforces.
		return errors.New("threshold: creator public key not present in roster members")
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("threshold: roster signature decode: %w", err)
	}
	canon := canonicalRosterBytes(r)
	if !ed25519.Verify(pubBytes, canon, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// VerifyRosterTrusted is VerifyRoster plus a trust anchor: the roster's
// signature must validate under its embedded creator key (self-consistency),
// AND that creator key must be one of the supplied trusted keys.
//
// VerifyRoster alone only proves a roster is internally consistent — signed
// by a key the roster itself names. That lets a repo-write attacker forge a
// 1-of-1 roster naming their own key as sole member + creator, sign it, and
// have it pass: the forged roster then governs which attestations satisfy a
// restore/shred quorum gate. Anchoring the creator key to an out-of-band
// trusted set (the operator keyring that also verifies manifests) closes
// that gap — the same embedded==trusted check backup.ParseAndVerify applies
// to manifests.
//
// Returns ErrNoTrustAnchor when trusted is empty (a misconfiguration — the
// caller asked for a trusted check but supplied nothing to trust), and
// ErrRosterUntrusted when the creator key is not among trusted.
func VerifyRosterTrusted(r *Roster, trusted ...ed25519.PublicKey) error {
	if err := VerifyRoster(r); err != nil {
		return err
	}
	if len(trusted) == 0 {
		return ErrNoTrustAnchor
	}
	creatorBytes, err := base64.StdEncoding.DecodeString(
		memberPublicKeyForFingerprint(r, r.CreatorPublicKeyFingerprint))
	if err != nil || len(creatorBytes) != ed25519.PublicKeySize {
		// VerifyRoster already proved the creator key is a present, valid
		// member key, so this is unreachable in practice; guard anyway.
		return errors.New("threshold: creator public key not present in roster members")
	}
	for _, t := range trusted {
		if len(t) == ed25519.PublicKeySize && bytes.Equal(creatorBytes, t) {
			return nil
		}
	}
	return fmt.Errorf("%w: creator fingerprint %s", ErrRosterUntrusted, r.CreatorPublicKeyFingerprint)
}

func memberPublicKeyForFingerprint(r *Roster, fp string) string {
	if m := r.MemberByFingerprint(fp); m != nil {
		return m.PublicKey
	}
	return ""
}

// ValidateRoster checks the structural invariants on a roster (used
// at create time and at load time).
func ValidateRoster(r *Roster) error {
	if r == nil {
		return errors.New("threshold: nil roster")
	}
	if !validID(r.ID) {
		return ErrInvalidID
	}
	if len(r.Description) > MaxDescription {
		return ErrDescriptionTooLong
	}
	if len(r.Members) == 0 || len(r.Members) > MaxMembers {
		return ErrInvalidMembers
	}
	seenFP := make(map[string]struct{}, len(r.Members))
	seenSigner := make(map[string]struct{}, len(r.Members))
	for _, m := range r.Members {
		if m.Signer == "" || m.PublicKeyFingerprint == "" || m.PublicKey == "" {
			return ErrInvalidMember
		}
		if _, ok := seenFP[m.PublicKeyFingerprint]; ok {
			return fmt.Errorf("%w: duplicate fingerprint %q", ErrInvalidMembers, m.PublicKeyFingerprint)
		}
		if _, ok := seenSigner[m.Signer]; ok {
			return fmt.Errorf("%w: duplicate signer %q", ErrInvalidMembers, m.Signer)
		}
		seenFP[m.PublicKeyFingerprint] = struct{}{}
		seenSigner[m.Signer] = struct{}{}
		// Validate the public-key/fingerprint relationship so a
		// malformed roster can't pass through.
		raw, err := base64.StdEncoding.DecodeString(m.PublicKey)
		if err != nil {
			return fmt.Errorf("%w: %q public_key not base64", ErrInvalidMember, m.Signer)
		}
		if len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: %q public_key wrong length", ErrInvalidMember, m.Signer)
		}
		if PublicKeyFingerprint(ed25519.PublicKey(raw)) != m.PublicKeyFingerprint {
			return fmt.Errorf("%w: %q fingerprint does not match public_key", ErrInvalidMember, m.Signer)
		}
	}
	if r.Threshold < MinThreshold || r.Threshold > len(r.Members) {
		return ErrInvalidThreshold
	}
	return nil
}

// PublicKeyFingerprint returns the canonical 16-hex-char SHA-256
// prefix of an ed25519 public key.  Same shape used by JIT + audit
// bundle; chosen to be uniformly visible to operators (long enough
// to avoid collisions in any realistic roster).
func PublicKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("%x", sum[:8])
}

// AttestationSubject identifies what is being attested.
type AttestationSubject struct {
	Kind string `json:"kind"` // "backup_manifest" | "audit_anchor" | "kek_rotation" | etc.
	ID   string `json:"id"`   // e.g. backup ID, anchor sequence, KEK ref
	Hash string `json:"hash"` // SHA-256 hex of canonical bytes of the subject (caller-supplied)
}

// AttestationHeader is the per-attestation metadata: subject + roster
// reference.  PutHeader treats two headers as logically equal when
// (Schema, Subject, RosterID, RosterHash, Threshold) match — CreatedAt
// drift is informational and does not trigger a conflict.  This makes
// header writes idempotent across:
//
//   - Concurrent first-signers racing on different goroutines (the
//     CLI layer truncates CreatedAt to whole seconds so most concurrent
//     races produce byte-equal bodies; in the rare case where they
//     don't, logical equality covers the gap).
//   - Sequential resigns by the same operator straddling a wallclock
//     second boundary (the v27-audit flake — fixed here).
type AttestationHeader struct {
	Schema     string             `json:"schema"`
	Subject    AttestationSubject `json:"subject"`
	RosterID   string             `json:"roster_id"`
	RosterHash string             `json:"roster_hash"`
	Threshold  int                `json:"threshold"` // informational; verifier uses roster.Threshold
	CreatedAt  time.Time          `json:"created_at"`
}

// AttestationSignature is one member's vouch.  Each is stored as a
// separate object so concurrent signers don't lose each other's work.
type AttestationSignature struct {
	Schema               string             `json:"schema"`
	Subject              AttestationSubject `json:"subject"`
	RosterID             string             `json:"roster_id"`
	RosterHash           string             `json:"roster_hash"`
	Signer               string             `json:"signer"`
	PublicKeyFingerprint string             `json:"public_key_fingerprint"`
	SignedAt             time.Time          `json:"signed_at"`
	Signature            string             `json:"signature"`
}

// canonicalSignatureBytes is the bytes a member's ed25519 signature
// covers.  Length-prefixed to make malleability impossible.
func canonicalSignatureBytes(s *AttestationSignature) []byte {
	var buf strings.Builder
	buf.WriteString(canonicalSigPreamble)
	buf.WriteByte(0)
	for _, field := range []string{
		s.Subject.Kind,
		s.Subject.ID,
		s.Subject.Hash,
		s.RosterID,
		s.RosterHash,
		s.Signer,
		s.PublicKeyFingerprint,
	} {
		binary.Write(&buf, binary.BigEndian, int64(len(field)))
		buf.WriteString(field)
	}
	binary.Write(&buf, binary.BigEndian, s.SignedAt.UTC().UnixNano())
	return []byte(buf.String())
}

// SignAttestation produces an AttestationSignature for the given
// subject + roster, using the supplied local signer.  Asserts the
// local key matches the named roster member; otherwise refuses.  The
// `as` parameter names which roster member we are signing as; if "",
// we look up by the local key's fingerprint (must be unambiguous).
func SignAttestation(
	subject AttestationSubject,
	r *Roster,
	signer Signer,
	as string,
	now time.Time,
) (*AttestationSignature, error) {
	if signer == nil {
		return nil, errors.New("threshold: nil signer")
	}
	if r == nil {
		return nil, errors.New("threshold: nil roster")
	}
	if subject.Kind == "" || subject.ID == "" || subject.Hash == "" {
		return nil, errors.New("threshold: subject missing kind/id/hash")
	}
	fp := PublicKeyFingerprint(signer.PublicKey())
	var member *Member
	switch as {
	case "":
		member = r.MemberByFingerprint(fp)
		if member == nil {
			return nil, fmt.Errorf("%w: no roster member with fingerprint %s",
				ErrLocalKeyDoesNotMatchMember, fp)
		}
	default:
		member = r.MemberBySigner(as)
		if member == nil {
			return nil, fmt.Errorf("%w: roster has no member named %q",
				ErrSignerNotInRoster, as)
		}
		if member.PublicKeyFingerprint != fp {
			return nil, fmt.Errorf("%w: member %q has fingerprint %s, local key has %s",
				ErrLocalKeyDoesNotMatchMember, as, member.PublicKeyFingerprint, fp)
		}
	}
	sig := &AttestationSignature{
		Schema:               SchemaAttestationSig,
		Subject:              subject,
		RosterID:             r.ID,
		RosterHash:           RosterHash(r),
		Signer:               member.Signer,
		PublicKeyFingerprint: member.PublicKeyFingerprint,
		SignedAt:             now.UTC(),
	}
	canon := canonicalSignatureBytes(sig)
	sig.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return sig, nil
}

// VerifySignature validates one attestation signature against the
// roster.  Used by VerifyAttestation; also exposed so callers can
// validate single signatures.
func VerifySignature(sig *AttestationSignature, r *Roster) error {
	if sig == nil {
		return errors.New("threshold: nil signature")
	}
	if sig.Schema != SchemaAttestationSig {
		return fmt.Errorf("threshold: signature schema = %q, want %q", sig.Schema, SchemaAttestationSig)
	}
	if sig.RosterID != r.ID {
		return fmt.Errorf("threshold: signature references roster %q, but verifying against %q",
			sig.RosterID, r.ID)
	}
	if sig.RosterHash != RosterHash(r) {
		return ErrRosterHashMismatch
	}
	member := r.MemberByFingerprint(sig.PublicKeyFingerprint)
	if member == nil {
		return fmt.Errorf("%w: %s", ErrSignerNotInRoster, sig.PublicKeyFingerprint)
	}
	if member.Signer != sig.Signer {
		return fmt.Errorf("%w: fingerprint claims signer %q but roster says %q",
			ErrSignerNotInRoster, sig.Signer, member.Signer)
	}
	pubBytes, err := base64.StdEncoding.DecodeString(member.PublicKey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("threshold: malformed public key for %s: %v", member.Signer, err)
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("threshold: signature decode: %w", err)
	}
	canon := canonicalSignatureBytes(sig)
	if !ed25519.Verify(pubBytes, canon, rawSig) {
		return ErrSignatureInvalid
	}
	return nil
}

// VerifyResult is the structured outcome of a quorum check.
type VerifyResult struct {
	Met               bool                     `json:"met"`
	Threshold         int                      `json:"threshold"`
	Members           int                      `json:"members"`
	ValidSignatures   []*AttestationSignature  `json:"valid_signatures"`
	InvalidSignatures []InvalidSignatureRecord `json:"invalid_signatures,omitempty"`
}

// InvalidSignatureRecord captures one rejected signature so the
// auditor can see what was tampered.
type InvalidSignatureRecord struct {
	Fingerprint string `json:"fingerprint"`
	Signer      string `json:"signer"`
	Reason      string `json:"reason"`
}

// VerifyAttestation enforces the quorum: counts distinct valid
// signatures from roster members, returns Met = true iff that count
// is ≥ roster.Threshold.  Header is verified for hash consistency.
func VerifyAttestation(
	header *AttestationHeader,
	signatures []*AttestationSignature,
	r *Roster,
) (*VerifyResult, error) {
	if header == nil {
		return nil, errors.New("threshold: nil header")
	}
	if r == nil {
		return nil, errors.New("threshold: nil roster")
	}
	if header.RosterID != r.ID {
		return nil, fmt.Errorf("threshold: header references roster %q, but verifying against %q",
			header.RosterID, r.ID)
	}
	if header.RosterHash != RosterHash(r) {
		return nil, ErrRosterHashMismatch
	}
	res := &VerifyResult{Threshold: r.Threshold}
	seen := make(map[string]struct{}, len(signatures))
	for _, s := range signatures {
		if s.Subject.Kind != header.Subject.Kind ||
			s.Subject.ID != header.Subject.ID ||
			s.Subject.Hash != header.Subject.Hash {
			res.InvalidSignatures = append(res.InvalidSignatures, InvalidSignatureRecord{
				Fingerprint: s.PublicKeyFingerprint,
				Signer:      s.Signer,
				Reason:      "subject mismatch with header",
			})
			continue
		}
		if err := VerifySignature(s, r); err != nil {
			res.InvalidSignatures = append(res.InvalidSignatures, InvalidSignatureRecord{
				Fingerprint: s.PublicKeyFingerprint,
				Signer:      s.Signer,
				Reason:      err.Error(),
			})
			continue
		}
		if _, dup := seen[s.PublicKeyFingerprint]; dup {
			// Same member signed twice; only one counts.  Don't flag
			// as invalid — it's just a duplicate, possibly from a
			// retried sign command.
			continue
		}
		seen[s.PublicKeyFingerprint] = struct{}{}
		res.ValidSignatures = append(res.ValidSignatures, s)
	}
	res.Members = len(res.ValidSignatures)
	res.Met = quorumMet(res.Members, r.Threshold)
	return res, nil
}

// ----- storage -----

// RosterStore reads + writes rosters under the standard repo prefix.
type RosterStore struct {
	sp storage.StoragePlugin

	// trusted, when non-empty, anchors roster verification: Get/Put
	// require the roster's creator key to be one of these keys (the
	// operator keyring). Empty keeps the legacy self-consistency-only
	// check for backward compatibility, but security-critical consumers
	// (the attestation gate) MUST set it.
	trusted []ed25519.PublicKey

	// retainUntil + retentionMode carry the repo's WORM policy. When
	// retainUntil is non-zero, Put re-locks the committed roster so a
	// compliance repo can't have its quorum policy deleted/replaced out
	// from under a restore/shred gate. Rosters are create-once (Put
	// refuses overwrite), so locking is consistent with their lifecycle.
	retainUntil   time.Time
	retentionMode storage.WORMMode
}

// NewRosterStore returns a roster store rooted at the given storage
// plugin.  Convention: callers Close the storage plugin themselves.
func NewRosterStore(sp storage.StoragePlugin) *RosterStore {
	return &RosterStore{sp: sp}
}

// WithTrustedKeys returns the store with a trust anchor installed: Get
// and Put will require every roster's creator key to be one of keys.
// Chainable. Pass the operator keyring's public key(s) — the same keys
// that verify manifests.
func (s *RosterStore) WithTrustedKeys(keys ...ed25519.PublicKey) *RosterStore {
	s.trusted = keys
	return s
}

// WithRetention returns the store configured to WORM-lock committed
// rosters until until under mode (empty mode → Compliance). Zero until
// disables locking. Chainable.
func (s *RosterStore) WithRetention(until time.Time, mode storage.WORMMode) *RosterStore {
	s.retainUntil = until
	s.retentionMode = mode
	return s
}

func rosterKey(id string) string { return "threshold/rosters/" + id + ".json" }

// Put writes the roster.  Refuses to overwrite (rosters are append-
// only by design — modify = create new ID).
func (s *RosterStore) Put(ctx context.Context, r *Roster) error {
	if err := ValidateRoster(r); err != nil {
		return err
	}
	if err := s.verify(r); err != nil {
		return err
	}
	body, err := stdjson.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	key := rosterKey(r.ID)
	if _, err := s.sp.Stat(ctx, key); err == nil {
		return fmt.Errorf("%w: %s", ErrRosterAlreadyExists, r.ID)
	}
	// Randomised tmp so concurrent first-writers of the same roster ID don't
	// share a staging path and tear each other's bytes — matching the
	// AttestationStore sibling in this file (a fixed key+".tmp" was the lone
	// torn-overwrite-prone path here).
	tmp := key + ".tmp." + randHex(8)
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("threshold: roster put tmp: %w", err)
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		return fmt.Errorf("threshold: roster commit: %w", err)
	}
	// Re-lock the committed roster on a WORM repo. The rename above carries
	// no retention, so without this the quorum policy that gates restore /
	// kms-shred would stay freely deletable on a compliance repo. Empty mode
	// → Compliance; non-WORM backends return ErrUnsupported, which we ignore.
	if !s.retainUntil.IsZero() {
		mode := s.retentionMode
		if mode == "" {
			mode = storage.WORMCompliance
		}
		if err := s.sp.SetRetention(ctx, key, s.retainUntil, mode); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return fmt.Errorf("threshold: roster lock: %w", err)
		}
	}
	return nil
}

// verify runs the configured roster check: trust-anchored when the store
// has trusted keys, self-consistency-only otherwise (legacy callers).
func (s *RosterStore) verify(r *Roster) error {
	if len(s.trusted) > 0 {
		return VerifyRosterTrusted(r, s.trusted...)
	}
	return VerifyRoster(r)
}

// Get reads + validates the roster.
func (s *RosterStore) Get(ctx context.Context, id string) (*Roster, error) {
	if !validID(id) {
		return nil, ErrInvalidID
	}
	rd, err := s.sp.Get(ctx, rosterKey(id))
	if err != nil {
		// Storage drivers don't share an "is not found" sentinel; the
		// caller maps generic errors to ErrRosterNotFound at the CLI
		// boundary if they want exit code 6.  Here we just signal it
		// when the plugin hasn't returned anything else informative.
		return nil, fmt.Errorf("%w: %s: %v", ErrRosterNotFound, id, err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("threshold: roster read: %w", err)
	}
	var r Roster
	if err := stdjson.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("threshold: roster decode: %w", err)
	}
	if err := ValidateRoster(&r); err != nil {
		return nil, fmt.Errorf("threshold: roster validate-on-load: %w", err)
	}
	if err := s.verify(&r); err != nil {
		return nil, fmt.Errorf("threshold: roster verify-on-load: %w", err)
	}
	return &r, nil
}

// List returns every roster in the repo, sorted by ID.
func (s *RosterStore) List(ctx context.Context) ([]*Roster, error) {
	var out []*Roster
	for obj, err := range s.sp.List(ctx, "threshold/rosters/") {
		if err != nil {
			return nil, fmt.Errorf("threshold: list rosters: %w", err)
		}
		if !strings.HasSuffix(obj.Key, ".json") || strings.HasSuffix(obj.Key, ".tmp") {
			continue
		}
		base := path.Base(obj.Key)
		id := strings.TrimSuffix(base, ".json")
		r, err := s.Get(ctx, id)
		if err != nil {
			// Skip malformed entries so one bad object doesn't break
			// the whole listing — but log via the caller.
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AttestationStore reads + writes attestation headers + signatures
// under the standard repo prefix.
type AttestationStore struct {
	sp storage.StoragePlugin
}

// NewAttestationStore returns an attestation store rooted at sp.
func NewAttestationStore(sp storage.StoragePlugin) *AttestationStore {
	return &AttestationStore{sp: sp}
}

func attestationDir(kind, id string) string {
	return "threshold/attestations/" + kind + "/" + url.PathEscape(id) + "/"
}

func headerKey(kind, id string) string  { return attestationDir(kind, id) + "header.json" }
func sigKey(kind, id, fp string) string { return attestationDir(kind, id) + "sig." + fp + ".json" }

// PutHeader writes the header.  Idempotent on logical equality:
// (Schema, Subject, RosterID, RosterHash, Threshold) — CreatedAt
// drift between writers is silently tolerated (the existing header's
// CreatedAt wins; the existing header is the canonical creation
// time).  Different roster, different subject hash, different
// threshold → conflictErr.
//
// Concurrency: two first-signers race-write the same key.  Both
// Get-checks miss the absent destination, both write a tmp, both
// attempt RenameIfNotExists.  Only one rename wins.  The loser
// re-reads the destination and takes the logical-equality branch.
//
// Pre-v27 the dup-check was strict byte-equality.  That broke when
// the CLI's CreatedAt truncation landed on opposite sides of a
// second boundary across two sequential signs in the same process —
// the second sign would surface put_header_failed instead of
// reaching the signature path's already_signed.  The current logic
// fixes that flake without weakening the "different roster" guard
// (which is the actual security-relevant comparison).
func (s *AttestationStore) PutHeader(ctx context.Context, h *AttestationHeader) error {
	if h.Schema == "" {
		h.Schema = SchemaAttestationHeader
	}
	body, err := stdjson.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	key := headerKey(h.Subject.Kind, h.Subject.ID)
	conflictErr := fmt.Errorf("threshold: existing header for %s/%s differs from new content",
		h.Subject.Kind, h.Subject.ID)

	// Existing-header path: read + logical-equality check.
	if existing, gerr := s.GetHeader(ctx, h.Subject.Kind, h.Subject.ID); gerr == nil {
		if headersLogicallyEqual(existing, h) {
			return nil
		}
		return conflictErr
	}

	// First-write path: tmp + RenameIfNotExists.  On rename failure
	// (concurrent winner), re-read and take the logical-equality
	// branch.
	tmp := key + ".tmp." + randHex(8)
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return err
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = s.sp.Delete(ctx, tmp)
		if existing, gerr := s.GetHeader(ctx, h.Subject.Kind, h.Subject.ID); gerr == nil {
			if headersLogicallyEqual(existing, h) {
				return nil
			}
			return conflictErr
		}
		return err
	}
	return nil
}

// headersLogicallyEqual reports whether two headers refer to the same
// (subject, roster, threshold) tuple.  CreatedAt is excluded — that's
// the whole point of the logical equality.  Schema is included so a
// schema-version drift is caught as a conflict.
func headersLogicallyEqual(a, b *AttestationHeader) bool {
	return a.Schema == b.Schema &&
		a.Subject == b.Subject &&
		a.RosterID == b.RosterID &&
		a.RosterHash == b.RosterHash &&
		a.Threshold == b.Threshold
}

// GetHeader reads + decodes the header.
func (s *AttestationStore) GetHeader(ctx context.Context, kind, id string) (*AttestationHeader, error) {
	rd, err := s.sp.Get(ctx, headerKey(kind, id))
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %v", ErrAttestationNotFound, kind, id, err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, err
	}
	var h AttestationHeader
	if err := stdjson.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("threshold: header decode: %w", err)
	}
	return &h, nil
}

// PutSignature writes one signature.  Idempotent on identical bytes.
// Refuses to overwrite a different signature for the same fingerprint
// (resign-with-changed-content is an attack vector).
//
// The TOCTOU posture is the same as PutHeader: if two operators race
// to write byte-equal signatures, the loser's RenameIfNotExists will
// fail and the helper re-checks the destination's bytes.  Equal →
// success; differ → ErrSubjectAlreadySigned.
func (s *AttestationStore) PutSignature(ctx context.Context, sig *AttestationSignature) error {
	if sig.Schema == "" {
		sig.Schema = SchemaAttestationSig
	}
	body, err := stdjson.MarshalIndent(sig, "", "  ")
	if err != nil {
		return err
	}
	key := sigKey(sig.Subject.Kind, sig.Subject.ID, sig.PublicKeyFingerprint)
	conflictErr := fmt.Errorf("%w: %s already signed (existing signature differs)",
		ErrSubjectAlreadySigned, sig.Signer)
	return commitIfBytesMatch(ctx, s.sp, key, body, conflictErr)
}

// commitIfBytesMatch is the shared race-safe commit helper.  Sequence:
//
//  1. Try Get(key).  If it returns content equal to body, success.
//     If it returns content differing from body, return conflictErr.
//  2. Otherwise Put a tmp file under a randomised suffix (per-call
//     unique so concurrent first-writers don't trample each other's
//     staging), then RenameIfNotExists into key.
//  3. If the rename fails (typically because a concurrent writer won
//     the race), re-Get(key) and compare.  Equal → success; differ →
//     conflictErr; some other Get error → return the original rename
//     error.  We always best-effort delete our tmp on the way out.
func commitIfBytesMatch(
	ctx context.Context,
	sp storage.StoragePlugin,
	key string,
	body []byte,
	conflictErr error,
) error {
	if rd, err := sp.Get(ctx, key); err == nil {
		existing, _ := io.ReadAll(rd)
		_ = rd.Close()
		if string(existing) == string(body) {
			return nil
		}
		return conflictErr
	}
	tmp := key + ".tmp." + randHex(8)
	if _, err := sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return err
	}
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Best-effort tmp cleanup; the rename outcome is what we care
		// about.
		_ = sp.Delete(ctx, tmp)
		// Race: did a concurrent writer win this slot with byte-equal
		// content?  Re-Get and compare; treat byte-equal as idempotent.
		if rd, gerr := sp.Get(ctx, key); gerr == nil {
			existing, _ := io.ReadAll(rd)
			_ = rd.Close()
			if string(existing) == string(body) {
				return nil
			}
			return conflictErr
		}
		return err
	}
	return nil
}

// randHex returns 2*n hex characters from crypto/rand.  Used for
// tmp-file disambiguation under concurrent first-writers.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}

// ListSignatures returns every signature for the given attestation.
func (s *AttestationStore) ListSignatures(ctx context.Context, kind, id string) ([]*AttestationSignature, error) {
	dir := attestationDir(kind, id)
	var out []*AttestationSignature
	for obj, err := range s.sp.List(ctx, dir) {
		if err != nil {
			return nil, fmt.Errorf("threshold: list signatures: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasPrefix(base, "sig.") || !strings.HasSuffix(base, ".json") {
			continue
		}
		if strings.HasSuffix(base, ".tmp") {
			continue
		}
		rd, err := s.sp.Get(ctx, obj.Key)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(rd)
		_ = rd.Close()
		var sig AttestationSignature
		if err := stdjson.Unmarshal(body, &sig); err != nil {
			continue
		}
		out = append(out, &sig)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SignedAt.Equal(out[j].SignedAt) {
			return out[i].Signer < out[j].Signer
		}
		return out[i].SignedAt.Before(out[j].SignedAt)
	})
	return out, nil
}

// LoadAttestation reads header + all signatures in one call.
type Attestation struct {
	Header     *AttestationHeader      `json:"header"`
	Signatures []*AttestationSignature `json:"signatures"`
}

// LoadAttestation reads header + all signatures for one (kind, id).
func (s *AttestationStore) LoadAttestation(ctx context.Context, kind, id string) (*Attestation, error) {
	h, err := s.GetHeader(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	sigs, err := s.ListSignatures(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	return &Attestation{Header: h, Signatures: sigs}, nil
}

// ----- helpers -----

// validID accepts our restricted ID alphabet: lowercase, digits, dots,
// underscores, hyphens.  No whitespace, no slashes, no path traversal.
func validID(id string) bool {
	if id == "" || len(id) > MaxIDLength {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// NewMember constructs a Member from a public-key blob (raw 32-byte
// ed25519 pubkey).  Computes the fingerprint deterministically.
func NewMember(signer string, pub ed25519.PublicKey) Member {
	return Member{
		Signer:               signer,
		PublicKeyFingerprint: PublicKeyFingerprint(pub),
		PublicKey:            base64.StdEncoding.EncodeToString(pub),
	}
}

// ParseMemberSpec parses an inline member spec of the form
// "signer:public_key_base64".  Used by the CLI for repeatable
// --member flags.  Returns (Member, ok); on parse failure returns
// the zero member and false.
func ParseMemberSpec(spec string) (Member, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return Member{}, fmt.Errorf("%w: %q (expected signer:public_key_base64)",
			ErrInvalidMember, spec)
	}
	signer := strings.TrimSpace(parts[0])
	pubB64 := strings.TrimSpace(parts[1])
	if signer == "" || pubB64 == "" {
		return Member{}, fmt.Errorf("%w: %q (signer or pubkey empty)", ErrInvalidMember, spec)
	}
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return Member{}, fmt.Errorf("%w: %q: pubkey not base64", ErrInvalidMember, spec)
	}
	if len(raw) != ed25519.PublicKeySize {
		return Member{}, fmt.Errorf("%w: %q: pubkey length %d != %d", ErrInvalidMember,
			spec, len(raw), ed25519.PublicKeySize)
	}
	return NewMember(signer, raw), nil
}

// NewRandomKeypair is a convenience for tests.  Production code
// generates keypairs via the operator's keystore.
func NewRandomKeypair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return pub, priv, err
}

// NewRoster is a convenience for callers that want to assemble a
// roster programmatically.  Sets schema + created_at.
func NewRoster(id, description string, threshold int, members []Member, now time.Time) *Roster {
	return &Roster{
		Schema:      SchemaRoster,
		ID:          id,
		Description: description,
		Threshold:   threshold,
		Members:     members,
		CreatedAt:   now.UTC(),
	}
}
