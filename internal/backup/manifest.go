// manifest.go — backup-manifest schema, deterministic canonicalisation,
// signing/verification, and shape-level Validate.

package backup

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Schema is the wire-format identifier carried on every committed
// Manifest. We commit to 24-month backward compatibility, same as the
// output schema and the repo schema.
const Schema = "pg_hardstorage.manifest.v1"

// BackupType discriminates the strategy that produced a manifest. The
// chunk content is identical across types (CAS dedup is type-blind);
// the type drives only restore-time orchestration.
type BackupType string

const (
	// BackupTypeFull is a complete BASE_BACKUP capture of the
	// cluster. Self-contained: restores without referencing any
	// parent manifest.
	BackupTypeFull BackupType = "full"

	// BackupTypeIncremental is a PG 17+ INCREMENTAL BASE_BACKUP
	// captured relative to ParentBackupID. Restores flatten the
	// chain back to a full before replaying.
	BackupTypeIncremental BackupType = "incremental_lsn"

	// BackupTypeSnapshot is a filesystem-snapshot-anchored backup
	// (LVM, ZFS, cloud-volume). The manifest still carries chunk
	// refs into the CAS; the type marks the capture mode for
	// audit and restore orchestration only.
	BackupTypeSnapshot BackupType = "snapshot"
)

// Manifest is the single-source-of-truth document describing a backup.
//
// Marshal order is the order fields are declared here — encoding/json
// is deterministic for structs, which lets us treat the canonical bytes
// (Manifest with Attestation == nil) as the signed payload.
//
// IMPORTANT: do not introduce any map[K]V fields. Map iteration is not
// deterministic, which would break the "round-trip Marshal-Unmarshal-
// Marshal yields identical bytes" property that signature verification
// depends on.
type Manifest struct {
	Schema           string          `json:"schema"`
	BackupID         string          `json:"backup_id"`
	Deployment       string          `json:"deployment"`
	Tenant           string          `json:"tenant,omitempty"`
	Type             BackupType      `json:"type"`
	ParentBackupID   string          `json:"parent_backup_id,omitempty"`
	PGVersion        int             `json:"pg_version"`
	SystemIdentifier string          `json:"system_identifier"`
	StartLSN         string          `json:"start_lsn"`
	StopLSN          string          `json:"stop_lsn"`
	Timeline         uint32          `json:"timeline"`
	StartedAt        time.Time       `json:"started_at"`
	StoppedAt        time.Time       `json:"stopped_at"`
	Compression      string          `json:"compression,omitempty"`
	Encryption       *EncryptionInfo `json:"encryption,omitempty"`
	// SourceTDE captures Transparent Data Encryption posture on
	// the source PG at backup time.  Nil means the deployment
	// was not declared TDE; the strict "look at PG byte layout"
	// invariants applied.  Non-nil means the operator declared
	// TDE in deployment config (config.TDEConfig.Enabled) — the
	// backup ran with relaxed-inspection semantics and the
	// restored bytes are opaque ciphertext until a TDE-capable
	// target PG decrypts them.  See docs/explanation/tde-
	// awareness.md.
	SourceTDE   *SourceTDEInfo `json:"source_tde,omitempty"`
	Tablespaces []Tablespace   `json:"tablespaces,omitempty"`
	Files       []FileEntry    `json:"files"`
	// Dirs are PGDATA subdirectories that PG sends as tar
	// TypeDir entries — pg_wal/, pg_dynshmem/, pg_notify/,
	// pg_replslot/, pg_serial/, pg_snapshots/, pg_stat/,
	// pg_stat_tmp/, pg_subtrans/, pg_tblspc/, pg_twophase/,
	// and any other empty directories the source datadir
	// has.  Pre-fix manifests omit this field; the restore
	// path handles a nil value by falling back to the
	// MkdirAll-from-file-parents behaviour (which is the
	// regression that prompted this field's introduction —
	// without it, empty dirs like pg_wal/ never get created
	// and PG refuses to start on the restored datadir).
	Dirs        []DirEntry `json:"dirs,omitempty"`
	WALRequired []string   `json:"wal_required,omitempty"`

	// BackupLabel is the verbatim contents of PG's backup_label file
	// extracted from the first tablespace's tar. Restore writes it
	// back to the data directory's root so PG recognises the
	// directory as a restored backup. Inline (not a chunk reference)
	// because it's small, per-backup-unique, and we want it visible
	// in human inspection of the manifest.
	BackupLabel string `json:"backup_label,omitempty"`

	// TablespaceMap is the verbatim contents of PG's tablespace_map
	// file. Empty when the cluster has only the default tablespace —
	// PG omits the file in that case.
	TablespaceMap string `json:"tablespace_map,omitempty"`

	// PGBackupManifest is the verbatim bytes of PG's own
	// backup_manifest blob (the JSON file pg_basebackup writes at
	// the data-dir root, captured via BASE_BACKUP MANIFEST 'yes').
	// Encoded as base64 in JSON. Required for PG 17+ incremental
	// backups: a child incremental's BASE_BACKUP INCREMENTAL needs
	// the parent's PG backup_manifest content as input.
	//
	// Empty for older backups (the field landed when incremental
	// support was wired). When empty, this manifest cannot anchor
	// an incremental child; operators wanting incrementals after
	// this field landed take a fresh full first.
	PGBackupManifest []byte `json:"pg_backup_manifest,omitempty"`

	// WALGaps records every Patroni-failover WAL gap that the
	// agent's leader-follow Coordinator detected on or before
	// this backup's commit. Embedded directly on the manifest
	// (in addition to the live wal/<deployment>/gaps/ state)
	// so:
	//
	//   - Restore can refuse PITR within the gap window even
	//     when gapstate has been GC'd / wiped / unreachable.
	//   - The manifest is signed; the gap record cannot be
	//     tampered with after commit.
	//   - Cross-region replicas of the manifest carry the gap
	//     metadata for free.
	//
	// Plan: "the manifest of any new backup taken after this
	// point notes the gap so restores within the window are
	// explicitly refused with a clear error."
	//
	// Empty for backups taken on a clean deployment (no gaps
	// detected) or on a older agent (the field is
	// additive). Restore consults BOTH this field and live
	// gapstate; either source can refuse.
	WALGaps []WALGap `json:"wal_gaps,omitempty"`

	// Attestation is set after Sign and zeroed during canonicalisation.
	Attestation *Attestation `json:"attestation,omitempty"`
}

