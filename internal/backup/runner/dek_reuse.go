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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
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
// Resolution goes through sharedkey.ResolveOrMint, which mints the DEK
// via an atomic single-winner PUT on a well-known shared-DEK object (and
// seeds that object from existing base-backup / WAL manifests for legacy
// repos), so a backup converges on the SAME DEK as concurrent WAL
// streaming even when neither has committed a manifest yet (issues #106,
// #31) — not just when one committed first.
//
// A fresh DEK is correct ONLY when we have positively confirmed there is
// NO prior DEK for this KEK anywhere. The two failure shapes that must NOT
// be mistaken for "no prior DEK" — a backend List error (couldn't
// enumerate) and a prior wrapped DEK that won't unwrap — instead FAIL the
// backup: generating a fresh DEK there yields a backup whose deduped chunks
// are unrestorable, discovered only at restore time (issue #28).
func selectDEK(ctx context.Context, sp storage.StoragePlugin, kekRef string, cfg *EncryptionConfig) ([encryption.KeyLen]byte, error) {
	var dek [encryption.KeyLen]byte

	// ResolveOrMint mints the shared DEK atomically (single-winner PUT on
	// the shared-DEK object), so a backup starting concurrently with `wal
	// stream` converges on ONE DEK instead of each minting its own — the
	// old behaviour left deduped WAL/base chunks unrestorable (issue #31).
	res, err := sharedkey.ResolveOrMint(ctx, sp, kekRef,
		func(wrapped []byte) ([]byte, error) { return unwrapDEKForReuse(ctx, cfg, wrapped) },
		func(d [encryption.KeyLen]byte) ([]byte, error) { return wrapDEKForReuse(ctx, cfg, d) },
	)
	if err != nil {
		return dek, fmt.Errorf("backup: cannot determine or mint the shared DEK for KEK %q; refusing a fresh DEK that the CAS's plaintext-hash dedup would leave unrestorable against existing chunks: %w", kekRef, err)
	}
	if res.UnusableCandidate {
		return dek, fmt.Errorf("backup: a prior DEK for KEK %q exists but none of its recorded wrapped form(s) could be unwrapped for reuse; refusing a fresh DEK that would leave deduped chunks unrestorable (verify the KEK material matches this ref)", kekRef)
	}
	if !res.Have {
		return dek, fmt.Errorf("backup: shared-DEK resolution returned no key for KEK %q", kekRef)
	}
	return res.DEK, nil
}

// leaseLossAborter builds the Maintain callback: lease LOSS aborts the
// backup via cancelBackup (another holder owns the deployment — running
// both to completion is exactly what the lease prevents), while
// transient renew errors only emit a warning. Factored out so the abort
// contract is unit-testable.
func leaseLossAborter(deployment string, emit func(*output.Event), cancelBackup func(error)) func(error) {
	return func(rerr error) {
		if errors.Is(rerr, backup.ErrLeaseLost) {
			emit(output.NewEvent(output.SeverityCritical, "backup", "lease_lost").
				WithSubject(output.Subject{Deployment: deployment}).
				WithBody(map[string]any{
					"error":  rerr.Error(),
					"action": "aborting this backup: another holder owns the deployment lease",
				}))
			cancelBackup(fmt.Errorf("backup lease lost: %w", rerr))
			return
		}
		emit(output.NewEvent(output.SeverityWarning, "backup", "lease_renew_failed").
			WithSubject(output.Subject{Deployment: deployment}).
			WithBody(map[string]any{"error": rerr.Error()}))
	}
}

// wrapDEKForReuse wraps a plaintext DEK for storage in the shared-DEK
// object, mirroring unwrapDEKForReuse's custody split (cloud KMS vs local
// KEK).
func wrapDEKForReuse(ctx context.Context, cfg *EncryptionConfig, dek [encryption.KeyLen]byte) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("dek-reuse: nil encryption config")
	}
	if cfg.Provider != nil {
		w, err := cfg.Provider.WrapDEK(ctx, dek[:])
		if err != nil {
			return nil, fmt.Errorf("dek-reuse: provider wrap: %w", err)
		}
		return w, nil
	}
	w, err := encryption.Wrap(cfg.KEK, dek)
	if err != nil {
		return nil, fmt.Errorf("dek-reuse: local wrap: %w", err)
	}
	return w, nil
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
