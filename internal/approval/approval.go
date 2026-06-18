// Package approval implements n-of-m approval workflows for
// destructive operations. The plan calls these out specifically
// for `kms shred`, `repo gc --delete`, `backup delete --force`, and
// `repo wipe` — anything that can't be undone deserves more than
// one operator's say-so.
//
// The model is deliberately small for:
//
//   - An initiator creates a Request specifying the op, the target,
//     a reason, a TTL, an approval threshold N, and an allowlist of
//     approver public keys (PEM-encoded ed25519, same shape as the
//     manifest-signing keys we already use). The Request is written
//     to the repo at approvals/<id>.json.
//   - An approver fetches the Request, decides yes/no, and posts an
//     Approval — a signed envelope containing the request ID, the
//     approver's public-key fingerprint, and a timestamp. The
//     approval lands inside the Request's Approvals slice via a
//     read-modify-write against the same key.
//   - Status() reads the Request, counts unique-approver
//     signatures over the canonical request bytes, and decides
//     pending / approved / expired / revoked.
//   - A destructive op consults Status() before mutating anything;
//     not-yet-approved → refuse with a structured "request more
//     approvals" error including the request ID.
//
// The audit chain (internal/audit) gets the trail: every Create +
// every Approve emits a hash-chained event, so a forensic walk of
// the repo can reconstruct who initiated, who approved, and when.
//
// What's deliberately NOT here for:
//
//   - Identity provider integration (OIDC / SAML / SCIM).
//     Approver identity is "who holds the matching ed25519 key";
//     layers a real identity provider on top.
//   - JIT (just-in-time) tokens for break-glass approvals;.
//   - Multi-region replication of approval requests; the repo's
//     existing cross-region copy story handles it.
package approval

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	stdio "io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema is the on-disk version tag for Request bodies.
const Schema = "pg_hardstorage.approval.v1"

// PEM block type for an approver public key. Same shape as the
// manifest-signing key (PG_HARDSTORAGE ED25519 PUBLIC KEY) so the
// existing keystore PEM files drop straight in.
const pemTypePublicKey = "PG_HARDSTORAGE ED25519 PUBLIC KEY"

// Status enumerates the request lifecycle.
type Status string

// The Status* constants enumerate the request lifecycle values.
const (
	// StatusPending — request created, awaiting approver votes.
	StatusPending Status = "pending"
	// StatusApproved — quorum threshold reached; the operation
	// gated on this request may proceed.
	StatusApproved Status = "approved"
	// StatusExpired — request reached its TTL without enough
	// approver votes. No longer actionable.
	StatusExpired Status = "expired"
	// StatusRevoked — explicitly cancelled by an authorized actor
	// before approval or expiry.
	StatusRevoked Status = "revoked"
)

// Op is the namespaced action being approved. Same dotted shape as
// audit Action so callers can cross-reference. Examples:
//
//	"backup.delete"
//	"kms.shred"
//	"repo.gc"
//	"repo.wipe"
//	"repo.set_mode"
//
// Approval consumers pin the exact op string they expect so an
// approval for `backup.delete` cannot be replayed against `kms.shred`.
type Op string

// Request is one outstanding approval workflow. Persisted at
// approvals/<id>.json. Read-modify-write (RWM) via storage Put with
// IfNotExists=false so concurrent approvers can both land their
// signatures; the canonical-request bytes are signed (not the
// post-approval Request), so adding approvals doesn't invalidate
// existing signatures.
type Request struct {
	Schema        string     `json:"schema"`
	ID            string     `json:"id"`
	Op            Op         `json:"op"`
	Initiator     string     `json:"initiator"`
	Target        string     `json:"target,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	Tenant        string     `json:"tenant,omitempty"`
	Threshold     int        `json:"threshold"`
	ApproverKeys  []string   `json:"approver_keys"` // PEM-encoded; reads from an identity provider
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Approvals     []Approval `json:"approvals,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedBy     string     `json:"revoked_by,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
	// ExpiredAt is stamped by SweepExpired when a request that passed
	// its TTL without reaching approval (and was not revoked) has had
	// its expiry recorded in the audit chain. It is post-creation
	// metadata — like the revocation fields, it is excluded from the
	// signed canonical bytes — and exists only to make the expire sweep
	// idempotent (a stamped request is not re-recorded). Status is still
	// DERIVED from ExpiresAt by computeStatus; ExpiredAt does not change
	// the verdict.
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
}

