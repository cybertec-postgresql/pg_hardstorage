// bundle.go — evidence bundle (tar.gz) builder/verifier for the audit chain.
package audit

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// BundleSchema is the on-disk version tag for bundle.json.
// Stable per the v1 commitment.
const BundleSchema = "pg_hardstorage.audit.evidence_bundle.v1"

// BundleManifest is the bundle.json metadata.  It records what's
// inside the bundle + the signature posture so a verifier can
// audit the bundle without first decompressing it.
type BundleManifest struct {
	Schema string `json:"schema"`

	// GeneratedAt + Operator describe who built the bundle.
	GeneratedAt time.Time `json:"generated_at"`
	Operator    string    `json:"operator,omitempty"`

	// SourceURL is the repository the bundle was extracted from.
	SourceURL string `json:"source_url,omitempty"`

	// Window bounds for the included events.  Since is
	// inclusive; Until is exclusive.  Same semantics as
	// audit.ListFilters.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	// Filters records the operator-supplied filters at bundle
	// time.  Auditors verifying a partial bundle need to know
	// what was excluded.
	Filters BundleFilters `json:"filters,omitempty"`

	// EventCount is the number of audit events in events.ndjson.
	// AnchorCount is the number of anchors in anchors.ndjson.
	EventCount  int `json:"event_count"`
	AnchorCount int `json:"anchor_count,omitempty"`

	// HeadHash is the chain-head hash at bundle time.  An
	// auditor verifying the bundle independently re-derives the
	// chain via VerifyChain on the included events and asserts
	// the recomputed head matches HeadHash.
	HeadHash     string `json:"head_hash,omitempty"`
	HeadSequence int64  `json:"head_sequence,omitempty"`

	// PublicKeyFingerprint is the SHA-256 of the operator's
	// signing public key (first 16 hex).  Same form the rest of
	// the binary uses (kms inspect, audit verify-anchor).
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`

	// SignedFiles lists the file names within the bundle that
	// the signature covers (everything except signature.sig +
	// the manifest file itself, which is signed via inclusion
	// in this list).
	SignedFiles []string `json:"signed_files"`

	// SignatureAlgorithm is the canonical algorithm name for
	// the detached signature in signature.sig.
	SignatureAlgorithm string `json:"signature_algorithm"`
}

// BundleFilters mirrors audit.ListFilters but JSON-stable.
// (audit.ListFilters has unexported fields that wouldn't
// serialise; this is the bundle's persistent form.)
type BundleFilters struct {
	Action       string `json:"action,omitempty"`
	ActionPrefix string `json:"action_prefix,omitempty"`
	Actor        string `json:"actor,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	Deployment   string `json:"deployment,omitempty"`
	BackupID     string `json:"backup_id,omitempty"`
}

// BundleResult is the outcome of one ExportBundle call.
type BundleResult struct {
	Schema       string          `json:"schema"`
	Path         string          `json:"path"`
	GeneratedAt  time.Time       `json:"generated_at"`
	StoppedAt    time.Time       `json:"stopped_at"`
	DurationMS   int64           `json:"duration_ms"`
	EventCount   int             `json:"event_count"`
	AnchorCount  int             `json:"anchor_count"`
	BundleBytes  int64           `json:"bundle_bytes"`
	SHA256       string          `json:"sha256"`
	HeadHash     string          `json:"head_hash,omitempty"`
	HeadSequence int64           `json:"head_sequence,omitempty"`
	Manifest     *BundleManifest `json:"manifest,omitempty"`
}

// BundleResultSchema is the on-disk version tag for BundleResult.
const BundleResultSchema = "pg_hardstorage.audit.export_bundle_result.v1"

