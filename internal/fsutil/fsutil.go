// Package fsutil centralises the "write a small file durably"
// patterns the rest of pg_hardstorage uses for state files,
// config files, PostgreSQL recovery-control files (backup_label,
// tablespace_map, standby.signal, recovery.signal), evidence
// bundles, and any other artefact whose loss after a crash would
// be a correctness issue.
//
// The fs storage plugin already implements the gold-standard
// pattern (see internal/plugin/storage/fs/fs.go: f.Sync() +
// syncDir() after every metadata-modifying syscall) — fsutil
// extracts that pattern so call sites outside the storage plugin
// can use it without re-implementing the dance.
//
// Why not just os.WriteFile?  os.WriteFile is opener+writer+
// closer with NO sync.  On a crash after WriteFile returns but
// before the kernel flushes:
//
//   - The file's data may be lost (if not yet flushed).
//   - The file's directory entry may be lost (if the parent
//     dentry list hasn't hit disk).
//
// POSIX is explicit: fsync(fd) flushes the file's content and
// inode but NOT the parent directory.  An unflushed parent
// directory can cause a successful rename() to vanish on power
// loss.  The fs plugin's syncDir() handles this; fsutil exports
// it so other packages share the contract.
//
// Three helpers are exposed:
//
//   - WriteFileSync(path, data, mode):  os.WriteFile semantics
//     plus f.Sync() and SyncDir(parent).  Use when the file is
//     written once into a directory the caller controls (e.g.
//     restore-target backup_label, recovery-signal files) and
//     a concurrent reader observing a half-written file is not
//     a concern.
//
//   - WriteFileAtomic(path, data, mode):  write to "<path>.tmp",
//     fsync, atomic rename to path, SyncDir(parent).  Use when
//     a concurrent reader could observe the file mid-write
//     (config files, on-disk state, anything PG or another
//     agent goroutine might read concurrently).
//
//   - SyncDir(dir):  exported syncDir for callers that already
//     own the write+rename loop and only need the parent-
//     directory durability guarantee.
//
// All three are best-effort on platforms where directory fsync
// is a no-op (Windows): they return nil rather than failing.
package fsutil

import (
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
)

// WriteFileSync writes data to path with the given mode, fsyncs
// the file, and fsyncs the parent directory.  Equivalent to
// os.WriteFile plus the durability dance.
//
// On a crash after the call returns successfully, the kernel
// guarantees the data is on stable storage AND the parent
// directory's dentry list reflects the new file.
//
// If the file already exists at path it is truncated and
// rewritten in place — this is NOT atomic from a concurrent
// reader's perspective.  Use WriteFileAtomic for that case.
func WriteFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("fsutil: open %q: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsutil: write %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsutil: fsync %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fsutil: close %q: %w", path, err)
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// WriteFileAtomic writes data to "<path>.tmp" with the given
// mode, fsyncs the tmp, atomically renames it to path, and
// fsyncs the parent directory.  A concurrent reader at path
// observes either the previous content or the new content,
// never a half-written tear.
//
// O_EXCL on the tmp open guards against a stale tmp from a
// previous crashed write — surfacing the error rather than
// silently trusting whatever bytes are on disk.  Callers that
// need to tolerate a stale tmp should explicitly remove it
// first.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	// O_EXCL: refuse if a stale tmp from a prior crash is
	// hanging around. Forces explicit cleanup before retry,
	// which keeps subtle "two writers raced" bugs visible.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("fsutil: open tmp %q: %w", tmp, err)
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("fsutil: write tmp %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("fsutil: fsync tmp %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("fsutil: close tmp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("fsutil: rename %q -> %q: %w", tmp, path, err)
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		// The data is committed at this point; the dir-fsync
		// failure is reported but not undone.  The caller can
		// retry; a re-rename of an absent tmp will fail
		// noisily, signalling the prior write succeeded.
		return err
	}
	return nil
}

// SyncDir fsyncs a directory inode so a metadata change
// (rename, link, unlink, create) committed within it survives
// a system crash.
//
// Required after every metadata-modifying syscall on POSIX:
// file-content fsync (via *os.File.Sync) only flushes the
// file's data + its own inode, NOT the parent directory's
// dentry list.  Without this dance, a kernel can report
// rename(2) / unlink(2) / link(2) as successful but lose the
// change on a power loss before the parent dentry is flushed.
//
// On Linux + macOS this is a real fsync syscall against the
// directory fd.  On Windows, fsync on a directory is a no-op
// at the syscall layer and Go's *os.File.Sync returns nil; the
// helper still works (just doesn't do anything) so callers
// remain portable.
//
// A non-existent dir returns nil — typical for the first write
// into a fresh prefix where MkdirAll just happened and we're
// racing another worker.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("fsutil: open dir %q for fsync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsutil: fsync dir %q: %w", dir, err)
	}
	return nil
}

// SyncFile fsyncs an already-open *os.File and reports the
// error verbatim.  Convenience for call sites that own the file
// handle (e.g. callers using OpenFile + manual writes).  Pair
// with SyncDir on the parent for full durability.
func SyncFile(f *os.File) error {
	if f == nil {
		return errors.New("fsutil: nil file")
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsutil: fsync %q: %w", f.Name(), err)
	}
	return nil
}
