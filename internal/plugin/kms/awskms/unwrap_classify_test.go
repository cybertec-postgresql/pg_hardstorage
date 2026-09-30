package awskms_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/smithy-go"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/awskms"
)

// TestUnwrapDEK_ClassifiesFailures pins M34: every Decrypt failure was
// wrapped as ErrUnwrap with %v, so throttling, AccessDenied, a 5xx and
// a cancelled context were all reported as `restore.kek_mismatch`, and
// the typed cause was lost.
func TestUnwrapDEK_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		ctxCanceled bool
		want        error // sentinel that must match
		notUnwrap   bool
		unreachable bool
	}{
		{name: "throttled", err: &smithy.GenericAPIError{Code: "ThrottlingException"}, want: stdkms.ErrUnavailable, notUnwrap: true, unreachable: true},
		{name: "internal", err: &smithy.GenericAPIError{Code: "KMSInternalException"}, want: stdkms.ErrUnavailable, notUnwrap: true, unreachable: true},
		{name: "access denied", err: &smithy.GenericAPIError{Code: "AccessDeniedException"}, want: stdkms.ErrAccessDenied, notUnwrap: true},
		{name: "wrong key", err: &smithy.GenericAPIError{Code: "IncorrectKeyException"}, want: stdkms.ErrUnwrap},
		{name: "bad ciphertext", err: &smithy.GenericAPIError{Code: "InvalidCiphertextException"}, want: stdkms.ErrUnwrap},
		{name: "ctx cancelled", err: context.Canceled, ctxCanceled: true, want: context.Canceled, notUnwrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := awskms.NewWithClient("aws-kms://arn:aws:kms:us-east-1:123:key/abcd", &fakeKMS{decryptErr: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ctxCanceled {
				cancel()
			}
			_, got := p.UnwrapDEK(ctx, []byte("WRAP:x"))
			if !errors.Is(got, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", got, tc.want)
			}
			if tc.notUnwrap && errors.Is(got, stdkms.ErrUnwrap) {
				t.Errorf("err = %v is ErrUnwrap (reported as kek_mismatch); it says nothing about the key", got)
			}
			if stdkms.IsUnreachable(got) != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v", !tc.unreachable, tc.unreachable)
			}
			var ae smithy.APIError
			if _, isAPI := tc.err.(smithy.APIError); isAPI && !errors.As(got, &ae) {
				t.Errorf("typed SDK cause lost (wrapped with %%v): %v", got)
			}
		})
	}
}
