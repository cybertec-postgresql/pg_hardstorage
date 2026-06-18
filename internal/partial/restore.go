// restore.go — selective (file-glob/db-name-scoped) restore orchestration on top of repo CAS.
package partial

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// RestoreSchema is the on-disk version tag for RestoreResult bodies.
const RestoreSchema = "pg_hardstorage.partial.restore.v1"

// RestoreOptions configures one selective-restore run.
//
// Identification of the heap files to extract uses one of two
// resolution paths:
//
//   - PGConnString: a libpq URI to the live source DB. We query
//     pg_class for each table's relfilenode + TOAST relfilenode and
//     match those against the manifest's FileEntries.
//
//   - RelfilenodeMap: a pre-built name→relfilenode map. The
//     air-gapped fallback when no live source DB is reachable. The
//     operator constructs this from a prior `partial inspect
//     --pg-connection=...` run or from their own pg_class snapshot.
//
// Exactly one must be set. With neither, RestoreOptions is a usage
// error.
type RestoreOptions struct {
	// RepoURL is the source repository.
	RepoURL string
	// Deployment + BackupID identify the manifest.
	Deployment string
	BackupID   string
	// Verifier verifies the manifest signature before any file is
	// written. Required.
	Verifier *backup.Verifier
	// Tables is the qualified table list to extract.
	Tables []string

	// PGConnString resolves table names → relfilenodes via a live
	// pg_class lookup. Mutually exclusive with RelfilenodeMap.
	PGConnString string

	// RelfilenodeMap is a pre-built map of qualified name →
	// resolved heap path (matching FileEntry.Path). Provide this
	// when no live PG is available.
	RelfilenodeMap map[string]Relfilenode

	// TargetDir is where the selected files materialise. Refuses
	// non-empty unless AllowOverwrite.
	TargetDir      string
	AllowOverwrite bool

	// KEKForRef resolves the manifest's encryption.KEKRef → 32-byte
	// KEK. Required when restoring a local-custody encrypted backup.
	KEKForRef func(ref string) ([encryption.KeyLen]byte, error)

	// UnwrapDEK unwraps a cloud-KMS-wrapped DEK server-side (issue #102);
	// required to partial-restore a backup wrapped with a cloud KMS KEK
	// (the KEK never leaves the HSM, so KEKForRef can't resolve it).
	UnwrapDEK func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)
}

// RestoreResult is the structured outcome.
type RestoreResult struct {
	Schema       string                `json:"schema"`
	BackupID     string                `json:"backup_id"`
	Deployment   string                `json:"deployment"`
	TargetDir    string                `json:"target_dir"`
	Tables       []string              `json:"tables"`
	Mappings     []RestoreTableMapping `json:"mappings"`
	FilesWritten int                   `json:"files_written"`
	BytesWritten int64                 `json:"bytes_written"`
	NotFound     []string              `json:"not_found,omitempty"`
	StartedAt    time.Time             `json:"started_at"`
	StoppedAt    time.Time             `json:"stopped_at"`
	DurationMS   int64                 `json:"duration_ms"`
}

// RestoreTableMapping records what was extracted for one table.
// HeapFiles + ToastFiles are the manifest paths actually written;
// they include segment files (e.g. base/16384/2619.1, .2) and
// visibility-map / FSM siblings.
type RestoreTableMapping struct {
	Qualified  string   `json:"qualified"`
	HeapPath   string   `json:"heap_path,omitempty"`
	HeapFiles  []string `json:"heap_files,omitempty"`
	HeapBytes  int64    `json:"heap_bytes"`
	ToastPath  string   `json:"toast_path,omitempty"`
	ToastFiles []string `json:"toast_files,omitempty"`
	ToastBytes int64    `json:"toast_bytes"`
}

// ErrNoTableResolution is returned when neither PGConnString nor
// RelfilenodeMap is set.
var ErrNoTableResolution = errors.New("partial: must set PGConnString or RelfilenodeMap")

// ErrEmptyTables is returned when Tables is empty.
var ErrEmptyTables = errors.New("partial: Tables is empty")