// WALGap is the manifest-embedded form of a leader-follow
// Coordinator's gapstate.Record. Same shape, signed-via-the-
// manifest's-attestation; restore consults this list in
// addition to the live wal/<deployment>/gaps/ records.
//
// Field set is a strict subset of gapstate.Record's. The two
// types are kept separate so the live gap-state package
// doesn't have to import the backup-manifest package (avoids
// a layering inversion: manifest is the higher-level
// concept, gapstate is the lower).
type WALGap struct {
	SlotName    string    `json:"slot_name"`
	SlotRole    string    `json:"slot_role,omitempty"`
	Timeline    uint32    `json:"timeline"`
	GapStartLSN string    `json:"gap_start_lsn"`
	GapEndLSN   string    `json:"gap_end_lsn"`
	GapBytes    uint64    `json:"gap_bytes"`
	DetectedAt  time.Time `json:"detected_at"`
}

// EncryptionInfo describes how chunks are encrypted on the storage
// backend. Nil/zero means plaintext; populated values come from the
// envelope-encryption layer (Slice "compliance").
type EncryptionInfo struct {
	Scheme          string `json:"scheme"`
	KEKRef          string `json:"kek_ref"`
	WrappedDEK      string `json:"wrapped_dek"`
	EnvelopeVersion int    `json:"envelope_version"`
}

