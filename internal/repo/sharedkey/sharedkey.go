// Package sharedkey resolves the single plaintext DEK that every encrypted
// artifact under one (deployment, KEK) MUST share.
//
// Why a shared DEK at all: the CAS deduplicates chunks by PLAINTEXT SHA-256
// across the whole repo, so a chunk first written by a base backup and later
// deduped by a WAL segment (or vice-versa) is stored exactly once. For the
// second writer to read that chunk back it has to decrypt the first writer's
// envelope — which only works if both used the same DEK. A divergent DEK
// leaves deduped chunks unrestorable (issue #28).
//
// Backups already converge via the backup-manifest scan; this package widens
// the search to BOTH namespaces — base-backup manifests (manifests/) and WAL
// segment manifests (wal/) — so the writer that runs first mints the DEK and
// whichever runs second finds and reuses it, in either order.
package sharedkey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Scheme is the only envelope scheme DEK reuse applies to.
const Scheme = "aes-256-gcm"

// sharedDEKPrefix holds the authoritative shared-DEK object — one per
// KEKRef. It is the serialization point that makes concurrent minting
// converge: the manifest scan (Resolve) only sees COMMITTED manifests,
// so two writers (e.g. `wal stream` + `backup`) that both start before
// either commits would each mint a DIFFERENT fresh DEK. Full-page images
// in WAL then dedup against base-backup chunks (same plaintext hash) but
// are stored under one DEK while the other's manifest references them
// under the other DEK — leaving a "successful" backup unrestorable
// (issue #31). Minting the DEK via an atomic IfNotExists PUT on this
// single object serialises the mint so every writer shares one DEK.
const sharedDEKPrefix = "keys/shared-dek/"

// Wrapper wraps a plaintext DEK under the caller's KEK custody model —
// local AES-256-GCM wrap or a cloud-KMS round-trip. Mirror of Unwrapper.
type Wrapper func(dek [encryption.KeyLen]byte) ([]byte, error)

// MintResult is the outcome of ResolveOrMint.
type MintResult struct {
	// DEK is the shared plaintext DEK; valid iff Have is true.
	DEK [encryption.KeyLen]byte
	// Have reports that DEK is set (resolved, adopted, or minted).
	Have bool
	// UnusableCandidate reports that a prior DEK exists for this KEK
	// (in the shared-DEK object or a manifest) but none of its wrapped
	// forms unwrap with the caller's KEK. The caller MUST fail rather
	// than mint a fresh DEK. Mutually exclusive with Have.
	UnusableCandidate bool
}

// sharedDEKKey is the storage key for the shared-DEK object of kekRef.
// The KEKRef is hashed so any scheme (local:default, aws-kms://arn:...,
// vault-transit://...) maps to a safe, fixed-width key.
func sharedDEKKey(kekRef string) string {
	sum := sha256.Sum256([]byte(kekRef))
	return sharedDEKPrefix + hex.EncodeToString(sum[:]) + ".json"
}

// Unwrapper turns a wrapped-DEK blob into the plaintext DEK. It encapsulates
// the KEK custody model — local AES-256-GCM unwrap or a cloud-KMS round-trip —
// so this package stays free of keystore / kms dependencies. Returns a non-nil
// error for any wrap that doesn't authenticate under the caller's KEK.
type Unwrapper func(wrapped []byte) ([]byte, error)

// Result is the outcome of Resolve.
type Result struct {
	// DEK is the shared plaintext DEK; non-nil iff a prior wrapped form was
	// found and unwrapped successfully.
	DEK []byte
	// SawCandidate reports whether any manifest recorded a wrapped DEK under
	// the requested KEK — regardless of whether it unwrapped. When DEK is nil
	// but SawCandidate is true, a prior DEK exists that the caller's KEK
	// can't unwrap: the caller MUST fail rather than mint a fresh DEK.
	SawCandidate bool
}

// envelope is the subset of a manifest's "encryption" block this package
// reads. Both backup.EncryptionInfo and walsink's segment envelope serialise
// with these JSON names, so one shape parses both manifest types without
// importing either package (and without a dependency cycle).
type envelope struct {
	Scheme     string `json:"scheme"`
	KEKRef     string `json:"kek_ref"`
	WrappedDEK string `json:"wrapped_dek"`
}

