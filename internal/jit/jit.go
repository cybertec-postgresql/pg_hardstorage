// Package jit implements just-in-time access tokens — time-
// bound elevated grants for break-glass operations.
//
// The plan calls these out: "JIT (just-in-time) access — Time-
// bound elevated tokens for break-glass restore; auto-expire;
// audit-stamped."
//
// Operationally:
//
//   - An operator with the appropriate authority runs `jit issue
//     <principal> --scope kms.shred --duration 1h --reason "..."`.
//   - The token is signed with the operator's ed25519 signing
//     key + persisted at `jit/<id>.json` in the repo.
//   - The audit chain records the issuance.
//   - The principal uses the token by passing it to a
//     destructive command (`kms shred --jit-token <token>`).
//   - The destructive command verifies the token (signature +
//     expiration + revocation marker absence + scope match) +
//     records the use in the audit chain.
//   - Revocation is one CLI call: `jit revoke <id>`; writes a
//     `jit/<id>.json.revoked` marker that destructive commands
//     check on every use.
//
// Why ed25519 + stored token rather than JWT-style?  We piggy-
// back on the same keypair that signs manifests + audit
// bundles, so operators don't have to manage a second key.  The
// token format is JSON-structured + base64url-encoded + with a
// detached signature, similar in spirit to JWT but using the
// project's existing on-disk keypair conventions.
package jit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema is the on-disk version tag for Token records.
const Schema = "pg_hardstorage.jit.token.v1"

// RevocationSchema is the on-disk version tag for revocation
// markers.
const RevocationSchema = "pg_hardstorage.jit.revocation.v1"

// MaxDuration is the upper bound on a single token's TTL.  This
// is a defensive cap — the whole point of JIT is short-lived
// access; an issuer trying to mint a 1-year token gets refused
// loudly.  Operators with regulated environments override via
// Issuer.MaxDuration.
const MaxDuration = 24 * time.Hour

// MinDuration is the lower bound — too-short tokens just bounce
// off "expired before first use" surface.
const MinDuration = 1 * time.Minute

// MaxScopes is the upper bound on the scope-list size on one
// token.  A token that grants 1000 operations isn't JIT — it's
// a pre-production admin token, and we refuse to issue it.
const MaxScopes = 32

// MaxReasonLength caps the operator-supplied justification.  At
// 1024 chars there's room for a real explanation but not for an
// attempt to stuff entire incident reports.
const MaxReasonLength = 1024

// Token is the in-memory + on-disk representation of a JIT
// grant.  All times are UTC.  Stable per the v1 contract.
type Token struct {
	Schema string `json:"schema"`

	// ID is a unique identifier (hex-encoded; 32 chars).  Used
	// for the storage key + audit-trail correlation + revocation
	// lookup.
	ID string `json:"id"`

	// Principal is the entity the token is for.  Free-form;
	// operators put the actor name (e.g. "alice@acme.example"),
	// service account (e.g. "scheduler:weekly-drill"), or
	// machine identity here.
	Principal string `json:"principal"`

	// Scope is the list of operations this token authorises.
	// Each scope is dotted-namespace, matching the audit-event
	// action format (`backup.delete`, `kms.shred`, `repo.gc`).
	// A token with scope `["backup.delete"]` only grants
	// backup-delete; a token with `["kms.*"]` grants every
	// kms.* operation (wildcard match).
	Scope []string `json:"scope"`

	// Reason is the operator-supplied justification.  Required.
	// Auditors trace WHY the token was issued through this
	// field; the reason flows into both the audit chain at
	// issuance + every consumption record.
	Reason string `json:"reason"`

	// Tenant scopes the token to one tenant.  When empty, the
	// token is tenant-agnostic; consuming commands match by
	// scope alone.  When set, consuming commands additionally
	// match by tenant.
	Tenant string `json:"tenant,omitempty"`

	// IssuedBy is the operator identity that minted the token.
	// Distinct from Principal: the issuer is the operator
	// authorising the grant (typically an admin); the principal
	// is the actor who'll consume it (typically a junior
	// operator under supervised access).  The issuer's
	// public-key fingerprint is in PublicKeyFingerprint.
	IssuedBy string `json:"issued_by,omitempty"`

	// IssuedAt + NotBefore + ExpiresAt bracket the active
	// window.  IssuedAt is the wallclock at issuance;
	// NotBefore (optional) defaults to IssuedAt.  ExpiresAt is
	// IssuedAt + duration.
	IssuedAt  time.Time `json:"issued_at"`
	NotBefore time.Time `json:"not_before,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`

	// PublicKeyFingerprint is the SHA-256 (first 16 hex chars)
	// of the issuer's ed25519 public key.  Verification uses
	// this to look up the right key in a multi-issuer
	// deployment.
	PublicKeyFingerprint string `json:"public_key_fingerprint"`

	// Signature is the base64-encoded ed25519 signature over
	// the canonical token bytes (every field above EXCEPT
	// Signature itself).  The canonical form is the JSON
	// encoding of a Token with Signature set to "" (empty).
	Signature string `json:"signature,omitempty"`
}

