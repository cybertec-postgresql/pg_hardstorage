// Package verifybackup re-implements PostgreSQL's
// `pg_verifybackup` in pure Go.
//
// Why we don't shell out to the upstream binary
// ----------------------------------------------
// `pg_verifybackup` is a CLI tool that ships with the
// postgresql-client package (postgresql-N-server on RHEL).
// Shelling out works on hosts that have it installed, but
// soft-skipping on hosts that don't is exactly the gap that
// let issue #7 ship.  Re-implementing the same check in
// process means EVERY restore — laptop, CI without PG, edge
// node, container without postgresql-client — runs the same
// integrity gate, with no soft-skip path.
//
// Source format
// -------------
// pg_basebackup writes a `backup_manifest` JSON file whose
// schema is fixed and stable across PG 13–18.  Our backup
// path captures it verbatim into Manifest.PGBackupManifest;
// this package parses that blob and re-hashes every file on
// the restored datadir, comparing to the recorded
// per-file checksum.
//
// What it catches
// ---------------
//   - missing files                 (file in manifest, not on disk)
//   - truncated files               (size mismatch)
//   - silent corruption             (checksum mismatch)
//   - wrong restore order           (any file written incorrectly)
//
// What it does NOT catch
// ----------------------
//   - empty PG-required dirs missing (issue #7) — those don't
//     appear in PG's backup_manifest at all.  Caught by L1
//     (Manifest.Validate) + L3 (start-cluster smoke test).
//   - bad WAL replay leading to data loss — caught by L4
//     (pg_dump round-trip) and L3 (cluster startup).
//   - tablespace symlink issues — caught by L3.
//
// Usage:
//
//	err := verifybackup.Verify(ctx, m.PGBackupManifest, restoredDir)
package verifybackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// pgManifest is the subset of PG's backup_manifest JSON we
// consume.  PG-versioned fields we don't read — WAL-Ranges,
// Manifest-Checksum, System-Identifier — are deliberately
// absent here so adding new ones in PG 19+ doesn't break
// parsing (encoding/json is permissive on unknown fields).
type pgManifest struct {
	Version int              `json:"PostgreSQL-Backup-Manifest-Version"`
	Files   []pgManifestFile `json:"Files"`
}

type pgManifestFile struct {
	Path string `json:"Path"`
	// EncodedPath is PG's alternative to Path for filenames that
	// aren't valid UTF-8 (or contain characters PG chooses not to
	// emit literally): the path is hex-encoded and carried here
	// instead.  Exactly one of Path / Encoded-Path is present per
	// entry.  See resolvePath.
	EncodedPath       string `json:"Encoded-Path"`
	Size              int64  `json:"Size"`
	LastModified      string `json:"Last-Modified"`
	ChecksumAlgorithm string `json:"Checksum-Algorithm"`
	Checksum          string `json:"Checksum"`
}

// resolvePath returns the file's path relative to the data
// directory, decoding PG's hex-encoded "Encoded-Path" when the
// plain "Path" field is absent.
//
// Why this matters: pg_basebackup emits "Encoded-Path" (a hex
// string) instead of "Path" for filenames that are not valid
// UTF-8 or that contain control characters.  Without decoding
// it, Path stays "" and filepath.Join(dataDir, "") resolves to
// dataDir itself — a directory — so verifyOne false-fails every
// such entry with "expected regular file, got mode=…dir".
func (f *pgManifestFile) resolvePath() (string, error) {
	if f.Path != "" {
		return f.Path, nil
	}
	if f.EncodedPath == "" {
		return "", errors.New("manifest entry has neither Path nor Encoded-Path")
	}
	raw, err := hex.DecodeString(strings.TrimSpace(f.EncodedPath))
	if err != nil {
		return "", fmt.Errorf("Encoded-Path is not valid hex: %w", err)
	}
	return string(raw), nil
}

// ErrNoManifest signals that the backup didn't carry PG's
// own backup_manifest (older backups taken before
// PGBackupManifest was wired into our manifest).  Callers
// can elect to fail-soft on this — it's not data loss, just
// missing the verifybackup defence layer.
var ErrNoManifest = errors.New("verifybackup: no PG backup_manifest captured (pre-backup?)")

// Result summarises what was checked.  Returned even on
// error so callers can record progress in the audit log.
type Result struct {
	FilesChecked  int
	BytesHashed   int64
	Algorithm     string // "CRC32C", "SHA256", or mixed-set marker
	SkippedReason string // non-empty when Verify returns nil but skipped (e.g. NONE algorithm)
}

