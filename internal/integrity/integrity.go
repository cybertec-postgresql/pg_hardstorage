// Package integrity implements continuous-attestation runs: periodic
// integrity scans of the repository that re-verify every committed
// manifest's signature, confirm referenced chunks are still present,
// and (optionally) re-fetch a sample of chunks for content
// verification.  Every Run is signed and stored in the repo so an
// auditor can prove the repo was intact at any historical attest
// time.
//
// This is the SPEC commitment "backup integrity continuous
// attestation — Periodic re-hash & re-sign of old chunks + manifest
// signature re-verify; finds bit-rot before restore."
//
// Design:
//
//   - The Run is the unit of evidence.  One Run is one signed
//     attestation that, at some point in time, the named subset of
//     the repo passed integrity checks.
//   - Strategies trade scan cost for assurance: `manifests-only` is
//     fastest (no chunk I/O); `presence` is the default (chunk Stat
//     for every referenced chunk); `content-sample:N` re-fetches N%
//     of chunks for plaintext-SHA-256 verification (skipped on
//     encrypted chunks the run can't decrypt — recorded as such);
//     `content-full` re-fetches every chunk.
//   - Runs pair with the threshold package: the Run's body hash can
//     be the subject of a threshold attestation, so a multi-party
//     blessing of "the repo passed integrity at time T" is one
//     `threshold attest sign integrity_run <run-id>` away.
//
// Storage layout:
//
//	integrity/runs/<id>.json
//
// Run IDs are lex-sortable: `<020d-unix-seconds>-<8-hex-fnv>`.
package integrity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Schema strings (24-month backward-compat).
const (
	SchemaRun    = "pg_hardstorage.integrity.run.v1"
	canonicalSig = "pg_hardstorage.integrity.run.canon.v1"
)

// Status of a Run.
type Status string

const (
	// StatusOK means the run completed with no issues found.
	StatusOK Status = "ok"
	// StatusFoundIssues means the run completed and surfaced at
	// least one integrity issue.
	StatusFoundIssues Status = "found_issues"
	// StatusError means the run aborted before completion (I/O,
	// permission, configuration, or other unrecoverable failure).
	StatusError Status = "error"
)

// Strategy tags how aggressively to scan.  Mode is one of
// "manifests-only" | "presence" | "content-sample" | "content-full".
type Strategy struct {
	Mode    string `json:"mode"`
	Percent int    `json:"percent,omitempty"` // 0..100 — used by "content-sample"
	Count   int    `json:"count,omitempty"`   // alternative to Percent
	Seed    int64  `json:"seed,omitempty"`    // deterministic sampling
}

// Default strategy: chunk presence checks + signature verification.
// Cheap; runs in seconds for repos with ~10k manifests.
func DefaultStrategy() Strategy {
	return Strategy{Mode: "presence"}
}

// ManifestSection summarises manifest signature verification.
type ManifestSection struct {
	Total          int               `json:"total"`
	SignaturesOK   int               `json:"signatures_ok"`
	SignaturesFail int               `json:"signatures_fail"`
	ReadFail       int               `json:"read_fail"`
	Failures       []ManifestFailure `json:"failures,omitempty"`
}

// ManifestFailure is one rejected manifest.
type ManifestFailure struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id,omitempty"`
	Reason     string `json:"reason"`
}

// ChunkSection summarises chunk presence + content verification.
type ChunkSection struct {
	DistinctReferenced int            `json:"distinct_referenced"`
	PresenceChecked    int            `json:"presence_checked"`
	Sampled            int            `json:"sampled"`
	Verified           int            `json:"verified"`
	Mismatched         int            `json:"mismatched"`
	Missing            int            `json:"missing"`
	Skipped            int            `json:"skipped"` // skipped because encrypted + no key
	Failures           []ChunkFailure `json:"failures,omitempty"`
}

// ChunkFailure is one chunk that didn't pass.  Reason values:
// "missing" | "hash_mismatch" | "fetch_failed".
type ChunkFailure struct {
	ChunkHash    string   `json:"chunk_hash"`
	Reason       string   `json:"reason"`
	Detail       string   `json:"detail,omitempty"`
	ReferencedBy []string `json:"referenced_by,omitempty"`
}

