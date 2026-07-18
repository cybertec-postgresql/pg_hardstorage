// dek_reuse.go — DEK reuse across backups sharing a KEKRef so dedup'd chunks stay decryptable.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

// selectDEK chooses the plaintext DEK for an encrypted backup under
// kekRef. It is the dedup-safety gate: the CAS deduplicates chunks by
// PLAINTEXT hash across every artifact in the repo, so all encrypted
// artifacts under one KEK MUST share one plaintext DEK — a chunk written
// by an earlier backup OR WAL segment is silently reused (IfNotExists) by
// this one, and this manifest's DEK has to decrypt it.
//
// Resolution scans BOTH base-backup manifests and WAL segment manifests
// (sharedkey.Resolve), so a backup converges on a DEK that WAL streaming
// minted first, and vice-versa (issue #106).
//
// A fresh DEK is correct ONLY when we have positively confirmed there is
// NO prior DEK for this KEK anywhere. The two failure shapes that must NOT
// be mistaken for "no prior DEK" — a backend List error (couldn't
// enumerate) and a prior wrapped DEK that won't unwrap — instead FAIL the
// backup: generating a fresh DEK there yields a backup whose deduped chunks
// are unrestorable, discovered only at restore time (issue #28).
func selectDEK(ctx context.Context, sp storage.StoragePlugin, kekRef string, cfg *EncryptionConfig) ([encryption.KeyLen]byte, error) {
	unwrap := func(wrapped []byte) ([]byte, error) {
		return unwrapDEKForReuse(ctx, cfg, wrapped)
	}
	wrap := func(dek [encryption.KeyLen]byte) ([]byte, error) {
		if cfg.Provider != nil {
			return cfg.Provider.WrapDEK(ctx, dek[:])
		}
		return encryption.Wrap(cfg.KEK, dek)
	}
	dek, err := sharedkey.ResolveOrCreate(ctx, sp, kekRef, unwrap, wrap)
	if err != nil {
		return [encryption.KeyLen]byte{}, fmt.Errorf("backup: establish repository encryption domain for KEK %q: %w", kekRef, err)
	}
	return dek, nil
}

// looksLikePrimaryManifest returns true for keys of the shape
// `manifests/<dep>/backups/<id>/manifest.json`.  Excludes the
// `_replicas` redundancy slot and `*.tombstone` markers.
func looksLikePrimaryManifest(key string) bool {
	const (
		prefix = "manifests/"
		suffix = "/manifest.json"
	)
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return false
	}
	rel := strings.TrimPrefix(key, prefix)
	if strings.HasPrefix(rel, "_replicas/") {
		return false
	}
	return true
}

// readManifestNoVerify pulls the manifest bytes off sp at key and
// unmarshals without checking the ed25519 signature.  Only the
// Encryption block is consumed downstream; the wrap-unwrap step
// authenticates the DEK.
func readManifestNoVerify(ctx context.Context, sp storage.StoragePlugin, key string) (*backup.Manifest, bool, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, false, err
	}
	var m backup.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, err
	}
	return &m, true, nil
}

// unwrapDEKForReuse decrypts an existing manifest's WrappedDEK with
// the caller's EncryptionConfig.  Cloud-KMS shape goes through
// Provider.UnwrapDEK; local-KEK shape calls encryption.Unwrap with
// the on-disk KEK.  Returns the plaintext DEK on success.  The
// caller treats every error as a soft miss and falls through to
// fresh-DEK generation.
func unwrapDEKForReuse(ctx context.Context, cfg *EncryptionConfig, wrapped []byte) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("dek-reuse: nil encryption config")
	}
	if cfg.Provider != nil {
		dek, err := cfg.Provider.UnwrapDEK(ctx, wrapped)
		if err != nil {
			return nil, fmt.Errorf("dek-reuse: provider unwrap: %w", err)
		}
		return dek, nil
	}
	dek, err := encryption.Unwrap(cfg.KEK, wrapped)
	if err != nil {
		return nil, fmt.Errorf("dek-reuse: local unwrap: %w", err)
	}
	return dek[:], nil
}