// Restore extracts the named tables' heap (and TOAST) files from
// the backup at RepoURL/Deployment/BackupID into TargetDir.
//
// What "extract" means here:
//
//  1. Resolve each table to a relfilenode (live PG or pre-built map).
//  2. Walk the manifest's FileEntries; for each entry whose Path
//     matches a heap-file pattern for the target relfilenodes,
//     materialise it into TargetDir at the same relative path.
//  3. Include segment files (".1", ".2", ...), visibility maps
//     ("_vm"), free-space maps ("_fsm"), and the TOAST relation's
//     analogous files.
//  4. Skip everything else — control files, WAL, other tables,
//     indexes (rebuildable from heap on the target DB).
//
// What this does NOT do:
//
//   - Spin up a sandbox PG. The selected files land in TargetDir
//     as-is; the operator runs their own pg_dump --table=... against
//     a PG started against TargetDir, OR copies the files into a
//     running cluster's analogous OID locations (advanced; risky).
//
//   - Generate SQL. The selective extraction is the binary's job;
//     the SQL layer (pg_dump invocation, DDL emission) belongs in
//     a follow-up that owns a sandbox PG runtime.
//
// The split lets+ ship the half that absolutely needs the
// repo + manifest + chunk store (file selection + materialisation)
// without waiting for the testcontainers + pg_dump plumbing.
func Restore(ctx context.Context, opts RestoreOptions) (*RestoreResult, error) {
	if err := validateRestoreOptions(&opts); err != nil {
		return nil, err
	}
	res := &RestoreResult{
		Schema:     RestoreSchema,
		BackupID:   opts.BackupID,
		Deployment: opts.Deployment,
		TargetDir:  opts.TargetDir,
		Tables:     append([]string(nil), opts.Tables...),
		StartedAt:  time.Now().UTC(),
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	// 1. Open the repo + read the manifest.
	_, sp, err := repo.Open(ctx, opts.RepoURL)
	if err != nil {
		finish()
		return res, fmt.Errorf("partial restore: open repo: %w", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, opts.Deployment, opts.BackupID, opts.Verifier)
	if err != nil {
		finish()
		return res, fmt.Errorf("partial restore: read manifest: %w", err)
	}

	// 2. Build the CAS (encryption-aware if needed).
	cas, err := buildCAS(ctx, sp, m, opts.KEKForRef, opts.UnwrapDEK)
	if err != nil {
		finish()
		return res, err
	}

	// 3. Resolve table → relfilenode.
	rfns, err := resolveRelfilenodes(ctx, opts)
	if err != nil {
		finish()
		return res, err
	}

	// 4. Pre-flight target dir.
	if err := preflightTarget(opts.TargetDir, opts.AllowOverwrite); err != nil {
		finish()
		return res, err
	}
	if err := os.MkdirAll(opts.TargetDir, 0o700); err != nil {
		finish()
		return res, fmt.Errorf("partial restore: mkdir target: %w", err)
	}

	// 5. For each resolved table, find matching FileEntries +
	//    materialise. NotFound tables get appended to res.NotFound.
	byPath := indexFiles(m.Files)
	for _, rfn := range rfns {
		if rfn.NotFound || rfn.Path == "" {
			res.NotFound = append(res.NotFound, rfn.Qualified)
			res.Mappings = append(res.Mappings, RestoreTableMapping{
				Qualified: rfn.Qualified,
			})
			continue
		}
		mapping := RestoreTableMapping{
			Qualified: rfn.Qualified,
			HeapPath:  rfn.Path,
		}
		// Heap + segments + _vm + _fsm.
		heapWritten, heapBytes, err := materialiseRelfilenodeFamily(
			ctx, cas, opts.TargetDir, rfn.Path, byPath)
		if err != nil {
			finish()
			return res, fmt.Errorf("partial restore: heap %s: %w", rfn.Path, err)
		}
		mapping.HeapFiles = heapWritten
		mapping.HeapBytes = heapBytes
		res.FilesWritten += len(heapWritten)
		res.BytesWritten += heapBytes

		// TOAST family (if present).
		if rfn.ToastPath != "" {
			mapping.ToastPath = rfn.ToastPath
			toastWritten, toastBytes, err := materialiseRelfilenodeFamily(
				ctx, cas, opts.TargetDir, rfn.ToastPath, byPath)
			if err != nil {
				finish()
				return res, fmt.Errorf("partial restore: toast %s: %w", rfn.ToastPath, err)
			}
			mapping.ToastFiles = toastWritten
			mapping.ToastBytes = toastBytes
			res.FilesWritten += len(toastWritten)
			res.BytesWritten += toastBytes
		}
		res.Mappings = append(res.Mappings, mapping)
	}

	finish()
	return res, nil
}

// resolveRelfilenodes picks the right resolution path based on
// which RestoreOptions field was set.
func resolveRelfilenodes(ctx context.Context, opts RestoreOptions) ([]Relfilenode, error) {
	if opts.PGConnString != "" {
		return LookupRelfilenodes(ctx, LookupOptions{
			PGConnString: opts.PGConnString,
			Tables:       opts.Tables,
		})
	}
	// RelfilenodeMap path — synthesise a Relfilenode list from the
	// caller-supplied map. Tables not in the map become NotFound.
	out := make([]Relfilenode, 0, len(opts.Tables))
	for _, t := range opts.Tables {
		rfn, ok := opts.RelfilenodeMap[t]
		if !ok {
			out = append(out, Relfilenode{Qualified: t, NotFound: true})
			continue
		}
		// Defensive: ensure Qualified is set on the entry the
		// caller built.
		rfn.Qualified = t
		out = append(out, rfn)
	}
	return out, nil
}

// indexFiles builds a path → FileEntry index for fast lookup
// during family-walks. Returned map is read-only after construction.
func indexFiles(files []backup.FileEntry) map[string]*backup.FileEntry {
	out := make(map[string]*backup.FileEntry, len(files))
	for i := range files {
		out[files[i].Path] = &files[i]
	}
	return out
}

// materialiseRelfilenodeFamily writes every manifest file under a
// relfilenode's path family — the base file (e.g. "base/16384/2619")
// plus segments (".1", ".2", ...) and visibility-map / FSM siblings
// ("_vm", "_fsm", "_vm.1", ...). PG's pg_relation_filepath returns
// the base path; the family is everything that shares its prefix
// followed by an empty string, "." + digits, "_vm", "_fsm" tokens.
//
// Why explicit family-walk rather than prefix match: a naive
// prefix match against "base/16384/2619" would also match
// "base/16384/26190" (a different relation). The specific tokens
// keep the match precise.
func materialiseRelfilenodeFamily(
	ctx context.Context, cas *repo.CAS, target, basePath string, byPath map[string]*backup.FileEntry,
) (written []string, totalBytes int64, err error) {
	// First, the base file itself.
	if entry, ok := byPath[basePath]; ok {
		n, err := materialiseOneFile(ctx, cas, target, entry)
		if err != nil {
			return written, totalBytes, err
		}
		written = append(written, basePath)
		totalBytes += n
	}
	// Then any sibling that matches the family pattern. We walk
	// all known paths once rather than enumerating every possible
	// suffix (segment counts can be unbounded).
	for path, entry := range byPath {
		if path == basePath {
			continue
		}
		if !isFamilyMember(path, basePath) {
			continue
		}
		n, err := materialiseOneFile(ctx, cas, target, entry)
		if err != nil {
			return written, totalBytes, err
		}
		written = append(written, path)
		totalBytes += n
	}
	return written, totalBytes, nil
}

// isFamilyMember reports whether path is a sibling of basePath in
// PG's relfilenode-family naming scheme: the same base followed by
// ".N" (segment), "_vm" (visibility map), "_fsm" (free-space map),
// or "_vm.N" / "_fsm.N" (segments of the maps).
func isFamilyMember(path, basePath string) bool {
	if !strings.HasPrefix(path, basePath) {
		return false
	}
	suffix := path[len(basePath):]
	if suffix == "" {
		return true
	}
	// Trim the optional sibling tags first.
	switch {
	case strings.HasPrefix(suffix, "_vm"):
		suffix = suffix[3:]
	case strings.HasPrefix(suffix, "_fsm"):
		suffix = suffix[4:]
	case strings.HasPrefix(suffix, "_init"):
		// init forks (unlogged tables) — match these too.
		suffix = suffix[5:]
	}
	if suffix == "" {
		return true
	}
	// What's left should be ".<digits>" for a segment.
	if !strings.HasPrefix(suffix, ".") {
		return false
	}
	for _, c := range suffix[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(suffix) > 1
}

// materialiseOneFile writes one FileEntry's bytes into target.
// Mirrors restore.materializeFile but lives here to keep partial
// independent of the restore package's full-restore ceremony
// (preflight, checkpoint resumption, recovery signals).
func materialiseOneFile(ctx context.Context, cas *repo.CAS, target string, f *backup.FileEntry) (int64, error) {
	full, err := safeJoinTarget(target, f.Path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir parent: %w", err)
	}
	mode := stdfs.FileMode(f.Mode)
	if mode == 0 {
		mode = 0o600
	}
	dst, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, fmt.Errorf("open destination: %w", err)
	}
	var bytesWritten int64
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close destination: %w", cerr)
		}
	}()
	for _, ref := range f.Chunks {
		if err := ctx.Err(); err != nil {
			return bytesWritten, err
		}
		body, err := cas.GetChunkBytes(ctx, ref.Hash)
		if err != nil {
			return bytesWritten, fmt.Errorf("fetch chunk %s: %w", ref.Hash, err)
		}
		if int64(len(body)) != ref.Len {
			return bytesWritten, fmt.Errorf("chunk %s len mismatch: got %d, want %d",
				ref.Hash, len(body), ref.Len)
		}
		if _, err := dst.Write(body); err != nil {
			return bytesWritten, fmt.Errorf("write chunk %s: %w", ref.Hash, err)
		}
		bytesWritten += int64(len(body))
	}
	if bytesWritten != f.Size {
		return bytesWritten, fmt.Errorf("size mismatch: chunks total %d, manifest says %d",
			bytesWritten, f.Size)
	}
	if err := dst.Sync(); err != nil {
		return bytesWritten, fmt.Errorf("fsync: %w", err)
	}
	return bytesWritten, nil
}

