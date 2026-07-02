// Package fs implements StoragePlugin against the local filesystem.
//
// Correctness highlights:
//
//   - IfNotExists uses O_CREATE|O_EXCL directly on the final path so the
//     existence check and creation are a single atomic syscall — no
//     read-modify-write race with concurrent writers.
//
//   - Overwrite Puts go to "<key>.hstmp-<rand>", get fsynced, then
//     atomically rename(2)'d into place. A crash between sync and rename
//     never leaves a half-written object visible.
//
//   - RenameIfNotExists is link(2) + unlink(2). link() returns EEXIST
//     atomically when the target exists, on every POSIX system; this
//     gives us the same semantics as Linux's renameat2(RENAME_NOREPLACE)
//     without a kernel-version dependency.
//
//   - Every write computes SHA-256 in-line. When PutOptions.ContentSHA256
//     is non-zero the plugin verifies and returns ErrChecksumMismatch on
//     disagreement — end-to-end checksum baked into the smallest plugin.
//
// URL form: file:///absolute/path  (the scheme's host must be empty;
// the path is the repository root).
package fs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func init() {
	storage.Register("file", func() storage.StoragePlugin { return &Plugin{} })
}

// repoDirMode is the permission bits we use for parent directories
// the plugin auto-creates inside the repository. 0750 (owner-rwx,
// group-rx, world-none) is the right default for a backup repo —
// world-readable parent dirs leak the repo's structure (chunk
// hashes, manifest paths) to anyone with shell access. Operators
// who want world-readability deliberately can chmod after the fact.
const repoDirMode = 0o750

// repoFileMode is the permission bits chunk and manifest files are
// created with.  0640 (owner-rw, group-r, world-none) matches the
// 0750 directory mode: an operator running as the backup user can
// read/write; the backup-readers group can read; everyone else is
// shut out.
//
// An audit flagged the previous 0o644 as world-readable.
// While chunks themselves are encrypted (KEK-wrapped DEKs), the
// plain-mode file metadata (size, mtime, layout under
// chunks/sha256/aa/bb/) is itself information about the operator's
// backup posture — chunk size correlates with workload, and the
// chunk-key prefix tree is a partial fingerprint of the
// deduplicated content.  Defence in depth.
const repoFileMode = 0o640

// Plugin is the filesystem-backed StoragePlugin.
type Plugin struct {
	root string

	// deferred holds chunk writes made with storage.DurabilityDeferred.
	// Each was written to a STAGING temp (<final>.deferred-<rand>),
	// NOT yet at its content-addressed key. Barrier makes the temps'
	// content durable, then links each to its final key — so a chunk
	// only ever appears at a real key once its bytes are crash-safe.
	// A crash before Barrier leaves only ".deferred-*" temps, never a
	// half-written file at a real key that a later run's O_EXCL dedup
	// could mistake for valid. mu guards the slice.
	mu       sync.Mutex
	deferred []deferredWrite
}

// deferredWrite pairs a staging temp with the final content key the
// next Barrier will link it to.
type deferredWrite struct {
	staging string // <final>.deferred-<rand>
	final   string
}

// recordDeferred queues a staged chunk write for the next Barrier.
// Safe for concurrent callers (the backup chunk worker pool).
func (p *Plugin) recordDeferred(staging, final string) {
	p.mu.Lock()
	p.deferred = append(p.deferred, deferredWrite{staging: staging, final: final})
	p.mu.Unlock()
}

// requeue puts unpublished deferred writes back so a retried Barrier
// finishes the job — dropping one would let a caller treat a
// non-durable chunk as committed.
func (p *Plugin) requeue(list []deferredWrite) {
	if len(list) == 0 {
		return
	}
	p.mu.Lock()
	p.deferred = append(list[:len(list):len(list)], p.deferred...)
	p.mu.Unlock()
}

