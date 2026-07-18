// Package bundle implements air-gapped repo-bundle export/import.
//
// A bundle is a deterministic tar archive containing:
//
//	bundle.json                    # describes scope + contents
//	manifests/<deployment>/backups/<id>/manifest.json
//	manifests/<deployment>/backups/<id>/attestation.intoto.jsonl  (if present)
//	manifests/_replicas/<id>.manifest.json                        (if present)
//	chunks/sha256/aa/bb/aabb<...>.chk                             (one per ref)
//	wal/<deployment>/<timeline>/<seg>.wal                         (if --include-wal)
//	manifests/<deployment>/timeline/<tli>.json                    (timeline files)
//
// The on-disk repo layout is preserved exactly inside the tar so
// Import is "untar onto destination repo" plus integrity checks.
//
// Why a tar (not a zip / proprietary container): tar is the
// universal Unix transport, every air-gapped network already
// passes through it, and `tar tvf` lets an operator inspect the
// bundle without any pg_hardstorage-specific tooling.  We do not
// compress the tar — chunks already carry the storage-layer
// compression posture (zstd/lz4/none) and double-compressing
// hurts more than it helps.  Operators who want compression
// pipe through gzip/zstd themselves.
//
// Idempotent imports: chunks are written via PutIfNotExists, the
// same posture as a normal backup commit.  An import that
// resumes after a partial failure is a no-op for chunks already
// present.
package bundle

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/none"
	compressionzstd "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// SchemaBundle is the on-disk schema string carried in
// bundle.json's `schema` field.  Bumped when the bundle layout
// changes incompatibly; v1 is the wire-format committed to.
const SchemaBundle = "pg_hardstorage.repobundle.v1"

// Manifest is the bundle's table-of-contents, written as
// `bundle.json` at the tar's root.  Operators inspect this file
// with `tar -xOf bundle.tar bundle.json` to see what's inside
// without unpacking everything.
type Manifest struct {
	Schema      string        `json:"schema"`
	GeneratedAt time.Time     `json:"generated_at"`
	SourceRepo  string        `json:"source_repo,omitempty"`
	Backups     []BackupEntry `json:"backups"`
	WAL         []WALSegment  `json:"wal,omitempty"`
	Timelines   []TimelineRef `json:"timelines,omitempty"`
	ChunkCount  int           `json:"chunk_count"`
	ChunkBytes  int64         `json:"chunk_bytes"`
}

// BackupEntry is one backup the bundle carries.  The chunk
// hashes are NOT inlined here (they live in the manifest the
// bundle ships); the BackupEntry is just the table-of-contents
// label.
type BackupEntry struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	Type       string `json:"type"`
	Tenant     string `json:"tenant,omitempty"`
}

// WALSegment is one WAL file the bundle carries.
type WALSegment struct {
	Deployment string `json:"deployment"`
	Timeline   string `json:"timeline"`
	Filename   string `json:"filename"`
}

// TimelineRef is one timeline-history file the bundle carries.
type TimelineRef struct {
	Deployment string `json:"deployment"`
	Timeline   string `json:"timeline"`
}

// ExportOptions tunes Export.
type ExportOptions struct {
	// Deployment scopes the export to one deployment.  Required.
	Deployment string

	// BackupID, if non-empty, exports just one backup.  Otherwise
	// every live (non-tombstoned) backup for Deployment is
	// included.
	BackupID string

	// IncludeWAL pulls every WAL segment listed in each manifest's
	// WALRequired field, plus every timeline-history file under
	// `wal/<deployment>/timelines/`.
	IncludeWAL bool

	// SourceRepoURL is recorded in the bundle's manifest so
	// auditors know where the bytes originated.  Optional.
	SourceRepoURL string

	// Verifier validates manifest signatures during the read.
	// Nil skips signature checks (useful for forensics on a
	// repo whose signing key is unavailable).
	Verifier *backup.Verifier
}