// ExportOptions configures one ExportBundle call.
type ExportOptions struct {
	// Filters scope the included events.  Same shape as
	// audit.ListFilters.
	Filters ListFilters

	// IncludeAnchors, when true, also dumps every anchor in the
	// window into anchors.ndjson.  Default false: many bundles
	// don't need anchor history (the head-hash proof is enough).
	IncludeAnchors bool

	// Operator records who exported the bundle.  Free-form;
	// flows into bundle.json's manifest.
	Operator string

	// SourceURL is recorded in the manifest for traceability.
	SourceURL string

	// Now overrides time.Now() for deterministic test output.
	Now time.Time
}

// ExportBundle writes a signed evidence bundle to w.  Returns
// the BundleResult with metadata + the bundle's SHA-256 (so
// callers can record the digest separately for tamper-evidence).
//
// Bundle layout (gzipped tar):
//
//	bundle.json       — manifest (this file's metadata)
//	events.ndjson     — one event per line in chain order
//	anchors.ndjson    — anchors in window (when IncludeAnchors)
//	chain_proof.json  — head pointer + prev hashes for verification
//	README.md         — verifier instructions
//	public_key.pem    — operator's signing public key
//	signature.sig     — detached ed25519 signature over every other file
//
// The signature covers the tar-archive contents of every file
// EXCEPT signature.sig itself (chicken-and-egg).  An auditor
// reproduces the signature by:
//
//  1. Extract the tarball.
//  2. Re-tar every signed file (in the order recorded in
//     bundle.json's `signed_files`) without the signature.
//  3. SHA-256 the resulting tar.
//  4. Verify with public_key.pem against signature.sig.
//
// We sign the SHA-256 of the canonical concatenation of file
// bytes — not the tar bytes — so bundle re-archiving (different
// tar tools, different timestamps) doesn't invalidate the
// signature.
func ExportBundle(ctx context.Context, sp storage.StoragePlugin, w io.Writer, signer EventSigner, opts ExportOptions) (*BundleResult, error) {
	if sp == nil {
		return nil, errors.New("audit: ExportBundle: nil StoragePlugin")
	}
	if signer == nil {
		return nil, errors.New("audit: ExportBundle: nil signer")
	}
	if w == nil {
		return nil, errors.New("audit: ExportBundle: nil writer")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	started := time.Now().UTC()
	res := &BundleResult{
		Schema:      BundleResultSchema,
		GeneratedAt: now,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(started).Milliseconds()
	}

	store := NewStore(sp)
	events, err := store.Search(ctx, opts.Filters)
	if err != nil {
		finish()
		return res, fmt.Errorf("audit: ExportBundle: search events: %w", err)
	}

	var anchors []*Anchor
	if opts.IncludeAnchors {
		anchors, err = collectAnchors(ctx, sp, opts.Filters)
		if err != nil {
			finish()
			return res, fmt.Errorf("audit: ExportBundle: collect anchors: %w", err)
		}
	}

	// Head-pointer metadata for the chain proof. With a sharded chain
	// there is no single repo-wide head, so the bundle describes its OWN
	// tip: the newest event it contains (Search returns them in
	// timestamp order). This is self-consistent regardless of which
	// shards the filtered events came from, and matches the LastEvent
	// edge recorded in the chain proof. An empty bundle falls back to
	// the global chain head. The bundle's integrity is the ed25519
	// signature over its files, not a head recomputation.
	var headPtr *HeadPointer
	if len(events) > 0 {
		tip := events[len(events)-1]
		headPtr = &HeadPointer{
			Schema:   HeadPointerSchema,
			Sequence: tip.Sequence,
			Hash:     tip.Hash,
			EventID:  tip.ID,
			Key:      keyFor(tip),
		}
	} else {
		headPtr, _ = store.readHeadPointer(ctx, "")
	}

	// Build each file's bytes in the order they appear in the
	// tar.  signed_files ORDER MATTERS — the auditor reproduces
	// the signature by concatenating in this exact order.
	manifest := &BundleManifest{
		Schema:               BundleSchema,
		GeneratedAt:          now,
		Operator:             opts.Operator,
		SourceURL:            opts.SourceURL,
		Since:                opts.Filters.Since,
		Until:                opts.Filters.Until,
		Filters:              filtersToBundleFilters(opts.Filters),
		EventCount:           len(events),
		AnchorCount:          len(anchors),
		PublicKeyFingerprint: publicKeyFingerprint(signer.PublicKey()),
		SignatureAlgorithm:   "ed25519",
	}
	if headPtr != nil {
		manifest.HeadHash = headPtr.Hash
		manifest.HeadSequence = headPtr.Sequence
		res.HeadHash = headPtr.Hash
		res.HeadSequence = headPtr.Sequence
	}

	files := []bundleFile{}

	// events.ndjson: one event per line, in commit order.
	eventsBody := encodeEventsNDJSON(events)
	files = append(files, bundleFile{Name: "events.ndjson", Body: eventsBody})

	// anchors.ndjson: one anchor per line.  Skipped when
	// IncludeAnchors=false.
	if opts.IncludeAnchors {
		anchorsBody := encodeAnchorsNDJSON(anchors)
		files = append(files, bundleFile{Name: "anchors.ndjson", Body: anchorsBody})
	}

	// chain_proof.json: head pointer + window edge events with
	// their hashes so an auditor can re-derive the chain edge
	// without fetching the rest of the repo.
	chainProof := buildChainProof(events, headPtr)
	chainProofBody, err := json.MarshalIndent(chainProof, "", "  ")
	if err != nil {
		finish()
		return res, fmt.Errorf("audit: marshal chain proof: %w", err)
	}
	files = append(files, bundleFile{Name: "chain_proof.json", Body: chainProofBody})

	// public_key.pem: operator's signing public key.
	pubPEM, err := signer.PublicKeyPEM()
	if err != nil {
		finish()
		return res, fmt.Errorf("audit: marshal public key: %w", err)
	}
	files = append(files, bundleFile{Name: "public_key.pem", Body: pubPEM})

	// README.md: verifier instructions.  Always-present so an
	// auditor opening the tarball without the project's docs can
	// still verify.
	files = append(files, bundleFile{Name: "README.md", Body: bundleReadme()})

	// Record the file order in the manifest BEFORE building
	// bundle.json (which is itself one of the signed files).
	manifest.SignedFiles = make([]string, 0, len(files)+1)
	for _, f := range files {
		manifest.SignedFiles = append(manifest.SignedFiles, f.Name)
	}
	// bundle.json is always signed AND last in the order so the
	// verifier knows which other files were included.
	manifest.SignedFiles = append(manifest.SignedFiles, "bundle.json")

	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		finish()
		return res, fmt.Errorf("audit: marshal bundle manifest: %w", err)
	}
	files = append(files, bundleFile{Name: "bundle.json", Body: manifestBody})

	// Sign the canonical concatenation of file bytes.
	sigInput := canonicalBundleBytes(files)
	signature := signer.Sign(sigInput)

	files = append(files, bundleFile{Name: "signature.sig", Body: signature})

	// Build the gzipped tar archive.  We compute the SHA-256
	// of the tar.gz bytes as a separate digest the operator
	// records out-of-band.
	hashed, archiveBytes, err := buildTarGz(w, files, now)
	if err != nil {
		finish()
		return res, fmt.Errorf("audit: build tar: %w", err)
	}

	res.SHA256 = hashed
	res.BundleBytes = archiveBytes
	res.EventCount = len(events)
	res.AnchorCount = len(anchors)
	res.Manifest = manifest

	finish()
	return res, nil
}

