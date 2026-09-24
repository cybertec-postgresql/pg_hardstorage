// kek.go — local-file KEK loader (kek.bin) with strict 0600 permission check.
package keystore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// KEKFileName is the canonical file name for the local-file-system
// KEK we ship in v0.1. Lives under the keyring directory next to
// the signing key. 32 raw bytes (no encoding).
const KEKFileName = "kek.bin"

// kekFileMode is the only permission bit pattern we accept for
// kek.bin on read. Same posture as the signing key (see
// keystore.go privateKeyMode): world- or group-readable secrets
// are refused loudly, not silently accepted. Symmetric KEK is at
// least as sensitive as the signing key — it unwraps every
// backup's DEK — so the asymmetry that used to exist (strict on
// signing key, lax on KEK) is corrected here.
const kekFileMode fs.FileMode = 0o600

// assertKEKFileMode stat()s path and refuses any permission bits
// other than kekFileMode. Returns a structured error message
// telling the operator exactly what to chmod. ENOENT is passed
// through for the caller's not-exist branch.
func assertKEKFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("keystore: %s is not a regular file", path)
	}
	// Owner-only subsets pass (0400 = read-only Secret mount); any
	// group/other bit is the actual hazard and is refused.
	if mode := info.Mode().Perm(); mode&^kekFileMode != 0 {
		return fmt.Errorf("keystore: %s has mode %#o; require %#o (chmod 0600 the file)",
			path, mode, kekFileMode)
	}
	return nil
}

// KEKRefLocal is the manifest's KEKRef value when the KEK was loaded
// from the local keyring file.+ adds kms-backed KEKs with refs
// like "kms:aws:arn:..." or "kms:vault:secret/..." — the manifest
// records WHICH ref so restore can pick the right resolver.
const KEKRefLocal = "local:default"

// IsLocalRef reports whether ref resolves to the local keyring's
// kek.bin — "", "local:default", and every other local:* ref a local
// rotation stamps (see KEKResolver). Anything that must know "which
// objects does THIS kek.bin protect" (kms shred's blast radius, kms
// verify) matches with this, never with == KEKRefLocal.
func IsLocalRef(ref string) bool {
	return ref == "" || strings.HasPrefix(ref, "local:")
}

// LoadOrGenerateKEK reads the KEK from <keyringDir>/kek.bin, or
// generates and writes a fresh one if absent.
//
// Idempotent: repeated calls with the same keyringDir return the
// same key. The file is mode 0600 — the keyring directory itself
// should already be 0700 (created by paths.Resolve).
//
// Returns the loaded/generated key + a bool reporting "did we just
// generate one?" so the caller (init wizard) can surface it.
func LoadOrGenerateKEK(keyringDir string) ([encryption.KeyLen]byte, bool, error) {
	var zero [encryption.KeyLen]byte
	if keyringDir == "" {
		return zero, false, errors.New("keystore: empty keyring dir")
	}
	path := filepath.Join(keyringDir, KEKFileName)

	// Permission gate BEFORE the read — refuse to load a
	// world/group-readable KEK so an operator running with a
	// chmod-mistake gets a loud error instead of a silently-broken
	// security posture.
	if err := assertKEKFileMode(path); err == nil {
		body, err := os.ReadFile(path)
		if err != nil {
			return zero, false, fmt.Errorf("keystore: read %s: %w", path, err)
		}
		if len(body) != encryption.KeyLen {
			return zero, false, fmt.Errorf("keystore: %s is %d bytes, want %d (corrupt or wrong file)",
				path, len(body), encryption.KeyLen)
		}
		if err := rejectZeroKEK(path, body); err != nil {
			return zero, false, err
		}
		var kek [encryption.KeyLen]byte
		copy(kek[:], body)
		return kek, false, nil
	} else if !os.IsNotExist(err) {
		return zero, false, fmt.Errorf("keystore: %w", err)
	}

	// Generate fresh.
	//
	// Every step below is checked, because this function returns an
	// in-memory KEK that the caller immediately uses to wrap a DEK and
	// encrypt a backup. If the on-disk copy did not actually make it to
	// stable storage, that backup is sealed under a key that exists
	// nowhere — unrecoverable, with nothing to notice at the time. A
	// dropped Close error or an unflushed parent dentry is enough.
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		return zero, false, fmt.Errorf("keystore: mkdir %s: %w", keyringDir, err)
	}
	if err := fsutil.SyncDir(filepath.Dir(keyringDir)); err != nil {
		return zero, false, fmt.Errorf("keystore: fsync parent of %s: %w", keyringDir, err)
	}
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		return zero, false, fmt.Errorf("keystore: random KEK: %w", err)
	}
	// O_EXCL guards against a racing init from a parallel process.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return zero, false, fmt.Errorf("keystore: create %s: %w", path, err)
	}
	if _, err := f.Write(kek[:]); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return zero, false, fmt.Errorf("keystore: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return zero, false, fmt.Errorf("keystore: fsync %s: %w", path, err)
	}
	// Checked, not deferred. Close is where a delayed-allocation or
	// network filesystem reports ENOSPC/EDQUOT, and a deferred Close
	// discards that: the caller would go on to encrypt a backup under a
	// KEK whose file was never written.
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return zero, false, fmt.Errorf("keystore: close %s: %w", path, err)
	}
	// f.Sync() flushed the KEK's bytes and inode but not the dentry
	// that O_CREATE added to the keyring directory, so without this the
	// file can vanish entirely on a power loss. (The repo's
	// rename/SyncDir guard does not cover this site: there is no rename
	// here, just a create.)
	if err := fsutil.SyncDir(keyringDir); err != nil {
		return zero, false, fmt.Errorf("keystore: fsync %s: %w", keyringDir, err)
	}
	return kek, true, nil
}

