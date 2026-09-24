package gcpkms_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/api/googleapi"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/gcpkms"
)

// TestUnwrapDEK_ClassifiesFailures pins M34 for gcp-kms: every
// Decrypt failure used to be ErrUnwrap (%v), i.e. `kek_mismatch`.
func TestUnwrapDEK_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		want        error
		notUnwrap   bool
		unreachable bool
	}{
		{"unavailable", &googleapi.Error{Code: 503}, stdkms.ErrUnavailable, true, true},
		{"quota", &googleapi.Error{Code: 429}, stdkms.ErrUnavailable, true, true},
		{"permission", &googleapi.Error{Code: 403}, stdkms.ErrAccessDenied, true, false},
		{"bad ciphertext", &googleapi.Error{Code: 400}, stdkms.ErrUnwrap, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := gcpkms.NewWithClient(sampleKeyRef, &fakeGCPKMS{decryptErr: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			_, got := p.UnwrapDEK(context.Background(), []byte("ENC:x"))
			if !errors.Is(got, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", got, tc.want)
			}
			if tc.notUnwrap && errors.Is(got, stdkms.ErrUnwrap) {
				t.Errorf("err = %v is ErrUnwrap (reported as kek_mismatch)", got)
			}
			if stdkms.IsUnreachable(got) != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v", !tc.unreachable, tc.unreachable)
			}
			var gerr *googleapi.Error
			if !errors.As(got, &gerr) {
				t.Errorf("typed SDK cause lost (wrapped with %%v): %v", got)
			}
		})
	}
}
