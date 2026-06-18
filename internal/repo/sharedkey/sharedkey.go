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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Scheme is the only envelope scheme DEK reuse applies to.
const Scheme = "aes-256-gcm"

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