// rejectZeroKEK refuses an all-zero key. ShredKEK zeroes the file before
// unlinking it, so a crash in between (older binaries zeroed in place)
// left a 32-byte kek.bin of zeros that loaded as a perfectly good KEK:
// backups were then "encrypted" under a publicly known key, and restores
// of pre-shred backups failed with a baffling unwrap error. crypto/rand
// never yields 32 zero bytes, so this rejects nothing real.
func rejectZeroKEK(path string, body []byte) error {
	for _, b := range body {
		if b != 0 {
			return nil
		}
	}
	return fmt.Errorf("keystore: %s is all zeros — the remains of an interrupted `kms shred`, not a key; "+
		"finish the shred (re-run `kms shred`) or restore kek.bin from backup", path)
}

// shredPendingName is where ShredKEK moves kek.bin before destroying
// it, so the canonical path stops being a KEK atomically.
const shredPendingName = KEKFileName + ".shredding"

// KEKExists reports whether a KEK is present at the canonical path.
// Used by the backup CLI to decide whether to enable encryption
// without requiring the operator to opt in explicitly.
func KEKExists(keyringDir string) bool {
	if keyringDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(keyringDir, KEKFileName))
	return err == nil
}

// ShredKEK irreversibly destroys the local KEK at <keyringDir>/kek.bin.
//
// What "irreversibly" means here: the file's bytes are overwritten
// with zeros + then the file is unlinked. We do NOT make
// claims about block-level secure erase — modern filesystems +
// SSDs reorder writes invisibly to userspace, and a journaling FS
// may keep the old contents in a transaction log for some window.
// Operators who need block-level secure erase should run
// `shred(1) -uvz` on top, or destroy the underlying volume.
//
// What we DO guarantee:
//   - the path stops being readable via normal file APIs (os.ReadFile
//     surfaces ENOENT immediately)
//   - any process that already has the KEK in memory is unaffected
//     (in-memory key material has its own lifecycle)
//   - subsequent calls with the same dir return ErrKEKAlreadyShred
//     so the caller's audit chain reflects a clear "no-op already
//     done" rather than a misleading success.
//
// Pre-condition: the operator has already authorised the shred via
// the n-of-m approval workflow. This function does NOT check;
// callers are responsible. The CLI's `kms shred` enforces the
// approval gate.
func ShredKEK(keyringDir string) error {
	if keyringDir == "" {
		return errors.New("keystore: empty keyring dir")
	}
	canonical := filepath.Join(keyringDir, KEKFileName)
	path := filepath.Join(keyringDir, shredPendingName)

	// Rename-then-zero. Moving kek.bin aside first makes the canonical
	// path stop being a KEK in one atomic step; zeroing in place (the
	// old order) meant a crash between the overwrite and the unlink
	// left an all-zero kek.bin that loaded as a key. A crash after the
	// rename leaves only kek.bin.shredding, which the next ShredKEK
	// finishes.
	if _, err := os.Stat(canonical); err == nil {
		if err := os.Rename(canonical, path); err != nil {
			return fmt.Errorf("keystore: move %s aside for shred: %w", canonical, err)
		}
		if err := fsutil.SyncDir(keyringDir); err != nil {
			return fmt.Errorf("keystore: fsync %s after rename: %w", keyringDir, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("keystore: stat %s: %w", canonical, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrKEKAlreadyShred
		}
		return fmt.Errorf("keystore: stat %s: %w", path, err)
	}
	// Best-effort overwrite. Open for write (no O_TRUNC; we want to
	// hit the same bytes), zero them, fsync, then unlink.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("keystore: open %s for shred: %w", path, err)
	}
	zero := make([]byte, info.Size())
	if _, err := f.Write(zero); err != nil {
		_ = f.Close()
		return fmt.Errorf("keystore: zero %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("keystore: fsync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("keystore: close %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("keystore: unlink %s: %w", path, err)
	}
	// Make the unlink durable. The zero-overwrite above is already
	// fsynced and is what actually makes the key unrecoverable, so a
	// lost unlink leaves an inert file rather than a live KEK — but
	// this function's contract is "the path stops being readable", and
	// a crypto-shred is exactly the operation where an operator is
	// entitled to that being true after a reboot.
	if err := fsutil.SyncDir(keyringDir); err != nil {
		return fmt.Errorf("keystore: fsync %s after shred: %w", keyringDir, err)
	}
	return nil
}

// ErrKEKAlreadyShred is returned by ShredKEK when the KEK file is
// already absent. The CLI surfaces this as an idempotent no-op
// success rather than a hard error — re-running shred against a
// shredded keyring is a perfectly reasonable cleanup operation.
var ErrKEKAlreadyShred = errors.New("keystore: KEK already shred (no kek.bin in keyring)")

// KEKResolver returns a function suitable for restore.Options.KEKForRef.
// It looks up KEKs by the manifest's KEKRef:
//
//   - "local:default" → loads from <keyringDir>/kek.bin
//
// Other refs return an error in v0.1; KMS resolvers add additional
// branches.
func KEKResolver(keyringDir string) func(ref string) ([encryption.KeyLen]byte, error) {
	return func(ref string) ([encryption.KeyLen]byte, error) {
		var zero [encryption.KeyLen]byte
		// Any local:* ref resolves to the keyring file: `kms rotate`
		// refuses old==new refs, so a local rotation necessarily
		// stamps a NEW local ref (e.g. "local:v2") on every rotated
		// manifest — and the operator's keyring holds the matching
		// key material. Restricting this resolver to exactly
		// "local:default" made every rotated backup unrestorable by
		// any shipped code path.
		if IsLocalRef(ref) {
			ref = KEKRefLocal
		}
		switch ref {
		case "", KEKRefLocal:
			path := filepath.Join(keyringDir, KEKFileName)
			// Permission gate BEFORE the read. Same posture as
			// LoadOrGenerateKEK — a chmod-mistake surfaces as a
			// clear error rather than a silently-broken security
			// posture. Restore-time KEK resolution is a hot path
			// (every chunk decrypt eventually goes through here);
			// the stat is cheap and the safety property matters.
			if err := assertKEKFileMode(path); err != nil {
				return zero, fmt.Errorf("keystore: %w", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return zero, fmt.Errorf("keystore: read %s: %w", path, err)
			}
			if len(body) != encryption.KeyLen {
				return zero, fmt.Errorf("keystore: %s is %d bytes, want %d",
					path, len(body), encryption.KeyLen)
			}
			if err := rejectZeroKEK(path, body); err != nil {
				return zero, err
			}
			var kek [encryption.KeyLen]byte
			copy(kek[:], body)
			return kek, nil
		default:
			return zero, fmt.Errorf("keystore: unknown KEKRef %q (v0.1 only supports %q)", ref, KEKRefLocal)
		}
	}
}
