// Package awskms implements a kms.Provider backed by AWS Key
// Management Service.
//
// The cloud-side KEK is an AWS KMS Customer Master Key (CMK).
// Its bytes never leave the AWS KMS HSM; we only ever see
// `Encrypt` / `Decrypt` ciphertext blobs.  This is the
// strongest production-grade KEK posture we can offer
// without bringing PKCS#11 / on-prem HSM into the binary.
//
// KEKRef format:
//
//	aws-kms://<key-id-or-arn>
//
// Examples:
//
//	aws-kms://arn:aws:kms:us-east-1:123456789012:key/abcd1234-...
//	aws-kms://alias/pg-hardstorage-prod
//	aws-kms://12345678-1234-1234-1234-123456789012      (key-id only)
//
// The host part is parsed off, then handed verbatim to
// AWS KMS's `KeyId` parameter — AWS accepts ARNs, key-IDs,
// and alias references in the same field.
//
// # FIPS posture
//
// AWS KMS is a FIPS 140-2 Level 3 certified service in the
// us-gov-west-1 / us-gov-east-1 / us-east-1 / us-west-2
// regions.  When the operator points at a FIPS-validated
// region (or sets the FIPS endpoint via `aws_use_fips_endpoint`
// in the config), this provider's FIPSMode() returns true.
//
// # GDPR Art. 17 / crypto-shred
//
// Shred calls `ScheduleKeyDeletion` with the configured
// pending-window (default 30 days; AWS allows 7-30).  After
// the window elapses, AWS destroys the key material; every
// backup whose wrapped DEK was wrapped with this CMK becomes
// permanently unrecoverable — by design.  The audit chain
// records the schedule + deletion-date for compliance.
//
// # Air-gap interaction
//
// AWS KMS only resolves over the public internet (or via a
// VPC endpoint with a private IP).  Operators running in
// air-gap mode (PG_HARDSTORAGE_AIRGAPPED=1) point at a VPC
// endpoint that resolves to an RFC1918 address; the
// air-gap policy honours the routable-private-IP allowlist.
package awskms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// Scheme is the KEKRef scheme this provider claims.  Stable
// across versions; on-disk manifest values use this string.
const Scheme = "aws-kms"

// Default cool-off window when an operator doesn't override.
// AWS KMS allows 7-30 days; we pick the maximum to give
// operators every opportunity to recover from an accidental
// shred.
const DefaultPendingWindowDays = 30

func init() {
	stdkms.DefaultRegistry.Register(Scheme, builder)
}

// Client is the subset of the AWS KMS SDK we depend on.  Tests
// inject a fake; production code uses the real SDK client.
type Client interface {
	Encrypt(ctx context.Context, in *kms.EncryptInput, opts ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, opts ...func(*kms.Options)) (*kms.DecryptOutput, error)
	ScheduleKeyDeletion(ctx context.Context, in *kms.ScheduleKeyDeletionInput, opts ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error)
	DescribeKey(ctx context.Context, in *kms.DescribeKeyInput, opts ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
}

// Provider implements kms.Provider over AWS KMS.
type Provider struct {
	kekRef            string
	keyID             string
	pendingWindowDays int32
	client            Client
	fipsMode          bool

	mu     sync.Mutex
	closed bool
}

// builder is the registry entry-point.  Reads the KEKRef +
// optional config map and constructs a real-AWS-SDK client.
//
// Config keys:
//
//	region: us-east-1                # AWS region
//	endpoint: ""                     # custom endpoint (LocalStack, VPC interface)
//	pending_window_days: 30          # 7..30; AWS minimum is 7
//	use_fips_endpoint: false         # enable AWS FIPS endpoints
func builder(ctx context.Context, kekRef string, cfg map[string]any) (stdkms.Provider, error) {
	keyID, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	region, _ := cfg["region"].(string)
	endpoint, _ := cfg["endpoint"].(string)
	useFIPS, _ := cfg["use_fips_endpoint"].(bool)

	pendingWindow := int32(DefaultPendingWindowDays)
	if v, ok := cfg["pending_window_days"]; ok {
		switch x := v.(type) {
		case int:
			pendingWindow = int32(x)
		case int32:
			pendingWindow = x
		case int64:
			pendingWindow = int32(x)
		case float64:
			pendingWindow = int32(x)
		}
	}
	if pendingWindow < 7 || pendingWindow > 30 {
		return nil, fmt.Errorf("aws-kms: pending_window_days %d out of range (AWS allows 7..30)", pendingWindow)
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(region))
	}
	if useFIPS {
		loadOpts = append(loadOpts, awsconfig.WithUseFIPSEndpoint(aws.FIPSEndpointStateEnabled))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws-kms: load AWS config: %w", err)
	}
	clientOpts := []func(*kms.Options){}
	if endpoint != "" {
		// Endpoint air-gap gate.
		if err := airgap.Default().EndpointAllowed(endpoint); err != nil {
			return nil, fmt.Errorf("aws-kms: %w", err)
		}
		clientOpts = append(clientOpts, func(o *kms.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}
	cli := kms.NewFromConfig(awsCfg, clientOpts...)

	return &Provider{
		kekRef:            kekRef,
		keyID:             keyID,
		client:            cli,
		pendingWindowDays: pendingWindow,
		fipsMode:          useFIPS,
	}, nil
}

// NewWithClient is the test-friendly constructor — pass any
// Client implementation.
func NewWithClient(kekRef string, client Client, opts ...Option) (*Provider, error) {
	keyID, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		kekRef:            kekRef,
		keyID:             keyID,
		client:            client,
		pendingWindowDays: DefaultPendingWindowDays,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Option tunes NewWithClient.
type Option func(*Provider)

// WithPendingWindow overrides the cool-off window.
func WithPendingWindow(days int32) Option { return func(p *Provider) { p.pendingWindowDays = days } }

// WithFIPSMode declares the provider's FIPS posture (used
// when wiring up a fake client where FIPSMode() can't be
// inferred).
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
	out, err := p.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(p.keyID),
		Plaintext: dek,
		EncryptionContext: map[string]string{
			"app": "pg_hardstorage",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("aws-kms: Encrypt: %w", err)
	}
	return out.CiphertextBlob, nil
}

// UnwrapDEK implements kms.Provider.
func (p *Provider) UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	out, err := p.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:          aws.String(p.keyID),
		CiphertextBlob: wrapped,
		EncryptionContext: map[string]string{
			"app": "pg_hardstorage",
		},
	})
	if err != nil {
		// Wrap the SDK error in our typed sentinel so the
		// crypto-shred audit can distinguish "key disabled"
		// from "auth/permission" from "transient network."
		return nil, fmt.Errorf("%w: %v", stdkms.ErrUnwrap, err)
	}
	return out.Plaintext, nil
}