// SourceTDEInfo records that the source PostgreSQL had Transparent
// Data Encryption enabled at backup time.  See config.TDEConfig
// for the operator-facing declaration and
// docs/explanation/tde-awareness.md for the operational story.
//
// Orthogonal to EncryptionInfo: a backup can be source-TDE-on +
// repo-plaintext (TDE-passthrough), source-TDE-on + repo-encrypted
// (defence in depth), source-TDE-off + repo-encrypted (envelope
// only), or both-off.  Repo-side envelope encryption is in
// Manifest.Encryption; source-side TDE is in Manifest.SourceTDE.
//
// Consulted at restore time to:
//   - Refuse a vanilla-PG restore target loudly (the restored data
//     dir would have ciphertext where vanilla PG expects plaintext).
//   - Skip pg_verifybackup against the chain-flattened datadir
//     when meaningful: PG's own checksums are over plaintext, but
//     the restored bytes are ciphertext until the target's TDE
//     engine reads them.  Run inside a TDE-capable sandbox or
//     skip cleanly.
type SourceTDEInfo struct {
	// Engine is the operator-declared TDE implementation name,
	// or "unspecified" when the deployment was declared TDE
	// without an engine label.  Free-form; pg_hardstorage never
	// branches on its value.
	Engine string `json:"engine"`

	// KeyRef is the operator-supplied key-set reference (opaque).
	// Empty when the operator did not declare one.
	KeyRef string `json:"key_ref,omitempty"`
}

// Tablespace records a non-default PostgreSQL tablespace and where it
// was mounted at backup time. Restore consults this to remap.
type Tablespace struct {
	OID      uint32 `json:"oid"`
	Location string `json:"location"`
}

// FileEntry is one file in PGDATA captured by the backup. The Chunks
// slice is the ordered list of chunk references whose concatenation
// reconstitutes the file's bytes.
//
// TablespaceOID records which tablespace the file belongs to. Zero
// (the default, and the zero value so pre-fix manifests read cleanly)
// means the DEFAULT tablespace — Path is relative to PGDATA root and
// restore materialises it under the target data directory. A non-zero
// OID means Path is relative to that non-default tablespace's ROOT
// (the tar PG streams for a user tablespace names its entries relative
// to the tablespace root, NOT PGDATA); restore must materialise the
// file under the tablespace's real location (from tablespace_map, or
// the operator's --tablespace-mapping remap) rather than under the
// data directory. Without this association every tablespace's tar
// entries flattened into one list and landed under PGDATA root while
// tablespace_map pointed at an empty dir — a silently-corrupt restore.
type FileEntry struct {
	Path          string     `json:"path"`
	Size          int64      `json:"size"`
	Mode          uint32     `json:"mode,omitempty"`
	ModTime       time.Time  `json:"mod_time,omitempty"`
	TablespaceOID uint32     `json:"tablespace_oid,omitempty"`
	Chunks        []ChunkRef `json:"chunks"`
}

// DirEntry is one directory PG sent as a tar TypeDir entry in
// the BASE_BACKUP stream.  Restore re-creates each one with
// MkdirAll(Path, Mode).  Capturing dirs explicitly (rather
// than relying on materialise-time MkdirAll-from-file-parent)
// is what makes EMPTY directories — pg_wal/, pg_dynshmem/,
// pg_notify/, pg_replslot/, pg_serial/, pg_snapshots/,
// pg_stat/, pg_stat_tmp/, pg_subtrans/, pg_tblspc/,
// pg_twophase/ — survive the round-trip.  Without these
// directories present, PG refuses to start on the restored
// datadir.
type DirEntry struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode,omitempty"`
	// TablespaceOID mirrors FileEntry.TablespaceOID: zero means the
	// directory lives under PGDATA root; a non-zero OID means Path is
	// relative to that non-default tablespace's root and restore
	// re-creates it under the tablespace's real location.
	TablespaceOID uint32 `json:"tablespace_oid,omitempty"`
}

