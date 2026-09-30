package gcs

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Without ?endpoint= the client dials storage.googleapis.com (or the
// emulator named by STORAGE_EMULATOR_HOST); `airgapped: strict` must
// check that host too, not only an explicit override.
func TestOpen_AirgapStrictChecksTheDefaultEndpoint(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	for _, tc := range []struct {
		name, rawURL, emulator string
		allowed                bool
	}{
		{"public default", "gcs://bkt/p", "", false},
		{"public override", "gcs://bkt/p?endpoint=https://gcs.example.com", "", false},
		{"loopback emulator", "gcs://bkt/p", "127.0.0.1:4443", true},
		{"loopback override", "gcs://bkt/p?endpoint=http://127.0.0.1:4443", "", true},
	} {
		t.Setenv("STORAGE_EMULATOR_HOST", tc.emulator)
		u, err := url.Parse(tc.rawURL)
		if err != nil {
			t.Fatal(err)
		}
		err = (&Plugin{}).Open(context.Background(), storage.StorageConfig{URL: u})
		refused := errors.Is(err, airgap.ErrEndpointNotAllowed)
		if tc.allowed && refused {
			t.Errorf("%s: refused under strict air-gap: %v", tc.name, err)
		}
		if !tc.allowed && !refused {
			t.Errorf("%s: Open = %v, want airgap.ErrEndpointNotAllowed", tc.name, err)
		}
	}
}