// publishDeferred links each staged temp to its final content key
// (link is atomically IfNotExists — EEXIST means the key was already
// published by a prior run or a same-run duplicate, so the temp is
// just discarded) and removes the temp. Barrier calls this only
// AFTER the staged content is durable. Returns the entries it did
// not reach (for requeue) and the first error.
func (p *Plugin) publishDeferred(ctx context.Context, list []deferredWrite) ([]deferredWrite, error) {
	for i, dw := range list {
		if err := ctx.Err(); err != nil {
			return list[i:], err
		}
		switch err := os.Link(dw.staging, dw.final); {
		case err == nil, errors.Is(err, stdfs.ErrExist):
			_ = os.Remove(dw.staging)
		case errors.Is(err, stdfs.ErrNotExist):
			// The staging temp is gone. This happens on a retried
			// Barrier: a previous run already linked this temp to its
			// final key and removed the temp, but the barrier was
			// requeued because a later step (e.g. the final syncfs)
			// failed. If the final key is present the publish is
			// already done — treat it as success. If neither exists,
			// the staged content was truly lost, which is a real
			// error.
			if _, statErr := os.Stat(dw.final); statErr == nil {
				continue
			}
			return list[i:], fmt.Errorf("fs: publish chunk %q: staging temp %q vanished and final absent: %w",
				dw.final, dw.staging, err)
		default:
			return list[i:], fmt.Errorf("fs: publish chunk %q: %w", dw.final, err)
		}
	}
	return nil, nil
}

// isFSStagingName reports whether name is an fs-internal staging/temp
// file produced by an atomic write (overwrite "<key>.hstmp-<rand>",
// deferred "<key>.deferred-<rand>", or exclusive "<key>.excl-<rand>").
// These exist only transiently and must never be returned by List as
// keys.
//
// Deliberately NOT matched: the generic ".tmp." infix. Caller-created
// keys legitimately use it — the repo layer stages manifest/history
// commits at "<name>.json.tmp.<rand>" / "<name>.history.tmp.<rand>",
// and GC's FindStaleTempManifests must be able to List those to reap
// them after a crash. Hiding every ".tmp." infix here would silently
// disable that reaper. Backend-internal staging therefore uses the
// reserved ".hstmp-" marker instead. (Leftover ".tmp.<rand>" overwrite
// temps written by older builds surface as keys — the pre-existing
// behaviour — and are harmless.)
func isFSStagingName(name string) bool {
	return strings.HasSuffix(name, ".tmp") ||
		strings.Contains(name, ".hstmp-") ||
		strings.Contains(name, ".deferred-") ||
		strings.Contains(name, ".excl-")
}

// randHex returns 16 hex chars of crypto-random for staging-temp
// names — collision-free in practice, and O_EXCL guards the rest.
func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Name returns the canonical plugin name.
func (p *Plugin) Name() string { return "fs" }

// Capabilities advertises the subset we support.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		ConditionalPut: true,
		// WORM: deferred. Local FS can sort-of approximate via chattr +i
		// or noatime mounts, but it's not regulatory-grade WORM. We keep
		// the bit false so callers route to a real WORM backend.

		// VerifiesContentSHA256: TRUE.  Put computes
		// SHA-256 in-line during the write and refuses with
		// ErrChecksumMismatch when the caller's
		// PutOptions.ContentSHA256 disagrees.  The CAS
		// layer relies on this signal to decide whether
		// to compute the envelope hash at all; for fs we
		// keep the integrity check (catches in-process
		// bit flips between cas.go's envelope assembly
		// and our Write).
		VerifiesContentSHA256: true,

		// DurabilityBarrier: TRUE — a DurabilityDeferred Put
		// skips its per-file fsync, and Barrier flushes the
		// file contents + parent dentries afterwards.
		// InlineDurable stays false: an fs Put's bytes sit in
		// the OS page cache until that fsync.
		DurabilityBarrier: true,
	}
}

