// verify_envelopes.go — per-manifest encryption-envelope verifier (KEK resolution + DEK unwrap).
package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// VerifyEnvelopesSchema is the on-disk version tag for VerifyEnvelopesResult
// bodies. Stable per the v1 schema commitment.
const VerifyEnvelopesSchema = "pg_hardstorage.backup.verify_envelopes.v1"

// maxVerifyEnvelopeFindings caps the per-result Failures slice (same posture
// as Replicate / Heal / RotateKEK). Counter totals stay unbounded; only
// per-key error detail is capped so a fleet with thousands of broken
// manifests doesn't blow out the JSON body size.
const maxVerifyEnvelopeFindings = 100

// VerifyEnvelopeStatus is the per-manifest verdict.
type VerifyEnvelopeStatus string

const (
	// EnvelopeStatusOK — encryption block resolves and the wrapped DEK
	// unwraps cleanly with the resolved KEK.
	EnvelopeStatusOK VerifyEnvelopeStatus = "ok"
	// EnvelopeStatusUnencrypted — manifest has no encryption block.
	// Whether this is a finding depends on the operator's policy; the
	// counter is reported separately so a fleet that's intentionally
	// unencrypted (dev / staging) reads as zero "broken".
	EnvelopeStatusUnencrypted VerifyEnvelopeStatus = "unencrypted"
	// EnvelopeStatusKEKUnknown — the KEKResolver couldn't resolve the
	// manifest's KEKRef. Either the operator's keyring is missing the
	// key, or the manifest's ref is corrupt.
	EnvelopeStatusKEKUnknown VerifyEnvelopeStatus = "kek_unknown"
	// EnvelopeStatusWrappedDEKCorrupt — the wrapped_dek field couldn't
	// be base64-decoded or the bytes are malformed.
	EnvelopeStatusWrappedDEKCorrupt VerifyEnvelopeStatus = "wrapped_dek_corrupt"
	// EnvelopeStatusUnwrapFailed — Unwrap returned an AEAD-tag failure.
	// The KEK at the resolved ref doesn't match the one that wrapped
	// this DEK. The most likely cause is an operator chmod/regenerate
	// of the keyring file without rotating manifests.
	EnvelopeStatusUnwrapFailed VerifyEnvelopeStatus = "unwrap_failed"
	// EnvelopeStatusUnknownScheme — the manifest's scheme isn't one we
	// know how to verify (e.g. a future scheme written by a newer
	// pg_hardstorage and now read by an older one).
	EnvelopeStatusUnknownScheme VerifyEnvelopeStatus = "unknown_scheme"
	// EnvelopeStatusSignatureFailed — the manifest's Ed25519 signature
	// failed verification at iteration time. This is the loudest
	// possible finding: a manifest in the repo isn't signed by the
	// trusted keypair.
	EnvelopeStatusSignatureFailed VerifyEnvelopeStatus = "signature_failed"
	// EnvelopeStatusSkipped — the manifest matched no filter (e.g. a
	// --kek-ref filter excluded it). Counted, not reported as a
	// failure.
	EnvelopeStatusSkipped VerifyEnvelopeStatus = "skipped"
)

// VerifyEnvelopeFinding is one classified manifest. OK + Unencrypted +
// Skipped findings stay aggregated as counters; everything else lands
// in Failures up to maxVerifyEnvelopeFindings entries.
type VerifyEnvelopeFinding struct {
	Deployment string               `json:"deployment"`
	BackupID   string               `json:"backup_id"`
	KEKRef     string               `json:"kek_ref,omitempty"`
	Status     VerifyEnvelopeStatus `json:"status"`
	Reason     string               `json:"reason,omitempty"`
}

