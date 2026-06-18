// Package gcpkms implements a kms.Provider backed by Google
// Cloud Key Management Service.
//
// The cloud-side KEK is a GCP KMS CryptoKey.  Its bytes never
// leave Google's HSMs (Software / HSM / External / Cloud-HSM
// protection levels are all supported by the same SDK).
// Like the AWS KMS provider, we only ever see Encrypt /
// Decrypt ciphertext blobs.
//
// KEKRef formats:
//
//	gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>
//	gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>/cryptoKeyVersions/<v>
//
// The version-bearing form is required for `Shred` (GCP
// destroys *versions*, not keys; without an explicit version
// we'd have to enumerate which is racy).  Wrap / Unwrap
// accept either form — Encrypt without a version routes to
// the primary; Decrypt picks the version from the ciphertext
// metadata GCP carries internally.
//
// # FIPS posture
//
// GCP KMS HSM-protection-level keys are FIPS 140-2 Level 3
// validated.  The provider's FIPSMode() reports whatever the
// operator declares via WithFIPSMode at construction (we
// can't infer it from the SDK alone — a Software-protection
// key looks identical at the wire level).  Document the
// posture in the operator's deployment runbook.
//
// # GDPR Art. 17 / crypto-shred
//
// Shred calls `DestroyCryptoKeyVersion` on the version
// embedded in the KEKRef.  GCP marks the version as
// `DESTROY_SCHEDULED` immediately and destroys the key
// material after a 24-hour cooldown by default (the
// `destroy_scheduled_duration` field on the parent key
// configures this; we don't override it in the wrapper).
//
// # Air-gap interaction
//
// GCP KMS is reachable over private VPC endpoints
// (Private Google Access).  Operators running in air-gap
// mode point at a private IP that the airgap policy's
// allowlist permits.  The default Google endpoint is public
// and refused under strict mode.
package gcpkms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	gcpkmsv1 "cloud.google.com/go/kms/apiv1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// Scheme is the KEKRef scheme this provider claims.  Stable
// across versions; on-disk manifest values use this string.
const Scheme = "gcp-kms"

func init() {
	stdkms.DefaultRegistry.Register(Scheme, builder)
}

// Client is the subset of the GCP KMS SDK we depend on.
// Tests inject a fake; production code uses the real SDK
// client.  Method signatures mirror the SDK shape with one
// adaptation: the variadic gax.CallOption is replaced by
// CallOption (an alias for `any`) so test fakes don't have
// to import gax just to type-match.
type Client interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest, opts ...CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest, opts ...CallOption) (*kmspb.DecryptResponse, error)
	DestroyCryptoKeyVersion(ctx context.Context, req *kmspb.DestroyCryptoKeyVersionRequest, opts ...CallOption) (*kmspb.CryptoKeyVersion, error)
	GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest, opts ...CallOption) (*kmspb.CryptoKey, error)
	Close() error
}

// CallOption is the test-friendly stand-in for gax.CallOption.
// The real SDK call signatures accept gax.CallOption; our
// realClient adapter ignores the variadic and forwards
// without it.
type CallOption = any

// Provider implements kms.Provider over GCP KMS.
type Provider struct {
	kekRef     string
	keyName    string // resource path: projects/.../cryptoKeys/<key>[/cryptoKeyVersions/<v>]
	versionRef string // empty when KEKRef has no /cryptoKeyVersions/ suffix
	client     Client
	fipsMode   bool

	mu     sync.Mutex
	closed bool
}

// builder is the registry entry-point.  Reads the KEKRef +
// optional config map and constructs a real GCP KMS client.
//
// Config keys:
//
//	endpoint: ""            # custom endpoint (private VPC, emulator)
//	credentials_file: ""    # alternative service-account JSON path
//	use_fips_mode: false    # operator-declared FIPS posture
func builder(ctx context.Context, kekRef string, cfg map[string]any) (stdkms.Provider, error) {
	keyName, versionRef, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	endpoint, _ := cfg["endpoint"].(string)
	credsFile, _ := cfg["credentials_file"].(string)
	fipsMode, _ := cfg["use_fips_mode"].(bool)

	if endpoint != "" {
		if err := airgap.Default().EndpointAllowed(endpoint); err != nil {
			return nil, fmt.Errorf("gcp-kms: %w", err)
		}
	}

	var clientOpts []option.ClientOption
	if endpoint != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(endpoint))
	}
	if credsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(credsFile))
	}
	cli, err := gcpkmsv1.NewKeyManagementClient(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("gcp-kms: open client: %w", err)
	}

	return &Provider{
		kekRef:     kekRef,
		keyName:    keyName,
		versionRef: versionRef,
		client:     &realClient{c: cli},
		fipsMode:   fipsMode,
	}, nil
}