// Open canonicalises the URL into an absolute root path. We do NOT create
// the root here — Init / repo init handles that explicitly.
func (p *Plugin) Open(_ context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("fs: nil URL")
	}
	if cfg.URL.Host != "" && cfg.URL.Host != "localhost" {
		return fmt.Errorf("fs: URL host %q not supported (use file:///path/...)", cfg.URL.Host)
	}
	root := cfg.URL.Path
	if root == "" {
		return errors.New("fs: empty URL path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("fs: resolve %q: %w", root, err)
	}
	p.root = abs
	return nil
}

// Close is a no-op (no persistent connections).
func (p *Plugin) Close() error { return nil }

// FreeSpace reports the underlying volume's total + available
// bytes by stat'ing the repo root. Implements the optional
// storage.FreeSpaceAware interface.
//
// AvailableBytes uses Bavail (the unprivileged free count)
// rather than Bfree (the absolute free count) because backups
// run under a non-root pgbackup user; the difference matters
// on volumes with reserved-blocks-for-root.
//
// A statfs failure surfaces as an error rather than zero —
// callers (capacity.Preflight) treat the unknown case as
// "fail-open"; we don't want to mistake a probe failure for a
// full disk.
func (p *Plugin) FreeSpace(ctx context.Context) (storage.FreeSpaceInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.FreeSpaceInfo{}, err
	}
	if p.root == "" {
		return storage.FreeSpaceInfo{}, errors.New("fs: FreeSpace called before Open")
	}
	return statfsFreeSpace(p.root)
}

// Root returns the resolved repository root. Useful for tests + diagnostics.
func (p *Plugin) Root() string { return p.root }

// resolve joins root + key after sanity-checking the key. We refuse keys
// that escape the root via ".." or by being absolute, so a buggy caller
// can't write outside the repository. We do NOT silently rewrite escaping
// keys to in-root names — that would let a typo turn into a wrong-place
// write that's hard to debug.
func (p *Plugin) resolve(key string) (string, error) {
	if p.root == "" {
		return "", errors.New("fs: plugin not opened")
	}
	if key == "" || key == "." {
		return "", fmt.Errorf("fs: empty key")
	}
	if filepath.IsAbs(key) {
		return "", fmt.Errorf("fs: key %q is absolute; keys must be relative to the repo root", key)
	}
	cleaned := filepath.ToSlash(filepath.Clean(key))
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return "", fmt.Errorf("fs: key %q escapes root", key)
		}
	}
	return filepath.Join(p.root, cleaned), nil
}

// Put implements storage.StoragePlugin.
//
// fs operations are POSIX syscalls that don't honour ctx natively
// (filepath.Walk, os.Open, os.Stat, etc. ignore the context).
// Adding a ctx.Err() check at the top of each public entry point
// gives the caller an early-bail when their context is already
// cancelled — saving one syscall round-trip and propagating
// cancellation cleanly into the storage layer's error contract.
// In-flight syscalls remain uninterruptible (POSIX has no fix for
// that) but the most common cancellation pattern — Ctrl-C between
// many small operations — is now respected.
func (p *Plugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := ctx.Err(); err != nil {
		return storage.PutResult{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.PutResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), repoDirMode); err != nil {
		return storage.PutResult{}, fmt.Errorf("fs: mkdir parent of %q: %w", key, err)
	}

	if opts.IfNotExists {
		return p.putExclusive(ctx, key, full, r, opts)
	}
	return p.putOverwrite(ctx, key, full, r, opts)
}

