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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Scheme is the only envelope scheme DEK reuse applies to.
const Scheme = "aes-256-gcm"

// DomainKey is the repository-wide encryption-domain record.  The CAS key
// space is global and keyed by plaintext hash, so every encrypted writer in a
// repository must use the same plaintext DEK.  A conditional create of this
// record is the serialization point for the first backup/WAL writer.
const DomainKey = "_encryption/domain.json"

const domainSchema = "pg_hardstorage.encryption_domain.v1"

type domainRecord struct {
	Schema     string `json:"schema"`
	Scheme     string `json:"scheme"`
	KEKRef     string `json:"kek_ref"`
	WrappedDEK string `json:"wrapped_dek"`
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
	var resolved []byte
	for _, prefix := range []string{"manifests/", "wal/"} {
		for info, err := range sp.List(ctx, prefix) {
			if err != nil {
				return Result{}, fmt.Errorf("sharedkey: list %s: %w", prefix, err)
			}
			if !isManifestKey(prefix, info.Key) {
				continue
			}
			env, err := readEnvelope(ctx, sp, info.Key)
			if err != nil {
				return Result{}, fmt.Errorf("sharedkey: read %s: %w", info.Key, err)
			}
			if env == nil || env.Scheme != Scheme || env.KEKRef != wantKEKRef {
				continue
			}
			wrapped, err := base64.StdEncoding.DecodeString(env.WrappedDEK)
			if err != nil {
				return Result{}, fmt.Errorf("sharedkey: decode wrapped DEK in %s: %w", info.Key, err)
			}
			ok := len(wrapped) > 0
			if !ok {
				return Result{}, fmt.Errorf("sharedkey: empty wrapped DEK in %s", info.Key)
			}
			res.SawCandidate = true
			dek, uerr := unwrap(wrapped)
			if uerr != nil || len(dek) != encryption.KeyLen {
				continue
			}
			if resolved == nil {
				resolved = append([]byte(nil), dek...)
				continue
			}
			if !bytes.Equal(resolved, dek) {
				return Result{}, fmt.Errorf("sharedkey: manifests under KEK %q contain divergent DEKs; refusing to choose one for a plaintext-addressed CAS", wantKEKRef)
			}
		}
	}
	res.DEK = resolved
	return res, nil
}

// ResolveOrCreate returns the one repository-wide plaintext DEK.  The first
// writer publishes DomainKey with IfNotExists; a concurrent loser reads and
// adopts the winner's record.  Repositories bootstrapped before DomainKey was
// introduced are migrated from their manifests, but only when all encrypted
// manifests use the requested KEKRef and resolve to one DEK.
func ResolveOrCreate(
	ctx context.Context,
	sp storage.StoragePlugin,
	kekRef string,
	unwrap Unwrapper,
	wrap func([encryption.KeyLen]byte) ([]byte, error),
) ([encryption.KeyLen]byte, error) {
	if sp == nil || unwrap == nil || wrap == nil || kekRef == "" {
		return [encryption.KeyLen]byte{}, errors.New("sharedkey: storage, kekRef, wrap and unwrap are required")
	}
	if rec, found, err := readDomain(ctx, sp); err != nil {
		return [encryption.KeyLen]byte{}, err
	} else if found {
		return unwrapDomain(rec, kekRef, unwrap)
	}

	refs, err := DiscoverKEKRefs(ctx, sp)
	if err != nil {
		return [encryption.KeyLen]byte{}, err
	}
	if len(refs) > 1 || len(refs) == 1 && refs[0] != kekRef {
		return [encryption.KeyLen]byte{}, fmt.Errorf("sharedkey: repository CAS already contains encrypted artifacts under KEKRef(s) %v; requested %q would create an undecryptable dedup collision", refs, kekRef)
	}

	res, err := Resolve(ctx, sp, kekRef, unwrap)
	if err != nil {
		return [encryption.KeyLen]byte{}, err
	}
	var dek [encryption.KeyLen]byte
	switch {
	case res.DEK != nil:
		copy(dek[:], res.DEK)
	case res.SawCandidate:
		return dek, fmt.Errorf("sharedkey: prior DEK for KEK %q cannot be unwrapped", kekRef)
	default:
		fresh, err := encryption.GenerateDEK()
		if err != nil {
			return dek, fmt.Errorf("sharedkey: generate DEK: %w", err)
		}
		dek = fresh
	}

	w, err := wrap(dek)
	if err != nil {
		return [encryption.KeyLen]byte{}, fmt.Errorf("sharedkey: wrap domain DEK: %w", err)
	}
	rec := domainRecord{Schema: domainSchema, Scheme: Scheme, KEKRef: kekRef, WrappedDEK: base64.StdEncoding.EncodeToString(w)}
	body, err := json.Marshal(rec)
	if err != nil {
		return [encryption.KeyLen]byte{}, fmt.Errorf("sharedkey: marshal domain: %w", err)
	}
	_, err = sp.Put(ctx, DomainKey, bytes.NewReader(body), storage.PutOptions{
		IfNotExists: true, ContentLength: int64(len(body)),
	})
	if err == nil {
		return dek, nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		return [encryption.KeyLen]byte{}, fmt.Errorf("sharedkey: publish domain: %w", err)
	}
	winner, found, err := readDomain(ctx, sp)
	if err != nil {
		return [encryption.KeyLen]byte{}, err
	}
	if !found {
		return [encryption.KeyLen]byte{}, errors.New("sharedkey: encryption-domain create lost race but winner is absent")
	}
	return unwrapDomain(winner, kekRef, unwrap)
}

