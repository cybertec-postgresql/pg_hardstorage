// Package acl implements the cross-account / cross-org replication
// ACL boundary.  Closes the SPEC commitment "cross-account /
// cross-org repo replication.  M&A, partner-data scenarios.  Async
// copy with explicit ACL boundary."
//
// Design:
//
//   - The SOURCE repo declares an `acl/source.json` policy: "I
//     permit replication of my data to these destinations, signed
//     by these keys, up to this classification level, for these
//     tenants."
//   - The DESTINATION repo declares an `acl/accept.json` policy:
//     "I accept incoming replication from these sources, signed
//     by these keys, at or above this classification level, for
//     these tenants."
//   - Before any byte-copy, both policies are loaded and verified.
//     The intersection has to be non-empty: source permits dst,
//     dst accepts source, classification + tenant scopes overlap.
//   - Each policy is admin-signed (ed25519) so a malicious actor
//     can't drop a bogus policy file into the repo and bypass
//     the boundary.
//
// Use case (M&A scenario):
//
//  1. Acme acquires Beta.  Beta's repo lives in Acme's S3 account
//     now but the data is "Beta data" — different tenant, different
//     classification, different operator key set.
//  2. Acme's compliance team writes `acl/accept.json` on the
//     destination: "accept from beta-prod-admins, classification
//     ≤ confidential, tenants = beta-tenant-*".
//  3. Beta's compliance team writes `acl/source.json` on the
//     source: "permit to acme-acquisitions, classification =
//     confidential, tenants = beta-tenant-*".
//  4. `pg_hardstorage repo replicate ...` consults both before
//     starting.  Mismatch → refused with a clear diagnostic.
//
// Storage layout (per repo):
//
//	acl/source.json   — source-side policy (admin-signed)
//	acl/accept.json   — destination-side policy (admin-signed)
//
// A repo can carry both files (typical when a repo is both a
// source for some destinations + a destination for others).
package acl

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema identifiers (24-month backward-compat).
const (
	SchemaSourcePolicy = "pg_hardstorage.repo.acl.source.v1"
	SchemaAcceptPolicy = "pg_hardstorage.repo.acl.accept.v1"
	canonicalPreamble  = "pg_hardstorage.repo.acl.canon.v1"
)

// Storage keys.
const (
	SourcePolicyKey = "acl/source.json"
	AcceptPolicyKey = "acl/accept.json"
)

// Classification level — totally ordered for comparison.
// Higher number = more sensitive.  An accept policy with
// MinClassification=Confidential refuses Internal traffic.
type Classification string

const (
	// ClassPublic is the lowest classification — content with no
	// confidentiality requirement.
	ClassPublic Classification = "public"

	// ClassInternal is corporate-internal content; safe inside the
	// organisation but not for external distribution.
	ClassInternal Classification = "internal"

	// ClassConfidential is content covered by NDA or equivalent
	// contractual restriction.
	ClassConfidential Classification = "confidential"

	// ClassRestricted is the highest level — content under
	// regulatory, statutory, or contractual export controls.
	ClassRestricted Classification = "restricted"
)

// classificationRank assigns a comparison weight.  Unknown
// values are treated as the most-sensitive (Restricted) so a
// typo doesn't accidentally relax an accept policy.
func classificationRank(c Classification) int {
	switch c {
	case ClassPublic:
		return 1
	case ClassInternal:
		return 2
	case ClassConfidential:
		return 3
	case ClassRestricted:
		return 4
	}
	return 4
}

// Sentinel errors.  Callers errors.Is for control flow.
var (
	ErrPolicyNotFound        = errors.New("acl: policy not found")
	ErrSignatureInvalid      = errors.New("acl: signature does not validate")
	ErrSourceRefuses         = errors.New("acl: source policy refuses this destination / signer")
	ErrAcceptRefuses         = errors.New("acl: accept policy refuses this source / signer")
	ErrClassificationTooLow  = errors.New("acl: source classification below destination's minimum")
	ErrClassificationTooHigh = errors.New("acl: source classification above what the source permits to leave")
	ErrTenantMismatch        = errors.New("acl: no tenant in source's permitted set is in destination's accepted set")
	ErrInvalidPolicy         = errors.New("acl: policy is malformed")
)