// putExclusive opens the final path with O_EXCL so the existence check is
// atomic with creation. No tmp file involved — if the open succeeds the
// caller owns the path.
func (p *Plugin) putExclusive(ctx context.Context, key, full string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if opts.Durability == storage.DurabilityDeferred {
		return p.putDeferred(ctx, key, full, r, opts)
	}
	// Atomic create-if-not-exists. Write the full body to a staging
	// file, fsync it, then link it into place. link(2) fails with
	// EEXIST when the key already exists (→ ErrAlreadyExists); and,
	// unlike O_EXCL-then-write, the key appears ONLY once its content
	// is complete — so a racing reader never observes a half-written
	// file. (O_EXCL-then-write left a window where the key existed but
	// was still empty: a concurrent reader could read 0 bytes and fail
	// to parse it. Surfaced by the backup-lease concurrency test.)
	staging := full + ".excl-" + randHex()
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, repoFileMode)
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("fs: open staging for %q: %w", key, err)
	}
	res, werr := writeAndVerify(ctx, f, r, opts)
	if werr != nil {
		_ = f.Close()
		_ = os.Remove(staging)
		return res, werr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(staging)
		return res, fmt.Errorf("fs: fsync %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(staging)
		return res, fmt.Errorf("fs: close staging for %q: %w", key, err)
	}
	if err := os.Link(staging, full); err != nil {
		_ = os.Remove(staging)
		if errors.Is(err, stdfs.ErrExist) {
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
		return storage.PutResult{}, fmt.Errorf("fs: link %q: %w", key, err)
	}
	// Drop the staging name; `full` keeps the (fully-written) inode.
	_ = os.Remove(staging)
	// Durably commit the parent dentry so a power loss after this
	// point doesn't lose the new file. See syncDir for why this
	// matters even when f.Sync() succeeded.
	if err := syncDir(filepath.Dir(full)); err != nil {
		return res, fmt.Errorf("fs: fsync parent of %q: %w", key, err)
	}
	res.Key = key
	return res, nil
}

// putDeferred is the IfNotExists Put path for storage.DurabilityDeferred.
//
// It does NOT create the file at its final content-addressed key —
// a crash before the Barrier would leave a truncated file there, and
// a later run's O_EXCL dedup would mistake it for a valid chunk. It
// writes the content to a staging temp instead; Barrier fsyncs the
// temp and only THEN links it to the final key. A file at a final
// key therefore always has durable, complete content.
//
// The final-key Stat up front is the cross-run dedup fast path: a
// file there was published by a completed Barrier, so it is valid —
// re-staging it would just be wasted IO that the Barrier's link
// would discard with EEXIST anyway.
func (p *Plugin) putDeferred(ctx context.Context, key, full string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if _, err := os.Stat(full); err == nil {
		return storage.PutResult{}, storage.ErrAlreadyExists
	} else if !errors.Is(err, stdfs.ErrNotExist) {
		return storage.PutResult{}, fmt.Errorf("fs: stat %q: %w", key, err)
	}
	staging := full + ".deferred-" + randHex()
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, repoFileMode)
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("fs: open staging for %q: %w", key, err)
	}
	res, werr := writeAndVerify(ctx, f, r, opts)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(staging)
		return res, werr
	}
	if cerr != nil {
		_ = os.Remove(staging)
		return res, fmt.Errorf("fs: close staging for %q: %w", key, cerr)
	}
	p.recordDeferred(staging, full)
	res.Key = key
	return res, nil
}

// putOverwrite writes <full>.tmp, fsyncs, then renames into place. On
// failure the tmp is removed.
func (p *Plugin) putOverwrite(ctx context.Context, key, full string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	// RANDOM staging suffix (not a fixed "<full>.tmp"): two concurrent
	// overwrites of the SAME key would otherwise share one tmp path —
	// O_TRUNC'ing and writing it simultaneously — and the rename could
	// publish a TORN/partial file. Each writer now stages at a distinct
	// path; os.Rename atomically publishes one complete content
	// (last-writer-wins, which is the overwrite contract). Mirrors the
	// IfNotExists path's `.excl-<rand>` staging.
	tmp := full + ".hstmp-" + randHex()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, repoFileMode)
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("fs: open tmp %q: %w", tmp, err)
	}
	cleanup := func() { _ = os.Remove(tmp) }

	res, err := writeAndVerify(ctx, f, r, opts)
	if err != nil {
		_ = f.Close()
		cleanup()
		return res, err
	}
	// putOverwrite is always inline-durable: PutOptions.Durability is
	// ignored here. The deferred path exists only for the IfNotExists
	// (chunk) Put — see putDeferred — which is the high-volume case;
	// overwrite Puts are low-volume (markers) and a per-call fsync is
	// the right, simple default for them.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return res, fmt.Errorf("fs: fsync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return res, fmt.Errorf("fs: close %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		cleanup()
		return res, fmt.Errorf("fs: rename %q -> %q: %w", tmp, full, err)
	}
	// fsync the parent dir so the rename is durable across a system
	// crash.  POSIX rename(2) is atomic but only at the in-memory
	// dentry layer; the on-disk directory inode has to be flushed
	// for the rename to survive a power loss.  Without this fsync
	// the manifest commit / WAL segment commit looks committed in
	// userspace but vanishes after a hard reboot.
	if err := syncDir(filepath.Dir(full)); err != nil {
		return res, fmt.Errorf("fs: fsync parent of %q: %w", key, err)
	}
	res.Key = key
	return res, nil
}