// Run is the result of one integrity-attestation pass.  Signed; the
// Signature covers the canonical body excluding Signature itself.
type Run struct {
	Schema     string    `json:"schema"`
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     Status    `json:"status"`
	Strategy   Strategy  `json:"strategy"`

	Manifests ManifestSection `json:"manifests"`
	Chunks    ChunkSection    `json:"chunks"`

	// Optional restriction (informational).  When set, only this
	// deployment was scanned; an empty value means "every deployment".
	Deployment string `json:"deployment,omitempty"`

	// Operator note recorded with the run (e.g. "weekly cron",
	// "post-incident sanity check").
	Note string `json:"note,omitempty"`

	// Signed-attestation block.
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	BodyHash             string `json:"body_hash,omitempty"`
	Signature            string `json:"signature,omitempty"`
}

// Sentinel errors.
var (
	ErrRunNotFound      = errors.New("integrity: run not found")
	ErrInvalidStrategy  = errors.New("integrity: invalid strategy")
	ErrSignatureInvalid = errors.New("integrity: signature does not validate")
)

// Signer is the signing side of the attestation.  backup.Signer
// satisfies it; tests use a struct literal.
type Signer interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
}

// KeyResolver returns the public key for the given fingerprint when
// verifying a Run's signature.  SingleKeyResolver is the
// straightforward case (operator's own key).
type KeyResolver interface {
	PublicKey(fingerprint string) (ed25519.PublicKey, error)
}

// SingleKeyResolver wraps one ed25519.PublicKey.
type SingleKeyResolver struct {
	Key ed25519.PublicKey
}

// PublicKey returns the wrapped key regardless of fingerprint.  Used
// for the "I trust this key" verifier path.
func (r *SingleKeyResolver) PublicKey(string) (ed25519.PublicKey, error) {
	if r.Key == nil {
		return nil, errors.New("integrity: no key configured")
	}
	return r.Key, nil
}

// Engine drives one integrity-attestation pass.  Constructed via
// NewEngine; Execute returns the populated, unsigned Run.  Sign with
// SignRun before persisting.
type Engine struct {
	sp        storage.StoragePlugin
	manifests *backup.ManifestStore
	verifier  *backup.Verifier
	cas       *repo.CAS
	now       func() time.Time
	rng       func(int64) *rand.Rand
}

// EngineOptions wires the dependencies.  cas may be nil — content
// verification just gets skipped in that case.
type EngineOptions struct {
	Storage   storage.StoragePlugin
	Manifests *backup.ManifestStore
	Verifier  *backup.Verifier
	CAS       *repo.CAS

	// Now overrides time.Now for deterministic tests.  Optional.
	Now func() time.Time
	// RNG overrides math/rand seeding for deterministic sampling.
	// Optional.
	RNG func(int64) *rand.Rand
}

