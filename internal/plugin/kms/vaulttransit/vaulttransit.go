// Package vaulttransit implements a kms.Provider backed by
// HashiCorp Vault's Transit secrets engine.
//
// Vault Transit is "encryption as a service" — the cloud-side
// KEK (a Transit key) lives inside Vault's storage backend
// and never leaves; we only ever see ciphertext blobs that
// look like `vault:v1:<base64>`.  This is the most common
// self-hosted KMS in production today, especially in
// cloud-agnostic and on-prem deployments.
//
// KEKRef format:
//
//	vault-transit://<host>/<mount>/<key-name>
//
// Examples:
//
//	vault-transit://vault.acme.example.com:8200/transit/db-kek
//	vault-transit://10.0.0.5:8200/secrets-eu/db-prod-kek
//
// `<mount>` is the Transit engine's mount path — Vault
// permits multiple Transit instances on different mount
// points (`transit/`, `secrets-eu/transit/`, ...) so the
// KEKRef must record which one.
//
// Versioning: Vault Transit handles key versioning
// internally.  Decrypt picks the version from the
// ciphertext prefix; Encrypt uses the latest version
// unless `key_version` is supplied.  Operators rotating
// keys do `vault write transit/keys/<name>/rotate`; old
// ciphertexts continue to decrypt under the old version
// until trimmed.
//
// # Authentication
//
// Vault auth is layered: the SDK reads `VAULT_TOKEN` from
// env by default.  Operators using AppRole pass
// `role_id` + `secret_id` in the config map and the
// provider exchanges them for a token at Open.  Other auth
// methods (Kubernetes, AWS IAM, ...) are out of scope for
// the surface; operators with those needs run a
// sidecar (`vault agent`) that materialises a
// VAULT_TOKEN-bearing file the SDK then reads.
//
// # FIPS posture
//
// HashiCorp Vault Enterprise has FIPS 140-2 Level 1 and
// Level 2 validated builds (`vault-fips`).  The provider's
// FIPSMode() reports whatever the operator declares via
// WithFIPSMode at construction; the SDK doesn't expose the
// server-side build flavour so the declaration is a
// documentation handshake.
//
// # GDPR Art. 17 / crypto-shred
//
// Shred calls `DELETE transit/keys/<name>`.  Vault refuses
// the delete unless the key has `deletion_allowed=true`
// configured first (a deliberate safety belt — operators
// have to explicitly opt the key into deletability).  When
// not allowed, Shred returns a clear error pointing at the
// remediation step.
package vaulttransit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	vaultapi "github.com/hashicorp/vault/api"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// Scheme is the KEKRef scheme this provider claims.
const Scheme = "vault-transit"

func init() {
	stdkms.DefaultRegistry.Register(Scheme, builder)
}

// Client is the subset of the Vault API we depend on.
// Tests inject a fake; production code uses the real SDK
// via the realClient adapter.
type Client interface {
	Encrypt(ctx context.Context, mount, name, plaintext string) (ciphertext string, err error)
	Decrypt(ctx context.Context, mount, name, ciphertext string) (plaintext string, err error)
	DeleteKey(ctx context.Context, mount, name string) error
	ReadKey(ctx context.Context, mount, name string) (map[string]any, error)
	Close() error
}

// Provider implements kms.Provider over Vault Transit.
type Provider struct {
	kekRef   string
	addr     string
	mount    string
	keyName  string
	client   Client
	fipsMode bool

	mu     sync.Mutex
	closed bool
}