type manifestEnvelope struct {
	Encryption *envelope `json:"encryption"`
}

// Resolve scans every base-backup manifest (manifests/) and WAL segment
// manifest (wal/) for a wrapped DEK recorded under wantKEKRef and returns the
// first that unwrap accepts. Backup manifests are scanned first — there are
// few of them and they are the common source — before the potentially many
// WAL segment manifests; both walks early-return on the first wrapped form
// that unwraps, so the steady-state cost is ~one manifest read.
//
//   - res.DEK != nil                       -> reuse this shared DEK.
//   - res.DEK == nil, res.SawCandidate true -> a prior DEK exists but none of
//     its recorded wrapped forms unwrap with the caller's KEK; the caller MUST
//     fail rather than mint a fresh DEK (a fresh DEK leaves deduped chunks
//     unrestorable — issue #28).
//   - res.DEK == nil, res.SawCandidate false -> no prior DEK; caller mints fresh.
//
// A hard storage / List error is returned as err (with a zero Result) so the
// caller can fail rather than mistake "couldn't enumerate" for "no DEK exists".
//
// Manifests are read without signature verification: an attacker who can write
// the repo can substitute any wrapped DEK they like, but the unwrap step is the
// real gate — a forged wrap won't authenticate and we fall through to the next
// candidate. This matches the backup-side dek-reuse posture.
func Resolve(ctx context.Context, sp storage.StoragePlugin, wantKEKRef string, unwrap Unwrapper) (Result, error) {
	var res Result
	for _, prefix := range []string{"manifests/", "wal/"} {
		for info, err := range sp.List(ctx, prefix) {
			if err != nil {
				return Result{}, fmt.Errorf("sharedkey: list %s: %w", prefix, err)
			}
			if !isManifestKey(prefix, info.Key) {
				continue
			}
			wrapped, ok := readWrappedDEK(ctx, sp, info.Key, wantKEKRef)
			if !ok {
				continue
			}
			res.SawCandidate = true
			if dek, uerr := unwrap(wrapped); uerr == nil && len(dek) == encryption.KeyLen {
				res.DEK = dek
				return res, nil
			}
		}
	}
	return res, nil
}

// ResolveOrMint returns the one shared DEK every encrypted artifact under
// (repo, kekRef) must use, minting it atomically the first time so that
// concurrent writers never diverge (issue #31).
//
// Order of resolution:
//
//  1. The authoritative shared-DEK object. Present + unwraps -> use it.
//     Present + won't unwrap -> UnusableCandidate (wrong KEK; caller fails).
//  2. Legacy repos (written before this object existed): scan committed
//     manifests via Resolve. A reusable DEK is ADOPTED into the shared-DEK
//     object (best-effort, IfNotExists) and returned. A manifest DEK that
//     won't unwrap -> UnusableCandidate.
//  3. Truly no prior DEK anywhere: generate a fresh DEK, wrap it, and PUT
//     the shared-DEK object with IfNotExists. If we win, use the fresh DEK.
//     If we lose (ErrAlreadyExists — a concurrent writer minted first), we
//     re-read the winner's object and use THAT DEK. This atomic single
//     winner is the whole fix: the old code had each writer mint its own.
//
// A hard storage error is returned as err so the caller fails rather than
// mistaking "couldn't coordinate" for "no DEK exists".
func ResolveOrMint(ctx context.Context, sp storage.StoragePlugin, kekRef string, unwrap Unwrapper, wrap Wrapper) (MintResult, error) {
	key := sharedDEKKey(kekRef)

	// 1. Authoritative object.
	if wrapped, ok := readWrappedDEK(ctx, sp, key, kekRef); ok {
		return unwrapInto(wrapped, unwrap)
	}

	// 2. Legacy seed: adopt an existing manifest DEK.
	res, err := Resolve(ctx, sp, kekRef, unwrap)
	if err != nil {
		return MintResult{}, err
	}
	if res.DEK != nil {
		var out MintResult
		if len(res.DEK) != encryption.KeyLen {
			return MintResult{}, fmt.Errorf("sharedkey: adopted DEK has wrong length %d (want %d)", len(res.DEK), encryption.KeyLen)
		}
		copy(out.DEK[:], res.DEK)
		out.Have = true
		// Best-effort promotion so future writers skip the manifest scan
		// and, more importantly, so a later concurrent fresh-mint can't
		// race in a divergent DEK. A conflict here is fine — someone else
		// adopted the same manifest DEK.
		if wrapped, werr := wrap(out.DEK); werr == nil {
			_ = putSharedDEK(ctx, sp, key, kekRef, wrapped)
		}
		return out, nil
	}
	if res.SawCandidate {
		return MintResult{UnusableCandidate: true}, nil
	}

	// 3. Atomic fresh mint.
	fresh, gerr := encryption.GenerateDEK()
	if gerr != nil {
		return MintResult{}, fmt.Errorf("sharedkey: generate DEK: %w", gerr)
	}
	wrapped, werr := wrap(fresh)
	if werr != nil {
		return MintResult{}, fmt.Errorf("sharedkey: wrap fresh DEK: %w", werr)
	}
	perr := putSharedDEK(ctx, sp, key, kekRef, wrapped)
	if perr == nil {
		return MintResult{DEK: fresh, Have: true}, nil // we minted it
	}
	if !errors.Is(perr, storage.ErrAlreadyExists) {
		return MintResult{}, fmt.Errorf("sharedkey: commit shared DEK: %w", perr)
	}
	// Lost the mint race — adopt the winner's DEK. This is exactly the
	// path that used to (incorrectly) keep the loser's own fresh DEK.
	winner, ok := readWrappedDEK(ctx, sp, key, kekRef)
	if !ok {
		return MintResult{}, fmt.Errorf("sharedkey: shared DEK object present after IfNotExists conflict but unreadable for KEK %q", kekRef)
	}
	return unwrapInto(winner, unwrap)
}

