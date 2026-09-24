package s3_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// TestOpen_AirgapStrictChecksEveryEndpoint pins the s3 half of H19:
// `airgapped: strict` promises every outbound path is checked, and the
// s3 plugin checked none — neither a custom ?endpoint= nor the default
// public AWS endpoint it falls back to.
func TestOpen_AirgapStrictChecksEveryEndpoint(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"s3://bkt/p?region=eu-west-1", false},                                    // public AWS
		{"s3://bkt/p?region=eu-west-1&endpoint=https://minio.example.com", false}, // public custom
		{"s3://bkt/p?region=eu-west-1&endpoint=http://10.0.0.5:9000", true},       // RFC1918
		{"s3://bkt/p?region=eu-west-1&endpoint=http://127.0.0.1:9000", true},      // loopback
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		p := &s3.Plugin{}
		err = p.Open(context.Background(), storage.StorageConfig{URL: u})
		refused := errors.Is(err, airgap.ErrEndpointNotAllowed)
		if tc.allowed && err != nil {
			t.Errorf("%s: Open = %v, want allowed under strict air-gap", tc.url, err)
		}
		if !tc.allowed && !refused {
			t.Errorf("%s: Open = %v, want airgap.ErrEndpointNotAllowed", tc.url, err)
		}
	}
}