// Signer is the minimal signing interface.  backup.Signer
// satisfies it; tests use a struct literal.
type Signer interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
}

// SourcePolicy is what the source-side admin publishes.  Says:
// "I permit my data to leave to one of these destinations,
// signed by one of these accepting keys, with this classification
// floor and these tenants."
type SourcePolicy struct {
	Schema      string `json:"schema"`
	Description string `json:"description,omitempty"`

	// PermittedDestinations: at least one must match the requested
	// (destination URL, destination signer fingerprint) pair.
	PermittedDestinations []DestinationGrant `json:"permitted_destinations"`

	// Classification of the data this repo holds.  Must be ≤ the
	// destination's accept policy's MinClassification.  Empty
	// defaults to ClassConfidential (the safer assumption).
	Classification Classification `json:"classification,omitempty"`

	// Tenants this repo is willing to send.  Empty = all tenants
	// (refused only by the destination's filter).  A specific
	// list narrows the cross-account scope further.
	Tenants []string `json:"tenants,omitempty"`

	CreatedAt                   time.Time `json:"created_at"`
	CreatedBy                   string    `json:"created_by"`
	CreatorPublicKeyFingerprint string    `json:"creator_public_key_fingerprint"`
	Signature                   string    `json:"signature"`
}

// DestinationGrant is one entry in a SourcePolicy.PermittedDestinations.
type DestinationGrant struct {
	// RepoURLPattern is a literal repo URL or a wildcard prefix
	// (e.g. "s3://acme-eu/").  The destination URL passed to
	// `repo replicate --to` must match.
	RepoURLPattern string `json:"repo_url_pattern"`

	// AcceptedSignerFingerprints is the set of destination admin
	// signing keys that the source recognises as valid recipients.
	// Empty = any signer (refused only by the destination's
	// own admin signature on its accept policy).
	AcceptedSignerFingerprints []string `json:"accepted_signer_fingerprints,omitempty"`
}

// AcceptPolicy is what the destination-side admin publishes.
// Says: "I accept incoming replication from these sources,
// signed by these source-admin keys, at or above this
// classification, for these tenants."
type AcceptPolicy struct {
	Schema      string `json:"schema"`
	Description string `json:"description,omitempty"`

	// AcceptedSources: at least one must match the requested
	// (source URL, source signer fingerprint) pair.
	AcceptedSources []SourceGrant `json:"accepted_sources"`

	// MinClassification: incoming data classified BELOW this
	// is refused.  Empty defaults to ClassPublic (lenient).
	MinClassification Classification `json:"min_classification,omitempty"`

	// TenantsAllowed: empty = any.  A specific list refuses
	// tenants outside the set.
	TenantsAllowed []string `json:"tenants_allowed,omitempty"`

	CreatedAt                   time.Time `json:"created_at"`
	CreatedBy                   string    `json:"created_by"`
	CreatorPublicKeyFingerprint string    `json:"creator_public_key_fingerprint"`
	Signature                   string    `json:"signature"`
}

// SourceGrant is one entry in an AcceptPolicy.AcceptedSources.
type SourceGrant struct {
	RepoURLPattern             string   `json:"repo_url_pattern"`
	AcceptedSignerFingerprints []string `json:"accepted_signer_fingerprints,omitempty"`
}

// PublicKeyFingerprint returns the canonical 16-hex-char SHA-256
// prefix.  Same shape as JIT / threshold / DSA — operators get a
// single fingerprint format across the system.
func PublicKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ----- canonical bytes + signing -----