// VerifyEnvelopesOptions configures one fleet-wide envelope check.
//
// What VerifyEnvelopes does: walks every committed (non-tombstoned)
// backup manifest in the repo, classifies each by encryption-envelope
// health, and returns aggregate counters + a capped list of failures.
//
// What it does NOT do:
//
//   - Fetch or decrypt chunks. Per-chunk verification is the
//     `verify <deployment>` command's job; it operates on one backup
//     at a time and is O(unique chunks). The envelope check is
//     O(manifest count) — orders of magnitude cheaper for a fleet
//     audit.
//   - Mutate anything. Read-only by construction; safe to run against
//     a read-only repo, against a WORM-locked repo, against a
//     production repo at any cadence.
//   - Decide policy. "Unencrypted backups are unacceptable" is the
//     operator's call; we surface the counter and let the operator
//     decide. The CLI's exit-code mapping is similarly conservative
//     (ExitVerifyFailed only when there's a real envelope break, not
//     for unencrypted-but-readable manifests).
//
// Resumability: trivially re-runnable. The walk is deterministic
// (sorted deployments + manifest keys) and stateless.
type VerifyEnvelopesOptions struct {
	// Verifier validates each manifest's signature at iteration time.
	// Required — a fleet-audit that doesn't notice tampered manifests
	// would be a security hole. Use NewVerifierFromKeyring or load
	// directly from the operator's signing-key public half.
	Verifier *Verifier

	// KEKResolver returns the KEK bytes for a given KEKRef. The
	// resolver decides whether a ref is recognised; an unrecognised
	// ref maps to EnvelopeStatusKEKUnknown.
	//
	// Required.
	KEKResolver func(ref string) ([encryption.KeyLen]byte, error)

	// DeploymentFilter restricts the walk to a single deployment.
	// Empty walks every deployment.
	DeploymentFilter string

	// KEKRefFilter restricts the walk to manifests whose KEKRef
	// matches this string. Empty walks every kek_ref. Useful for
	// post-rotation audits ("show me everything still wrapped under
	// the old ref").
	KEKRefFilter string

	// OnProgress fires per finding (after classification). Optional.
	// Synchronous; do not block.
	OnProgress func(VerifyEnvelopeFinding)
}

// VerifyEnvelopesResult is the structured outcome.
type VerifyEnvelopesResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	DeploymentFilter string `json:"deployment_filter,omitempty"`
	KEKRefFilter     string `json:"kek_ref_filter,omitempty"`

	// Considered counts every manifest we attempted to classify
	// (including ones filtered out by KEKRefFilter, which appear under
	// Skipped). The classification counters below sum to Considered.
	Considered int `json:"considered"`

	OK                int `json:"ok"`
	Unencrypted       int `json:"unencrypted"`
	KEKUnknown        int `json:"kek_unknown,omitempty"`
	WrappedDEKCorrupt int `json:"wrapped_dek_corrupt,omitempty"`
	UnwrapFailed      int `json:"unwrap_failed,omitempty"`
	UnknownScheme     int `json:"unknown_scheme,omitempty"`
	SignatureFailed   int `json:"signature_failed,omitempty"`
	Skipped           int `json:"skipped,omitempty"`

	// Failures aggregates non-OK findings, capped at
	// maxVerifyEnvelopeFindings. Counters above stay unbounded so the
	// total picture is preserved even when the per-key list is
	// truncated.
	Failures []VerifyEnvelopeFinding `json:"failures,omitempty"`
}

// AnyBroken reports whether any finding indicates an envelope break.
// Unencrypted manifests are not "broken" — they're a policy concern
// the operator handles. Skipped manifests are explicitly excluded.
func (r *VerifyEnvelopesResult) AnyBroken() bool {
	return r.KEKUnknown+r.WrappedDEKCorrupt+r.UnwrapFailed+r.UnknownScheme+r.SignatureFailed > 0
}