// unwrapInto unwraps a shared-DEK wrapped form into a MintResult. A wrap
// that won't authenticate under the caller's KEK yields UnusableCandidate
// (a prior DEK exists but this KEK can't read it) rather than an error.
func unwrapInto(wrapped []byte, unwrap Unwrapper) (MintResult, error) {
	dek, uerr := unwrap(wrapped)
	if uerr != nil || len(dek) != encryption.KeyLen {
		return MintResult{UnusableCandidate: true}, nil
	}
	var out MintResult
	copy(out.DEK[:], dek)
	out.Have = true
	return out, nil
}

// putSharedDEK writes the shared-DEK envelope at key with IfNotExists, so
// only the first writer wins. Returns storage.ErrAlreadyExists on conflict.
func putSharedDEK(ctx context.Context, sp storage.StoragePlugin, key, kekRef string, wrapped []byte) error {
	body, err := json.Marshal(manifestEnvelope{Encryption: &envelope{
		Scheme:     Scheme,
		KEKRef:     kekRef,
		WrappedDEK: base64.StdEncoding.EncodeToString(wrapped),
	}})
	if err != nil {
		return err
	}
	_, err = sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		IfNotExists:   true,
		ContentLength: int64(len(body)),
	})
	return err
}

// isManifestKey reports whether key under prefix is a manifest worth reading
// for an envelope: a primary base-backup manifest, or a committed WAL segment
// manifest (never a staging .tmp or a replica copy).
func isManifestKey(prefix, key string) bool {
	switch prefix {
	case "manifests/":
		if !strings.HasSuffix(key, "/manifest.json") {
			return false
		}
		return !strings.HasPrefix(strings.TrimPrefix(key, "manifests/"), "_replicas/")
	case "wal/":
		return strings.HasSuffix(key, ".json") && !strings.Contains(key, ".tmp")
	default:
		return false
	}
}

// readWrappedDEK fetches key, parses its encryption envelope, and returns the
// decoded wrapped-DEK bytes when the envelope is an aes-256-gcm wrap under
// wantKEKRef. Any read / parse / mismatch yields ok=false so one bad or
// unrelated object never aborts the scan.
func readWrappedDEK(ctx context.Context, sp storage.StoragePlugin, key, wantKEKRef string) ([]byte, bool) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	var m manifestEnvelope
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	if m.Encryption == nil || m.Encryption.Scheme != Scheme || m.Encryption.KEKRef != wantKEKRef {
		return nil, false
	}
	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		return nil, false
	}
	return wrapped, true
}