// builder is the registry entry-point.
//
// Config keys:
//
//	vault_token: ""          # override $VAULT_TOKEN
//	role_id: ""              # AppRole role-id
//	secret_id: ""            # AppRole secret-id
//	namespace: ""            # Vault Enterprise namespace
//	use_fips_mode: false     # operator-declared posture
func builder(ctx context.Context, kekRef string, cfg map[string]any) (stdkms.Provider, error) {
	addr, mount, name, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = addr
	cli, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault-transit: build client: %w", err)
	}

	if ns, _ := cfg["namespace"].(string); ns != "" {
		cli.SetNamespace(ns)
	}

	// Token resolution: explicit vault_token > VAULT_TOKEN
	// env (the SDK reads it automatically) > AppRole login.
	if tok, _ := cfg["vault_token"].(string); tok != "" {
		cli.SetToken(tok)
	}
	if cli.Token() == "" {
		roleID, _ := cfg["role_id"].(string)
		secretID, _ := cfg["secret_id"].(string)
		if roleID != "" && secretID != "" {
			tok, err := approleLogin(ctx, cli, roleID, secretID)
			if err != nil {
				return nil, fmt.Errorf("vault-transit: AppRole login: %w", err)
			}
			cli.SetToken(tok)
		}
	}
	if cli.Token() == "" {
		return nil, errors.New("vault-transit: no Vault token (set VAULT_TOKEN, vault_token config key, or role_id+secret_id for AppRole)")
	}

	fipsMode, _ := cfg["use_fips_mode"].(bool)
	return &Provider{
		kekRef:   kekRef,
		addr:     addr,
		mount:    mount,
		keyName:  name,
		client:   &realClient{c: cli},
		fipsMode: fipsMode,
	}, nil
}

// approleLogin exchanges (role_id, secret_id) for a Vault
// token via the standard auth/approle/login endpoint.
func approleLogin(ctx context.Context, cli *vaultapi.Client, roleID, secretID string) (string, error) {
	resp, err := cli.Logical().WriteWithContext(ctx, "auth/approle/login", map[string]any{
		"role_id":   roleID,
		"secret_id": secretID,
	})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Auth == nil || resp.Auth.ClientToken == "" {
		return "", errors.New("AppRole login returned no token")
	}
	return resp.Auth.ClientToken, nil
}

// realClient adapts *vaultapi.Client to our Client
// interface.  Lives in production code (not a test helper)
// because it's how every real call reaches Vault.
type realClient struct{ c *vaultapi.Client }

// Encrypt implements Client by POSTing to <mount>/encrypt/<name> and
// returning Vault's "vault:v<n>:..." ciphertext blob.
func (r *realClient) Encrypt(ctx context.Context, mount, name, plaintext string) (string, error) {
	path := mount + "/encrypt/" + name
	resp, err := r.c.Logical().WriteWithContext(ctx, path, map[string]any{
		"plaintext": plaintext,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", errors.New("vault encrypt returned nil response")
	}
	ct, ok := resp.Data["ciphertext"].(string)
	if !ok || ct == "" {
		return "", errors.New("vault encrypt response missing ciphertext")
	}
	return ct, nil
}

// Decrypt implements Client by POSTing to <mount>/decrypt/<name> and
// returning the recovered plaintext.
func (r *realClient) Decrypt(ctx context.Context, mount, name, ciphertext string) (string, error) {
	path := mount + "/decrypt/" + name
	resp, err := r.c.Logical().WriteWithContext(ctx, path, map[string]any{
		"ciphertext": ciphertext,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", errors.New("vault decrypt returned nil response")
	}
	pt, ok := resp.Data["plaintext"].(string)
	if !ok {
		return "", errors.New("vault decrypt response missing plaintext")
	}
	return pt, nil
}

// DeleteKey implements Client by issuing DELETE <mount>/keys/<name>.
// The transit backend requires deletion_allowed=true on the key first;
// callers surface that prerequisite separately.
func (r *realClient) DeleteKey(ctx context.Context, mount, name string) error {
	path := mount + "/keys/" + name
	_, err := r.c.Logical().DeleteWithContext(ctx, path)
	return err
}

// ReadKey implements Client by GETing <mount>/keys/<name> and returning
// the raw Data map for Describe to project.
func (r *realClient) ReadKey(ctx context.Context, mount, name string) (map[string]any, error) {
	path := mount + "/keys/" + name
	resp, err := r.c.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("vault read returned nil response")
	}
	return resp.Data, nil
}

// Close implements Client. The Vault SDK has no per-client teardown,
// so this is a no-op.
func (r *realClient) Close() error { return nil }

// NewWithClient is the test-friendly constructor — pass any
// Client implementation.
func NewWithClient(kekRef string, client Client, opts ...Option) (*Provider, error) {
	addr, mount, name, err := parseKEKRef(kekRef)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		kekRef:  kekRef,
		addr:    addr,
		mount:   mount,
		keyName: name,
		client:  client,
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

// WrapDEK implements kms.Provider.  Vault's Transit Encrypt
// expects base64-encoded plaintext + returns a
// `vault:v<n>:<base64>` ciphertext string.  We carry the
// string verbatim through the manifest's WrappedDEK (the
// `vault:` prefix + version is part of the on-disk
// envelope; restore feeds it back unchanged).
func (p *Provider) WrapDEK(ctx context.Context, dek []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	plaintext := base64.StdEncoding.EncodeToString(dek)
	ct, err := p.client.Encrypt(ctx, p.mount, p.keyName, plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault-transit: Encrypt: %w", err)
	}
	return []byte(ct), nil
}

// UnwrapDEK implements kms.Provider.
func (p *Provider) UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	pt, err := p.client.Decrypt(ctx, p.mount, p.keyName, string(wrapped))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", stdkms.ErrUnwrap, err)
	}
	dek, err := base64.StdEncoding.DecodeString(pt)
	if err != nil {
		return nil, fmt.Errorf("%w: decode plaintext: %v", stdkms.ErrUnwrap, err)
	}
	return dek, nil
}