// EnsurePlaintextAllowed refuses a plaintext writer once any encrypted
// artifact/domain exists.  An encryption-aware CAS can read legacy plaintext
// envelopes, but a plaintext CAS cannot read an encrypted dedup winner.
func EnsurePlaintextAllowed(ctx context.Context, sp storage.StoragePlugin) error {
	if rec, found, err := readDomain(ctx, sp); err != nil {
		return err
	} else if found {
		if rec.Scheme == "none" {
			return nil
		}
		return errors.New("sharedkey: repository encryption domain is established; refusing a plaintext writer that could deduplicate against ciphertext it cannot restore")
	}
	refs, err := DiscoverKEKRefs(ctx, sp)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return fmt.Errorf("sharedkey: repository contains encrypted artifacts under KEKRef(s) %v; refusing plaintext writes", refs)
	}
	rec := domainRecord{Schema: domainSchema, Scheme: "none"}
	body, _ := json.Marshal(rec)
	_, err = sp.Put(ctx, DomainKey, bytes.NewReader(body), storage.PutOptions{
		IfNotExists: true, ContentLength: int64(len(body)),
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		return fmt.Errorf("sharedkey: publish plaintext domain: %w", err)
	}
	winner, found, err := readDomain(ctx, sp)
	if err != nil {
		return err
	}
	if !found || winner.Scheme != "none" {
		return errors.New("sharedkey: concurrent encrypted writer established the repository domain; refusing plaintext writes")
	}
	return nil
}

// RotateDomain re-wraps the repository DEK after every manifest has been
// rotated. The caller must hold the repository mutation lock so no writer can
// observe a half-rotated key posture. Legacy repositories without DomainKey
// are left alone; their next writer bootstraps the record from rotated
// manifests.
func RotateDomain(
	ctx context.Context,
	sp storage.StoragePlugin,
	oldKEKRef, newKEKRef string,
	unwrapOld Unwrapper,
	wrapNew func([encryption.KeyLen]byte) ([]byte, error),
) error {
	rec, found, err := readDomain(ctx, sp)
	if err != nil || !found {
		return err
	}
	dek, err := unwrapDomain(rec, oldKEKRef, unwrapOld)
	if err != nil {
		return err
	}
	w, err := wrapNew(dek)
	if err != nil {
		return fmt.Errorf("sharedkey: wrap rotated domain DEK: %w", err)
	}
	next := domainRecord{Schema: domainSchema, Scheme: Scheme, KEKRef: newKEKRef, WrappedDEK: base64.StdEncoding.EncodeToString(w)}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := sp.Put(ctx, DomainKey, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		return fmt.Errorf("sharedkey: rewrite encryption domain: %w", err)
	}
	return nil
}

// DiscoverKEKRefs strictly reads every committed manifest and returns the
// distinct encryption KEKRefs.  Read/parse failures are fatal: treating an
// unreadable encrypted manifest as "no key exists" can mint a divergent DEK.
func DiscoverKEKRefs(ctx context.Context, sp storage.StoragePlugin) ([]string, error) {
	set := map[string]struct{}{}
	for _, prefix := range []string{"manifests/", "wal/"} {
		for info, err := range sp.List(ctx, prefix) {
			if err != nil {
				return nil, fmt.Errorf("sharedkey: list %s: %w", prefix, err)
			}
			if !isManifestKey(prefix, info.Key) {
				continue
			}
			env, err := readEnvelope(ctx, sp, info.Key)
			if err != nil {
				return nil, fmt.Errorf("sharedkey: read %s: %w", info.Key, err)
			}
			if env != nil && env.Scheme == Scheme && env.KEKRef != "" {
				set[env.KEKRef] = struct{}{}
			}
		}
	}
	refs := make([]string, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
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
func readEnvelope(ctx context.Context, sp storage.StoragePlugin, key string) (*envelope, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var m manifestEnvelope
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m.Encryption, nil
}

func readDomain(ctx context.Context, sp storage.StoragePlugin) (domainRecord, bool, error) {
	rc, err := sp.Get(ctx, DomainKey)
	if errors.Is(err, storage.ErrNotFound) {
		return domainRecord{}, false, nil
	}
	if err != nil {
		return domainRecord{}, false, fmt.Errorf("sharedkey: read encryption domain: %w", err)
	}
	defer rc.Close()
	var rec domainRecord
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&rec); err != nil {
		return domainRecord{}, false, fmt.Errorf("sharedkey: decode encryption domain: %w", err)
	}
	validPlain := rec.Scheme == "none" && rec.KEKRef == "" && rec.WrappedDEK == ""
	validEncrypted := rec.Scheme == Scheme && rec.KEKRef != "" && rec.WrappedDEK != ""
	if rec.Schema != domainSchema || !validPlain && !validEncrypted {
		return domainRecord{}, false, fmt.Errorf("sharedkey: invalid encryption domain record")
	}
	return rec, true, nil
}

func unwrapDomain(rec domainRecord, wantKEKRef string, unwrap Unwrapper) ([encryption.KeyLen]byte, error) {
	var out [encryption.KeyLen]byte
	if rec.KEKRef != wantKEKRef {
		return out, fmt.Errorf("sharedkey: repository encryption domain uses KEKRef %q; requested %q would create an undecryptable dedup collision", rec.KEKRef, wantKEKRef)
	}
	w, err := base64.StdEncoding.DecodeString(rec.WrappedDEK)
	if err != nil {
		return out, fmt.Errorf("sharedkey: decode domain wrapped DEK: %w", err)
	}
	dek, err := unwrap(w)
	if err != nil {
		return out, fmt.Errorf("sharedkey: unwrap domain DEK: %w", err)
	}
	if len(dek) != encryption.KeyLen {
		return out, fmt.Errorf("sharedkey: domain DEK length %d, want %d", len(dek), encryption.KeyLen)
	}
	copy(out[:], dek)
	return out, nil
}