// NewEngine constructs an Engine with sensible defaults for any
// optional fields.
func NewEngine(opts EngineOptions) *Engine {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	rng := opts.RNG
	if rng == nil {
		rng = func(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
	}
	return &Engine{
		sp:        opts.Storage,
		manifests: opts.Manifests,
		verifier:  opts.Verifier,
		cas:       opts.CAS,
		now:       now,
		rng:       rng,
	}
}

// Execute walks the configured deployments, validates manifests,
// checks chunk presence, optionally re-hashes a sample of chunks.
// Returns the populated Run in unsigned form (caller signs).
func (e *Engine) Execute(ctx context.Context, deployment string, strategy Strategy, note string) (*Run, error) {
	if err := validateStrategy(strategy); err != nil {
		return nil, err
	}
	startedAt := e.now().UTC()
	id := newRunID(startedAt, deployment)
	run := &Run{
		Schema:     SchemaRun,
		ID:         id,
		StartedAt:  startedAt,
		Strategy:   strategy,
		Deployment: deployment,
		Note:       note,
	}

	// 1. Resolve deployments.
	var deployments []string
	if deployment != "" {
		deployments = []string{deployment}
	} else {
		ds, err := e.manifests.Deployments(ctx)
		if err != nil {
			run.FinishedAt = e.now().UTC()
			run.Status = StatusError
			run.Manifests.Failures = append(run.Manifests.Failures, ManifestFailure{
				Reason: "list deployments: " + err.Error(),
			})
			return run, nil
		}
		deployments = ds
	}

	// 2. Walk manifests, collect chunk references.
	chunkRefs := make(map[repo.Hash][]string) // hash → list of backup IDs
	for _, d := range deployments {
		// List yields verified manifests; failures (signature break /
		// unreadable / decode failure) come through the error channel.
		for m, err := range e.manifests.List(ctx, d, e.verifier) {
			run.Manifests.Total++
			if err != nil {
				run.Manifests.SignaturesFail++
				run.Manifests.Failures = append(run.Manifests.Failures, ManifestFailure{
					Deployment: d,
					Reason:     err.Error(),
				})
				continue
			}
			run.Manifests.SignaturesOK++
			if strategy.Mode == "manifests-only" {
				continue
			}
			for _, file := range m.Files {
				for _, c := range file.Chunks {
					chunkRefs[c.Hash] = append(chunkRefs[c.Hash], m.BackupID)
				}
			}
		}
	}

	if strategy.Mode == "manifests-only" {
		finishRun(run, e.now())
		return run, nil
	}

	// 3. Order chunks deterministically so sampling is reproducible.
	hashes := make([]repo.Hash, 0, len(chunkRefs))
	for h := range chunkRefs {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
	run.Chunks.DistinctReferenced = len(hashes)

	// 4. Pick the sample for content-verification.
	sample := pickSample(hashes, strategy, e.rng)

	// 5. Walk every referenced hash for presence.  For each hash in
	// the sample, additionally fetch + verify content.
	sampleSet := make(map[repo.Hash]struct{}, len(sample))
	for _, h := range sample {
		sampleSet[h] = struct{}{}
	}
	for _, h := range hashes {
		run.Chunks.PresenceChecked++
		_, err := e.sp.Stat(ctx, repo.ChunkKey(h))
		if err != nil {
			run.Chunks.Missing++
			run.Chunks.Failures = append(run.Chunks.Failures, ChunkFailure{
				ChunkHash:    h.String(),
				Reason:       "missing",
				Detail:       err.Error(),
				ReferencedBy: dedupeIDs(chunkRefs[h]),
			})
			continue
		}
		if _, ok := sampleSet[h]; !ok {
			continue
		}
		run.Chunks.Sampled++
		if e.cas == nil {
			run.Chunks.Skipped++
			continue
		}
		// GetChunkBytes does decompress + plaintext-SHA-256 check.
		// On encrypted chunks without a registered decryptor, it
		// returns a "no decryptor" error — we treat that as Skipped
		// rather than a verification failure (it's a tooling gap, not
		// a bit-rot signal).
		if _, err := e.cas.GetChunkBytes(ctx, h); err != nil {
			if isMissingDecryptorErr(err) {
				run.Chunks.Skipped++
				continue
			}
			// Distinguish a corrupted-content failure from a "could
			// not even fetch the bytes" failure for the report.
			reason := "fetch_failed"
			if isHashMismatchErr(err) {
				reason = "hash_mismatch"
				run.Chunks.Mismatched++
			}
			run.Chunks.Failures = append(run.Chunks.Failures, ChunkFailure{
				ChunkHash:    h.String(),
				Reason:       reason,
				Detail:       err.Error(),
				ReferencedBy: dedupeIDs(chunkRefs[h]),
			})
			continue
		}
		run.Chunks.Verified++
	}

	finishRun(run, e.now())
	return run, nil
}

func finishRun(run *Run, now time.Time) {
	run.FinishedAt = now.UTC()
	if run.Status != "" { // already errored
		return
	}
	if run.Manifests.SignaturesFail > 0 ||
		run.Chunks.Missing > 0 || run.Chunks.Mismatched > 0 ||
		len(run.Chunks.Failures) > 0 {
		run.Status = StatusFoundIssues
		return
	}
	run.Status = StatusOK
}

// dedupeIDs strips repeats from the per-chunk reference list so the
// report doesn't echo the same backup ID 100 times for a popular
// chunk.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > 5 {
		out = append(out[:5], "…+"+itoa(len(out)-5)+" more")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func isMissingDecryptorErr(err error) bool {
	// CAS errors flow up as wrapped strings; we look for the
	// fingerprint of the "no decryptor for X" message rather than
	// imposing a sentinel-error contract on the encryption package
	// (which evolves separately).
	msg := err.Error()
	return strings.Contains(msg, "no decryptor") ||
		strings.Contains(msg, "no encryptor configured")
}

func isHashMismatchErr(err error) bool {
	// The authoritative signal: CAS.GetChunkBytes wraps
	// storage.ErrChecksumMismatch (with %w) when a chunk's stored bytes
	// no longer hash to its content-address. Match it by identity, not
	// by message text — its string is "storage: checksum mismatch",
	// which contains neither "sha256 mismatch" nor "hash mismatch", so
	// the old substring check miscategorised real bit-rot as
	// fetch_failed and left the Mismatched counter at 0 (the very
	// signal operators triage corruption on).
	if errors.Is(err, storage.ErrChecksumMismatch) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "sha256 mismatch") ||
		strings.Contains(msg, "hash mismatch") ||
		strings.Contains(msg, "checksum mismatch")
}

// pickSample resolves the strategy + chunk set into the list of
// chunks to content-verify.
func pickSample(hashes []repo.Hash, s Strategy, rng func(int64) *rand.Rand) []repo.Hash {
	if len(hashes) == 0 {
		return nil
	}
	switch s.Mode {
	case "manifests-only", "presence":
		return nil
	case "content-full":
		return hashes
	case "content-sample":
		count := s.Count
		if count == 0 && s.Percent > 0 {
			count = (s.Percent * len(hashes)) / 100
			if count < 1 {
				count = 1
			}
		}
		if count == 0 {
			return nil
		}
		if count >= len(hashes) {
			return hashes
		}
		seed := s.Seed
		if seed == 0 {
			seed = int64(len(hashes))
		}
		r := rng(seed)
		// Fisher-Yates partial shuffle.
		idx := make([]int, len(hashes))
		for i := range idx {
			idx[i] = i
		}
		for i := 0; i < count; i++ {
			j := i + r.Intn(len(idx)-i)
			idx[i], idx[j] = idx[j], idx[i]
		}
		out := make([]repo.Hash, count)
		for i := 0; i < count; i++ {
			out[i] = hashes[idx[i]]
		}
		// Sort for stable output.
		sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
		return out
	}
	return nil
}

func validateStrategy(s Strategy) error {
	switch s.Mode {
	case "manifests-only", "presence", "content-full":
		return nil
	case "content-sample":
		if s.Percent < 0 || s.Percent > 100 {
			return fmt.Errorf("%w: percent %d not in 0..100", ErrInvalidStrategy, s.Percent)
		}
		if s.Count < 0 {
			return fmt.Errorf("%w: count must be ≥ 0", ErrInvalidStrategy)
		}
		if s.Percent == 0 && s.Count == 0 {
			return fmt.Errorf("%w: content-sample requires --percent or --count", ErrInvalidStrategy)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown mode %q", ErrInvalidStrategy, s.Mode)
}

// canonicalRunBytes is the byte sequence the operator's signature
// covers.  Length-prefixed; deterministic across Go runtimes.
func canonicalRunBytes(r *Run) []byte {
	var buf strings.Builder
	buf.WriteString(canonicalSig)
	buf.WriteByte(0)
	for _, field := range []string{
		r.Schema,
		r.ID,
		string(r.Status),
		r.Strategy.Mode,
		r.Deployment,
		r.Note,
	} {
		binary.Write(&buf, binary.BigEndian, int64(len(field)))
		buf.WriteString(field)
	}
	binary.Write(&buf, binary.BigEndian, r.StartedAt.UTC().UnixNano())
	binary.Write(&buf, binary.BigEndian, r.FinishedAt.UTC().UnixNano())
	binary.Write(&buf, binary.BigEndian, int64(r.Strategy.Percent))
	binary.Write(&buf, binary.BigEndian, int64(r.Strategy.Count))
	binary.Write(&buf, binary.BigEndian, int64(r.Strategy.Seed))
	binary.Write(&buf, binary.BigEndian, int64(r.Manifests.Total))
	binary.Write(&buf, binary.BigEndian, int64(r.Manifests.SignaturesOK))
	binary.Write(&buf, binary.BigEndian, int64(r.Manifests.SignaturesFail))
	binary.Write(&buf, binary.BigEndian, int64(r.Manifests.ReadFail))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.DistinctReferenced))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.PresenceChecked))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.Sampled))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.Verified))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.Mismatched))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.Missing))
	binary.Write(&buf, binary.BigEndian, int64(r.Chunks.Skipped))
	// Failures are committed-to via a digest of their canonical
	// JSON; embedding them in length-prefixed form would explode the
	// signing input for noisy reports.
	failureDigest := digestFailures(r)
	buf.Write(failureDigest[:])
	return []byte(buf.String())
}