// realClient adapts *gcpkmsv1.KeyManagementClient to our
// Client interface (variadic option type erasure).
type realClient struct{ c *gcpkmsv1.KeyManagementClient }

// Encrypt implements Client by forwarding to the GCP KMS client.
func (r *realClient) Encrypt(ctx context.Context, req *kmspb.EncryptRequest, _ ...CallOption) (*kmspb.EncryptResponse, error) {
	return r.c.Encrypt(ctx, req)
}

// Decrypt implements Client by forwarding to the GCP KMS client.
func (r *realClient) Decrypt(ctx context.Context, req *kmspb.DecryptRequest, _ ...CallOption) (*kmspb.DecryptResponse, error) {
	return r.c.Decrypt(ctx, req)
}

// DestroyCryptoKeyVersion implements Client by forwarding to the
// GCP KMS DestroyCryptoKeyVersion RPC; this is the call that backs
// Shred (GCP schedules destruction after the configured retention).
func (r *realClient) DestroyCryptoKeyVersion(ctx context.Context, req *kmspb.DestroyCryptoKeyVersionRequest, _ ...CallOption) (*kmspb.CryptoKeyVersion, error) {
	return r.c.DestroyCryptoKeyVersion(ctx, req)
}

// GetCryptoKey implements Client by forwarding to the GCP KMS
// GetCryptoKey RPC.
func (r *realClient) GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest, _ ...CallOption) (*kmspb.CryptoKey, error) {
	return r.c.GetCryptoKey(ctx, req)
}

// Close implements Client by closing the underlying GCP KMS client.
func (r *realClient) Close() error { return r.c.Close() }

// NewWithClient is the test-friendly constructor — pass any
// Client implementation.
func NewWithClient(kekRef string, client Client, opts ...Option) (*Provider, error) {
	keyName, versionRef, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		kekRef:     kekRef,
		keyName:    keyName,
		versionRef: versionRef,
		client:     client,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Option tunes NewWithClient.
type Option func(*Provider)

// WithFIPSMode declares the provider's FIPS posture.
func WithFIPSMode(fips bool) Option { return func(p *Provider) { p.fipsMode = fips } }

// Name implements kms.Provider.
func (p *Provider) Name() string { return Scheme }

// KEKRef implements kms.Provider.
func (p *Provider) KEKRef() string { return p.kekRef }

// FIPSMode implements kms.Provider.
func (p *Provider) FIPSMode() bool { return p.fipsMode }

// WrapDEK implements kms.Provider.  Encrypt routes to the
// CryptoKey's primary version when no version is specified
// in the KEKRef.
func (p *Provider) WrapDEK(ctx context.Context, dek []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	keyForEncrypt := p.keyName
	if p.versionRef != "" {
		// When the operator pinned a version in the KEKRef,
		// encrypt under that exact version.
		keyForEncrypt = p.keyName + "/cryptoKeyVersions/" + p.versionRef
	}
	out, err := p.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                        keyForEncrypt,
		Plaintext:                   dek,
		AdditionalAuthenticatedData: aadBytes(),
	})
	if err != nil {
		return nil, fmt.Errorf("gcp-kms: Encrypt: %w", err)
	}
	return out.Ciphertext, nil
}

// UnwrapDEK implements kms.Provider.  GCP KMS picks the
// version from the ciphertext's metadata; we only pass the
// CryptoKey resource path.
func (p *Provider) UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	out, err := p.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:                        p.keyName,
		Ciphertext:                  wrapped,
		AdditionalAuthenticatedData: aadBytes(),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", stdkms.ErrUnwrap, err)
	}
	return out.Plaintext, nil
}