// Export streams a bundle covering opts.Deployment from sp into
// w.  Returns the bundle Manifest as written, after the tar's
// final padding has flushed.
func Export(ctx context.Context, sp storage.StoragePlugin, w io.Writer, opts ExportOptions) (*Manifest, error) {
	if opts.Deployment == "" {
		return nil, errors.New("bundle: ExportOptions.Deployment is required")
	}
	tw := tar.NewWriter(w)
	defer tw.Close()

	// Step 1 — collect every manifest in scope.  We read the raw
	// bytes via Storage.Get + ParseAttestationless rather than
	// ManifestStore.{Read,List}, because:
	//
	//   1. The bundle is the right surface for forensics on a
	//      repo whose signing key is unavailable — refusing to
	//      export an unsigned manifest defeats the use case.
	//   2. Verification is the destination's job: an Import
	//      that wants strict signing reads each manifest with a
	//      Verifier-bearing ParseAndVerify after ingest.
	//
	// When opts.Verifier is non-nil we still verify on the
	// source side (the operator's "I want a verified bundle"
	// posture).
	var manifests []*backup.Manifest
	if opts.BackupID != "" {
		m, err := readBundleManifest(ctx, sp, opts.Deployment, opts.BackupID, opts.Verifier)
		if err != nil {
			return nil, fmt.Errorf("bundle: read manifest %s/%s: %w", opts.Deployment, opts.BackupID, err)
		}
		manifests = append(manifests, m)
	} else {
		ids, err := listManifestIDs(ctx, sp, opts.Deployment)
		if err != nil {
			return nil, fmt.Errorf("bundle: list deployment %s: %w", opts.Deployment, err)
		}
		for _, id := range ids {
			m, err := readBundleManifest(ctx, sp, opts.Deployment, id, opts.Verifier)
			if err != nil {
				return nil, fmt.Errorf("bundle: read %s/%s: %w", opts.Deployment, id, err)
			}
			manifests = append(manifests, m)
		}
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("bundle: no manifests for deployment %s (BackupID=%q)", opts.Deployment, opts.BackupID)
	}

	bundleManifest := &Manifest{
		Schema:      SchemaBundle,
		GeneratedAt: time.Now().UTC(),
		SourceRepo:  opts.SourceRepoURL,
	}

	chunkSeen := map[repo.Hash]struct{}{}
	for _, m := range manifests {
		bundleManifest.Backups = append(bundleManifest.Backups, BackupEntry{
			Deployment: m.Deployment,
			BackupID:   m.BackupID,
			Type:       string(m.Type),
			Tenant:     m.Tenant,
		})

		// Step 2 — write the manifest itself.  We re-marshal from
		// the in-memory struct (canonical) so the bundle has
		// reproducible bytes regardless of how the source repo
		// stored them.
		mPath := backup.PrimaryPath(m.Deployment, m.BackupID)
		if err := writeManifestEntry(tw, mPath, m); err != nil {
			return nil, err
		}
		// Replica copy (best-effort: missing replica is not an
		// error, the source repo may have skipped manifest
		// redundancy at commit time).
		replicaKey := backup.ReplicaPath(m.BackupID)
		if err := copyKey(ctx, sp, tw, replicaKey, true); err != nil {
			return nil, fmt.Errorf("bundle: copy replica %s: %w", replicaKey, err)
		}
		// Attestation if present.
		attestKey := "manifests/" + m.Deployment + "/backups/" + m.BackupID + "/attestation.intoto.jsonl"
		if err := copyKey(ctx, sp, tw, attestKey, true); err != nil {
			return nil, fmt.Errorf("bundle: copy attestation %s: %w", attestKey, err)
		}

		// Step 3 — every chunk this manifest references.
		for _, fe := range m.Files {
			for _, ref := range fe.Chunks {
				if _, dup := chunkSeen[ref.Hash]; dup {
					continue
				}
				chunkSeen[ref.Hash] = struct{}{}
				key := repo.ChunkKey(ref.Hash)
				size, err := copyChunk(ctx, sp, tw, key)
				if err != nil {
					return nil, fmt.Errorf("bundle: copy chunk %s: %w", key, err)
				}
				bundleManifest.ChunkCount++
				bundleManifest.ChunkBytes += size
			}
		}

		// Step 4 — WAL files when requested.  WAL layout:
		//   wal/<deployment>/<timeline>/<segment-prefix>/<seg>.wal
		// We use the manifest's Timeline as the directory; the
		// in-tree convention shards segments by their first 8
		// bytes for readability but the bundle keeps the same
		// path so an Import doesn't need to reshape.
		if opts.IncludeWAL {
			for _, seg := range m.WALRequired {
				key := walSegmentKey(m.Deployment, m.Timeline, seg)
				if err := copyKey(ctx, sp, tw, key, false); err != nil {
					return nil, fmt.Errorf("bundle: copy WAL %s: %w", key, err)
				}
				bundleManifest.WAL = append(bundleManifest.WAL, WALSegment{
					Deployment: m.Deployment,
					Timeline:   fmt.Sprintf("%d", m.Timeline),
					Filename:   seg,
				})
			}
		}
	}

	// Step 5 — bundle.json at the tar root.  Sort embedded slices
	// so the bundle is reproducible across runs over the same
	// repo state.
	sort.Slice(bundleManifest.Backups, func(i, j int) bool {
		if bundleManifest.Backups[i].Deployment == bundleManifest.Backups[j].Deployment {
			return bundleManifest.Backups[i].BackupID < bundleManifest.Backups[j].BackupID
		}
		return bundleManifest.Backups[i].Deployment < bundleManifest.Backups[j].Deployment
	})
	sort.Slice(bundleManifest.WAL, func(i, j int) bool {
		return bundleManifest.WAL[i].Filename < bundleManifest.WAL[j].Filename
	})
	body, err := json.MarshalIndent(bundleManifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bundle: marshal bundle.json: %w", err)
	}
	if err := writeTarEntry(tw, "bundle.json", body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("bundle: tar.Close: %w", err)
	}
	return bundleManifest, nil
}

