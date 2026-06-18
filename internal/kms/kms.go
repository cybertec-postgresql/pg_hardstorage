// Package kms is the KEK-provider abstraction.
//
// A KEK provider holds the master Key-Encryption Key — the
// thing that wraps every backup's per-backup Data-Encryption
// Key (DEK).  Today's repo carries the wrapped DEK on every
// manifest; resolving the KEK back to plaintext is what lets
// restore decrypt chunks.
//
// Providers:
//
//   - local — KEK lives on disk in <keyring>/kek.bin (v0.1).
//   - aws-kms — KEK is an AWS KMS CMK, never local; AWS KMS
//     wraps/unwraps DEKs over the network.
//   - gcp-kms / azure-kv / vault-transit / pkcs11 — same shape,
//     deferred to follow-up sessions.
//
// The provider is selected by the manifest's KEKRef.  Schemes:
//
//   - "local:default"       → local keystore
//   - "aws-kms://<arn>"     → AWS KMS
//   - "gcp-kms://<resource>" → (future) GCP KMS
//   - "azure-kv://<vault>/<key>" → (future) Azure Key Vault
//   - "vault-transit://<addr>/<key>" → (future) HashiCorp Vault
//
// The DefaultRegistry dispatches by scheme.  Every provider
// re-exports a constructor that can be wired in either at init()
// (built-in / Tier-1) or at runtime via go-plugin (Tier-2).
//
// # Why this lives in its own package, not in keystore
//
// The keystore package owns the on-disk *local* keyring.
// Cloud-KMS providers don't have a keyring — they have an ARN
// and an SDK call.  Keeping the abstraction here means the
// keystore package doesn't grow a dependency on every cloud
// SDK we ever support.
package kms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Provider is the KEK-provider contract.  Implementations are
// stateful (they hold connection state, refresh tokens, etc.)
// but every method is goroutine-safe.
//
// Scope: this interface is deliberately leaner than the SPEC's
// "EncryptionPlugin" surface (which mentions GenerateDEK and
// RotateKEK as plugin methods).  In those are **host-side
// operations layered on top of the leaner Provider contract**:
//
//   - DEK generation lives in `internal/backup/keystore` via
//     `crypto/rand`; the host generates the DEK and asks the
//     Provider only to wrap it.  This keeps DEK material on
//     the host and out of the cloud-KMS audit log.
//   - KEK rotation is `internal/cli/kms_rotate.go`: it walks
//     every manifest, opens TWO Providers (old + new KEKRef),
//     and re-wraps the DEK.  Plugin authors don't implement
//     a `RotateKEK` method — they only need to honour Wrap +
//     Unwrap correctly.
//
// Plugin authors writing a Tier-1 Provider implement just the
// six methods below.
type Provider interface {
	// Name is the canonical scheme name ("local", "aws-kms",
	// "gcp-kms", ...).  Stable across versions; goes into
	// audit-log Subject fields.
	Name() string

	// KEKRef returns the manifest-stamped KEKRef string this
	// provider resolves.  Round-trips through the manifest:
	// `provider.KEKRef() == manifest.Encryption.KEKRef`.
	KEKRef() string

	// WrapDEK encrypts the per-backup DEK with the cloud-side
	// KEK and returns the wrapped form.  The wrapped bytes go
	// into manifest.Encryption.WrappedDEK.
	WrapDEK(ctx context.Context, dek []byte) ([]byte, error)

	// UnwrapDEK decrypts a previously-wrapped DEK using the
	// cloud-side KEK.  Authentication failure surfaces as a
	// wrapped ErrUnwrap.
	UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error)

	// Shred schedules the destruction of the cloud-side KEK.
	// Cloud KMS providers typically schedule deletion with a
	// cool-off window (AWS KMS: 7-30 days).  After Shred fires,
	// every backup whose wrapped_dek depends on this KEK
	// becomes permanently unrecoverable — by design (GDPR Art.
	// 17 / right to erasure).
	//
	// Shred is the most consequential op in the binary.  CLI
	// callers gate it behind n-of-m approval + typed-keyring
	// confirmation + --yes (see internal/cli/kms_shred.go).
	Shred(ctx context.Context) error

	// FIPSMode reports whether this provider is operating in
	// FIPS-validated mode.  Used by `pg_hardstorage doctor`
	// to surface the compliance posture and by the runtime
	// `--fips-strict` flag to refuse non-FIPS providers.
	FIPSMode() bool

	// Close releases provider-side resources.  Idempotent.
	Close() error
}

// ErrUnwrap is the typed error every provider's UnwrapDEK
// wraps when authentication fails.  Callers errors.Is to
// distinguish "wrong key" from "network error" from "key
// scheduled for deletion."
var ErrUnwrap = errors.New("kms: DEK unwrap failed")

// ErrShredFailed wraps Shred errors that aren't network
// failures.  Cloud KMS often refuses Shred with structured
// errors (key already pending deletion, key in different
// account, ...); the provider returns these wrapped in
// ErrShredFailed.
var ErrShredFailed = errors.New("kms: shred failed")

// ErrUnknownScheme is returned by the registry when the
// caller's KEKRef has a scheme no registered provider claims.
var ErrUnknownScheme = errors.New("kms: unknown KEKRef scheme")

// Builder constructs a Provider from a KEKRef plus optional
// per-call configuration.  Cloud SDKs typically need
// credentials + region; those flow through the Config map so
// the kms package itself is SDK-agnostic.
type Builder func(ctx context.Context, kekRef string, cfg map[string]any) (Provider, error)

// Registry holds Builder funcs keyed by scheme prefix.
type Registry struct {
	mu       sync.RWMutex
	builders map[string]Builder
}

// NewRegistry returns an empty Registry.  Production code uses
// DefaultRegistry; tests construct fresh ones for isolation.
func NewRegistry() *Registry {
	return &Registry{builders: map[string]Builder{}}
}

// Register adds a Builder under the given scheme.  Schemes are
// matched against the KEKRef's prefix up to (and excluding)
// the first ":" or "://".  Re-registration overwrites — the
// idiom Tier-2 plugins use to override a Tier-1 default.
func (r *Registry) Register(scheme string, b Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builders[scheme] = b
}

// Open looks up the Builder for the KEKRef's scheme and
// constructs a Provider.  ErrUnknownScheme is returned for an
// unregistered scheme.
func (r *Registry) Open(ctx context.Context, kekRef string, cfg map[string]any) (Provider, error) {
	scheme := SchemeOf(kekRef)
	r.mu.RLock()
	b, ok := r.builders[scheme]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (no provider registered for scheme %q)",
			ErrUnknownScheme, kekRef, scheme)
	}
	p, err := b(ctx, kekRef, cfg)
	if err != nil {
		return nil, fmt.Errorf("kms: open %q: %w", kekRef, err)
	}
	return p, nil
}

// Schemes returns the registered scheme list, sorted.  Used
// by `doctor` and `kms inspect` to surface the available
// providers.
func (r *Registry) Schemes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.builders))
	for s := range r.builders {
		out = append(out, s)
	}
	return out
}

// SchemeOf extracts the scheme prefix from a KEKRef.
// "aws-kms://arn:..." → "aws-kms"
// "local:default"     → "local"
// "garbage"           → ""
func SchemeOf(kekRef string) string {
	if i := strings.Index(kekRef, "://"); i > 0 {
		return kekRef[:i]
	}
	if i := strings.Index(kekRef, ":"); i > 0 {
		return kekRef[:i]
	}
	return ""
}

// DefaultRegistry is the process-wide registry every Tier-1
// provider self-registers into via init().
var DefaultRegistry = NewRegistry()