// buildCAS constructs a CAS that can decrypt the manifest's chunks
// (when encrypted). Mirrors the restore package's
// buildEncryptedCAS helper — duplicated here rather than imported
// so partial doesn't drag the full restore package + its
// checkpoint/recovery machinery into the dependency graph.
//
// Errors are deliberately plain (not output.NewError) so the
// caller can wrap them at the CLI boundary; this matches the rest
// of partial's error idiom and keeps the package free of
// CLI-rendering concerns.
func buildCAS(ctx context.Context, sp storage.StoragePlugin, m *backup.Manifest, kekForRef func(string) ([encryption.KeyLen]byte, error), unwrapDEK func(context.Context, string, []byte) ([]byte, error)) (*repo.CAS, error) {
	if m.Encryption == nil {
		return casdefault.New(sp), nil
	}
	if m.Encryption.Scheme != "aes-256-gcm" {
		return nil, fmt.Errorf("partial restore: unsupported encryption scheme %q (this backup may have been written by a future pg_hardstorage version)",
			m.Encryption.Scheme)
	}
	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("partial restore: wrapped_dek not valid base64: %w", err)
	}

	var dek []byte
	// Cloud KMS: unwrap the DEK server-side (issue #102) — the KEK never
	// leaves the HSM, so the local kekForRef can't resolve it.
	if scheme := kms.SchemeOf(m.Encryption.KEKRef); scheme != "" && scheme != "local" {
		if unwrapDEK == nil {
			return nil, fmt.Errorf("partial restore: backup is wrapped with a cloud KMS KEK (%q) but no cloud-KMS unwrap resolver was provided (set RestoreOptions.UnwrapDEK)", m.Encryption.KEKRef)
		}
		dek, err = unwrapDEK(ctx, m.Encryption.KEKRef, wrapped)
		if err != nil {
			return nil, fmt.Errorf("partial restore: unwrap DEK via cloud KMS %q: %w", m.Encryption.KEKRef, err)
		}
	} else {
		if kekForRef == nil {
			return nil, fmt.Errorf("partial restore: backup is encrypted but no KEK resolver was provided (set RestoreOptions.KEKForRef)")
		}
		kek, kerr := kekForRef(m.Encryption.KEKRef)
		if kerr != nil {
			return nil, fmt.Errorf("partial restore: resolve KEK %q: %w", m.Encryption.KEKRef, kerr)
		}
		dekArr, uerr := encryption.Unwrap(kek, wrapped)
		if uerr != nil {
			return nil, fmt.Errorf("partial restore: unwrap DEK with KEK %q: %w (the supplied KEK doesn't match what wrapped this backup)",
				m.Encryption.KEKRef, uerr)
		}
		dek = dekArr[:]
	}

	enc, err := aesgcm.New(dek)
	if err != nil {
		return nil, fmt.Errorf("partial restore: build aes-gcm encryptor: %w", err)
	}
	return casdefault.NewEncrypted(sp, enc), nil
}