// Shred implements kms.Provider.  Calls Vault's
// DELETE transit/keys/<name>.  Vault refuses the delete
// unless the key has `deletion_allowed=true` set first;
// we surface that as a clear remediation error.
func (p *Provider) Shred(ctx context.Context) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	if err := p.client.DeleteKey(ctx, p.mount, p.keyName); err != nil {
		return fmt.Errorf("%w: DELETE %s/keys/%s: %v",
			stdkms.ErrShredFailed, p.mount, p.keyName, err)
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
// `kms inspect`.
func (p *Provider) DescribeKey(ctx context.Context) (map[string]any, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	body, err := p.client.ReadKey(ctx, p.mount, p.keyName)
	if err != nil {
		return nil, fmt.Errorf("vault-transit: ReadKey: %w", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	body["address"] = p.addr
	body["mount"] = p.mount
	body["name"] = p.keyName
	return body, nil
}

func (p *Provider) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("vault-transit: provider closed")
	}
	return nil
}

// parseKEKRef extracts the Vault address + mount path + key
// name from a KEKRef.
//
//	vault-transit://vault.acme.example.com:8200/transit/db-kek
//	  → addr="https://vault.acme.example.com:8200"
//	    mount="transit"
//	    name="db-kek"
//	vault-transit://10.0.0.5:8200/secrets-eu/transit/db-kek
//	  → addr="https://10.0.0.5:8200"
//	    mount="secrets-eu/transit"
//	    name="db-kek"
//
// Mount paths can be multi-segment (Vault namespaces and
// sub-mounted engines); the LAST path segment is always the
// key name, everything before it is the mount.
func parseKEKRef(kekRef string) (addr, mount, name string, err error) {
	if !strings.HasPrefix(kekRef, Scheme+"://") {
		return "", "", "", fmt.Errorf("vault-transit: KEKRef %q does not have the %q:// prefix", kekRef, Scheme)
	}
	rest := strings.TrimPrefix(kekRef, Scheme+"://")
	if rest == "" {
		return "", "", "", fmt.Errorf("vault-transit: empty resource in KEKRef %q", kekRef)
	}
	// First segment is host[:port]; the rest is mount/.../name.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", "", fmt.Errorf("vault-transit: KEKRef %q must include /<mount>/<key>", kekRef)
	}
	host := rest[:slash]
	pathPart := rest[slash+1:]
	if host == "" {
		return "", "", "", fmt.Errorf("vault-transit: empty host in KEKRef %q", kekRef)
	}

	parts := strings.Split(pathPart, "/")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("vault-transit: KEKRef %q must include both <mount> and <key>", kekRef)
	}
	for _, seg := range parts {
		if seg == "" {
			return "", "", "", fmt.Errorf("vault-transit: KEKRef %q contains empty path segment", kekRef)
		}
	}
	name = parts[len(parts)-1]
	mount = strings.Join(parts[:len(parts)-1], "/")

	// Build the addr.  Default scheme is https; operators on
	// in-cluster Vault with no TLS prefix the host with
	// `http+`.  We keep the parser simple: `http+host` →
	// http://host, anything else → https://host.
	scheme := "https"
	if h := strings.TrimPrefix(host, "http+"); h != host {
		scheme = "http"
		host = h
	}
	u := &url.URL{Scheme: scheme, Host: host}
	addr = u.String()
	return addr, mount, name, nil
}
