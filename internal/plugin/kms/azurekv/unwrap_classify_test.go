package azurekv_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/azurekv"
)

// TestUnwrapDEK_ClassifiesFailures pins M34 for azure-kv: every
// UnwrapKey failure used to be ErrUnwrap (%v), i.e. `kek_mismatch`.
func TestUnwrapDEK_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		want        error
		notUnwrap   bool
		unreachable bool
	}{
		{"throttled", 429, stdkms.ErrUnavailable, true, true},
		{"server error", 503, stdkms.ErrUnavailable, true, true},
		{"forbidden", 403, stdkms.ErrAccessDenied, true, false},
		{"unauthorized", 401, stdkms.ErrAccessDenied, true, false},
		{"bad request", 400, stdkms.ErrUnwrap, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &azcore.ResponseError{StatusCode: tc.status, ErrorCode: "X"}
			p, err := azurekv.NewWithClient(sampleKeyRef, &fakeAzureKV{unwrapErr: cause})
			if err != nil {
				t.Fatal(err)
			}
			_, got := p.UnwrapDEK(context.Background(), []byte("wrapped-bytes-longer-than-a-dek-000000000"))
			if !errors.Is(got, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", got, tc.want)
			}
			if tc.notUnwrap && errors.Is(got, stdkms.ErrUnwrap) {
				t.Errorf("err = %v is ErrUnwrap (reported as kek_mismatch)", got)
			}
			if stdkms.IsUnreachable(got) != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v", !tc.unreachable, tc.unreachable)
			}
			var re *azcore.ResponseError
			if !errors.As(got, &re) {
				t.Errorf("typed SDK cause lost (wrapped with %%v): %v", got)
			}
		})
	}
}