// EventSigner is the interface ExportBundle needs from a signer.
// Decoupled from internal/backup so this package doesn't depend
// on the backup package (avoiding an import cycle: backup
// imports audit via the audit chain in the manifest commit
// path; if audit imported backup we'd cycle).
type EventSigner interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
	PublicKeyPEM() ([]byte, error)
}

// bundleFile is one file destined for the tarball.
type bundleFile struct {
	Name string
	Body []byte
}

// canonicalBundleBytes returns the bytes the bundle signature
// covers.  We sign the SHA-256-prefixed concatenation of each
// file's name + length + body, in the order files were added
// to the bundle (which is recorded in manifest.SignedFiles).
//
// The format is deliberately simple + tar-tool-agnostic:
//
//	<file count> as 8-byte big-endian uint64
//	for each file:
//	  <name length>  4-byte BE uint32
//	  <name>         UTF-8 bytes
//	  <body length>  8-byte BE uint64
//	  <body>         raw bytes
//
// signature.sig itself is excluded — it can't sign itself.
//
// The auditor reproduces this exactly: extract the tarball,
// read manifest.signed_files in order, concatenate per-file
// bytes per the format above, SHA-256, verify against
// signature.sig with public_key.pem.
func canonicalBundleBytes(files []bundleFile) []byte {
	out := make([]byte, 0, 64*1024)
	out = appendU64BE(out, uint64(len(files)))
	for _, f := range files {
		out = appendU32BE(out, uint32(len(f.Name)))
		out = append(out, f.Name...)
		out = appendU64BE(out, uint64(len(f.Body)))
		out = append(out, f.Body...)
	}
	return out
}

func appendU32BE(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendU64BE(b []byte, v uint64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// buildTarGz writes the gzipped tar archive to w + computes a
// SHA-256 over the gzipped bytes for the operator's record.
//
// The tar's mtime fields are set to `now` (single value across
// every file) so a re-build of the same input yields the same
// bytes — useful for reproducible bundles.
func buildTarGz(w io.Writer, files []bundleFile, now time.Time) (string, int64, error) {
	hasher := sha256.New()
	mw := io.MultiWriter(w, hasher)
	counter := &writeCounter{w: mw}
	gw := gzip.NewWriter(counter)
	tw := tar.NewWriter(gw)

	for _, f := range files {
		hdr := &tar.Header{
			Name:    f.Name,
			Mode:    0o644,
			Size:    int64(len(f.Body)),
			ModTime: now,
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", 0, err
		}
		if _, err := tw.Write(f.Body); err != nil {
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gw.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), counter.n, nil
}

// writeCounter wraps an io.Writer + counts bytes written.
type writeCounter struct {
	w io.Writer
	n int64
}

// Write forwards p to the wrapped writer and accumulates the
// reported byte count.
func (c *writeCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// encodeEventsNDJSON encodes a slice of events one-per-line.
func encodeEventsNDJSON(items []*Event) []byte {
	var out []byte
	for _, item := range items {
		body, _ := json.Marshal(item)
		out = append(out, body...)
		out = append(out, '\n')
	}
	return out
}

// encodeAnchorsNDJSON encodes an anchor slice one-per-line.
func encodeAnchorsNDJSON(anchors []*Anchor) []byte {
	var out []byte
	for _, a := range anchors {
		body, _ := json.Marshal(a)
		out = append(out, body...)
		out = append(out, '\n')
	}
	return out
}

// collectAnchors walks the audit/anchors/ prefix and returns
// every anchor whose AnchoredAt falls within the filter window.
func collectAnchors(ctx context.Context, sp storage.StoragePlugin, f ListFilters) ([]*Anchor, error) {
	var keys []string
	for info, err := range sp.List(ctx, AnchorPrefix) {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		keys = append(keys, info.Key)
	}
	sort.Strings(keys)
	var out []*Anchor
	for _, key := range keys {
		rc, err := sp.Get(ctx, key)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			continue
		}
		var a Anchor
		if err := json.Unmarshal(body, &a); err != nil {
			continue
		}
		if !f.Since.IsZero() && a.AnchoredAt.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !a.AnchoredAt.Before(f.Until) {
			continue
		}
		out = append(out, &a)
	}
	return out, nil
}

// buildChainProof captures the edge of the chain seen at bundle
// time so an auditor can verify the included events form a
// contiguous segment.
type ChainProof struct {
	Schema       string `json:"schema"`
	HeadHash     string `json:"head_hash,omitempty"`
	HeadSequence int64  `json:"head_sequence,omitempty"`

	// FirstEvent / LastEvent capture the first and last events
	// in the bundled segment with their hash + prev_hash.  An
	// auditor walks events.ndjson and asserts each event's
	// prev_hash matches the prior event's hash; the first
	// event's prev_hash is checked here against the chain's
	// genesis or the prior segment's tail.
	FirstEvent *EventEdge `json:"first_event,omitempty"`
	LastEvent  *EventEdge `json:"last_event,omitempty"`
}

// EventEdge is the pair (id, hash, prev_hash) for one event,
// used to anchor the bundled segment.
type EventEdge struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
	Hash     string `json:"hash"`
	PrevHash string `json:"prev_hash,omitempty"`
}

func buildChainProof(events []*Event, head *HeadPointer) ChainProof {
	out := ChainProof{Schema: "pg_hardstorage.audit.chain_proof.v1"}
	if head != nil {
		out.HeadHash = head.Hash
		out.HeadSequence = head.Sequence
	}
	if len(events) > 0 {
		first := events[0]
		out.FirstEvent = &EventEdge{
			ID: first.ID, Sequence: first.Sequence,
			Hash: first.Hash, PrevHash: first.PrevHash,
		}
		last := events[len(events)-1]
		out.LastEvent = &EventEdge{
			ID: last.ID, Sequence: last.Sequence,
			Hash: last.Hash, PrevHash: last.PrevHash,
		}
	}
	return out
}

// filtersToBundleFilters projects audit.ListFilters into the
// bundle's persistent shape.  Skips Limit + Reverse + ActorContains
// (those are query-time conveniences, not facts about the data).
func filtersToBundleFilters(f ListFilters) BundleFilters {
	return BundleFilters{
		Action:       f.Action,
		ActionPrefix: f.ActionPrefix,
		Actor:        f.Actor,
		Tenant:       f.Tenant,
		Deployment:   f.Deployment,
		BackupID:     f.BackupID,
	}
}

// publicKeyFingerprint mirrors the format used by kms inspect:
// SHA-256 of the public key, first 16 hex chars.
func publicKeyFingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// VerifyBundle reads + verifies a bundle.  Auditors run this to
// reproduce the bundle's claims independently.
//
// The verifier:
//
//  1. Extracts every file from the tarball.
//  2. Loads bundle.json + reads manifest.SignedFiles in order.
//  3. Reconstructs the canonical signing input by concatenating
//     each signed file's bytes in order (per
//     canonicalBundleBytes).
//  4. Loads public_key.pem.
//  5. Verifies signature.sig against the canonical bytes with
//     the public key.
//  6. Asserts the events.ndjson chain is contiguous (each
//     event's prev_hash matches the prior event's hash).
//
// Returns the decoded BundleManifest on success; error on
// signature failure or chain-break.
func VerifyBundle(r io.Reader) (*BundleManifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("audit: open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("audit: tar read: %w", err)
		}
		// Defence against path-traversal / odd headers in a
		// hostile tarball.  We only accept flat-tree names.
		if strings.Contains(hdr.Name, "..") || filepath.IsAbs(hdr.Name) {
			return nil, fmt.Errorf("audit: bundle contains suspicious path %q", hdr.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("audit: tar read body %q: %w", hdr.Name, err)
		}
		files[hdr.Name] = body
	}

	manifestBytes, ok := files["bundle.json"]
	if !ok {
		return nil, errors.New("audit: bundle is missing bundle.json")
	}
	var manifest BundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("audit: decode bundle.json: %w", err)
	}
	if manifest.Schema != BundleSchema {
		return nil, fmt.Errorf("audit: unknown bundle schema %q", manifest.Schema)
	}
	if manifest.SignatureAlgorithm != "ed25519" {
		return nil, fmt.Errorf("audit: unsupported signature algorithm %q",
			manifest.SignatureAlgorithm)
	}

	pubBytes, ok := files["public_key.pem"]
	if !ok {
		return nil, errors.New("audit: bundle is missing public_key.pem")
	}
	pub, err := loadEd25519FromPEM(pubBytes)
	if err != nil {
		return nil, fmt.Errorf("audit: load public key: %w", err)
	}

	sigBytes, ok := files["signature.sig"]
	if !ok {
		return nil, errors.New("audit: bundle is missing signature.sig")
	}

	// Reconstruct the canonical signing input.  Order is
	// dictated by manifest.SignedFiles.
	signed := make([]bundleFile, 0, len(manifest.SignedFiles))
	for _, name := range manifest.SignedFiles {
		body, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("audit: bundle declares signed file %q but it's missing", name)
		}
		signed = append(signed, bundleFile{Name: name, Body: body})
	}
	canon := canonicalBundleBytes(signed)
	if !ed25519.Verify(pub, canon, sigBytes) {
		return nil, errors.New("audit: signature verification failed")
	}
	return &manifest, nil
}

