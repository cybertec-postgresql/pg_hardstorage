package kms_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

type fakeProvider struct {
	name   string
	kekRef string
	closed bool
}

func (f *fakeProvider) Name() string                                          { return f.name }
func (f *fakeProvider) KEKRef() string                                        { return f.kekRef }
func (f *fakeProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) { return dek, nil }
func (f *fakeProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	return wrapped, nil
}
func (f *fakeProvider) Shred(_ context.Context) error { return nil }
func (f *fakeProvider) FIPSMode() bool                { return false }
func (f *fakeProvider) Close() error                  { f.closed = true; return nil }

func TestSchemeOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"aws-kms://arn:aws:kms:us-east-1:123:key/abc", "aws-kms"},
		{"local:default", "local"},
		{"gcp-kms://projects/foo/locations/global/keyRings/bar/cryptoKeys/baz", "gcp-kms"},
		{"vault-transit://vault.example.com:8200/transit/keys/pg", "vault-transit"},
		{"", ""},
		{"garbage", ""},
	}
	for _, tc := range cases {
		if got := kms.SchemeOf(tc.in); got != tc.want {
			t.Errorf("SchemeOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRegistry_RoundTrip(t *testing.T) {
	r := kms.NewRegistry()
	r.Register("fake", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &fakeProvider{name: "fake", kekRef: ref}, nil
	})

	p, err := r.Open(context.Background(), "fake://my-key", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if p.KEKRef() != "fake://my-key" {
		t.Errorf("KEKRef = %q", p.KEKRef())
	}
	wrapped, err := p.WrapDEK(context.Background(), []byte("dek-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	dek, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(dek) != "dek-bytes" {
		t.Errorf("round-trip lost bytes: %q", dek)
	}
}

func TestRegistry_UnknownSchemeRefuses(t *testing.T) {
	r := kms.NewRegistry()
	_, err := r.Open(context.Background(), "no-such-scheme://anything", nil)
	if !errors.Is(err, kms.ErrUnknownScheme) {
		t.Errorf("expected ErrUnknownScheme, got %v", err)
	}
}

func TestRegistry_BuilderError(t *testing.T) {
	r := kms.NewRegistry()
	r.Register("broken", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
		return nil, errors.New("intentional failure")
	})
	_, err := r.Open(context.Background(), "broken://x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "intentional failure") {
		t.Errorf("error doesn't carry builder cause: %v", err)
	}
}

func TestRegistry_OverridePrecedence(t *testing.T) {
	r := kms.NewRegistry()
	r.Register("scheme", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
		return &fakeProvider{name: "v1"}, nil
	})
	// Tier-2 plugin overrides Tier-1 default.
	r.Register("scheme", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
		return &fakeProvider{name: "v2"}, nil
	})
	p, err := r.Open(context.Background(), "scheme://anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "v2" {
		t.Errorf("override didn't win: %s", p.Name())
	}
}

func TestRegistry_Schemes(t *testing.T) {
	r := kms.NewRegistry()
	r.Register("alpha", nil)
	r.Register("beta", nil)
	r.Register("gamma", nil)
	got := r.Schemes()
	if len(got) != 3 {
		t.Errorf("Schemes() = %v, want 3 entries", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && (s[:len(sub)] == sub || s[len(s)-len(sub):] == sub || indexAny(s, sub))))
}

func indexAny(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