// validateRestoreOptions checks the options struct for required
// fields and mutually-exclusive flags.
func validateRestoreOptions(opts *RestoreOptions) error {
	if opts.RepoURL == "" {
		return errors.New("partial restore: RepoURL is required")
	}
	if opts.Deployment == "" {
		return errors.New("partial restore: Deployment is required")
	}
	if opts.BackupID == "" {
		return errors.New("partial restore: BackupID is required")
	}
	if opts.Verifier == nil {
		return errors.New("partial restore: Verifier is required")
	}
	if len(opts.Tables) == 0 {
		return ErrEmptyTables
	}
	if opts.PGConnString == "" && len(opts.RelfilenodeMap) == 0 {
		return ErrNoTableResolution
	}
	if opts.PGConnString != "" && len(opts.RelfilenodeMap) > 0 {
		return errors.New("partial restore: PGConnString and RelfilenodeMap are mutually exclusive")
	}
	if opts.TargetDir == "" {
		return errors.New("partial restore: TargetDir is required")
	}
	for _, t := range opts.Tables {
		if !strings.Contains(t, ".") {
			return fmt.Errorf("partial restore: %q is unqualified; pass schema.table", t)
		}
	}
	return nil
}

// preflightTarget refuses non-empty target dirs unless overwrite is
// permitted. The behaviour matches restore.preflightTarget but is
// not shared (partial doesn't depend on restore).
func preflightTarget(target string, allowOverwrite bool) error {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil // we'll create it
		}
		return fmt.Errorf("stat target: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("target %q exists but is not a directory", target)
	}
	if allowOverwrite {
		return nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return fmt.Errorf("read target: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("target %q is not empty (%d entries); pass --force to overwrite",
			target, len(entries))
	}
	return nil
}

// safeJoinTarget joins target + rel and refuses any result that
// escapes target via "..", absolute paths, or cross-volume
// indirection.  Defence-in-depth against a malicious manifest whose
// FileEntry.Path contains traversal tokens.  Mirrors the
// implementation in internal/restore (the two packages stay
// independent on purpose; the helper is small enough to duplicate).
func safeJoinTarget(target, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("partial: empty file path in manifest")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("partial: refusing absolute path %q in manifest", rel)
	}
	full := filepath.Join(target, rel)
	cleanTarget := filepath.Clean(target)
	cleanFull := filepath.Clean(full)
	relCheck, err := filepath.Rel(cleanTarget, cleanFull)
	if err != nil {
		return "", fmt.Errorf("partial: path %q is on a different volume than target %q", rel, target)
	}
	if relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("partial: refusing path %q that escapes target %q", rel, target)
	}
	return cleanFull, nil
}