// ImportOptions tunes Import.
type ImportOptions struct {
	// Verifier validates manifest signatures during ingest.  Nil
	// skips verification — useful for ingesting forensic
	// bundles whose source signing key isn't available.
	Verifier *backup.Verifier

	// MaxEntries / MaxTotalBytes bound a whole import. MaxEntryBytes
	// already caps each entry, but without a TOTAL cap a bundle with
	// millions of tiny entries (or a huge aggregate) could exhaust
	// disk / work during import (input-validation audit #4). Zero or
	// negative selects the package default.
	MaxEntries    int
	MaxTotalBytes int64
}

// DefaultMaxBundleEntries / DefaultMaxBundleBytes are generous sanity
// ceilings on a whole import — high enough for any real repo bundle (a
// 10 TB backup at the 256 KiB chunk size is ~40M chunks), finite enough
// to refuse a maliciously unbounded bundle.
const (
	DefaultMaxBundleEntries = 50_000_000
	DefaultMaxBundleBytes   = 64 << 40 // 64 TiB
)

// Import reads a bundle from r and writes it into sp.  Idempotent:
// chunks already present are skipped via PutIfNotExists, manifests
// already present are tolerated (Commit returns ErrExists which is
// downgraded to a no-op here).  Returns the bundle's Manifest.
func Import(ctx context.Context, r io.Reader, sp storage.StoragePlugin, opts ImportOptions) (*Manifest, error) {
	mutationLock, err := repo.AcquireMutationLock(ctx, sp, "repository bundle import")
	if err != nil {
		return nil, fmt.Errorf("bundle: mutation lock: %w", err)
	}
	defer func() { _ = mutationLock.Release(context.Background()) }()
	tr := tar.NewReader(r)

	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxBundleEntries
	}
	maxTotalBytes := opts.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxBundleBytes
	}

	// We process the tar in a single pass.  The bundle.json may
	// arrive at the end (writer order), so we accumulate
	// manifest paths + chunk paths + WAL paths and apply them as
	// we go.  bundle.json itself is consumed and parsed into the
	// returned value.
	var bm Manifest
	var entryCount int
	var totalBytes int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: tar.Next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Whole-bundle caps: MaxEntryBytes below bounds each entry, but
		// without a total cap a bundle of millions of tiny entries (or a
		// huge aggregate) exhausts disk / work (input-validation audit
		// #4). hdr.Size is the declared upper bound; the per-entry read
		// is independently capped at MaxEntryBytes.
		entryCount++
		if entryCount > maxEntries {
			return nil, fmt.Errorf("bundle: entry count exceeds the limit (%d) — refusing a bundle that could exhaust resources", maxEntries)
		}
		if hdr.Size > 0 {
			totalBytes += hdr.Size
			if totalBytes > maxTotalBytes {
				return nil, fmt.Errorf("bundle: total size exceeds the limit (%d bytes) — refusing an oversized bundle", maxTotalBytes)
			}
		}

		// Path-traversal defence (audit).  The tar's
		// hdr.Name comes from a potentially-untrusted bundle
		// (forensic transports cross trust boundaries).
		// Reject anything that doesn't path.Clean to itself
		// or that escapes via "..".  Without this gate, a
		// malicious bundle could write to
		// `../../etc/something` via storage backends that
		// don't independently sandbox keys.
		clean, ok := pathClean(hdr.Name)
		if !ok {
			return nil, fmt.Errorf("bundle: rejected entry name %q (path traversal)", hdr.Name)
		}

		// Size sanity (audit).  A malicious tar can
		// declare a 10 TB chunk and OOM the importer via
		// io.ReadAll.  Cap to MaxEntryBytes (256 MiB —
		// 4× the FastCDC max chunk size; one MaxFile from
		// our own writers will always fit, anything bigger
		// is suspect).
		if hdr.Size > MaxEntryBytes {
			return nil, fmt.Errorf("bundle: entry %q size %d exceeds MaxEntryBytes (%d)",
				clean, hdr.Size, MaxEntryBytes)
		}

		switch {
		case clean == "bundle.json":
			if err := json.NewDecoder(tr).Decode(&bm); err != nil {
				return nil, fmt.Errorf("bundle: decode bundle.json: %w", err)
			}
			if bm.Schema != SchemaBundle {
				return nil, fmt.Errorf("bundle: unsupported bundle schema %q (want %q)", bm.Schema, SchemaBundle)
			}
		case strings.HasPrefix(clean, "chunks/"):
			// Chunk: content-addressed write.  Before trusting the
			// bundle we MUST confirm the payload's SHA-256 matches
			// the hash embedded in the key — otherwise a corrupt or
			// malicious bundle could plant a chunk whose bytes don't
			// match its address, silently poisoning the CAS (every
			// future reader that trusts the key would serve wrong
			// content).  Read the entry fully, verify, then
			// PutIfNotExists (idempotent re-import for free).
			body, err := io.ReadAll(io.LimitReader(tr, MaxEntryBytes+1))
			if err != nil {
				return nil, fmt.Errorf("bundle: read chunk %s: %w", clean, err)
			}
			if err := verifyChunkPayload(clean, body); err != nil {
				return nil, err
			}
			if err := putIfNotExists(ctx, sp, clean, strings_NewReader(string(body))); err != nil {
				return nil, fmt.Errorf("bundle: write chunk %s: %w", clean, err)
			}
		case strings.HasPrefix(clean, "manifests/") || strings.HasPrefix(clean, "wal/"):
			// Manifest, replica, attestation, WAL file, or
			// timeline file.  All conditional-write keys.
			//
			// When the caller supplies a Verifier (forensic import of an
			// untrusted bundle), signed backup manifests
			// (manifests/<dep>/backups/<id>/manifest.json) MUST validate
			// against it before ingest. Previously opts.Verifier was
			// accepted but never consulted here, so a forged manifest
			// was imported regardless — silently violating the
			// verify-on-ingest contract. (Restore re-verifies, so a
			// forged manifest can't be restored, but it should never
			// land in the repo when the operator asked us to check.)
			// Replica/attestation/WAL entries are not ed25519 backup
			// manifests and stream as-is.
			if opts.Verifier != nil && isSignedBackupManifestKey(clean) {
				body, err := io.ReadAll(io.LimitReader(tr, MaxEntryBytes+1))
				if err != nil {
					return nil, fmt.Errorf("bundle: read %s: %w", clean, err)
				}
				if _, verr := backup.ParseAndVerify(body, opts.Verifier); verr != nil {
					return nil, fmt.Errorf("bundle: reject %s: manifest signature verification failed: %w", clean, verr)
				}
				if err := putIfNotExists(ctx, sp, clean, strings_NewReader(string(body))); err != nil {
					return nil, fmt.Errorf("bundle: write %s: %w", clean, err)
				}
			} else if err := putIfNotExists(ctx, sp, clean, tr); err != nil {
				return nil, fmt.Errorf("bundle: write %s: %w", clean, err)
			}
		default:
			// Unknown top-level: skip.  Future bundle versions
			// may carry extra files; tolerating them keeps
			// older binaries forward-compatible.
		}
	}
	if bm.Schema == "" {
		return nil, errors.New("bundle: archive did not contain bundle.json")
	}
	return &bm, nil
}