// VerifyEnvelopes runs one fleet-wide envelope check against sp.
// See VerifyEnvelopesOptions for semantics.
func VerifyEnvelopes(ctx context.Context, sp storage.StoragePlugin, opts VerifyEnvelopesOptions) (*VerifyEnvelopesResult, error) {
	if sp == nil {
		return nil, errors.New("backup verify-envelopes: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("backup verify-envelopes: Verifier is required")
	}
	if opts.KEKResolver == nil {
		return nil, errors.New("backup verify-envelopes: KEKResolver is required")
	}

	res := &VerifyEnvelopesResult{
		Schema:           VerifyEnvelopesSchema,
		StartedAt:        time.Now().UTC(),
		DeploymentFilter: opts.DeploymentFilter,
		KEKRefFilter:     opts.KEKRefFilter,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	store := NewManifestStore(sp)

	var deployments []string
	if opts.DeploymentFilter != "" {
		deployments = []string{opts.DeploymentFilter}
	} else {
		ds, err := store.Deployments(ctx)
		if err != nil {
			finish()
			return res, fmt.Errorf("backup verify-envelopes: enumerate deployments: %w", err)
		}
		deployments = ds
	}
	sort.Strings(deployments)

	for _, deployment := range deployments {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			res.Considered++
			if lerr != nil {
				// readVerified returns wrapped signature errors here.
				// The List walk already excludes tombstoned manifests,
				// so this is a real "manifest in the repo doesn't
				// verify" finding — the loudest possible envelope
				// problem.
				f := VerifyEnvelopeFinding{
					Deployment: deployment,
					BackupID:   "<unknown>",
					Status:     EnvelopeStatusSignatureFailed,
					Reason:     lerr.Error(),
				}
				res.SignatureFailed++
				recordFinding(res, f)
				emitFinding(opts, f)
				continue
			}
			classifyEnvelope(res, opts, deployment, m)
		}
	}

	finish()
	return res, nil
}

// classifyEnvelope inspects one manifest's encryption block and updates
// res accordingly. Pure function over (manifest, opts) — no I/O beyond
// the KEKResolver call.
func classifyEnvelope(res *VerifyEnvelopesResult, opts VerifyEnvelopesOptions, deployment string, m *Manifest) {
	f := VerifyEnvelopeFinding{
		Deployment: deployment,
		BackupID:   m.BackupID,
	}

	if m.Encryption == nil {
		// Unencrypted manifest. The KEKRefFilter (if set) inherently
		// excludes these — an unencrypted manifest has no KEKRef to
		// match.
		if opts.KEKRefFilter != "" {
			f.Status = EnvelopeStatusSkipped
			res.Skipped++
			emitFinding(opts, f)
			return
		}
		f.Status = EnvelopeStatusUnencrypted
		res.Unencrypted++
		emitFinding(opts, f)
		return
	}

	f.KEKRef = m.Encryption.KEKRef

	if opts.KEKRefFilter != "" && m.Encryption.KEKRef != opts.KEKRefFilter {
		f.Status = EnvelopeStatusSkipped
		res.Skipped++
		emitFinding(opts, f)
		return
	}

	if m.Encryption.Scheme != "aes-256-gcm" {
		f.Status = EnvelopeStatusUnknownScheme
		f.Reason = fmt.Sprintf("scheme %q is not recognised by this binary", m.Encryption.Scheme)
		res.UnknownScheme++
		recordFinding(res, f)
		emitFinding(opts, f)
		return
	}

	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		f.Status = EnvelopeStatusWrappedDEKCorrupt
		f.Reason = fmt.Sprintf("decode wrapped_dek: %v", err)
		res.WrappedDEKCorrupt++
		recordFinding(res, f)
		emitFinding(opts, f)
		return
	}

	kek, err := opts.KEKResolver(m.Encryption.KEKRef)
	if err != nil {
		f.Status = EnvelopeStatusKEKUnknown
		f.Reason = fmt.Sprintf("resolve KEK %q: %v", m.Encryption.KEKRef, err)
		res.KEKUnknown++
		recordFinding(res, f)
		emitFinding(opts, f)
		return
	}

	if _, err := encryption.Unwrap(kek, wrapped); err != nil {
		f.Status = EnvelopeStatusUnwrapFailed
		f.Reason = fmt.Sprintf("unwrap DEK with KEK %q: %v", m.Encryption.KEKRef, err)
		res.UnwrapFailed++
		recordFinding(res, f)
		emitFinding(opts, f)
		return
	}

	f.Status = EnvelopeStatusOK
	res.OK++
	emitFinding(opts, f)
}

func recordFinding(res *VerifyEnvelopesResult, f VerifyEnvelopeFinding) {
	if len(res.Failures) >= maxVerifyEnvelopeFindings {
		return
	}
	res.Failures = append(res.Failures, f)
}

func emitFinding(opts VerifyEnvelopesOptions, f VerifyEnvelopeFinding) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(f)
}
