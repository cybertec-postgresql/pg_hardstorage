// Package azurekv implements a kms.Provider backed by
// Azure Key Vault.
//
// The cloud-side KEK is an Azure Key Vault key (RSA or AES);
// its bytes never leave the vault's HSM (when the vault tier
// is Premium / Managed HSM the keys are FIPS 140-2 Level 3
// validated; Standard tier vaults are FIPS 140-2 Level 2).
// We use the WrapKey / UnwrapKey operations — Azure's
// dedicated DEK-wrapping primitives — rather than the
// general Encrypt / Decrypt path.
//
// KEKRef formats:
//
//	azure-kv://<vault-name>/<key-name>
//	azure-kv://<vault-name>/<key-name>/<version>
//
// The version-pinned form is required for `Shred` (Azure
// soft-deletes the key — the recovery window is configured
// at vault creation time, default 90 days — and the resource
// goes through full purge on the configured cadence).
//
// # Authentication
//
// The provider opens an `azkeys.Client` using
// `azidentity.NewDefaultAzureCredential()`, which chains
// through env vars → managed identity → Azure CLI → IDE
// integrated auth in the order documented at
// https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication.
// Operators on Azure VMs / AKS pods get managed-identity
// auth for free; CI environments use service-principal env
// vars; developers use `az login`.
//
// # FIPS posture
//
// Azure Key Vault Premium tier + Managed HSM are FIPS 140-2
// Level 3.  Standard tier is FIPS 140-2 Level 2.  The
// provider's FIPSMode() reports whatever the operator
// declares via WithFIPSMode at construction; the SDK
// doesn't expose the tier in the key metadata.
//
// # GDPR Art. 17 / crypto-shred
//
// Shred calls `DeleteKey`, which moves the key into Azure's
// soft-delete state.  The actual destruction occurs after
// the vault's recovery window (default 90 days; configurable
// 7..90).  Operators wanting immediate destruction must
// follow up with `az keyvault key purge` (privileged op);
// pg_hardstorage doesn't issue purges directly because
// they're irrecoverable and operators benefit from the
// soft-delete safety net.
package azurekv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// Scheme is the KEKRef scheme this provider claims.  Stable
// across versions; on-disk manifest values use this string.
const Scheme = "azure-kv"

// DefaultWrapAlgorithm is what we use for WrapKey /
// UnwrapKey when the operator doesn't override.  RSA-OAEP-256
// is the recommended default for envelope-encrypting a
// per-backup DEK with an RSA KEK; for AES key types the
// SDK auto-resolves to A256KW which we don't need to
// declare explicitly.
const DefaultWrapAlgorithm = string(azkeys.EncryptionAlgorithmRSAOAEP256)

func init() {
	stdkms.DefaultRegistry.Register(Scheme, builder)
}

// Client is the subset of the Azure Key Vault SDK we depend
// on.  Tests inject a fake; production code uses the real
// SDK client via the realClient adapter.  Method shapes are
// deliberately domain-specific (not the SDK's exact
// signatures) so the fake doesn't have to import azcore.
type Client interface {
	Wrap(ctx context.Context, version string, alg string, dek []byte) ([]byte, error)
	Unwrap(ctx context.Context, version string, alg string, ciphertext []byte) ([]byte, error)
	Delete(ctx context.Context) error
	Describe(ctx context.Context, version string) (map[string]any, error)
	Close() error
}

// Provider implements kms.Provider over Azure Key Vault.
type Provider struct {
	kekRef     string
	vaultURL   string
	keyName    string
	versionRef string // empty when KEKRef has no /<version> suffix
	wrapAlg    string
	client     Client
	fipsMode   bool

	mu     sync.Mutex
	closed bool
}

// builder is the registry entry-point.
//
// Config keys:
//
//	wrap_algorithm: ""              # default RSA-OAEP-256 for RSA keys
//	use_fips_mode: false            # operator-declared posture
func builder(ctx context.Context, kekRef string, cfg map[string]any) (stdkms.Provider, error) {
	vault, key, version, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	wrapAlg, _ := cfg["wrap_algorithm"].(string)
	if wrapAlg == "" {
		wrapAlg = DefaultWrapAlgorithm
	}
	fipsMode, _ := cfg["use_fips_mode"].(bool)

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure-kv: build credential: %w", err)
	}
	azCli, err := azkeys.NewClient(vault, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure-kv: open client for %s: %w", vault, err)
	}

	return &Provider{
		kekRef:     kekRef,
		vaultURL:   vault,
		keyName:    key,
		versionRef: version,
		wrapAlg:    wrapAlg,
		client:     &realClient{client: azCli, key: key},
		fipsMode:   fipsMode,
	}, nil
}

