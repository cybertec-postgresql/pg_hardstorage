// encryption_resolve.go — the single encryption-posture decision that
// every backup path shares.
//
// The interactive CLI and the agent (schedule engine + control-plane
// executor) used to make this decision in two different places, and
// the agent's copy knew only about the local keyring. A deployment
// that declared a cloud `kek_ref` in pg_hardstorage.yaml therefore got
// KMS-wrapped backups when an operator ran `backup db1` by hand and
// local-KEK backups when the scheduler fired — two postures welded
// onto the same deduplicated chunks (issue #44). One resolver, called
// from both, is what keeps them honest; internal/cli/encryption_parity_test.go
// pins the equivalence.
package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// Sentinel errors. Callers errors.Is against these to map a refusal
// onto their own structured error code (the CLI's
// usage.conflicting_flags / backup.encrypt_no_kek / backup.kek_load_failed
// / backup.kms_open_failed) without this package having to know about
// the output layer's taxonomy.
//
// The texts carry no package prefix on purpose: callers prepend their
// own command name, and "backup: runner: load KEK…" reads like a bug
// report rather than an error message.
//
// Note these are wrapped with the two-verb form (`%w: %w`), which makes
// a multi-error — errors.Is works, errors.Unwrap returns nil. Match by
// identity, never by unwrapping.
var (
	// ErrConflictingEncryptFlags: --encrypt and --no-encrypt both set.
	ErrConflictingEncryptFlags = errors.New("--encrypt and --no-encrypt are mutually exclusive")

	// ErrEncryptNoKEK: encryption was demanded but the keyring has no
	// KEK to encrypt with.
	ErrEncryptNoKEK = errors.New("encryption requested but no KEK file found at the keyring")

	// ErrKEKLoad: the keyring's KEK exists but couldn't be read.
	ErrKEKLoad = errors.New("load KEK from keyring")

	// ErrKMSOpen: the cloud-KMS provider for the requested KEKRef
	// couldn't be opened. Always wraps the registry's error, so
	// kms.IsUnreachable still classifies it.
	ErrKMSOpen = errors.New("open cloud KMS provider")
)

// EncryptionRequest is the input to ResolveEncryption. Every field is
// already resolved from its source (flag, config, or keyring path) —
// this package deliberately takes no dependency on internal/config so
// it stays importable from every layer.
type EncryptionRequest struct {
	// KeyringDir is the local keyring root, consulted only on the
	// local-custody path.
	KeyringDir string

	// KEKRef selects the key. Empty or a "local:" scheme uses the
	// keyring; any other registered scheme opens a cloud provider.
	KEKRef string

	// KMSConfig is the per-provider configuration map (region,
	// credentials, endpoint, …). Ignored on the local path.
	KMSConfig map[string]any

	// Encrypt / NoEncrypt are the operator's explicit --encrypt /
	// --no-encrypt. Non-interactive callers leave both false and get
	// the auto-detect posture.
	Encrypt   bool
	NoEncrypt bool

	// Registry overrides kms.DefaultRegistry. Tests inject a fresh
	// registry; production passes nil.
	Registry *kms.Registry
}

// ResolveEncryption decides whether a run encrypts and, if so, returns
// the EncryptionConfig the runner consumes. Posture matrix:
//
//	Encrypt && NoEncrypt         → ErrConflictingEncryptFlags
//	NoEncrypt                    → plaintext (regardless of key material)
//	cloud KEKRef                 → encrypt via the opened provider
//	Encrypt && no KEK on keyring → ErrEncryptNoKEK
//	KEK on keyring               → encrypt (auto-on)
//	no KEK on keyring            → plaintext (auto-off)
//
// A configured KEKRef that fails to open is an error, never a silent
// downgrade to the local KEK or to plaintext: a posture change behind
// the operator's back is exactly what welds mismatched manifests onto
// shared chunks.
//
// When a provider is returned the caller owns its lifecycle and must
// Close it once the backup finishes.
func ResolveEncryption(ctx context.Context, req EncryptionRequest) (*EncryptionConfig, error) {
	if req.Encrypt && req.NoEncrypt {
		return nil, ErrConflictingEncryptFlags
	}
	if req.NoEncrypt {
		return nil, nil
	}

	// Cloud-KMS branch. The provider is opened eagerly so an
	// auth/region misconfig surfaces here rather than mid-backup.
	if req.KEKRef != "" && req.KEKRef != keystore.KEKRefLocal && keystore.SchemeOf(req.KEKRef) != "local" {
		registry := req.Registry
		if registry == nil {
			registry = kms.DefaultRegistry
		}
		provider, err := registry.Open(ctx, req.KEKRef, req.KMSConfig)
		if err != nil {
			return nil, fmt.Errorf("%w %q: %w", ErrKMSOpen, req.KEKRef, err)
		}
		return &EncryptionConfig{
			Provider: provider,
			KEKRef:   provider.KEKRef(),
		}, nil
	}

	// Local-custody branch.
	hasKEK := keystore.KEKExists(req.KeyringDir)
	if req.Encrypt && !hasKEK {
		return nil, ErrEncryptNoKEK
	}
	if !hasKEK {
		return nil, nil
	}
	kek, _, err := keystore.LoadOrGenerateKEK(req.KeyringDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKEKLoad, err)
	}
	return &EncryptionConfig{KEK: kek, KEKRef: keystore.KEKRefLocal}, nil
}