// Shred implements kms.Provider.  Schedules deletion of the
// CMK with the configured pending-window.  AWS runs the
// destruction at the end of the window unless the operator
// cancels via `aws kms cancel-key-deletion`.
func (p *Provider) Shred(ctx context.Context) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	out, err := p.client.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
		KeyId:               aws.String(p.keyID),
		PendingWindowInDays: aws.Int32(p.pendingWindowDays),
	})
	if err != nil {
		return fmt.Errorf("%w: ScheduleKeyDeletion %s: %v", stdkms.ErrShredFailed, p.keyID, err)
	}
	if out != nil && out.DeletionDate != nil {
		// Deletion-date logging is the operator's compliance
		// receipt — they need to know exactly when the key
		// material is gone, in case the window's about to
		// elapse on a backup they actually want to keep.
		_ = out.DeletionDate
	}
	return nil
}

// Close implements kms.Provider.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// DescribeKey is an awskms-specific helper used by `kms
// inspect` to surface key metadata (state, deletion-date,
// description) without going through the generic Provider
// interface.  Returns an opaque map so the CLI body doesn't
// depend on AWS SDK types.
func (p *Provider) DescribeKey(ctx context.Context) (map[string]any, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	out, err := p.client.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(p.keyID)})
	if err != nil {
		return nil, fmt.Errorf("aws-kms: DescribeKey: %w", err)
	}
	if out == nil || out.KeyMetadata == nil {
		return nil, errors.New("aws-kms: DescribeKey returned no metadata")
	}
	m := out.KeyMetadata
	body := map[string]any{
		"key_id":  awsString(m.KeyId),
		"arn":     awsString(m.Arn),
		"enabled": m.Enabled,
		"state":   string(m.KeyState),
	}
	if m.Description != nil {
		body["description"] = *m.Description
	}
	if m.DeletionDate != nil {
		body["deletion_date"] = m.DeletionDate.Format(time.RFC3339)
	}
	if m.KeyUsage != "" {
		body["key_usage"] = string(m.KeyUsage)
	}
	if m.KeySpec != "" {
		body["key_spec"] = string(m.KeySpec)
	}
	// Surface customer-managed vs AWS-managed (the latter is
	// almost never the right choice for backup KEKs).
	if m.KeyManager == kmstypes.KeyManagerTypeCustomer {
		body["key_manager"] = "customer"
	} else if m.KeyManager == kmstypes.KeyManagerTypeAws {
		body["key_manager"] = "aws"
	}
	return body, nil
}

func (p *Provider) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("aws-kms: provider closed")
	}
	return nil
}

// parseKEKRef extracts the AWS KMS key identifier from a
// KEKRef string.  Accepts:
//
//	aws-kms://<arn>
//	aws-kms://alias/<alias-name>
//	aws-kms://<key-id>
func parseKEKRef(kekRef string) (string, error) {
	if !strings.HasPrefix(kekRef, Scheme+"://") {
		return "", fmt.Errorf("aws-kms: KEKRef %q does not have the %q:// prefix", kekRef, Scheme)
	}
	id := strings.TrimPrefix(kekRef, Scheme+"://")
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("aws-kms: empty key id in KEKRef %q", kekRef)
	}
	return id, nil
}

func awsString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