// realClient adapts *azkeys.Client to our Client interface.
// Lives here (not in tests) because production code uses it;
// tests substitute a fake without importing azcore.
type realClient struct {
	client *azkeys.Client
	key    string
}

// Wrap implements Client by forwarding to azkeys WrapKey.
func (r *realClient) Wrap(ctx context.Context, version, alg string, dek []byte) ([]byte, error) {
	algo := azkeys.EncryptionAlgorithm(alg)
	resp, err := r.client.WrapKey(ctx, r.key, version, azkeys.KeyOperationParameters{
		Algorithm: &algo,
		Value:     dek,
	}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// Unwrap implements Client by forwarding to azkeys UnwrapKey.
func (r *realClient) Unwrap(ctx context.Context, version, alg string, ciphertext []byte) ([]byte, error) {
	algo := azkeys.EncryptionAlgorithm(alg)
	resp, err := r.client.UnwrapKey(ctx, r.key, version, azkeys.KeyOperationParameters{
		Algorithm: &algo,
		Value:     ciphertext,
	}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// Delete implements Client by soft-deleting the underlying key
// (Azure's DeleteKey moves it to the recovery window; purge is a
// separate privileged operation).
func (r *realClient) Delete(ctx context.Context) error {
	_, err := r.client.DeleteKey(ctx, r.key, nil)
	return err
}

// Describe implements Client by issuing GetKey and projecting the
// SDK response into a stable map (kid, key_ops, enabled, created,
// expires) for the kms.Provider Describe contract.
func (r *realClient) Describe(ctx context.Context, version string) (map[string]any, error) {
	resp, err := r.client.GetKey(ctx, r.key, version, nil)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	// v27 audit F5: guard the Key.KeyOps access against a nil
	// resp.Key — Azure SDK can theoretically return Attributes
	// with no Key body on partial failures; the previous code
	// nil-checked resp.Key for the KID branch but then accessed
	// resp.Key.KeyOps unconditionally, NPE-ing if Key was nil.
	if resp.Key != nil {
		if resp.Key.KID != nil {
			body["kid"] = string(*resp.Key.KID)
		}
		if len(resp.Key.KeyOps) > 0 {
			ops := make([]string, 0, len(resp.Key.KeyOps))
			for _, op := range resp.Key.KeyOps {
				if op != nil {
					ops = append(ops, string(*op))
				}
			}
			body["key_ops"] = ops
		}
	}
	if resp.Attributes != nil {
		if resp.Attributes.Enabled != nil {
			body["enabled"] = *resp.Attributes.Enabled
		}
		if resp.Attributes.Created != nil {
			body["created"] = resp.Attributes.Created.Format("2006-01-02T15:04:05Z")
		}
		if resp.Attributes.Expires != nil {
			body["expires"] = resp.Attributes.Expires.Format("2006-01-02T15:04:05Z")
		}
	}
	return body, nil
}

// Close implements Client. The azkeys SDK has no per-client teardown,
// so this is a no-op.
func (r *realClient) Close() error { return nil }

// NewWithClient is the test-friendly constructor — pass any
// Client implementation.
func NewWithClient(kekRef string, client Client, opts ...Option) (*Provider, error) {
	vault, key, version, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		kekRef:     kekRef,
		vaultURL:   vault,
		keyName:    key,
		versionRef: version,
		wrapAlg:    DefaultWrapAlgorithm,
		client:     client,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Option tunes NewWithClient.
type Option func(*Provider)

// WithWrapAlgorithm overrides the default wrap algorithm.
func WithWrapAlgorithm(alg string) Option { return func(p *Provider) { p.wrapAlg = alg } }

// WithFIPSMode declares the provider's FIPS posture.
func WithFIPSMode(fips bool) Option { return func(p *Provider) { p.fipsMode = fips } }

// Name implements kms.Provider.
func (p *Provider) Name() string { return Scheme }

// KEKRef implements kms.Provider.
func (p *Provider) KEKRef() string { return p.kekRef }

// FIPSMode implements kms.Provider.
func (p *Provider) FIPSMode() bool { return p.fipsMode }

// WrapDEK implements kms.Provider.
func (p *Provider) WrapDEK(ctx context.Context, dek []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	wrapped, err := p.client.Wrap(ctx, p.versionRef, p.wrapAlg, dek)
	if err != nil {
		return nil, fmt.Errorf("azure-kv: WrapKey: %w", err)
	}
	return wrapped, nil
}

// UnwrapDEK implements kms.Provider.
func (p *Provider) UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	plain, err := p.client.Unwrap(ctx, p.versionRef, p.wrapAlg, wrapped)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", stdkms.ErrUnwrap, err)
	}
	return plain, nil
}

// Shred implements kms.Provider.  Calls Azure's DeleteKey,
// which moves the key into the soft-delete state.  After
// the vault's recovery window elapses (default 90 days),
// the key material is destroyed.  Operators who want
// immediate destruction follow up with `az keyvault key
// purge`; pg_hardstorage doesn't issue purges directly
// because they're irrecoverable and the soft-delete safety
// net protects against operator error.
func (p *Provider) Shred(ctx context.Context) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	if err := p.client.Delete(ctx); err != nil {
		return fmt.Errorf("%w: DeleteKey %s/%s: %v", stdkms.ErrShredFailed, p.vaultURL, p.keyName, err)
	}
	return nil
}

// Close implements kms.Provider.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.client.Close()
}

// DescribeKey is the operator-facing inspector used by
// `kms inspect`.  Surfaces a kid + enabled/created/expires
// + supported key_ops; normalised into a plain
// map[string]any so the CLI's stable-schema body doesn't
// pull in azkeys types.
func (p *Provider) DescribeKey(ctx context.Context) (map[string]any, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	body, err := p.client.Describe(ctx, p.versionRef)
	if err != nil {
		return nil, fmt.Errorf("azure-kv: GetKey: %w", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	body["vault_url"] = p.vaultURL
	body["key_name"] = p.keyName
	if p.versionRef != "" {
		body["version"] = p.versionRef
	}
	return body, nil
}

func (p *Provider) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("azure-kv: provider closed")
	}
	return nil
}

// parseKEKRef extracts the vault URL + key name + optional
// version from the KEKRef.
//
//	azure-kv://acme-vault/db-backup-kek
//	  → vaultURL="https://acme-vault.vault.azure.net/"
//	    keyName="db-backup-kek"
//	    versionRef=""
//	azure-kv://acme-vault/db-backup-kek/abcdef1234567890
//	  → vaultURL=...
//	    keyName="db-backup-kek"
//	    versionRef="abcdef1234567890"
//
// The vault host suffix `.vault.azure.net` is the public
// Azure cloud's domain; sovereign clouds (us-gov, china)
// have different suffixes which the operator can supply
// as the host:
//
//	azure-kv://acme-vault.vault.azure.cn/db-backup-kek
//
// — the prefix-with-dot triggers literal-host mode.
func parseKEKRef(kekRef string) (vaultURL, keyName, versionRef string, err error) {
	if !strings.HasPrefix(kekRef, Scheme+"://") {
		return "", "", "", fmt.Errorf("azure-kv: KEKRef %q does not have the %q:// prefix", kekRef, Scheme)
	}
	rest := strings.TrimPrefix(kekRef, Scheme+"://")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", "", fmt.Errorf("azure-kv: empty resource in KEKRef %q", kekRef)
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("azure-kv: KEKRef %q must be azure-kv://<vault>/<key>[/<version>]", kekRef)
	}
	vaultPart := parts[0]
	keyName = parts[1]
	if keyName == "" {
		return "", "", "", fmt.Errorf("azure-kv: empty key name in KEKRef %q", kekRef)
	}
	if len(parts) == 3 {
		versionRef = parts[2]
		if versionRef == "" {
			return "", "", "", fmt.Errorf("azure-kv: empty version suffix in KEKRef %q", kekRef)
		}
	}
	// vault host: bare name → public-cloud .vault.azure.net;
	// dotted name → literal hostname (sovereign clouds).
	if strings.Contains(vaultPart, ".") {
		vaultURL = "https://" + vaultPart + "/"
	} else {
		vaultURL = "https://" + vaultPart + ".vault.azure.net/"
	}
	return vaultURL, keyName, versionRef, nil
}

// silence unused-import warning for to + azcore (the
// realClient adapter references types from these packages
// transitively; an explicit reference here keeps a future
// SDK upgrade from breaking the build via dropped import).
var (
	_ = to.Ptr[string]
	_ = azcore.NewClient
)