// Shred implements kms.Provider.  Schedules destruction of
// the version pinned in the KEKRef.  GCP destroys the
// material after a 24-hour cooldown by default.
//
// Refuses when the KEKRef has no /cryptoKeyVersions/<v>
// suffix — destroying "the key" without naming a version
// is ambiguous (GCP keys have N versions; destroying all
// of them via the wrapper would mask the operator's intent
// in the audit chain).
func (p *Provider) Shred(ctx context.Context) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	if p.versionRef == "" {
		return fmt.Errorf("%w: gcp-kms: Shred requires a version-pinned KEKRef (e.g. .../cryptoKeyVersions/3); aggregate-shred is intentionally not supported",
			stdkms.ErrShredFailed)
	}
	versionPath := p.keyName + "/cryptoKeyVersions/" + p.versionRef
	_, err := p.client.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{
		Name: versionPath,
	})
	if err != nil {
		return fmt.Errorf("%w: DestroyCryptoKeyVersion %s: %v", stdkms.ErrShredFailed, versionPath, err)
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

// DescribeKey is a GCP-specific helper used by `kms inspect`
// to surface key metadata.  Returns an opaque map so the
// CLI body doesn't depend on kmspb types.
func (p *Provider) DescribeKey(ctx context.Context) (map[string]any, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	out, err := p.client.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: p.keyName})
	if err != nil {
		return nil, fmt.Errorf("gcp-kms: GetCryptoKey: %w", err)
	}
	if out == nil {
		return nil, errors.New("gcp-kms: GetCryptoKey returned nil")
	}
	body := map[string]any{
		"name":    out.Name,
		"purpose": out.Purpose.String(),
	}
	if out.Primary != nil {
		body["primary_version"] = out.Primary.Name
		body["primary_state"] = out.Primary.State.String()
		body["primary_protection_level"] = out.Primary.ProtectionLevel.String()
	}
	// RotationSchedule is a oneof; the populated case carries
	// the rotation period.  We surface whatever the operator
	// has set, leaving the exact case-name out of the
	// stable-schema body so a future SDK adding new oneof
	// arms doesn't break our JSON shape.
	if rp, ok := out.RotationSchedule.(*kmspb.CryptoKey_RotationPeriod); ok && rp != nil && rp.RotationPeriod != nil {
		body["rotation_period_seconds"] = rp.RotationPeriod.Seconds
	}
	if out.NextRotationTime != nil {
		body["next_rotation_time"] = out.NextRotationTime.AsTime().Format("2006-01-02T15:04:05Z")
	}
	return body, nil
}

func (p *Provider) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("gcp-kms: provider closed")
	}
	return nil
}

// parseKEKRef splits the KEKRef into the CryptoKey resource
// path and (optionally) the version suffix.
//
//	gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k
//	  → keyName="projects/p/locations/l/keyRings/r/cryptoKeys/k", versionRef=""
//	gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/3
//	  → keyName="projects/p/locations/l/keyRings/r/cryptoKeys/k", versionRef="3"
func parseKEKRef(kekRef string) (keyName, versionRef string, err error) {
	if !strings.HasPrefix(kekRef, Scheme+"://") {
		return "", "", fmt.Errorf("gcp-kms: KEKRef %q does not have the %q:// prefix", kekRef, Scheme)
	}
	resource := strings.TrimPrefix(kekRef, Scheme+"://")
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return "", "", fmt.Errorf("gcp-kms: empty resource path in KEKRef %q", kekRef)
	}
	// Validate the basic shape: projects/.../locations/.../keyRings/.../cryptoKeys/...
	if !strings.HasPrefix(resource, "projects/") ||
		!strings.Contains(resource, "/locations/") ||
		!strings.Contains(resource, "/keyRings/") ||
		!strings.Contains(resource, "/cryptoKeys/") {
		return "", "", fmt.Errorf("gcp-kms: KEKRef %q is not a CryptoKey resource path (expected projects/.../cryptoKeys/...)", kekRef)
	}
	if i := strings.Index(resource, "/cryptoKeyVersions/"); i >= 0 {
		keyName = resource[:i]
		versionRef = strings.TrimPrefix(resource[i:], "/cryptoKeyVersions/")
		if versionRef == "" {
			return "", "", fmt.Errorf("gcp-kms: KEKRef %q has empty version suffix", kekRef)
		}
		return keyName, versionRef, nil
	}
	return resource, "", nil
}

// aadBytes returns the additional-authenticated-data we bind
// to every encrypt/decrypt.  Same posture as the AWS KMS
// provider's encryption_context: ties the ciphertext to
// pg_hardstorage so it can't be replayed against an
// unrelated key reference.
func aadBytes() []byte { return []byte("pg_hardstorage") }