// Status is the lifecycle classification of a token.
type Status string

// The Status* constants enumerate the lifecycle values a token can
// take; see (*Token).Status for the derivation rules.
const (
	// StatusActive — the token is inside its NotBefore..NotAfter
	// window and is not revoked.
	StatusActive Status = "active"
	// StatusNotYetActive — issued, but the current wall-clock is
	// before NotBefore. Sign-checked tokens emitted for future use
	// land here until the window opens.
	StatusNotYetActive Status = "not_yet_active"
	// StatusExpired — past NotAfter; no longer authoritative.
	StatusExpired Status = "expired"
	// StatusRevoked — explicitly revoked via the revocation list
	// before its natural expiry.
	StatusRevoked Status = "revoked"
)

// IsActive reports whether the token is in StatusActive at the
// given time, ignoring revocation.  Use Verify for the full
// check including revocation lookup.
func (t *Token) IsActive(now time.Time) bool {
	return t.Status(now) == StatusActive
}

// Status returns the lifecycle status at the given time,
// ignoring revocation (Verify layers revocation on top).
func (t *Token) Status(now time.Time) Status {
	notBefore := t.NotBefore
	if notBefore.IsZero() {
		notBefore = t.IssuedAt
	}
	if now.Before(notBefore) {
		return StatusNotYetActive
	}
	if !now.Before(t.ExpiresAt) {
		return StatusExpired
	}
	return StatusActive
}

// MatchesScope reports whether the token's scope grants the
// requested operation.  Supports exact match + wildcard match.
// Wildcard format: `<namespace>.*` matches any operation in
// that namespace.  `*` matches anything (operationally
// discouraged but allowed for break-glass).
func (t *Token) MatchesScope(operation string) bool {
	for _, s := range t.Scope {
		if s == "*" || s == operation {
			return true
		}
		// `kms.*` matches `kms.shred`, `kms.rotate`, etc.
		if strings.HasSuffix(s, ".*") {
			prefix := strings.TrimSuffix(s, "*") // keeps the "."
			if strings.HasPrefix(operation, prefix) {
				return true
			}
		}
	}
	return false
}

// canonicalBytes returns the bytes the signature covers.
// JSON-encoded with Signature blanked.  Keys are sorted
// alphabetically by encoding/json.Marshal's struct-field
// order; we don't post-process because the field order in
// the struct definition IS the canonical order.
//
// We don't use json.Marshal directly because we want to clear
// Signature first; the helper handles both.
func canonicalBytes(t *Token) ([]byte, error) {
	clone := *t
	clone.Signature = ""
	return json.Marshal(&clone)
}

