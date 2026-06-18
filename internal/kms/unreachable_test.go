package kms_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// TestIsUnreachable pins the classifier behind the kms.unreachable fix:
// network-reachability failures against the KMS endpoint are recognised
// (so callers can map them to ExitUnreachable/8), while structured KMS
// errors (wrong key, access denied) are not.
func TestIsUnreachable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"net.OpError dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"DNS no such host", &net.DNSError{Err: "no such host", Name: "kms.example.com", IsNotFound: true}, true},
		{"syscall ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped net error", fmt.Errorf("aws-kms: wrap dek: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"opaque sdk connection refused", errors.New("RequestError: send request failed caused by: dial tcp 10.0.0.1:443: connect: connection refused"), true},
		{"opaque tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"access denied (not network)", errors.New("AccessDeniedException: not authorized to perform kms:Decrypt"), false},
		{"wrong-key unwrap (not network)", fmt.Errorf("decrypt: %w", kms.ErrUnwrap), false},
		{"unknown scheme (not network)", kms.ErrUnknownScheme, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kms.IsUnreachable(tc.err); got != tc.want {
				t.Errorf("IsUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