// Verify walks every entry in PG's backup_manifest JSON
// (manifestBytes) and asserts the matching file under
// dataDir has the declared size + checksum.  Any mismatch
// is a hard fail — the restored datadir is suspect.
//
// Returns ErrNoManifest when manifestBytes is empty.  The
// caller decides whether to escalate; restore.Restore today
// records a `verifybackup.skipped_no_manifest` event and
// continues, since older backups can't supply this data.
func Verify(ctx context.Context, manifestBytes []byte, dataDir string) (*Result, error) {
	if len(manifestBytes) == 0 {
		return nil, ErrNoManifest
	}
	var m pgManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("verifybackup: parse PG backup_manifest JSON: %w", err)
	}
	if m.Version == 0 {
		return nil, errors.New("verifybackup: backup_manifest has no PostgreSQL-Backup-Manifest-Version field")
	}

	res := &Result{}
	algos := map[string]struct{}{}

	for i := range m.Files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		f := &m.Files[i]
		if err := verifyOne(dataDir, f, res); err != nil {
			// Prefer the resolved (possibly hex-decoded) path in the
			// error; fall back to whatever raw path field was set.
			name := f.Path
			if rp, rerr := f.resolvePath(); rerr == nil {
				name = rp
			}
			return res, fmt.Errorf("verifybackup: file[%d] %q: %w", i, name, err)
		}
		if alg := strings.ToUpper(f.ChecksumAlgorithm); alg != "" {
			algos[alg] = struct{}{}
		}
	}

	switch {
	case len(algos) == 0:
		res.Algorithm = "(none)"
	case len(algos) == 1:
		for k := range algos {
			res.Algorithm = k
		}
	default:
		// Mixed: rare in practice (operators rarely change
		// pg_basebackup's --manifest-checksums mid-run) but
		// possible.  Surface it.
		var keys []string
		for k := range algos {
			keys = append(keys, k)
		}
		res.Algorithm = "mixed:" + strings.Join(keys, ",")
	}
	return res, nil
}

// verifyOne stats + hashes one file, comparing against the
// manifest entry.  Updates res counters in-place.
func verifyOne(dataDir string, f *pgManifestFile, res *Result) error {
	relPath, err := f.resolvePath()
	if err != nil {
		return err
	}
	full := filepath.Join(dataDir, relPath)
	st, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("file missing from restored datadir")
		}
		return fmt.Errorf("stat: %w", err)
	}
	// PG's manifest only lists regular files — no dir / symlink
	// entries — so any non-regular result is a structural
	// surprise worth surfacing rather than tolerating.
	if !st.Mode().IsRegular() {
		return fmt.Errorf("expected regular file, got mode=%v", st.Mode())
	}
	if st.Size() != f.Size {
		return fmt.Errorf("size mismatch: on-disk %d, manifest %d", st.Size(), f.Size)
	}
	res.FilesChecked++
	res.BytesHashed += st.Size()

	alg := strings.ToUpper(strings.TrimSpace(f.ChecksumAlgorithm))
	if alg == "NONE" || alg == "" {
		// PG_BASEBACKUP can be invoked with
		// --manifest-checksums=NONE; the manifest still
		// records every file but skips checksums.  Size +
		// presence are still validated above.
		return nil
	}
	hasher, err := newHasher(alg)
	if err != nil {
		return err
	}
	fh, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer fh.Close()
	if _, err := io.Copy(hasher, fh); err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	got := hasher.Sum(nil)
	want, err := hex.DecodeString(strings.TrimSpace(f.Checksum))
	if err != nil {
		return fmt.Errorf("manifest checksum is not hex: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s checksum mismatch: on-disk %s, manifest %s",
			alg, hex.EncodeToString(got), hex.EncodeToString(want))
	}
	return nil
}

// newHasher maps PG's manifest algorithm names to Go hashers.
// The set covers everything PG documents (NONE, CRC32C,
// SHA224, SHA256, SHA384, SHA512); NONE is handled by the
// caller.  Unknown algorithms hard-fail rather than treating
// them as opaque-ok.
func newHasher(alg string) (hash.Hash, error) {
	switch alg {
	case "CRC32C":
		return castagnoliHash{crc32.New(crc32.MakeTable(crc32.Castagnoli))}, nil
	case "SHA224":
		return sha256.New224(), nil
	case "SHA256":
		return sha256.New(), nil
	case "SHA384":
		return sha512.New384(), nil
	case "SHA512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported checksum algorithm %q", alg)
}

// castagnoliHash adapts crc32.Hash32 (32-bit checksum)
// to hash.Hash so the verifier code can treat all
// algorithms uniformly.  Sum() returns 4 bytes
// LITTLE-ENDIAN — matching the byte order PG's
// backup_manifest serialises CRC32C in.
//
// Endianness regression: an earlier version of this code
// emitted big-endian, which produced byte-reversed hex like
// "f1c7ab58" when the manifest claimed "58abc7f1".  Every
// CRC32C-checksummed file would fail verification with a
// false positive.  The fix is one byte order; the test
// helper must match.
type castagnoliHash struct{ h hash.Hash32 }

// Write forwards p to the wrapped 32-bit CRC.
func (c castagnoliHash) Write(p []byte) (int, error) { return c.h.Write(p) }

// Sum appends the current CRC32C as four little-endian bytes to b,
// matching the byte order PG's backup_manifest serialises CRC32C in.
func (c castagnoliHash) Sum(b []byte) []byte {
	v := c.h.Sum32()
	return append(b,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// Reset clears the wrapped CRC state.
func (c castagnoliHash) Reset() { c.h.Reset() }

// Size returns 4 — the byte length of a CRC32C digest.
func (c castagnoliHash) Size() int { return 4 }

// BlockSize returns 1 — CRC32C has no preferred block boundary.
func (c castagnoliHash) BlockSize() int { return 1 }