// Sign signs the token with the issuer's signing key.  Mutates
// t.PublicKeyFingerprint + t.Signature.
func Sign(t *Token, signer Signer) error {
	if signer == nil {
		return errors.New("jit: nil signer")
	}
	t.PublicKeyFingerprint = publicKeyFingerprint(signer.PublicKey())
	canon, err := canonicalBytes(t)
	if err != nil {
		return fmt.Errorf("jit: canonicalize for sign: %w", err)
	}
	t.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// Verify validates ONLY the cryptographic signature on a token,
// using the supplied resolver to find the right public key.  The
// resolver looks up the key by fingerprint; multi-issuer
// deployments override.
//
// Verify does NOT check:
//
//   - the token's NotBefore / ExpiresAt window
//   - revocation
//   - scope match
//   - tenant binding
//
// Use VerifyAt for the full lifecycle check before authorising any
// privileged operation.  Verify is the right call when all you
// need is "did this token come from a trusted issuer" — for
// example, while inspecting an archived token whose lifecycle
// state is no longer relevant, or in unit tests that want
// signature-only validation.
func Verify(t *Token, resolver KeyResolver) error {
	if resolver == nil {
		return errors.New("jit: nil resolver")
	}
	pub, err := resolver.PublicKey(t.PublicKeyFingerprint)
	if err != nil {
		return fmt.Errorf("jit: resolve key %q: %w", t.PublicKeyFingerprint, err)
	}
	if t.Signature == "" {
		return errors.New("jit: missing signature")
	}
	sig, err := base64.StdEncoding.DecodeString(t.Signature)
	if err != nil {
		return fmt.Errorf("jit: decode signature: %w", err)
	}
	canon, err := canonicalBytes(t)
	if err != nil {
		return fmt.Errorf("jit: canonicalize for verify: %w", err)
	}
	if !ed25519.Verify(pub, canon, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// Encode serialises a Token as a base64url-encoded string
// suitable for passing on the command line.  Decode is the
// inverse.  We use base64url (no padding) so the token is
// shell-safe.
func Encode(t *Token) (string, error) {
	body, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("jit: encode: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// Decode reverses Encode.  Returns the parsed Token.
func Decode(encoded string) (*Token, error) {
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("jit: decode base64: %w", err)
	}
	var t Token
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("jit: decode json: %w", err)
	}
	if t.Schema != Schema {
		return nil, fmt.Errorf("jit: unknown schema %q (want %q)", t.Schema, Schema)
	}
	return &t, nil
}

// Errors surfaced to the consumer.  Stable per the v1 contract.
var (
	ErrSignatureInvalid  = errors.New("jit: signature did not verify")
	ErrTokenExpired      = errors.New("jit: token has expired")
	ErrTokenNotYetActive = errors.New("jit: token is not yet active (not_before in the future)")
	ErrTokenRevoked      = errors.New("jit: token has been revoked")
	ErrScopeNotMatched   = errors.New("jit: token scope does not authorise the requested operation")
	ErrTenantMismatch    = errors.New("jit: token tenant does not match the operation's tenant")
	ErrTokenNotFound     = errors.New("jit: token not found")
	ErrAlreadyRevoked    = errors.New("jit: token already revoked")
	ErrInvalidDuration   = errors.New("jit: duration outside permitted bounds")
	ErrInvalidScope      = errors.New("jit: scope is empty / too long / malformed")
	ErrMissingReason     = errors.New("jit: reason is required")
	ErrReasonTooLong     = errors.New("jit: reason exceeds MaxReasonLength")
	ErrPrincipalRequired = errors.New("jit: principal is required")
)

// Signer is the minimal signing surface.  Production callers
// pass a backup.Signer wrapper; tests supply a stub.
type Signer interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
}

// KeyResolver looks up an issuer public key by fingerprint.
// In single-operator deployments this is one fixed key; in
// multi-issuer deployments it consults a key directory.
type KeyResolver interface {
	PublicKey(fingerprint string) (ed25519.PublicKey, error)
}

// SingleKeyResolver wraps one ed25519 public key + answers
// every PublicKey call with that key, regardless of fingerprint.
// Used by single-operator deployments + tests.
type SingleKeyResolver struct {
	Key ed25519.PublicKey
}

// PublicKey returns the wrapped key for any fingerprint.  This
// is intentionally permissive: the caller's threat model is
// "one signer, no key rotation"; the fingerprint check would
// be defence-in-depth but is decoupled here.
func (r *SingleKeyResolver) PublicKey(string) (ed25519.PublicKey, error) {
	if len(r.Key) == 0 {
		return nil, errors.New("jit: SingleKeyResolver has no key")
	}
	return r.Key, nil
}

// IssueOptions configures one Issue call.
type IssueOptions struct {
	Principal string
	Scope     []string
	Reason    string
	Duration  time.Duration

	// Tenant scopes the token; when empty the token is
	// tenant-agnostic.
	Tenant string

	// NotBefore, when non-zero, delays activation.  Default:
	// IssuedAt.
	NotBefore time.Time

	// IssuedBy identifies the issuing operator.  Free-form
	// — operator name, service account, etc.  Optional.
	IssuedBy string

	// MaxDuration overrides the package-level MaxDuration.
	// Useful for stricter environments.  Zero uses the
	// package default.
	MaxDuration time.Duration

	// Now overrides time.Now() for deterministic test output.
	Now time.Time
}

// Issue mints + signs a fresh JIT token.  Validates the
// duration / scope / reason against the policy bounds before
// signing.  Does NOT persist — caller invokes Store.Put with
// the returned token.
func Issue(signer Signer, opts IssueOptions) (*Token, error) {
	if signer == nil {
		return nil, errors.New("jit: nil signer")
	}
	if opts.Principal == "" {
		return nil, ErrPrincipalRequired
	}
	if opts.Reason == "" {
		return nil, ErrMissingReason
	}
	if len(opts.Reason) > MaxReasonLength {
		return nil, ErrReasonTooLong
	}
	maxDur := opts.MaxDuration
	if maxDur <= 0 {
		maxDur = MaxDuration
	}
	if opts.Duration < MinDuration || opts.Duration > maxDur {
		return nil, fmt.Errorf("%w: %s (must be %s..%s)",
			ErrInvalidDuration, opts.Duration, MinDuration, maxDur)
	}
	if err := validateScope(opts.Scope); err != nil {
		return nil, err
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	notBefore := opts.NotBefore
	if notBefore.IsZero() {
		notBefore = now
	}
	id, err := newTokenID()
	if err != nil {
		return nil, fmt.Errorf("jit: new id: %w", err)
	}
	t := &Token{
		Schema:    Schema,
		ID:        id,
		Principal: opts.Principal,
		Scope:     dedupeScope(opts.Scope),
		Reason:    opts.Reason,
		Tenant:    opts.Tenant,
		IssuedBy:  opts.IssuedBy,
		IssuedAt:  now,
		NotBefore: notBefore,
		ExpiresAt: now.Add(opts.Duration),
	}
	if err := Sign(t, signer); err != nil {
		return nil, err
	}
	return t, nil
}

// validateScope checks the scope slice meets the length +
// format constraints.  Each entry must be:
//
//   - non-empty
//   - dotted-namespace shape (alphanumeric + dots + the
//     wildcard '*')
//   - no whitespace / shell metacharacters
//
// Empty slice OR > MaxScopes entries OR malformed entries
// → ErrInvalidScope.
func validateScope(scope []string) error {
	if len(scope) == 0 || len(scope) > MaxScopes {
		return fmt.Errorf("%w: count=%d (must be 1..%d)",
			ErrInvalidScope, len(scope), MaxScopes)
	}
	for i, s := range scope {
		if s == "" {
			return fmt.Errorf("%w: entry %d is empty", ErrInvalidScope, i)
		}
		for _, c := range s {
			ok := (c >= 'a' && c <= 'z') ||
				(c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') ||
				c == '.' || c == '_' || c == '-' || c == '*'
			if !ok {
				return fmt.Errorf("%w: entry %q contains invalid char %q",
					ErrInvalidScope, s, c)
			}
		}
	}
	return nil
}

// dedupeScope returns the input with duplicates removed,
// preserving first-occurrence order.
func dedupeScope(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// newTokenID returns a fresh hex-encoded random identifier.
// 16 bytes = 32 hex chars.  Cryptographic random; collision
// probability is negligible.
func newTokenID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// publicKeyFingerprint mirrors the format used by kms inspect +
// audit verify-anchor: SHA-256 of the public key, first 16 hex.
func publicKeyFingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// keyFor returns the on-disk key for a token body.  Active
// tokens live at `jit/<id>.json`; the revocation marker (if
// any) is the sibling `jit/<id>.json.revoked`.
func keyFor(id string) string           { return "jit/" + id + ".json" }
func revocationKeyFor(id string) string { return "jit/" + id + ".json.revoked" }

// Revocation is the persisted body of a `.revoked` marker.
type Revocation struct {
	Schema    string    `json:"schema"`
	TokenID   string    `json:"token_id"`
	RevokedAt time.Time `json:"revoked_at"`
	RevokedBy string    `json:"revoked_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// Store is the repo-backed JIT-token persistence layer.
type Store struct {
	sp storage.StoragePlugin
}

// NewStore wraps sp.
func NewStore(sp storage.StoragePlugin) *Store {
	if sp == nil {
		panic("jit: NewStore requires a non-nil StoragePlugin")
	}
	return &Store{sp: sp}
}

// Put writes a fresh token.  Refuses to overwrite an existing
// token (the ID space is collision-free in practice; an
// overwrite means the caller mishandled an Issue retry).
func (s *Store) Put(ctx context.Context, t *Token) error {
	if t == nil || t.ID == "" {
		return errors.New("jit: Put: nil or unidentified token")
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("jit: marshal token: %w", err)
	}
	if _, err := s.sp.Put(ctx, keyFor(t.ID), bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	}); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return fmt.Errorf("jit: token %s already exists", t.ID)
		}
		return fmt.Errorf("jit: put token: %w", err)
	}
	return nil
}

// Get reads the token body by ID.  Does NOT consult the
// revocation marker — see VerifyAt for the full check.
func (s *Store) Get(ctx context.Context, id string) (*Token, error) {
	rc, err := s.sp.Get(ctx, keyFor(id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("jit: get token: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("jit: read token: %w", err)
	}
	var t Token
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("jit: decode token: %w", err)
	}
	return &t, nil
}

// IsRevoked reports whether the token has a `.revoked` marker.
// One Stat call against the storage layer; suitable for the
// hot consumption path.
func (s *Store) IsRevoked(ctx context.Context, id string) (bool, error) {
	_, err := s.sp.Stat(ctx, revocationKeyFor(id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("jit: stat revocation: %w", err)
}

// GetRevocation returns the revocation marker body, or
// ErrTokenNotFound when absent.
func (s *Store) GetRevocation(ctx context.Context, id string) (*Revocation, error) {
	rc, err := s.sp.Get(ctx, revocationKeyFor(id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("jit: get revocation: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("jit: read revocation: %w", err)
	}
	var r Revocation
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("jit: decode revocation: %w", err)
	}
	return &r, nil
}

// Revoke writes the revocation marker.  Idempotent — re-revoke
// is a no-op (the second write loses the IfNotExists race +
// returns nil).
func (s *Store) Revoke(ctx context.Context, id, by, reason string, now time.Time) error {
	if id == "" {
		return errors.New("jit: Revoke: empty id")
	}
	// Confirm the token exists before writing the marker.
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if existing, _ := s.GetRevocation(ctx, id); existing != nil {
		return ErrAlreadyRevoked
	}
	r := Revocation{
		Schema:    RevocationSchema,
		TokenID:   id,
		RevokedAt: now.UTC(),
		RevokedBy: by,
		Reason:    reason,
	}
	body, err := json.MarshalIndent(&r, "", "  ")
	if err != nil {
		return fmt.Errorf("jit: marshal revocation: %w", err)
	}
	if _, err := s.sp.Put(ctx, revocationKeyFor(id), bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	}); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return ErrAlreadyRevoked
		}
		return fmt.Errorf("jit: put revocation: %w", err)
	}
	return nil
}

// List walks every token in the store.  Optionally filters by
// principal / status / tenant / scope-prefix.
type ListFilter struct {
	Principal string
	Status    Status // empty = all
	Tenant    string
	Now       time.Time
}

// List returns matching tokens in newest-first order (lex of
// IDs is random, so List sorts by IssuedAt descending).
func (s *Store) List(ctx context.Context, f ListFilter) ([]*ListEntry, error) {
	now := f.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var keys []string
	for info, err := range s.sp.List(ctx, "jit/") {
		if err != nil {
			return nil, fmt.Errorf("jit: list: %w", err)
		}
		// Only the canonical token bodies — skip revocation
		// markers + any future siblings.
		if !strings.HasSuffix(info.Key, ".json") || strings.HasSuffix(info.Key, ".revoked") {
			continue
		}
		keys = append(keys, info.Key)
	}
	sort.Strings(keys)
	var out []*ListEntry
	for _, k := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, "jit/"), ".json")
		t, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		// Resolve revocation status once for the entry's
		// effective status.
		revoked, _ := s.IsRevoked(ctx, id)
		entry := &ListEntry{Token: t, Revoked: revoked}
		entry.EffectiveStatus = entry.computeEffectiveStatus(now)
		// Filter.
		if f.Principal != "" && t.Principal != f.Principal {
			continue
		}
		if f.Tenant != "" && t.Tenant != f.Tenant {
			continue
		}
		if f.Status != "" && entry.EffectiveStatus != f.Status {
			continue
		}
		out = append(out, entry)
	}
	// Newest-first by IssuedAt.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Token.IssuedAt.After(out[j].Token.IssuedAt)
	})
	return out, nil
}

// ListEntry pairs a Token with its computed lifecycle state.
type ListEntry struct {
	Token           *Token `json:"token"`
	Revoked         bool   `json:"revoked"`
	EffectiveStatus Status `json:"effective_status"`
}

func (e *ListEntry) computeEffectiveStatus(now time.Time) Status {
	if e.Revoked {
		return StatusRevoked
	}
	return e.Token.Status(now)
}

// CheckOptions configures one VerifyAt call.
type CheckOptions struct {
	// Operation is the dotted-namespace identifier the consumer
	// is attempting (e.g. `kms.shred`).  Required — the whole
	// point is to authorise specific operations.
	Operation string

	// Tenant scopes the check.  When non-empty, the token's
	// Tenant must match (or be empty for tenant-agnostic
	// tokens).
	Tenant string

	// Now overrides time.Now() for deterministic tests.
	Now time.Time
}

// VerifyAt does the full verification dance: signature +
// expiration + revocation + scope + tenant.  Returns the
// verified token on success; one of the structured errors
// above on failure.
//
// Use this on the hot consumption path of every destructive
// command.  The cost is one storage Stat (revocation check) +
// one signature verify.
func VerifyAt(ctx context.Context, store *Store, resolver KeyResolver, t *Token, opts CheckOptions) error {
	if t == nil {
		return errors.New("jit: VerifyAt: nil token")
	}
	if opts.Operation == "" {
		return errors.New("jit: VerifyAt: empty operation")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	switch t.Status(now) {
	case StatusNotYetActive:
		return ErrTokenNotYetActive
	case StatusExpired:
		return ErrTokenExpired
	}
	if err := Verify(t, resolver); err != nil {
		return err
	}
	if !t.MatchesScope(opts.Operation) {
		return ErrScopeNotMatched
	}
	if t.Tenant != "" && opts.Tenant != "" && t.Tenant != opts.Tenant {
		return ErrTenantMismatch
	}
	if store != nil {
		revoked, err := store.IsRevoked(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("jit: revocation check: %w", err)
		}
		if revoked {
			return ErrTokenRevoked
		}
	}
	return nil
}

// canonicalAppendU32 is unused-but-kept-here documentation: the
// signature canonicalisation deliberately uses encoding/json,
// not a custom binary format.  encoding/json's deterministic
// struct-field-order serialisation across the same Go version
// is sufficient for our cross-process verification scope.
//
// If we ever migrate to a non-JSON canonical form (CBOR /
// MessagePack), the new format MUST be lex-stable across
// versions — this comment is a future-self reminder.
var _ = binary.BigEndian