// digestFailures hashes every failure entry in stable order so the
// signature commits to the failure list without verbatim inclusion.
func digestFailures(r *Run) [32]byte {
	type item struct {
		Section, Key, Reason string
	}
	items := make([]item, 0, len(r.Manifests.Failures)+len(r.Chunks.Failures))
	for _, f := range r.Manifests.Failures {
		items = append(items, item{"manifest", f.Deployment + "/" + f.BackupID, f.Reason})
	}
	for _, f := range r.Chunks.Failures {
		items = append(items, item{"chunk", f.ChunkHash, f.Reason})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Section != items[j].Section {
			return items[i].Section < items[j].Section
		}
		if items[i].Key != items[j].Key {
			return items[i].Key < items[j].Key
		}
		return items[i].Reason < items[j].Reason
	})
	hash := sha256.New()
	for _, it := range items {
		fmt.Fprintf(hash, "%s|%s|%s\n", it.Section, it.Key, it.Reason)
	}
	var out [32]byte
	hash.Sum(out[:0])
	return out
}

// SignRun signs the run with the supplied signer.  Mutates
// r.PublicKeyFingerprint + r.BodyHash + r.Signature.
func SignRun(r *Run, signer Signer) error {
	if signer == nil {
		return errors.New("integrity: nil signer")
	}
	r.PublicKeyFingerprint = publicKeyFingerprint(signer.PublicKey())
	canon := canonicalRunBytes(r)
	bodyHash := sha256.Sum256(canon)
	r.BodyHash = fmt.Sprintf("%x", bodyHash[:])
	r.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// VerifyRun re-derives the canonical bytes, cross-checks BodyHash,
// looks up the public key by fingerprint, and validates the
// signature.
func VerifyRun(r *Run, resolver KeyResolver) error {
	if r == nil {
		return errors.New("integrity: nil run")
	}
	if r.Signature == "" {
		return ErrSignatureInvalid
	}
	canon := canonicalRunBytes(r)
	bodyHash := sha256.Sum256(canon)
	if fmt.Sprintf("%x", bodyHash[:]) != r.BodyHash {
		return fmt.Errorf("%w: body_hash drift", ErrSignatureInvalid)
	}
	pub, err := resolver.PublicKey(r.PublicKeyFingerprint)
	if err != nil {
		return fmt.Errorf("integrity: resolve public key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("integrity: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, canon, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// ----- ID helpers -----

func newRunID(at time.Time, deployment string) string {
	hasher := fnv.New32a()
	hasher.Write([]byte(at.UTC().Format(time.RFC3339Nano)))
	hasher.Write([]byte(deployment))
	short := fmt.Sprintf("%08x", hasher.Sum32())
	return fmt.Sprintf("%020d-%s", at.UTC().Unix(), short)
}

func publicKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("%x", sum[:8])
}

// ----- storage -----

// RunStore writes + reads runs under integrity/runs/<id>.json.
type RunStore struct {
	sp storage.StoragePlugin
}

// NewRunStore returns a store rooted at the given storage plugin.
func NewRunStore(sp storage.StoragePlugin) *RunStore {
	return &RunStore{sp: sp}
}

func runKey(id string) string { return "integrity/runs/" + id + ".json" }

// Put persists a run.  Refuses to overwrite (run IDs include a
// timestamp; collision is a programmer error).
func (s *RunStore) Put(ctx context.Context, r *Run) error {
	body, err := stdjson.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	key := runKey(r.ID)
	// Randomised tmp so two concurrent first-writers of the same run ID
	// don't share a staging path. A fixed key+".tmp" lets one writer's
	// truncate-then-write tear the other's bytes before the rename installs
	// them — the torn-overwrite class fixed in fs/timeline and already used
	// by the dsa/threshold sibling stores.
	tmp := key + fmt.Sprintf(".tmp.%016x", rand.Uint64())
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("integrity: put tmp: %w", err)
	}
	return s.sp.RenameIfNotExists(ctx, tmp, key)
}

// Get reads + decodes one run.
func (s *RunStore) Get(ctx context.Context, id string) (*Run, error) {
	rd, err := s.sp.Get(ctx, runKey(id))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrRunNotFound, id, err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("integrity: run read: %w", err)
	}
	var r Run
	if err := stdjson.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("integrity: run decode: %w", err)
	}
	return &r, nil
}

// ListFilter filters the List output.  Zero values mean "no filter".
type ListFilter struct {
	Since      *time.Time
	Status     Status
	Deployment string
}

// List returns every run, newest-first, matching the filter.
func (s *RunStore) List(ctx context.Context, f ListFilter) ([]*Run, error) {
	const prefix = "integrity/runs/"
	var out []*Run
	for obj, err := range s.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("integrity: list runs: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(base, ".json")
		r, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		if f.Since != nil && r.StartedAt.Before(*f.Since) {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		if f.Deployment != "" && r.Deployment != f.Deployment {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}
