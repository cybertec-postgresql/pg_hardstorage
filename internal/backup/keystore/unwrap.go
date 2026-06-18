// unwrap.go — unified DEK unwrap dispatcher across local KEK and cloud KMS schemes.
package keystore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// UnwrapDEK is the unified DEK-unwrap dispatcher.  Production
// callers (restore, partial-restore, recovery-drill, the
// integrity verifier) consult this single entry-point rather
// than the legacy local-only KEKResolver.
//
// Dispatches by the manifest's KEKRef scheme:
//
//   - "" / "local:default": legacy path — reads kek.bin from
//     the local keyring directory, AES-256-GCM-decrypts the
//     wrapped DEK using the on-disk KEK + the manifest's
//     nonce.  This matches the v0.1..wrap shape exactly.
//
//   - any registered scheme (e.g. "aws-kms://arn:...",
//     "gcp-kms://...", "vault-transit://..."): dispatches
//     through kms.DefaultRegistry, opens the provider with
//     the supplied config map, and calls
//     provider.UnwrapDEK(wrapped) to get the plaintext DEK
//     directly.  No local KEK is involved — the cloud KMS
//     does the unwrap server-side.
//
// The dual-shape API exists because cloud KMS providers have
// no concept of "give me the KEK bytes" — the KEK never
// leaves the cloud HSM.  The legacy KEKResolver returned
// `[KeyLen]byte` (the KEK).  This function returns the
// plaintext DEK directly, which is what every caller
// actually wants.
//
// Nonce semantics (legacy local path):
//
//	The manifest carries `wrapped_dek` as base64-encoded
//	bytes prefixed by a 12-byte AEAD nonce: `[12 nonce | N
//	ciphertext+tag]`.  Cloud KMS providers manage their own
//	nonce / encryption-context internally; the wrapped form
//	they return is opaque to us.
//
// Errors:
//
//   - kms.ErrUnknownScheme — KEKRef has a scheme no provider
//     claims.  Operators see "configure the matching plugin"
//     in their suggestion.
//   - kms.ErrUnwrap — provider authenticated successfully
//     but the wrapped bytes don't decrypt (wrong key
//     material, tampered manifest, key scheduled for
//     deletion).  Critical-severity audit event.
//   - encryption.ErrAuthenticationFailed — local-path AES-GCM
//     authentication tag mismatch.
type UnwrapOpts struct {
	// KeyringDir is the local keyring root.  Required for
	// the local:default path; ignored for cloud-KMS schemes.
	KeyringDir string

	// ProviderConfig is the per-call configuration map the
	// kms.Builder consumes.  AWS KMS reads `region`,
	// `endpoint`, `pending_window_days`, etc.; other
	// providers their own keys.  Empty for the local path.
	ProviderConfig map[string]any

	// Registry overrides kms.DefaultRegistry.  Tests inject
	// a fresh registry; production passes nil.
	Registry *kms.Registry
}

// UnwrapDEK returns the plaintext DEK for `wrappedDEK` under
// `kekRef`.  See package-level docs for the dispatch
// semantics.
func UnwrapDEK(ctx context.Context, kekRef string, wrappedDEK []byte, opts UnwrapOpts) (dek []byte, retErr error) {
	// Metrics: time every unwrap and bucket it by scheme + result.  An
	// empty KEKRef is the local AES-GCM path; everything else is a cloud
	// KMS round-trip whose latency operators want to watch (a slow KMS
	// stalls every restore behind it).
	scheme := "local"
	if kekRef != "" && kekRef != KEKRefLocal {
		scheme = kms.SchemeOf(kekRef)
	}
	start := time.Now()
	defer func() {
		result := "success"
		if retErr != nil {
			result = "failure"
		}
		metrics.ObserveKMSUnwrap(scheme, result, time.Since(start).Seconds())
	}()

	if kekRef == "" || kekRef == KEKRefLocal {
		if opts.KeyringDir == "" {
			return nil, errors.New("keystore: UnwrapDEK with local KEKRef requires opts.KeyringDir")
		}
		return unwrapLocal(opts.KeyringDir, wrappedDEK)
	}
	registry := opts.Registry
	if registry == nil {
		registry = kms.DefaultRegistry
	}
	provider, err := registry.Open(ctx, kekRef, opts.ProviderConfig)
	if err != nil {
		return nil, err
	}
	defer provider.Close()
	dek, err = provider.UnwrapDEK(ctx, wrappedDEK)
	if err != nil {
		return nil, err
	}
	if len(dek) != encryption.KeyLen {
		return nil, fmt.Errorf("keystore: provider %s returned %d-byte DEK; want %d",
			provider.Name(), len(dek), encryption.KeyLen)
	}
	return dek, nil
}

// DEKResolver returns a restore-side DEK resolver: a closure that unwraps a
// manifest's wrapped DEK for any KEKRef — local custody (kek.bin) or cloud
// KMS (server-side, via the provider). It is the cloud-capable counterpart
// of KEKResolver, which returns raw KEK bytes and therefore cannot handle a
// cloud KEK that never leaves the HSM. Wire it into restore.Options.UnwrapDEK
// (issue #102).
//
// providerConfig carries cloud-provider configuration (region / endpoint /
// credential overrides); nil or empty relies on the provider's ambient
// credentials (e.g. the AWS SDK's env / instance-profile chain).
func DEKResolver(keyringDir string, providerConfig map[string]any) func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
	return func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
		return UnwrapDEK(ctx, kekRef, wrapped, UnwrapOpts{
			KeyringDir:     keyringDir,
			ProviderConfig: providerConfig,
		})
	}
}

// unwrapLocal decrypts the wrapped DEK using the KEK in the
// local keyring.  The wrapped bytes are expected to be
// `[12 nonce | N ciphertext+tag]` per the v0.1 wrap shape.
func unwrapLocal(keyringDir string, wrapped []byte) ([]byte, error) {
	if len(wrapped) < 12+16 {
		return nil, fmt.Errorf("keystore: wrapped DEK too short (%d bytes)", len(wrapped))
	}
	resolver := KEKResolver(keyringDir)
	kek, err := resolver(KEKRefLocal)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(kek[:])
	if err != nil {
		return nil, fmt.Errorf("keystore: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keystore: gcm: %w", err)
	}
	nonce := wrapped[:12]
	ct := wrapped[12:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w: %v", encryption.ErrAuthenticationFailed, err)
	}
	if len(plain) != encryption.KeyLen {
		return nil, fmt.Errorf("keystore: unwrapped DEK is %d bytes; want %d",
			len(plain), encryption.KeyLen)
	}
	return plain, nil
}

// SchemeOf is a convenience re-export of kms.SchemeOf so
// callers in the keystore package don't have to import
// internal/kms just to inspect the scheme.
func SchemeOf(kekRef string) string { return kms.SchemeOf(kekRef) }