// Approval is one signed yes-vote on a Request. Multiple Approvals
// from the same KeyFingerprint count as one (no double-voting).
type Approval struct {
	KeyFingerprint string    `json:"key_fingerprint"`
	Approver       string    `json:"approver,omitempty"`
	At             time.Time `json:"at"`
	Reason         string    `json:"reason,omitempty"`
	Signature      string    `json:"signature"` // base64; covers canonicalForApproval()
}

// keyFor returns the on-disk key for a request body. The body holds
// the immutable request fields (Op, Target, Threshold, ApproverKeys,
// CreatedAt, ExpiresAt, RevokedAt). It is NOT the source of truth
// for the Approvals slice — see approverKeyFor / approverPrefixFor.
func keyFor(id string) string {
	return "approvals/" + id + ".json"
}

// approverPrefixFor returns the prefix under which per-approver
// signatures live. Each approver writes exactly one file at
// approverKeyFor(id, fp); concurrent approvers therefore can't
// collide on the same key, eliminating any read-modify-write
// race on the approval set.
func approverPrefixFor(id string) string {
	return "approvals/" + id + "/approvers/"
}

// approverKeyFor returns the on-disk key for ONE approver's
// signed yes-vote on the request. The fp is the approver's
// ed25519-public-key fingerprint (lowercase hex SHA-256 of the
// raw 32-byte key). Per-approver keys are write-once
// (IfNotExists: true); a re-approval by the same approver is a
// race-loser → idempotent.
func approverKeyFor(id, fp string) string {
	return approverPrefixFor(id) + fp + ".json"
}

// Store reads + writes approval requests against any StoragePlugin.
type Store struct {
	sp storage.StoragePlugin
}

// NewStore wraps sp.
func NewStore(sp storage.StoragePlugin) *Store {
	if sp == nil {
		panic("approval: NewStore requires a non-nil StoragePlugin")
	}
	return &Store{sp: sp}
}

// CreateOptions tunes Create.
type CreateOptions struct {
	Op           Op
	Initiator    string
	Target       string
	Reason       string
	Tenant       string
	Threshold    int
	ApproverKeys [][]byte // each is a PEM-encoded ed25519 public key
	TTL          time.Duration
}

// Create persists a fresh approval request and returns it.
//
// Validation:
//   - Op required.
//   - Threshold ≥ 1.
//   - At least Threshold ApproverKeys (we refuse to write a request
//     that's structurally impossible to approve).
//   - Each ApproverKey must parse as ed25519 PEM.
func (s *Store) Create(ctx context.Context, opts CreateOptions) (*Request, error) {
	if opts.Op == "" {
		return nil, errors.New("approval: Op is required")
	}
	if opts.Threshold < 1 {
		return nil, errors.New("approval: Threshold must be ≥ 1")
	}
	if len(opts.ApproverKeys) < opts.Threshold {
		return nil, fmt.Errorf("approval: %d approver keys supplied, threshold is %d (request would be structurally unfulfillable)",
			len(opts.ApproverKeys), opts.Threshold)
	}
	if opts.TTL <= 0 {
		opts.TTL = 24 * time.Hour
	}

	// Sanity-check every key parses + canonicalise (decode → re-encode)
	// so the stored PEMs are byte-identical regardless of how the
	// caller produced them. Fingerprint comparison later relies on
	// canonical bytes.
	canonical := make([]string, 0, len(opts.ApproverKeys))
	for i, raw := range opts.ApproverKeys {
		pub, err := parseED25519PublicKeyPEM(raw)
		if err != nil {
			return nil, fmt.Errorf("approval: approver key %d: %w", i, err)
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, fmt.Errorf("approval: re-marshal approver key %d: %w", i, err)
		}
		out := pem.EncodeToMemory(&pem.Block{Type: pemTypePublicKey, Bytes: der})
		canonical = append(canonical, string(out))
	}

	now := time.Now().UTC()
	id, err := newRequestID(now)
	if err != nil {
		return nil, fmt.Errorf("approval: new request id: %w", err)
	}
	req := &Request{
		Schema:       Schema,
		ID:           id,
		Op:           opts.Op,
		Initiator:    opts.Initiator,
		Target:       opts.Target,
		Reason:       opts.Reason,
		Tenant:       opts.Tenant,
		Threshold:    opts.Threshold,
		ApproverKeys: canonical,
		CreatedAt:    now,
		ExpiresAt:    now.Add(opts.TTL),
	}

	body, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := s.sp.Put(ctx, keyFor(id), bytesReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	}); err != nil {
		return nil, fmt.Errorf("approval: put request: %w", err)
	}
	return req, nil
}

// Get reads the request by ID and aggregates the Approvals slice
// from per-approver keys at `approvals/<id>/approvers/<fp>.json`,
// one per approver.  The on-disk request body never carries an
// in-line Approvals slice in the current layout.
func (s *Store) Get(ctx context.Context, id string) (*Request, error) {
	rc, err := s.sp.Get(ctx, keyFor(id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("approval: get request: %w", err)
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("approval: decode request: %w", err)
	}
	if req.Schema != Schema {
		return nil, fmt.Errorf("approval: unknown request schema %q", req.Schema)
	}

	byFP := map[string]Approval{}
	for info, lerr := range s.sp.List(ctx, approverPrefixFor(id)) {
		if lerr != nil {
			return nil, fmt.Errorf("approval: list approver votes: %w", lerr)
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		approverRC, gerr := s.sp.Get(ctx, info.Key)
		if gerr != nil {
			// A deleted approver key mid-list is benign; surface only
			// genuine failures (auth / corruption). Log-and-continue
			// would be safer but we don't have a logger threaded in;
			// returning preserves the strict-read posture.
			if errors.Is(gerr, storage.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("approval: get approver vote %q: %w", info.Key, gerr)
		}
		body, rerr := stdio.ReadAll(approverRC)
		_ = approverRC.Close()
		if rerr != nil {
			return nil, fmt.Errorf("approval: read approver vote %q: %w", info.Key, rerr)
		}
		var a Approval
		if err := json.Unmarshal(body, &a); err != nil {
			// Skip malformed approver votes rather than failing the
			// whole Get — one corrupted file shouldn't lock out the
			// approval system.
			continue
		}
		byFP[a.KeyFingerprint] = a
	}

	// Re-flatten in deterministic (lex by fingerprint) order so
	// callers and downstream code (audit-emitting CLI) see a stable
	// shape across runs.
	fps := make([]string, 0, len(byFP))
	for fp := range byFP {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	merged := make([]Approval, 0, len(fps))
	for _, fp := range fps {
		merged = append(merged, byFP[fp])
	}
	req.Approvals = merged
	return &req, nil
}

// List walks every approval request in the repo. Filters apply
// post-fetch (no index files).
type ListFilters struct {
	Op     Op
	Status Status
	Tenant string
}

// List returns matching requests in newest-first order.
//
// Walks `approvals/<id>.json` as the canonical request body. The
// `approvals/<id>/approvers/<fp>.json` per-approver-key files
// share the .json suffix but live one prefix level deeper; we
// distinguish by counting slash-separated segments. The shape:
//
//	approvals/<id>.json                          → request body (2 segments)
//	approvals/<id>/approvers/<fp>.json           → per-approver vote (4 segments)
func (s *Store) List(ctx context.Context, f ListFilters) ([]*Request, error) {
	var keys []string
	for info, err := range s.sp.List(ctx, "approvals/") {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		// Filter out per-approver-key files: they share the .json
		// suffix but sit at a deeper path than the request body.
		// A request key has exactly 2 segments
		// ("approvals/<id>.json"); per-approver keys have 4.
		if strings.Count(info.Key, "/") > 1 {
			continue
		}
		keys = append(keys, info.Key)
	}
	// Newest-first; IDs embed sortable timestamps.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	now := time.Now().UTC()
	var out []*Request
	for _, k := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, "approvals/"), ".json")
		r, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		if f.Op != "" && r.Op != f.Op {
			continue
		}
		if f.Tenant != "" && r.Tenant != f.Tenant {
			continue
		}
		st := computeStatus(r, now)
		if f.Status != "" && st != f.Status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Approve adds an approval to the named request. The approver supplies
// the ed25519 private key whose public half must be in the request's
// ApproverKeys list. Same approver-key (by fingerprint) approving twice
// is a no-op (idempotent re-approval).
//
// Concurrency model: each approver's signed yes-vote lands at its
// own per-approver key (`approvals/<id>/approvers/<fp>.json`) using
// `IfNotExists: true`. Two approvers running Approve concurrently
// write DISTINCT keys, so there is no read-modify-write race on a
// shared body. A re-approval by the SAME approver hits an existing
// key, observes ErrAlreadyExists, and treats the prior write as
// authoritative — idempotent re-approval.
//
// The returned *Request's Approvals slice is the freshly-aggregated
// view of all per-approver keys AFTER our write lands. Callers see
// the post-state directly.
func (s *Store) Approve(ctx context.Context, id string, approverPriv ed25519.PrivateKey, approverID string, reason string) (*Request, error) {
	if len(approverPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("approval: invalid approver private key")
	}
	approverPub, ok := approverPriv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("approval: private key did not yield a public key")
	}
	fp := keyFingerprint(approverPub)

	req, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if st := computeStatus(req, now); st == StatusExpired {
		return nil, ErrExpired
	} else if st == StatusRevoked {
		return nil, ErrRevoked
	}
	// Match approverPub against the allowlist by fingerprint.
	allowed := false
	for _, raw := range req.ApproverKeys {
		pub, err := parseED25519PublicKeyPEM([]byte(raw))
		if err != nil {
			continue // a corrupt entry shouldn't poison the whole request
		}
		if keyFingerprint(pub) == fp {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrApproverNotAllowed
	}

	// Idempotent re-approval: if this fingerprint already approved,
	// we return the freshly-aggregated request unchanged.
	for _, a := range req.Approvals {
		if a.KeyFingerprint == fp {
			return req, nil
		}
	}

	// Sign the canonical request bytes (everything that's stable
	// from the moment of creation — NOT the Approvals slice itself,
	// which mutates).
	canon, err := canonicalForApproval(req)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(approverPriv, canon)

	approval := Approval{
		KeyFingerprint: fp,
		Approver:       approverID,
		At:             now,
		Reason:         reason,
		Signature:      base64.StdEncoding.EncodeToString(sig),
	}
	body, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return nil, err
	}
	// Per-approver key + IfNotExists: each approver writes its own
	// key, so two concurrent Approve calls never collide. A
	// re-approval by the same approver loses the IfNotExists race
	// → ErrAlreadyExists, which we map to "already approved" (no-op).
	_, putErr := s.sp.Put(ctx, approverKeyFor(id, fp), bytesReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	})
	if putErr != nil && !errors.Is(putErr, storage.ErrAlreadyExists) {
		return nil, fmt.Errorf("approval: put approver vote: %w", putErr)
	}

	// Re-aggregate the post-write view so the caller sees their
	// approval reflected.
	return s.Get(ctx, id)
}

// Revoke marks the request as revoked. Idempotent; calling twice is
// fine. Initiator (or any operator with admin scope) can revoke; the
// caller is responsible for the RBAC decision.
func (s *Store) Revoke(ctx context.Context, id, by, reason string) (*Request, error) {
	req, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.RevokedAt != nil {
		return req, nil
	}
	now := time.Now().UTC()
	req.RevokedAt = &now
	req.RevokedBy = by
	req.RevokedReason = reason
	body, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := s.sp.Put(ctx, keyFor(id), bytesReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return nil, err
	}
	return req, nil
}

// SweepExpired records expiry for every request that has passed its TTL
// without reaching its approval threshold (and was not revoked) and whose
// expiry has not already been recorded. For each such request it stamps
// ExpiredAt (a read-modify-write, like Revoke) and returns it so the
// caller can emit one audit event per request — expiry is otherwise a
// purely derived state that leaves no trail.
//
// Idempotent: a request whose ExpiredAt is already set is skipped, so a
// re-run records (and the caller emits) nothing new. When dryRun is true
// the matching requests are returned WITHOUT stamping, for preview.
func (s *Store) SweepExpired(ctx context.Context, dryRun bool) ([]*Request, error) {
	reqs, err := s.List(ctx, ListFilters{})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var expired []*Request
	for _, r := range reqs {
		if r.ExpiredAt != nil {
			continue // already recorded
		}
		if computeStatus(r, now) != StatusExpired {
			continue
		}
		if !dryRun {
			stamp := now
			r.ExpiredAt = &stamp
			body, merr := json.MarshalIndent(r, "", "  ")
			if merr != nil {
				return expired, merr
			}
			if _, perr := s.sp.Put(ctx, keyFor(r.ID), bytesReader(body), storage.PutOptions{
				ContentLength: int64(len(body)),
			}); perr != nil {
				// Return what we've recorded so far; the sweep is
				// idempotent, so a re-run drains the rest.
				return expired, fmt.Errorf("approval: stamp expired %s: %w", r.ID, perr)
			}
		}
		expired = append(expired, r)
	}
	return expired, nil
}

// StatusOf returns the current status (computed) of the request.
// Convenience over Get + computeStatus.
func (s *Store) StatusOf(ctx context.Context, id string) (Status, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return computeStatus(r, time.Now().UTC()), nil
}

// VerifyApprovals walks the approvals slice and verifies every
// signature against the canonical request bytes. Returns the count
// of valid + distinct approvers. A consumer (destructive op) should
// gate on this >= req.Threshold AND status == StatusApproved.
//
// This is the trust-input layer: even if the storage layer were
// adversarial, the signed approvals can't be forged or
// silently-rewritten without invalidating the signature.
func VerifyApprovals(req *Request) (int, error) {
	canon, err := canonicalForApproval(req)
	if err != nil {
		return 0, err
	}
	// Build fingerprint → public-key map from the allowlist.
	allowed := map[string]ed25519.PublicKey{}
	for _, raw := range req.ApproverKeys {
		pub, err := parseED25519PublicKeyPEM([]byte(raw))
		if err != nil {
			continue
		}
		allowed[keyFingerprint(pub)] = pub
	}
	seen := map[string]struct{}{}
	for _, a := range req.Approvals {
		pub, ok := allowed[a.KeyFingerprint]
		if !ok {
			continue // an approval from a non-allowlisted key doesn't count
		}
		sig, err := base64.StdEncoding.DecodeString(a.Signature)
		if err != nil {
			continue
		}
		if !ed25519.Verify(pub, canon, sig) {
			continue
		}
		seen[a.KeyFingerprint] = struct{}{}
	}
	return len(seen), nil
}

// computeStatus derives the status from the request's persisted
// fields and the current time.
func computeStatus(r *Request, now time.Time) Status {
	if r.RevokedAt != nil {
		return StatusRevoked
	}
	count, _ := VerifyApprovals(r)
	if count >= r.Threshold {
		return StatusApproved
	}
	if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
		return StatusExpired
	}
	return StatusPending
}

