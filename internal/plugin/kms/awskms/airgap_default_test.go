package awskms

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

// With no configured endpoint the client dials the public regional KMS
// host; `airgapped: strict` must check that too, not only an explicit
// endpoint override.
func TestBuilder_AirgapStrictChecksTheDefaultEndpoint(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_KMS", "")
	ref := "aws-kms://arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

	for _, tc := range []struct {
		name    string
		cfg     map[string]any
		allowed bool
	}{
		{"default public endpoint", map[string]any{"region": "eu-west-1"}, false},
		{"default FIPS endpoint", map[string]any{"region": "us-east-1", "use_fips_endpoint": true}, false},
		{"loopback override", map[string]any{"region": "eu-west-1", "endpoint": "http://127.0.0.1:4566"}, true},
	} {
		_, err := builder(context.Background(), ref, tc.cfg)
		refused := errors.Is(err, airgap.ErrEndpointNotAllowed)
		if tc.allowed && refused {
			t.Errorf("%s: refused under strict air-gap: %v", tc.name, err)
		}
		if !tc.allowed && !refused {
			t.Errorf("%s: builder = %v, want airgap.ErrEndpointNotAllowed", tc.name, err)
		}
	}
}