// writeAndVerify streams r into w, hashing as it goes, and returns the
// PutResult. When opts.ContentSHA256 is non-zero, mismatched hashes
// produce ErrChecksumMismatch.
func writeAndVerify(_ context.Context, w io.Writer, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	h := sha256.New()
	mw := io.MultiWriter(w, h)
	n, err := io.Copy(mw, r)
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("fs: write body: %w", err)
	}
	if opts.ContentLength > 0 && opts.ContentLength != n {
		return storage.PutResult{}, fmt.Errorf("fs: short write: declared %d, got %d", opts.ContentLength, n)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	zero := [32]byte{}
	if opts.ContentSHA256 != zero && opts.ContentSHA256 != sum {
		return storage.PutResult{}, storage.ErrChecksumMismatch
	}
	return storage.PutResult{Size: n, ContentSHA256: sum}, nil
}

// Get returns a ReadCloser for key.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("fs: open %q: %w", key, err)
	}
	return f, nil
}

// Stat returns ObjectInfo for key.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return storage.ObjectInfo{}, storage.ErrNotFound
		}
		return storage.ObjectInfo{}, fmt.Errorf("fs: stat %q: %w", key, err)
	}
	return storage.ObjectInfo{
		Key:     key,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

// List walks files under prefix in the filesystem rooted at p.root.
// Directories are not yielded; only regular files.
func (p *Plugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if p.root == "" {
			yield(storage.ObjectInfo{}, errors.New("fs: plugin not opened"))
			return
		}
		// Pre-walk ctx check; the per-entry check inside WalkDir
		// covers cooperative cancellation during a deep tree walk.
		if err := ctx.Err(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		base := p.root
		if prefix != "" {
			base = filepath.Join(p.root, filepath.Clean("/"+prefix))
		}
		walkErr := filepath.WalkDir(base, func(path string, d stdfs.DirEntry, err error) error {
			// Honour ctx between dir entries — large trees
			// (chunks/sha256/aa/bb/... × 65k buckets) need this
			// to be Ctrl-C interruptive.
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				if errors.Is(err, stdfs.ErrNotExist) {
					// Missing prefix directory yields an empty result —
					// listing a not-yet-populated subtree is normal.
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			// Skip fs-internal staging/temp files — they are an
			// implementation detail of atomic writes (a brief
			// staging name that's linked into place then removed) and
			// must never surface as a real key. Filtering by name
			// also avoids lstat-ing a file that a concurrent writer is
			// about to remove.
			if isFSStagingName(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(p.root, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				// A file that vanished between readdir and lstat (a
				// concurrent writer's staging file, or a key deleted
				// mid-walk) is not a list error — just skip it.
				if errors.Is(err, stdfs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !yield(storage.ObjectInfo{
				Key:     filepath.ToSlash(rel),
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}, nil) {
				return stdfs.SkipAll
			}
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, stdfs.SkipAll) {
			yield(storage.ObjectInfo{}, walkErr)
		}
	}
}

// Delete removes key. Removing a non-existent key is a no-op.
//
// Durability: after the unlink we fsync the parent directory so the
// removal survives a system crash.  Without the fsync, a kernel
// would happily report the unlink as successful but a power loss
// before the inode is flushed would resurrect the file on next
// boot — confusing for any caller that observed the successful
// delete (e.g. GC, retention rotation, kms shred).
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("fs: delete %q: %w", key, err)
	}
	if err := syncDir(filepath.Dir(full)); err != nil {
		return fmt.Errorf("fs: fsync parent of %q: %w", key, err)
	}
	return nil
}

// RenameIfNotExists atomically links src -> dst (failing if dst exists)
// and unlinks src. link(2) is atomic on every POSIX system; on EEXIST
// we get ErrAlreadyExists without ever touching dst.
func (p *Plugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	srcFull, err := p.resolve(src)
	if err != nil {
		return err
	}
	dstFull, err := p.resolve(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstFull), repoDirMode); err != nil {
		return fmt.Errorf("fs: mkdir parent of %q: %w", dst, err)
	}
	if err := os.Link(srcFull, dstFull); err != nil {
		if errors.Is(err, stdfs.ErrExist) {
			return storage.ErrAlreadyExists
		}
		return fmt.Errorf("fs: link %q -> %q: %w", src, dst, err)
	}
	// Durably commit the destination dentry before unlinking the
	// source: if the system crashes between link and unlink,
	// re-syncing on recovery has to find dst already committed.
	// Without the fsync we could end up with a state where neither
	// src nor dst is durable, even though we observed both in
	// userspace.
	if err := syncDir(filepath.Dir(dstFull)); err != nil {
		return fmt.Errorf("fs: fsync parent of %q: %w", dst, err)
	}
	if err := os.Remove(srcFull); err != nil {
		// link succeeded but unlink failed — dst is correct, src lingers.
		// We surface this as an error so the caller can retry the
		// unlink rather than have a stale file silently persist.
		return fmt.Errorf("fs: unlink old %q after link: %w", src, err)
	}
	// fsync src's parent so the unlink is durable too — otherwise
	// a crash after this point leaves a phantom src on disk.
	if err := syncDir(filepath.Dir(srcFull)); err != nil {
		return fmt.Errorf("fs: fsync parent of %q: %w", src, err)
	}
	return nil
}

// SetRetention is unsupported on plain fs; returns ErrUnsupported so
// callers route to a real WORM backend (S3 Object Lock, Azure immutable
// blob, NetApp SnapLock, etc.).
func (p *Plugin) SetRetention(_ context.Context, _ string, _ time.Time, _ storage.WORMMode) error {
	return storage.ErrUnsupported
}

// Barrier (storage.StoragePlugin) makes every preceding
// DurabilityDeferred Put crash-durable. The implementation is
// platform-split: barrier_linux.go uses a single syncfs(2);
// barrier_other.go fsyncs each deferred file + dir individually.

// syncDir fsyncs a directory inode so a metadata change (rename,
// link, unlink, create) committed within it survives a system
// crash.  Required after every metadata-modifying syscall
// because POSIX file-content fsync (via *os.File.Sync) only
// flushes the file's data + its own inode, NOT the parent
// directory's dentry list.  Without this, a kernel can report
// rename(2) / unlink(2) / link(2) as successful but lose the
// change on a power loss before the parent dentry is flushed.
//
// On Linux + macOS this is a real fsync syscall against the
// directory fd.  On Windows, fsync on a directory is a no-op
// at the syscall layer and Go's *os.File.Sync returns nil; the
// helper still works (just doesn't do anything) so the caller
// path is portable.
//
// Best-effort failure path: a failure to fsync the directory
// is reported back to the caller but doesn't undo the
// data/metadata change.  The caller can retry; the underlying
// modification is already in place either way.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		// A non-existent dir means nothing to fsync — typical for
		// the first put into a fresh prefix where MkdirAll happened
		// before us but we're racing with another worker.  Silent
		// success keeps this path lock-free.
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}
