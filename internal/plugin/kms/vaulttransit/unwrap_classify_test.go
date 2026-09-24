package vaulttransit_test

import (
	"context"
	"errors"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/vaulttransit"
)

// TestUnwrapDEK_ClassifiesFailures pins M34 for vault-transit: every
// Decrypt failure used to be ErrUnwrap (%v), i.e. `kek_mismatch`.
func TestUnwrapDEK_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		want        error
		notUnwrap   bool
		unreachable bool
	}{
		{"rate limited", 429, stdkms.ErrUnavailable, true, true},
		{"sealed", 503, stdkms.ErrUnavailable, true, true},
		{"permission denied", 403, stdkms.ErrAccessDenied, true, false},
		{"bad ciphertext", 400, stdkms.ErrUnwrap, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &vaultapi.ResponseError{StatusCode: tc.status, Errors: []string{"x"}}
			p, err := vaulttransit.NewWithClient(sampleKeyRef, &fakeVault{decryptErr: cause})
			if err != nil {
				t.Fatal(err)
			}
			_, got := p.UnwrapDEK(context.Background(), []byte("vault:v1:xxx"))
			if !errors.Is(got, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", got, tc.want)
			}
			if tc.notUnwrap && errors.Is(got, stdkms.ErrUnwrap) {
				t.Errorf("err = %v is ErrUnwrap (reported as kek_mismatch)", got)
			}
			if stdkms.IsUnreachable(got) != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v", !tc.unreachable, tc.unreachable)
			}
			var re *vaultapi.ResponseError
			if !errors.As(got, &re) {
				t.Errorf("typed SDK cause lost (wrapped with %%v): %v", got)
			}
		})
	}
}