// ChunkRef points at one chunk in the CAS. Offset is the chunk's first
// byte's position within FileEntry; Len is the chunk's byte length
// (matches the CAS-stored size for unencrypted chunks).
type ChunkRef struct {
	Hash   repo.Hash `json:"hash"`
	Offset int64     `json:"offset"`
	Len    int64     `json:"len"`
}

// Attestation is the manifest's signature block. Self-contained: the
// public key is embedded so a reader holding the manifest can verify
// without a separate keystore (verifying-against-trusted-key is a
// separate, layered concern).
type Attestation struct {
	Scheme    string `json:"scheme"`
	PublicKey string `json:"public_key"` // PEM, base64-shaped by encoding/pem
	Signature string `json:"signature"`  // base64 of raw signature bytes
}

// Canonicalize returns the manifest's signing bytes: a deterministic
// JSON marshal of the manifest with Attestation == nil. The signer
// signs these bytes; the verifier reconstructs them by parsing the
// on-disk file, zeroing Attestation, and re-marshalling.
//
// Determinism is guaranteed because:
//   - encoding/json emits struct fields in declaration order;
//   - we use no map fields anywhere in the schema;
//   - SetEscapeHTML is off so < / > / & are preserved verbatim;
//   - no pretty-printing whitespace is added.
func (m *Manifest) Canonicalize() ([]byte, error) {
	cp := *m
	cp.Attestation = nil
	// json.Encoder lets us turn off HTML escaping; the trailing newline
	// it adds is stripped so callers get the exact bytes that were
	// hashed.
	var buf canonicalBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&cp); err != nil {
		return nil, fmt.Errorf("manifest: canonicalize: %w", err)
	}
	return buf.TrimTrailingNewline(), nil
}

// MarshalToBytes returns the on-disk form: the same canonical encoding
// (so signer and on-disk bytes minus the attestation are bit-identical).
//
// We deliberately skip indentation: the manifest is a machine document.
// Humans wanting to read it pipe through `jq`.
func (m *Manifest) MarshalToBytes() ([]byte, error) {
	var buf canonicalBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("manifest: marshal: %w", err)
	}
	return buf.TrimTrailingNewline(), nil
}

// canonicalBuffer is a thin []byte wrapper exposing the trailing-newline
// strip that json.Encoder.Encode appends. Using bytes.Buffer + Bytes()
// would also work; this keeps the intent obvious.
type canonicalBuffer []byte

// Write appends p to the buffer. Implements io.Writer so json.Encoder
// can target a canonicalBuffer directly.
func (b *canonicalBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// TrimTrailingNewline returns the buffer contents with the single
// trailing '\n' that json.Encoder.Encode appends stripped off. The
// returned slice aliases the buffer's backing array.
func (b *canonicalBuffer) TrimTrailingNewline() []byte {
	out := []byte(*b)
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}

// Sign canonicalises the manifest, signs the canonical bytes with
// signer, and stores the resulting Attestation in m. The attestation
// embeds the signer's public key so the manifest is self-verifying.
func (m *Manifest) Sign(signer *Signer) error {
	if signer == nil {
		return errors.New("manifest: nil signer")
	}
	canonical, err := m.Canonicalize()
	if err != nil {
		return err
	}
	pubPEM, err := signer.PublicKeyPEM()
	if err != nil {
		return err
	}
	sig := signer.Sign(canonical)
	m.Attestation = &Attestation{
		Scheme:    SchemeEd25519,
		PublicKey: string(pubPEM),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
	return nil
}

// ParseAttestationless parses on-disk bytes WITHOUT signature
// verification. Used by `repair attestation` and `repair manifest`
// — paths whose entire purpose is to deal with a body whose
// signature is broken or wrong-keypair-bound. Production read paths
// (Read / List on the ManifestStore) must continue to go through
// ParseAndVerify; this is the explicit "I know I'm bypassing the
// signature check" surface.
func ParseAttestationless(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("manifest: schema %q is not supported; expected %q",
			m.Schema, Schema)
	}
	return &m, nil
}

// ParseAndVerify parses on-disk bytes, verifies the signature against
// the supplied verifier, and returns the parsed manifest.
//
// The verifier's public key MUST equal the public key embedded in the
// Attestation; otherwise ErrPublicKeyMismatch is returned. This catches
// the case where a manifest is signed by a key the caller hasn't
// pre-trusted — a genuine signature, but not by the right party.
func ParseAndVerify(raw []byte, verifier *Verifier) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("manifest: schema %q is not supported; expected %q", m.Schema, Schema)
	}
	if m.Attestation == nil {
		return nil, ErrUnsigned
	}
	if m.Attestation.Scheme != SchemeEd25519 {
		return nil, fmt.Errorf("manifest: unsupported signature scheme %q", m.Attestation.Scheme)
	}
	if verifier == nil {
		return nil, errors.New("manifest: nil verifier")
	}

	// The attestation's embedded public key must match the verifier's
	// public key. Compare canonical PEM bytes.
	if err := matchPublicKeys(m.Attestation.PublicKey, verifier); err != nil {
		return nil, err
	}

	sig, err := base64.StdEncoding.DecodeString(m.Attestation.Signature)
	if err != nil {
		return nil, fmt.Errorf("manifest: decode signature: %w", err)
	}

	canonical, err := m.Canonicalize()
	if err != nil {
		return nil, err
	}
	if err := verifier.Verify(canonical, sig); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", m.BackupID, err)
	}
	return &m, nil
}