// canonicalSourceBytes is the byte sequence the source-admin
// signature covers.  Length-prefixed; deterministic across Go
// versions.
func canonicalSourceBytes(p *SourcePolicy) []byte {
	var buf strings.Builder
	buf.WriteString(canonicalPreamble)
	buf.WriteByte(0)
	buf.WriteString(p.Schema)
	buf.WriteByte(0)
	buf.WriteString(p.Description)
	buf.WriteByte(0)
	buf.WriteString(string(p.Classification))
	buf.WriteByte(0)
	tenants := append([]string(nil), p.Tenants...)
	sort.Strings(tenants)
	binary.Write(&buf, binary.BigEndian, int64(len(tenants)))
	for _, t := range tenants {
		buf.WriteString(t)
		buf.WriteByte(0)
	}
	dests := append([]DestinationGrant(nil), p.PermittedDestinations...)
	sort.Slice(dests, func(i, j int) bool {
		return dests[i].RepoURLPattern < dests[j].RepoURLPattern
	})
	binary.Write(&buf, binary.BigEndian, int64(len(dests)))
	for _, d := range dests {
		buf.WriteString(d.RepoURLPattern)
		buf.WriteByte(0)
		fps := append([]string(nil), d.AcceptedSignerFingerprints...)
		sort.Strings(fps)
		binary.Write(&buf, binary.BigEndian, int64(len(fps)))
		for _, fp := range fps {
			buf.WriteString(fp)
			buf.WriteByte(0)
		}
	}
	binary.Write(&buf, binary.BigEndian, p.CreatedAt.UTC().UnixNano())
	buf.WriteString(p.CreatedBy)
	buf.WriteByte(0)
	buf.WriteString(p.CreatorPublicKeyFingerprint)
	return []byte(buf.String())
}

// canonicalAcceptBytes is the byte sequence the destination-admin
// signature covers.  Same shape as canonicalSourceBytes adjusted
// for the AcceptPolicy fields.
func canonicalAcceptBytes(p *AcceptPolicy) []byte {
	var buf strings.Builder
	buf.WriteString(canonicalPreamble)
	buf.WriteByte(0)
	buf.WriteString(p.Schema)
	buf.WriteByte(0)
	buf.WriteString(p.Description)
	buf.WriteByte(0)
	buf.WriteString(string(p.MinClassification))
	buf.WriteByte(0)
	tenants := append([]string(nil), p.TenantsAllowed...)
	sort.Strings(tenants)
	binary.Write(&buf, binary.BigEndian, int64(len(tenants)))
	for _, t := range tenants {
		buf.WriteString(t)
		buf.WriteByte(0)
	}
	srcs := append([]SourceGrant(nil), p.AcceptedSources...)
	sort.Slice(srcs, func(i, j int) bool {
		return srcs[i].RepoURLPattern < srcs[j].RepoURLPattern
	})
	binary.Write(&buf, binary.BigEndian, int64(len(srcs)))
	for _, s := range srcs {
		buf.WriteString(s.RepoURLPattern)
		buf.WriteByte(0)
		fps := append([]string(nil), s.AcceptedSignerFingerprints...)
		sort.Strings(fps)
		binary.Write(&buf, binary.BigEndian, int64(len(fps)))
		for _, fp := range fps {
			buf.WriteString(fp)
			buf.WriteByte(0)
		}
	}
	binary.Write(&buf, binary.BigEndian, p.CreatedAt.UTC().UnixNano())
	buf.WriteString(p.CreatedBy)
	buf.WriteByte(0)
	buf.WriteString(p.CreatorPublicKeyFingerprint)
	return []byte(buf.String())
}