// canonicalForApproval returns the bytes that approvers sign. We
// deliberately exclude Approvals (so signatures don't depend on each
// other) and the revocation fields (those are post-creation
// metadata, not part of the contract being approved).
func canonicalForApproval(r *Request) ([]byte, error) {
	clone := *r
	clone.Approvals = nil
	clone.RevokedAt = nil
	clone.RevokedBy = ""
	clone.RevokedReason = ""
	clone.ExpiredAt = nil
	// json.Marshal gives stable key order for structs, which is what
	// we need — no third-party canonicaliser required.
	return json.Marshal(&clone)
}

// keyFingerprint is a SHA-256 of the raw public key, hex-encoded.
// 64 hex chars; collision-resistant; comparable byte-stable.
func keyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// parseED25519PublicKeyPEM decodes a PEM block (matching
// pemTypePublicKey) and returns the embedded ed25519 key.
func parseED25519PublicKeyPEM(raw []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("approval: no PEM block")
	}
	if block.Type != pemTypePublicKey {
		return nil, fmt.Errorf("approval: PEM block %q (want %q)", block.Type, pemTypePublicKey)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("approval: parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("approval: not an ed25519 public key (got %T)", key)
	}
	return pub, nil
}

// newRequestID returns a sortable approval-request ID. 8 hex of
// UTC unix-seconds (sortable to 2106) + 8 hex of crypto-random.
// Different shape from audit IDs so a glance at a string tells you
// what kind of object it points at.
func newRequestID(t time.Time) (string, error) {
	const width = 16
	id := make([]byte, width)
	secs := uint64(t.UTC().Unix())
	for i := 7; i >= 0; i-- {
		id[i] = "0123456789abcdef"[secs&0xF]
		secs >>= 4
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	hex.Encode(id[8:], rnd[:])
	return "appr-" + string(id), nil
}

// bytesReader is the io.Reader-from-bytes helper.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Errors.
var (
	ErrNotFound           = errors.New("approval: not found")
	ErrExpired            = errors.New("approval: request expired")
	ErrRevoked            = errors.New("approval: request revoked")
	ErrApproverNotAllowed = errors.New("approval: approver public key not in request's allowlist")
	ErrThresholdNotMet    = errors.New("approval: threshold not met")
	ErrOpMismatch         = errors.New("approval: request op doesn't match the destructive op being attempted")
	ErrTargetMismatch     = errors.New("approval: request target doesn't match the destructive op's target")
)

// GateOptions describes what a destructive op expects of an approval
// request. The destructive op calls Gate to refuse-or-proceed; Gate
// returns nil only when EVERY check passes:
//
//   - request exists
//   - status is StatusApproved (signature-verified count >= threshold)
//   - request.Op matches Op (so an approval for backup.delete cannot
//     be replayed against kms.shred)
//   - when Target is non-empty, request.Target matches (so an
//     approval to delete db1.full.X cannot be redeemed for
//     db2.full.Y)
//
// Op + Target binding is the trust-foundation property: the
// approver signed bytes that include op + target, so an attacker
// who steals an approved request can't redirect it to a different
// destructive action without re-collecting signatures.
type GateOptions struct {
	RequestID string
	Op        Op
	Target    string
}

// Gate is the destructive-op check. Reads the request, verifies
// signatures, confirms op + target, and either returns the request
// (for the caller to log into the audit chain) or one of the
// structured errors above for the caller to map to a CLI exit code.
func (s *Store) Gate(ctx context.Context, opts GateOptions) (*Request, error) {
	if opts.RequestID == "" {
		return nil, ErrNotFound
	}
	req, err := s.Get(ctx, opts.RequestID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	switch computeStatus(req, now) {
	case StatusRevoked:
		return req, ErrRevoked
	case StatusExpired:
		return req, ErrExpired
	case StatusPending:
		return req, ErrThresholdNotMet
	}
	// Op + target binding — the heart of the gate.
	if opts.Op != "" && req.Op != opts.Op {
		return req, ErrOpMismatch
	}
	if opts.Target != "" && req.Target != "" && req.Target != opts.Target {
		return req, ErrTargetMismatch
	}
	return req, nil
}