// isSignedBackupManifestKey reports whether key names a primary backup
// manifest (manifests/<dep>/backups/<id>/manifest.json) — the only
// bundle entry that carries an ed25519 signature Import can verify.
// Replica copies live at manifests/_replicas/<id>.manifest.json (suffix
// ".manifest.json", not "/manifest.json") and are excluded, as are
// attestation (.intoto.jsonl) and WAL-segment manifests.
func isSignedBackupManifestKey(key string) bool {
	return strings.HasPrefix(key, "manifests/") &&
		strings.HasSuffix(key, "/manifest.json")
}

// MaxEntryBytes caps the size of any single tar entry the
// importer will accept.  256 MiB is well above the FastCDC
// max chunk size (64 KiB by default; 256 KiB worst-case
// after page-aligned splits on a heavily-fragmented file),
// so a legitimate bundle's largest entry sits comfortably
// below this ceiling.  Anything bigger is either a misconfig
// or a DoS attempt.
const MaxEntryBytes = 256 << 20

// --- internal helpers --------------------------------------------------

// writeManifestEntry serialises m through the package's canonical
// JSON form (omitting attestation; the source repo's
// attestation.intoto.jsonl is bundled separately under the same
// directory).  Using the in-memory struct keeps bundle bytes
// reproducible regardless of source-repo metadata noise.
func writeManifestEntry(tw *tar.Writer, name string, m *backup.Manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("bundle: marshal manifest: %w", err)
	}
	return writeTarEntry(tw, name, body)
}