// SignSource admin-signs a source policy.  Mutates Schema +
// CreatedBy + CreatorPublicKeyFingerprint + Signature.
func SignSource(p *SourcePolicy, signer Signer, createdBy string) error {
	if signer == nil {
		return errors.New("acl: nil signer")
	}
	p.Schema = SchemaSourcePolicy
	p.CreatedBy = createdBy
	p.CreatorPublicKeyFingerprint = PublicKeyFingerprint(signer.PublicKey())
	canon := canonicalSourceBytes(p)
	p.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// SignAccept admin-signs an accept policy.
func SignAccept(p *AcceptPolicy, signer Signer, createdBy string) error {
	if signer == nil {
		return errors.New("acl: nil signer")
	}
	p.Schema = SchemaAcceptPolicy
	p.CreatedBy = createdBy
	p.CreatorPublicKeyFingerprint = PublicKeyFingerprint(signer.PublicKey())
	canon := canonicalAcceptBytes(p)
	p.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// VerifySource validates the admin signature on a source policy.
// pubFor returns the public key for the given fingerprint —
// callers wire it to their keystore or to a SingleKeyResolver.
func VerifySource(p *SourcePolicy, pubFor func(string) (ed25519.PublicKey, error)) error {
	if p == nil {
		return errors.New("acl: nil policy")
	}
	if p.Signature == "" {
		return ErrSignatureInvalid
	}
	pub, err := pubFor(p.CreatorPublicKeyFingerprint)
	if err != nil {
		return fmt.Errorf("acl: resolve creator key %s: %w", p.CreatorPublicKeyFingerprint, err)
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return fmt.Errorf("acl: decode source signature: %w", err)
	}
	if !ed25519.Verify(pub, canonicalSourceBytes(p), sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// VerifyAccept validates the admin signature on an accept policy.
func VerifyAccept(p *AcceptPolicy, pubFor func(string) (ed25519.PublicKey, error)) error {
	if p == nil {
		return errors.New("acl: nil policy")
	}
	if p.Signature == "" {
		return ErrSignatureInvalid
	}
	pub, err := pubFor(p.CreatorPublicKeyFingerprint)
	if err != nil {
		return fmt.Errorf("acl: resolve creator key %s: %w", p.CreatorPublicKeyFingerprint, err)
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return fmt.Errorf("acl: decode accept signature: %w", err)
	}
	if !ed25519.Verify(pub, canonicalAcceptBytes(p), sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// ----- gate -----

// Request describes one cross-account replication attempt.
type Request struct {
	SourceURL                    string
	SourceSignerFingerprint      string // typically the source admin's signer fingerprint
	DestinationURL               string
	DestinationSignerFingerprint string
	Tenants                      []string // tenants being replicated
}

// Authorize checks that source.PermittedDestinations + accept.
// AcceptedSources both grant the proposed (sourceURL, dstURL,
// signer fingerprints) pairing AND that classification +
// tenant filters intersect.  Both policies must be valid (callers
// run VerifySource / VerifyAccept first).
func Authorize(source *SourcePolicy, accept *AcceptPolicy, req Request) error {
	if source == nil {
		return fmt.Errorf("%w: source policy nil", ErrSourceRefuses)
	}
	if accept == nil {
		return fmt.Errorf("%w: accept policy nil", ErrAcceptRefuses)
	}

	// 1. Source's permitted-destinations must list the requested
	//    (dst URL, dst signer).
	dstMatched := false
	for _, dest := range source.PermittedDestinations {
		if !urlMatch(dest.RepoURLPattern, req.DestinationURL) {
			continue
		}
		if len(dest.AcceptedSignerFingerprints) == 0 {
			dstMatched = true
			break
		}
		for _, fp := range dest.AcceptedSignerFingerprints {
			if fp == req.DestinationSignerFingerprint {
				dstMatched = true
				break
			}
		}
		if dstMatched {
			break
		}
	}
	if !dstMatched {
		return fmt.Errorf("%w: destination %q signer %s not in source permits",
			ErrSourceRefuses, req.DestinationURL, req.DestinationSignerFingerprint)
	}

	// 2. Accept's accepted-sources must list the requested
	//    (src URL, src signer).
	srcMatched := false
	for _, src := range accept.AcceptedSources {
		if !urlMatch(src.RepoURLPattern, req.SourceURL) {
			continue
		}
		if len(src.AcceptedSignerFingerprints) == 0 {
			srcMatched = true
			break
		}
		for _, fp := range src.AcceptedSignerFingerprints {
			if fp == req.SourceSignerFingerprint {
				srcMatched = true
				break
			}
		}
		if srcMatched {
			break
		}
	}
	if !srcMatched {
		return fmt.Errorf("%w: source %q signer %s not in accept policy",
			ErrAcceptRefuses, req.SourceURL, req.SourceSignerFingerprint)
	}

	// 3. Classification: source's classification must be ≥ accept's
	//    minimum.  (Higher classification = more sensitive; a
	//    confidential accept policy refuses public traffic.)
	srcClass := source.Classification
	if srcClass == "" {
		srcClass = ClassConfidential
	}
	minClass := accept.MinClassification
	if minClass == "" {
		minClass = ClassPublic
	}
	if classificationRank(srcClass) < classificationRank(minClass) {
		return fmt.Errorf("%w: source=%s, accept-min=%s",
			ErrClassificationTooLow, srcClass, minClass)
	}

	// 4. Tenant intersection.  Empty source tenants = "all".
	//    Empty accept tenants = "all".  Otherwise the requested
	//    tenants must be a subset of BOTH.
	if len(req.Tenants) > 0 {
		if len(source.Tenants) > 0 {
			permitted := make(map[string]struct{}, len(source.Tenants))
			for _, t := range source.Tenants {
				permitted[t] = struct{}{}
			}
			for _, t := range req.Tenants {
				if _, ok := permitted[t]; !ok {
					return fmt.Errorf("%w: tenant %q not in source's permitted set %v",
						ErrTenantMismatch, t, source.Tenants)
				}
			}
		}
		if len(accept.TenantsAllowed) > 0 {
			allowed := make(map[string]struct{}, len(accept.TenantsAllowed))
			for _, t := range accept.TenantsAllowed {
				allowed[t] = struct{}{}
			}
			for _, t := range req.Tenants {
				if _, ok := allowed[t]; !ok {
					return fmt.Errorf("%w: tenant %q not in accept's allowed set %v",
						ErrTenantMismatch, t, accept.TenantsAllowed)
				}
			}
		}
	}
	return nil
}

// urlMatch is the pattern matcher for repo URL grants.  Suffix
// "*" is a prefix wildcard ("s3://acme-eu/*" matches every
// bucket-prefix path under s3://acme-eu/).  No suffix = exact
// match.  We keep the matcher tiny on purpose — operators write
// these patterns by hand and shouldn't have to learn glob
// semantics.
func urlMatch(pattern, url string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(url, prefix)
	}
	return pattern == url
}

// ----- storage -----

// LoadSource reads + decodes the source policy from sp.  Returns
// ErrPolicyNotFound when the file is absent.
func LoadSource(ctx context.Context, sp storage.StoragePlugin) (*SourcePolicy, error) {
	body, err := readKey(ctx, sp, SourcePolicyKey)
	if err != nil {
		return nil, err
	}
	var p SourcePolicy
	if err := stdjson.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("acl: decode source policy: %w", err)
	}
	if p.Schema != SchemaSourcePolicy {
		return nil, fmt.Errorf("%w: source schema %q, want %q",
			ErrInvalidPolicy, p.Schema, SchemaSourcePolicy)
	}
	return &p, nil
}

// LoadAccept reads + decodes the accept policy from sp.
func LoadAccept(ctx context.Context, sp storage.StoragePlugin) (*AcceptPolicy, error) {
	body, err := readKey(ctx, sp, AcceptPolicyKey)
	if err != nil {
		return nil, err
	}
	var p AcceptPolicy
	if err := stdjson.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("acl: decode accept policy: %w", err)
	}
	if p.Schema != SchemaAcceptPolicy {
		return nil, fmt.Errorf("%w: accept schema %q, want %q",
			ErrInvalidPolicy, p.Schema, SchemaAcceptPolicy)
	}
	return &p, nil
}

// SaveSource persists a signed source policy to sp.
func SaveSource(ctx context.Context, sp storage.StoragePlugin, p *SourcePolicy) error {
	body, err := stdjson.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return putKey(ctx, sp, SourcePolicyKey, body)
}

// SaveAccept persists a signed accept policy to sp.
func SaveAccept(ctx context.Context, sp storage.StoragePlugin, p *AcceptPolicy) error {
	body, err := stdjson.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return putKey(ctx, sp, AcceptPolicyKey, body)
}

// ----- helpers -----

func readKey(ctx context.Context, sp storage.StoragePlugin, key string) ([]byte, error) {
	rd, err := sp.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrPolicyNotFound, key, err)
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

func putKey(ctx context.Context, sp storage.StoragePlugin, key string, body []byte) error {
	_, err := sp.Put(ctx, key, bytesReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	if err != nil {
		return fmt.Errorf("acl: put %s: %w", key, err)
	}
	return nil
}

func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }
