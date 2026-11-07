package audit

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
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

// This file is the evidence-bundle exporter as it was before it stopped
// holding the window in memory (#87): the events as a slice, the NDJSON
// bodies and the canonical signing input as []byte, the archive built
// from those. It is kept, unchanged apart from the ref prefix and the
// extra return value, as the oracle for the bundle FORMAT: the streaming
// exporter must produce the same bytes for the same repository, clock and
// signer (bundle_stream_internal_test.go). Do not "modernise" it; a
// difference between the two is the thing the tests exist to catch.

// refExportBundle is the pre-#87 ExportBundle. It additionally returns
// the canonical signing input.
func refExportBundle(ctx context.Context, sp storage.StoragePlugin, w io.Writer, signer EventSigner, opts ExportOptions) (*BundleResult, []byte, error) {
	if sp == nil {
		return nil, nil, errors.New("audit: ExportBundle: nil StoragePlugin")
	}
	if signer == nil {
		return nil, nil, errors.New("audit: ExportBundle: nil signer")
	}
	if w == nil {
		return nil, nil, errors.New("audit: ExportBundle: nil writer")
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
		return res, nil, fmt.Errorf("audit: ExportBundle: search events: %w", err)
	}

	var anchors []*Anchor
	if opts.IncludeAnchors {
		anchors, err = refCollectAnchors(ctx, sp, opts.Filters)
		if err != nil {
			finish()
			return res, nil, fmt.Errorf("audit: ExportBundle: collect anchors: %w", err)
		}
	}

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

	eventsBody := refEncodeEventsNDJSON(events)
	files = append(files, bundleFile{Name: "events.ndjson", Body: eventsBody})

	if opts.IncludeAnchors {
		anchorsBody := refEncodeAnchorsNDJSON(anchors)
		files = append(files, bundleFile{Name: "anchors.ndjson", Body: anchorsBody})
	}

	chainProof := refBuildChainProof(events, headPtr)
	chainProofBody, err := json.MarshalIndent(chainProof, "", "  ")
	if err != nil {
		finish()
		return res, nil, fmt.Errorf("audit: marshal chain proof: %w", err)
	}
	files = append(files, bundleFile{Name: "chain_proof.json", Body: chainProofBody})

	pubPEM, err := signer.PublicKeyPEM()
	if err != nil {
		finish()
		return res, nil, fmt.Errorf("audit: marshal public key: %w", err)
	}
	files = append(files, bundleFile{Name: "public_key.pem", Body: pubPEM})

	files = append(files, bundleFile{Name: "README.md", Body: bundleReadme()})

	manifest.SignedFiles = make([]string, 0, len(files)+1)
	for _, f := range files {
		manifest.SignedFiles = append(manifest.SignedFiles, f.Name)
	}
	manifest.SignedFiles = append(manifest.SignedFiles, "bundle.json")

	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		finish()
		return res, nil, fmt.Errorf("audit: marshal bundle manifest: %w", err)
	}
	files = append(files, bundleFile{Name: "bundle.json", Body: manifestBody})

	sigInput := canonicalBundleBytes(files)
	signature := signer.Sign(sigInput)

	files = append(files, bundleFile{Name: "signature.sig", Body: signature})

	hashed, archiveBytes, err := refBuildTarGz(w, files, now)
	if err != nil {
		finish()
		return res, nil, fmt.Errorf("audit: build tar: %w", err)
	}

	res.SHA256 = hashed
	res.BundleBytes = archiveBytes
	res.EventCount = len(events)
	res.AnchorCount = len(anchors)
	res.Manifest = manifest

	finish()
	return res, sigInput, nil
}

func refBuildTarGz(w io.Writer, files []bundleFile, now time.Time) (string, int64, error) {
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

func refEncodeEventsNDJSON(items []*Event) []byte {
	var out []byte
	for _, item := range items {
		body, _ := json.Marshal(item)
		out = append(out, body...)
		out = append(out, '\n')
	}
	return out
}

func refEncodeAnchorsNDJSON(anchors []*Anchor) []byte {
	var out []byte
	for _, a := range anchors {
		body, _ := json.Marshal(a)
		out = append(out, body...)
		out = append(out, '\n')
	}
	return out
}

func refCollectAnchors(ctx context.Context, sp storage.StoragePlugin, f ListFilters) ([]*Anchor, error) {
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
		body, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
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

func refBuildChainProof(events []*Event, head *HeadPointer) ChainProof {
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