// matchPublicKeys returns nil when the embedded PEM matches the
// verifier's known public key. We round-trip both sides through the
// same loader to compare against parsed-then-re-emitted PEM, so trivial
// formatting differences (whitespace, header ordering) don't matter.
func matchPublicKeys(embeddedPEM string, verifier *Verifier) error {
	emb, err := LoadVerifier([]byte(embeddedPEM))
	if err != nil {
		return fmt.Errorf("manifest: parse embedded public key: %w", err)
	}
	if !emb.publicKeyEquals(verifier) {
		return ErrPublicKeyMismatch
	}
	return nil
}

// VerifyEmbedded checks that the manifest's signature is valid under its
// OWN embedded public key — i.e. the content is self-consistent with
// whatever key signed it, independent of whether that key is trusted by
// any verifier.
//
// ParseAndVerify rejects a manifest signed by an untrusted key
// (ErrPublicKeyMismatch) BEFORE it ever checks the signature, so it cannot
// distinguish a legitimately key-rotated manifest (authentic content,
// signed by an old/foreign-but-self-consistent key) from a tampered one
// (content altered after signing). `repair attestation` uses this to
// re-sign ONLY self-consistent manifests — re-signing a tampered manifest
// would launder attacker-modified content under the operator's key.
//
// Returns the parsed manifest on success; an error when the manifest is
// unsigned, its embedded key is unparseable, or — the case that matters —
// the signature does not match the embedded key (content tampered).
func VerifyEmbedded(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	if m.Attestation == nil {
		return nil, ErrUnsigned
	}
	if m.Attestation.Scheme != SchemeEd25519 {
		return nil, fmt.Errorf("manifest: unsupported signature scheme %q", m.Attestation.Scheme)
	}
	emb, err := LoadVerifier([]byte(m.Attestation.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("manifest: parse embedded public key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Attestation.Signature)
	if err != nil {
		return nil, fmt.Errorf("manifest: decode signature: %w", err)
	}
	canonical, err := m.Canonicalize()
	if err != nil {
		return nil, err
	}
	if err := emb.Verify(canonical, sig); err != nil {
		return nil, fmt.Errorf("manifest %s: signature does not match its embedded key (content tampered after signing): %w", m.BackupID, err)
	}
	return &m, nil
}

// publicKeyEquals reports whether v and other carry the same Ed25519
// public-key bytes. Defined as a method so Verifier remains the only
// holder of its raw key material.
func (v *Verifier) publicKeyEquals(other *Verifier) bool {
	if len(v.pub) != len(other.pub) {
		return false
	}
	for i := range v.pub {
		if v.pub[i] != other.pub[i] {
			return false
		}
	}
	return true
}

// Validate checks the manifest's internal invariants — the
// shape-level integrity that must hold for any well-formed
// backup, independent of CAS reachability or signature.  Run
// at the top of every restore as a fast safety gate: a manifest
// that fails Validate cannot produce a correct restore, no
// matter what else looks fine.  Defence-in-depth (L1 of the
// restore-correctness stack); L2 / L3 layer additional checks
// that need CAS / PG access.
//
// Returns nil on success, or an error describing the FIRST
// violation found.  Callers should treat any non-nil return as
// a hard fail.
func (m *Manifest) Validate() error {
	if m == nil {
		return errors.New("manifest: nil")
	}
	if m.Schema != Schema {
		return fmt.Errorf("manifest: schema %q != %q", m.Schema, Schema)
	}
	if m.BackupID == "" {
		return errors.New("manifest: backup_id is empty")
	}
	if m.Deployment == "" {
		return errors.New("manifest: deployment is empty")
	}
	if m.PGVersion <= 0 {
		return fmt.Errorf("manifest: pg_version=%d (must be ≥1)", m.PGVersion)
	}
	if m.SystemIdentifier == "" {
		return errors.New("manifest: system_identifier is empty")
	}
	if m.StartLSN == "" || m.StopLSN == "" {
		return fmt.Errorf("manifest: missing LSN(s) start=%q stop=%q", m.StartLSN, m.StopLSN)
	}
	if m.BackupLabel == "" {
		return errors.New("manifest: backup_label is empty (required for restore)")
	}
	if len(m.Tablespaces) == 0 {
		return errors.New("manifest: no tablespaces (default tablespace must be present)")
	}

	// File invariants — uniqueness, chunk-len-sums-to-size,
	// every chunk has a non-zero hash and len.
	//
	// Uniqueness is keyed on (TablespaceOID, Path), not Path alone:
	// a non-default tablespace's tar entries are named relative to
	// the tablespace root (e.g. "PG_18_202209061/16384/12345"), so
	// two DIFFERENT tablespaces can legitimately carry the same
	// relative path.  Keying on Path alone would reject a
	// perfectly-valid multi-tablespace backup.
	seenFile := map[string]bool{}
	for i := range m.Files {
		f := &m.Files[i]
		if f.Path == "" {
			return fmt.Errorf("manifest: files[%d] empty path", i)
		}
		fileKey := fmt.Sprintf("%d\x00%s", f.TablespaceOID, f.Path)
		if seenFile[fileKey] {
			return fmt.Errorf("manifest: duplicate file path %q in tablespace %d", f.Path, f.TablespaceOID)
		}
		if err := safeManifestPath(f.Path); err != nil {
			return fmt.Errorf("manifest: files[%d]: %w", i, err)
		}
		seenFile[fileKey] = true
		if f.Size < 0 {
			return fmt.Errorf("manifest: files[%d] %q size=%d (must be ≥0)", i, f.Path, f.Size)
		}
		var chunkSum int64
		var nextOff int64
		for j, ref := range f.Chunks {
			if ref.Hash == (repo.Hash{}) {
				return fmt.Errorf("manifest: files[%d] %q chunk[%d] zero hash", i, f.Path, j)
			}
			if ref.Len <= 0 {
				return fmt.Errorf("manifest: files[%d] %q chunk[%d] len=%d", i, f.Path, j, ref.Len)
			}
			if ref.Offset != nextOff {
				return fmt.Errorf("manifest: files[%d] %q chunk[%d] offset=%d, want %d (chunks must be contiguous)",
					i, f.Path, j, ref.Offset, nextOff)
			}
			nextOff += ref.Len
			chunkSum += ref.Len
		}
		if chunkSum != f.Size {
			return fmt.Errorf("manifest: files[%d] %q chunks total %d != size %d",
				i, f.Path, chunkSum, f.Size)
		}
		// (Size==0 with non-empty Chunks is naturally caught
		// by the sum check above — chunkSum will be ≥1 ≠ 0.)
	}

	// Dir invariants — uniqueness, no collision with file paths.
	// Collisions matter because the restore loop calls MkdirAll on
	// dirs AFTER materialising files; if a path appears as both,
	// the second-loser wins silently and the result is
	// non-deterministic.
	seenDir := map[string]bool{}
	for i, d := range m.Dirs {
		if d.Path == "" {
			return fmt.Errorf("manifest: dirs[%d] empty path", i)
		}
		dirKey := fmt.Sprintf("%d\x00%s", d.TablespaceOID, d.Path)
		if seenDir[dirKey] {
			return fmt.Errorf("manifest: duplicate dir path %q in tablespace %d", d.Path, d.TablespaceOID)
		}
		if err := safeManifestPath(d.Path); err != nil {
			return fmt.Errorf("manifest: dirs[%d]: %w", i, err)
		}
		seenDir[dirKey] = true
		if seenFile[dirKey] {
			return fmt.Errorf("manifest: dir %q collides with a file path of the same name in tablespace %d", d.Path, d.TablespaceOID)
		}
	}

	// Encryption self-consistency. When a manifest declares itself
	// encrypted, the metadata needed to UNWRAP the data key must be
	// present and well-formed — otherwise the chunks are ciphertext we
	// can never decrypt, i.e. a healthy-looking but permanently
	// unrestorable backup. Catching it here (the single commit gate)
	// turns "silent data loss discovered at restore" into "refused at
	// backup time." See the data-loss audit, encryption path.
	if e := m.Encryption; e != nil {
		if e.Scheme == "" {
			return errors.New("manifest: encryption present but scheme is empty")
		}
		if e.KEKRef == "" {
			return errors.New("manifest: encryption present but kek_ref is empty (the wrapping key can't be resolved at restore)")
		}
		if e.WrappedDEK == "" {
			return errors.New("manifest: encryption present but wrapped_dek is empty (the data key is unrecoverable — backup would be undecryptable)")
		}
		raw, err := base64.StdEncoding.DecodeString(e.WrappedDEK)
		if err != nil {
			return fmt.Errorf("manifest: wrapped_dek is not valid base64: %w", err)
		}
		if len(raw) == 0 {
			return errors.New("manifest: wrapped_dek decodes to zero bytes (the data key is unrecoverable)")
		}
		if e.EnvelopeVersion < 1 {
			return fmt.Errorf("manifest: encryption envelope_version %d is invalid (must be >= 1)", e.EnvelopeVersion)
		}
	}
	return nil
}

// safeManifestPath rejects a file/dir Path that would escape the
// restore target: absolute paths, paths carrying a NUL or backslash,
// and any path that — once cleaned — keeps a leading "..". PG only
// emits clean PGDATA-relative names, so this is a manifest invariant:
// it runs at commit (every backup is validated before write) and at
// restore. The restore loop's safeJoinTarget independently re-checks
// each path before any write, but enforcing it here means a manifest
// carrying an escaping path can neither be committed nor pass
// validation — defence-in-depth that also covers consumers which read
// the manifest without going through safeJoinTarget.
func safeManifestPath(p string) error {
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path %q contains a NUL byte", p)
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path %q contains a backslash", p)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q is absolute", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q escapes the backup root via \"..\"", p)
	}
	return nil
}

// ErrUnsigned is returned by ParseAndVerify when the on-disk manifest
// has no Attestation block.
var ErrUnsigned = errors.New("manifest: unsigned (no attestation block)")

// ErrPublicKeyMismatch is returned by ParseAndVerify when the manifest's
// embedded public key differs from the verifier's public key. The
// signature is genuine but not by a trusted signer.
var ErrPublicKeyMismatch = errors.New("manifest: embedded public key does not match verifier")