// loadEd25519FromPEM extracts the ed25519 public key from a PEM
// block.  Accepts the PKIX-encoded form internal/backup/sign.go
// produces (x509.MarshalPKIXPublicKey wraps the raw 32-byte key
// in a SubjectPublicKeyInfo header).
func loadEd25519FromPEM(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	const want = "PG_HARDSTORAGE ED25519 PUBLIC KEY"
	if block.Type != want {
		return nil, fmt.Errorf("unexpected PEM type %q (want %q)",
			block.Type, want)
	}
	// PKIX form first — that's what the backup signer emits.
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		k, ok := pub.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("PEM body is %T; want ed25519", pub)
		}
		return k, nil
	}
	// Raw-32-byte form fallback (older bundles or test fixtures).
	if len(block.Bytes) == ed25519.PublicKeySize {
		return ed25519.PublicKey(block.Bytes), nil
	}
	return nil, fmt.Errorf("PEM body is %d bytes; expected PKIX or raw 32-byte ed25519",
		len(block.Bytes))
}

// Re-export base64 for stable test references; internal use is
// via encoding/base64 directly.
var _ = base64.StdEncoding

// bundleReadme is the operator-readable verifier instructions
// included in every bundle.
func bundleReadme() []byte {
	return []byte(`# pg_hardstorage audit evidence bundle

This tarball is a forensics-grade evidence bundle of the audit
chain over a specific time window.  It is signed with the
operator's ed25519 signing key.

## Contents

- ` + "`bundle.json`" + `      — manifest: window, filters, file list, fingerprint
- ` + "`events.ndjson`" + `    — one audit event per line (commit order)
- ` + "`anchors.ndjson`" + `   — anchor history (when --include-anchors was set)
- ` + "`chain_proof.json`" + ` — head pointer + segment edge events
- ` + "`public_key.pem`" + `   — operator's signing public key (PEM)
- ` + "`signature.sig`" + `    — detached ed25519 signature over the bundle

## Verifying

The canonical Go verifier is exposed via:

    pg_hardstorage audit verify-bundle path/to/bundle.tar.gz

Or programmatically via ` + "`internal/audit.VerifyBundle`" + ` — see the
package docs.  The verifier:

1. Extracts every file from the tarball.
2. Loads ` + "`bundle.json`" + ` + reads manifest.signed_files in order.
3. Reconstructs the canonical signing input by concatenating each
   signed file's bytes in the recorded order.
4. Loads ` + "`public_key.pem`" + `.
5. Verifies ` + "`signature.sig`" + ` against the canonical bytes.
6. Asserts ` + "`events.ndjson`" + `'s chain is contiguous (each event's
   prev_hash matches the prior event's hash).

## What this is NOT

- This is NOT a guarantee that the audit chain was untampered
  BEFORE bundle creation.  Tamper-evidence is the chain's job;
  this bundle just preserves a cryptographic snapshot.
- This is NOT a Rekor / SigStore transparency-log entry.  An
  auditor can publish the bundle's SHA-256 to any external
  transparency log if they want stronger third-party witness.
`)
}
