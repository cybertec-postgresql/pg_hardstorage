package acl_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/acl"
)

type signerFromKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s signerFromKey) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s signerFromKey) PublicKey() ed25519.PublicKey { return s.pub }

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func mustSigner(t *testing.T) (signerFromKey, string) {
	t.Helper()
	pub, priv := mustKeypair(t)
	return signerFromKey{pub: pub, priv: priv}, acl.PublicKeyFingerprint(pub)
}

// resolverFor returns a func that maps the given fingerprint to
// the given pubkey; any other fingerprint is "not found".
func resolverFor(want string, key ed25519.PublicKey) func(string) (ed25519.PublicKey, error) {
	return func(fp string) (ed25519.PublicKey, error) {
		if fp == want {
			return key, nil
		}
		return nil, errors.New("acl_test: unknown fingerprint")
	}
}

// freshStorage spins up a fresh repo + fs storage plugin.
func freshStorage(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("file://" + root)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// ----- sign + verify round-trip -----

func TestSignVerifySource_RoundTrip(t *testing.T) {
	sgn, fp := mustSigner(t)
	p := &acl.SourcePolicy{
		Description: "Beta data permitted to flow to Acme",
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme-eu/*",
				AcceptedSignerFingerprints: []string{"abc123"}},
		},
		Classification: acl.ClassConfidential,
		Tenants:        []string{"beta-tenant-a"},
		CreatedAt:      time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := acl.SignSource(p, sgn, "beta-prod-admins"); err != nil {
		t.Fatal(err)
	}
	if p.Signature == "" {
		t.Errorf("Signature empty")
	}
	if err := acl.VerifySource(p, resolverFor(fp, sgn.pub)); err != nil {
		t.Errorf("VerifySource: %v", err)
	}
}

func TestSignVerifyAccept_RoundTrip(t *testing.T) {
	sgn, fp := mustSigner(t)
	p := &acl.AcceptPolicy{
		Description: "Acme accepts Beta data",
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta-eu/*",
				AcceptedSignerFingerprints: []string{"def456"}},
		},
		MinClassification: acl.ClassConfidential,
		TenantsAllowed:    []string{"beta-tenant-a", "beta-tenant-b"},
		CreatedAt:         time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := acl.SignAccept(p, sgn, "acme-acquisitions"); err != nil {
		t.Fatal(err)
	}
	if err := acl.VerifyAccept(p, resolverFor(fp, sgn.pub)); err != nil {
		t.Errorf("VerifyAccept: %v", err)
	}
}

func TestVerifySource_TamperedClassification(t *testing.T) {
	sgn, fp := mustSigner(t)
	p := &acl.SourcePolicy{
		Classification:        acl.ClassRestricted,
		PermittedDestinations: []acl.DestinationGrant{{RepoURLPattern: "s3://x/"}},
		CreatedAt:             time.Now().UTC(),
	}
	_ = acl.SignSource(p, sgn, "alice")
	p.Classification = acl.ClassPublic // attacker downgrades
	if err := acl.VerifySource(p, resolverFor(fp, sgn.pub)); !errors.Is(err, acl.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// ----- Authorize -----

func TestAuthorize_HappyPath(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme-eu/repo",
				AcceptedSignerFingerprints: []string{"acme-fp"}},
		},
		Classification: acl.ClassConfidential,
		Tenants:        []string{"beta-tenant-a"},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta-eu/repo",
				AcceptedSignerFingerprints: []string{"beta-fp"}},
		},
		MinClassification: acl.ClassConfidential,
		TenantsAllowed:    []string{"beta-tenant-a", "beta-tenant-b"},
	}
	if err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://beta-eu/repo", SourceSignerFingerprint: "beta-fp",
		DestinationURL: "s3://acme-eu/repo", DestinationSignerFingerprint: "acme-fp",
		Tenants: []string{"beta-tenant-a"},
	}); err != nil {
		t.Errorf("Authorize: %v", err)
	}
}

func TestAuthorize_SourceRefusesUnknownDestination(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme-eu/repo"},
		},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta-eu/repo"},
		},
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL:      "s3://beta-eu/repo",
		DestinationURL: "s3://stranger/x", // not in source's permits
	})
	if !errors.Is(err, acl.ErrSourceRefuses) {
		t.Errorf("err = %v, want ErrSourceRefuses", err)
	}
}

func TestAuthorize_SourceRefusesUnknownSigner(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme/*", AcceptedSignerFingerprints: []string{"acme-fp"}},
		},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta/*"},
		},
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://beta/x", DestinationURL: "s3://acme/x",
		DestinationSignerFingerprint: "different-fp",
	})
	if !errors.Is(err, acl.ErrSourceRefuses) {
		t.Errorf("err = %v, want ErrSourceRefuses", err)
	}
}

func TestAuthorize_AcceptRefusesUnknownSource(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme/*"},
		},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta-only/*"},
		},
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://stranger/", DestinationURL: "s3://acme/x",
	})
	if !errors.Is(err, acl.ErrAcceptRefuses) {
		t.Errorf("err = %v, want ErrAcceptRefuses", err)
	}
}

func TestAuthorize_ClassificationTooLow(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{{RepoURLPattern: "s3://acme/*"}},
		Classification:        acl.ClassPublic,
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources:   []acl.SourceGrant{{RepoURLPattern: "s3://beta/*"}},
		MinClassification: acl.ClassConfidential,
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://beta/x", DestinationURL: "s3://acme/x",
	})
	if !errors.Is(err, acl.ErrClassificationTooLow) {
		t.Errorf("err = %v, want ErrClassificationTooLow", err)
	}
}

func TestAuthorize_TenantMismatch_NotInSourceSet(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{{RepoURLPattern: "s3://acme/*"}},
		Tenants:               []string{"only-tenant-a"},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{{RepoURLPattern: "s3://beta/*"}},
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://beta/x", DestinationURL: "s3://acme/x",
		Tenants: []string{"forbidden-tenant"},
	})
	if !errors.Is(err, acl.ErrTenantMismatch) {
		t.Errorf("err = %v, want ErrTenantMismatch", err)
	}
}

func TestAuthorize_TenantMismatch_NotInAcceptSet(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{{RepoURLPattern: "s3://acme/*"}},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{{RepoURLPattern: "s3://beta/*"}},
		TenantsAllowed:  []string{"only-tenant-a"},
	}
	err := acl.Authorize(source, accept, acl.Request{
		SourceURL: "s3://beta/x", DestinationURL: "s3://acme/x",
		Tenants: []string{"another-tenant"},
	})
	if !errors.Is(err, acl.ErrTenantMismatch) {
		t.Errorf("err = %v, want ErrTenantMismatch", err)
	}
}

func TestAuthorize_WildcardURLMatches(t *testing.T) {
	source := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme-eu/*"},
		},
	}
	accept := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta/*"},
		},
	}
	if err := acl.Authorize(source, accept, acl.Request{
		SourceURL:      "s3://beta/some/path",
		DestinationURL: "s3://acme-eu/some/other/path",
	}); err != nil {
		t.Errorf("wildcard match should succeed: %v", err)
	}
}

func TestAuthorize_NilPoliciesRefused(t *testing.T) {
	if err := acl.Authorize(nil, &acl.AcceptPolicy{}, acl.Request{}); err == nil {
		t.Errorf("nil source should be refused")
	}
	if err := acl.Authorize(&acl.SourcePolicy{}, nil, acl.Request{}); err == nil {
		t.Errorf("nil accept should be refused")
	}
}

// ----- storage round-trip -----

func TestSaveLoadSource_RoundTrip(t *testing.T) {
	sp := freshStorage(t)
	sgn, fp := mustSigner(t)
	p := &acl.SourcePolicy{
		PermittedDestinations: []acl.DestinationGrant{
			{RepoURLPattern: "s3://acme/*"},
		},
		Classification: acl.ClassConfidential,
		Tenants:        []string{"a"},
		CreatedAt:      time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	_ = acl.SignSource(p, sgn, "alice")
	if err := acl.SaveSource(context.Background(), sp, p); err != nil {
		t.Fatal(err)
	}
	got, err := acl.LoadSource(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Classification != p.Classification {
		t.Errorf("classification drift: %s", got.Classification)
	}
	if err := acl.VerifySource(got, resolverFor(fp, sgn.pub)); err != nil {
		t.Errorf("VerifySource on read-back: %v", err)
	}
}

func TestLoadSource_NotFound(t *testing.T) {
	sp := freshStorage(t)
	_, err := acl.LoadSource(context.Background(), sp)
	if !errors.Is(err, acl.ErrPolicyNotFound) {
		t.Errorf("err = %v, want ErrPolicyNotFound", err)
	}
}

func TestSaveLoadAccept_RoundTrip(t *testing.T) {
	sp := freshStorage(t)
	sgn, fp := mustSigner(t)
	p := &acl.AcceptPolicy{
		AcceptedSources: []acl.SourceGrant{
			{RepoURLPattern: "s3://beta/*"},
		},
		MinClassification: acl.ClassConfidential,
		CreatedAt:         time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	_ = acl.SignAccept(p, sgn, "ops")
	if err := acl.SaveAccept(context.Background(), sp, p); err != nil {
		t.Fatal(err)
	}
	got, err := acl.LoadAccept(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := acl.VerifyAccept(got, resolverFor(fp, sgn.pub)); err != nil {
		t.Errorf("VerifyAccept on read-back: %v", err)
	}
}