func writeTarEntry(tw *tar.Writer, name string, body []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(body)),
		ModTime: time.Time{}, // zero for reproducibility
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("bundle: tar header for %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("bundle: tar write %s: %w", name, err)
	}
	return nil
}

// copyKey reads the source object at key (if present) and writes
// it as a tar entry under the same name.  When skipMissing is
// true, an absent source key is a no-op; otherwise it's an
// error.
func copyKey(ctx context.Context, sp storage.StoragePlugin, tw *tar.Writer, key string, skipMissing bool) error {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		if skipMissing && isNotFound(err) {
			return nil
		}
		return err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return writeTarEntry(tw, key, body)
}

// copyChunk is copyKey for chunks, returning the chunk's byte
// length so the bundle.json's chunk_bytes field is accurate.
func copyChunk(ctx context.Context, sp storage.StoragePlugin, tw *tar.Writer, key string) (int64, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return 0, err
	}
	if err := writeTarEntry(tw, key, body); err != nil {
		return 0, err
	}
	return int64(len(body)), nil
}

// putIfNotExists is the idempotent write the import path uses.
// For Storage implementations that don't expose IfNotExists at
// the API level, we Stat first and skip on hit.  Tolerates a
// race where the same key is written twice concurrently —
// chunks are content-addressed so a duplicate write is safe.
func putIfNotExists(ctx context.Context, sp storage.StoragePlugin, key string, r io.Reader) error {
	if _, err := sp.Stat(ctx, key); err == nil {
		// Already present — drain the reader and skip.
		_, _ = io.Copy(io.Discard, r)
		return nil
	} else if !isNotFound(err) {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = sp.Put(ctx, key, strings_NewReader(string(body)), storage.PutOptions{IfNotExists: true})
	if isAlreadyExists(err) {
		return nil
	}
	return err
}

// isNotFound checks the Storage plugin's "key absent" error
// shape.  Plugins return storage.ErrNotFound; callers check by
// errors.Is.
func isNotFound(err error) bool {
	return err != nil && errors.Is(err, storage.ErrNotFound)
}

// isAlreadyExists is the inverse: Put with IfNotExists=true
// returns ErrAlreadyExists when the key was raced.
func isAlreadyExists(err error) bool {
	return err != nil && errors.Is(err, storage.ErrAlreadyExists)
}

// strings_NewReader wraps strings.NewReader without importing
// strings here (we only need it inline for putIfNotExists).
// The deliberate underscore avoids shadowing the package import
// when callers add `import "strings"`.
func strings_NewReader(s string) io.Reader {
	return strings.NewReader(s)
}

// pathClean normalises a tar path before writing — protects the
// import side from `..` traversal attempts in a malicious
// bundle.  Reject anything that doesn't path.Clean to itself
// (relative, no leading slash, no parent traversal).
func pathClean(p string) (string, bool) {
	cleaned := path.Clean(p)
	if cleaned != p || strings.HasPrefix(cleaned, "..") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

// verifyChunkPayload confirms that body's SHA-256 matches the
// content-address embedded in a chunk key (chunks/sha256/aa/bb/<hex>.chk).
// The CAS is content-addressed: the key IS the hash of the bytes, so a
// bundle that ships a chunk whose payload doesn't hash to its key is
// corrupt or forged and must be rejected before it lands in the repo —
// otherwise every later reader that trusts the key serves wrong content.
func verifyChunkPayload(key string, body []byte) error {
	want, err := repo.ParseChunkKey(key)
	if err != nil {
		return fmt.Errorf("bundle: reject chunk %s: not a valid chunk key: %w", key, err)
	}
	algo, enc, payload, err := compression.ReadEnvelope(body)
	if err != nil {
		return fmt.Errorf("bundle: reject chunk %s: invalid storage envelope: %w", key, err)
	}
	// The key addresses plaintext, while bundle entries preserve the raw
	// compression/encryption envelope. Encrypted payloads cannot be checked
	// against the plaintext address without the manifest's KEK; AES-GCM will
	// authenticate them on restore. We can still reject malformed envelopes.
	if enc.IsEncrypted() {
		return nil
	}
	var plaintext []byte
	switch algo {
	case compression.AlgoNone:
		plaintext, err = (none.Compressor{}).Decompress(payload)
	case compression.AlgoZstd:
		plaintext, err = compressionzstd.NewDefault().Decompress(payload)
	default:
		err = fmt.Errorf("unsupported compression algorithm %d", algo)
	}
	if err != nil {
		return fmt.Errorf("bundle: reject chunk %s: decode storage envelope: %w", key, err)
	}
	got := repo.HashOf(plaintext)
	if got != want {
		return fmt.Errorf("bundle: reject chunk %s: payload SHA-256 %s does not match key hash %s",
			key, got.String(), want.String())
	}
	return nil
}

// walSegmentKey builds the repo-relative storage key of a WAL
// segment under the agent's standard layout.  The segment name
// itself encodes timeline+LSN, so we don't subdivide further.
func walSegmentKey(deployment string, timeline uint32, segName string) string {
	// WAL segment manifests live at wal/<dep>/<TIMELINE-8-hex>/<seg>.json
	// (see internal/pg/walsink.SegmentPath).  The timeline is an
	// 8-digit uppercase-hex directory and the file carries a .json
	// suffix; a decimal timeline with no suffix pointed at a key that
	// never existed, breaking bundle WAL export/import.  segName may
	// already carry .json (defensive) — strip it before re-adding.
	segName = strings.TrimSuffix(segName, ".json")
	return fmt.Sprintf("wal/%s/%08X/%s.json", deployment, timeline, segName)
}

// readBundleManifest reads + parses one manifest, optionally
// verifying its signature.  Distinct from ManifestStore.Read so
// the bundle can operate on unsigned forensic repos.
func readBundleManifest(ctx context.Context, sp storage.StoragePlugin, deployment, backupID string, verifier *backup.Verifier) (*backup.Manifest, error) {
	key := backup.PrimaryPath(deployment, backupID)
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if verifier != nil {
		return backup.ParseAndVerify(body, verifier)
	}
	return backup.ParseAttestationless(body)
}

// listManifestIDs enumerates the live (non-tombstoned) backup
// IDs for a deployment without going through ManifestStore (so
// unsigned manifests are visible to the bundle scan).
func listManifestIDs(ctx context.Context, sp storage.StoragePlugin, deployment string) ([]string, error) {
	prefix := "manifests/" + deployment + "/backups/"
	const manifestSuffix = "/manifest.json"
	const tombstoneSuffix = "/manifest.json.tombstone"

	var ids []string
	tombstoned := map[string]struct{}{}
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasSuffix(info.Key, tombstoneSuffix):
			rel := strings.TrimPrefix(info.Key, prefix)
			if slash := strings.IndexByte(rel, '/'); slash > 0 {
				tombstoned[rel[:slash]] = struct{}{}
			}
		case strings.HasSuffix(info.Key, manifestSuffix):
			rel := strings.TrimPrefix(info.Key, prefix)
			if slash := strings.IndexByte(rel, '/'); slash > 0 {
				ids = append(ids, rel[:slash])
			}
		}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dead := tombstoned[id]; !dead {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}
