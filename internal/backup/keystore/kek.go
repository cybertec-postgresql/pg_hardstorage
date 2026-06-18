// kek.go — local-file KEK loader (kek.bin) with strict 0600 permission check.
package keystore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	if mode := info.Mode().Perm(); mode != kekFileMode {
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
		var kek [encryption.KeyLen]byte
		copy(kek[:], body)
		return kek, false, nil
	} else if !os.IsNotExist(err) {
		return zero, false, fmt.Errorf("keystore: %w", err)
	}

	// Generate fresh.
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		return zero, false, fmt.Errorf("keystore: mkdir %s: %w", keyringDir, err)
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
	defer f.Close()
	if _, err := f.Write(kek[:]); err != nil {
		_ = os.Remove(path)
		return zero, false, fmt.Errorf("keystore: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = os.Remove(path)
		return zero, false, fmt.Errorf("keystore: fsync %s: %w", path, err)
	}
	return kek, true, nil
}

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
	path := filepath.Join(keyringDir, KEKFileName)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrKEKAlreadyShred
		}
		return fmt.Errorf("keystore: stat %s: %w", path, err)
	}
	// Best-effort overwrite. Open for write (no O_TRUNC; we want to
	// hit the same bytes), zero them, fsync, then unlink. A failure
	// after the overwrite but before the unlink leaves a file of
	// zeros — still inert but visible; the next ShredKEK call
	// removes it (we treat zero-byte content as "shred-pending").
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
			var kek [encryption.KeyLen]byte
			copy(kek[:], body)
			return kek, nil
		default:
			return zero, fmt.Errorf("keystore: unknown KEKRef %q (v0.1 only supports %q)", ref, KEKRefLocal)
		}
	}
}
