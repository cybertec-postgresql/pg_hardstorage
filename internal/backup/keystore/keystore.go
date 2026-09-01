// Package keystore loads or bootstraps the signing keypair used to
// sign manifests.
//
// On-disk layout (relative to the resolved keyring directory):
//
//	manifest_signing.ed25519       private key (mode 0600)
//	manifest_signing.pub           public key (mode 0644)
//
// Both files are PEM-wrapped via the backup package's helpers. The
// private file MUST be mode 0600 — we refuse to load it if the
// permissions are looser than that on first read, surfacing the
// problem as a structured error rather than silently trusting a
// world-readable secret.
package keystore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
)

// File names within the keyring directory.
const (
	PrivateKeyFile = "manifest_signing.ed25519"
	PublicKeyFile  = "manifest_signing.pub"
)

// Mode bits we allow on the private-key file: owner read/write.
// Any GROUP or OTHER bit is refused — that is the property the gate
// exists for. Owner-only subsets (0400: a read-only mounted Secret)
// are fine; the old exact-0600 match refused them and shipped a Helm
// chart whose own defaultMode could never load (#46).
const privateKeyMode fs.FileMode = 0o600

// LoadOrGenerate returns a Signer + Verifier rooted on the keypair
// stored in keyringDir. If neither file exists, a fresh keypair is
// generated, written, and returned.
//
// Refuses to load when:
//   - only one of the two files exists (paired writes are mandatory)
//   - the private key file has permissions broader than 0600
func LoadOrGenerate(keyringDir string) (*backup.Signer, *backup.Verifier, error) {
	privPath := filepath.Join(keyringDir, PrivateKeyFile)
	pubPath := filepath.Join(keyringDir, PublicKeyFile)

	privExists, err := fileExists(privPath)
	if err != nil {
		return nil, nil, err
	}
	pubExists, err := fileExists(pubPath)
	if err != nil {
		return nil, nil, err
	}

	switch {
	case privExists && pubExists:
		return load(privPath, pubPath)
	case privExists != pubExists:
		// One half missing — refuse to silently regenerate.
		return nil, nil, fmt.Errorf("keystore: only one of {%s, %s} exists; manage the pair together",
			PrivateKeyFile, PublicKeyFile)
	default:
		return generate(keyringDir, privPath, pubPath)
	}
}

// fileExists returns whether path is a regular file. Returns an error
// for any non-NotExist stat failure (permission denied, etc.).
func fileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("keystore: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("keystore: %s is not a regular file", path)
	}
	return true, nil
}

// load reads both PEM files, asserts the private key's permissions are
// exactly 0600, and returns Signer + Verifier.
func load(privPath, pubPath string) (*backup.Signer, *backup.Verifier, error) {
	info, err := os.Stat(privPath)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: stat private key: %w", err)
	}
	if mode := info.Mode().Perm(); mode&^privateKeyMode != 0 {
		return nil, nil, fmt.Errorf("keystore: %s has mode %#o; require %#o (chmod 0600 the file)",
			privPath, mode, privateKeyMode)
	}

	privPEM, err := os.ReadFile(privPath)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: read private key: %w", err)
	}
	pubPEM, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: read public key: %w", err)
	}
	signer, err := backup.LoadSigner(privPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: parse private key: %w", err)
	}
	verifier, err := backup.LoadVerifier(pubPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("keystore: parse public key: %w", err)
	}
	return signer, verifier, nil
}

// generate writes a fresh keypair atomically into keyringDir.
//
// Atomicity: we write into temporary files and rename — if anything
// fails midway, we don't leave a dangling private key. The keyring
// directory is created with mode 0700 so any later file the user adds
// inherits a tight enclosing permission.
func generate(keyringDir, privPath, pubPath string) (*backup.Signer, *backup.Verifier, error) {
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("keystore: mkdir %s: %w", keyringDir, err)
	}
	// The keyring directory's own dentry has to reach disk too: if it
	// is lost, the keypair inside it is lost with it, however durably
	// the two files themselves were written.
	if err := fsutil.SyncDir(filepath.Dir(keyringDir)); err != nil {
		return nil, nil, fmt.Errorf("keystore: fsync parent of %s: %w", keyringDir, err)
	}

	privPEM, pubPEM, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	// fsutil.WriteFileAtomic, not a local tmp+rename: it adds the
	// SyncDir(parent) that a hand-rolled version keeps forgetting.
	// Losing the rename of the manifest-signing keypair to a power
	// loss is not recoverable — the next LoadOrGenerate makes a
	// DIFFERENT key and every manifest signed with the old one stops
	// verifying, or (if only one of the two renames survives) refuses
	// to start at all with "manage the pair together".
	//
	// Public key first — it's the less sensitive write; failures here
	// don't leave a dangling private key on disk.
	if err := fsutil.WriteFileAtomic(pubPath, pubPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("keystore: write public key: %w", err)
	}
	if err := fsutil.WriteFileAtomic(privPath, privPEM, 0o600); err != nil {
		// Roll back the public-key write so a retry can regenerate cleanly.
		_ = os.Remove(pubPath)
		return nil, nil, fmt.Errorf("keystore: write private key: %w", err)
	}

	// LoadSigner / LoadVerifier failures here would mean we wrote
	// PEM bytes that don't parse — a bug in GenerateKeypair, not an
	// environmental issue. But keeping orphan files on disk would
	// trip the next LoadOrGenerate call into an "exists but won't
	// load" state that's worse than a clean retry. Roll back BOTH
	// files on failure so a second invocation regenerates from
	// scratch.
	signer, err := backup.LoadSigner(privPEM)
	if err != nil {
		_ = os.Remove(privPath)
		_ = os.Remove(pubPath)
		return nil, nil, fmt.Errorf("keystore: re-parse generated private key: %w", err)
	}
	verifier, err := backup.LoadVerifier(pubPEM)
	if err != nil {
		_ = os.Remove(privPath)
		_ = os.Remove(pubPath)
		return nil, nil, fmt.Errorf("keystore: re-parse generated public key: %w", err)
	}
	return signer, verifier, nil
}
