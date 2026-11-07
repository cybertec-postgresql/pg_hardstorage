// Contract suite for the Azure Blob plugin.  Drives the
// shared invariant set against Azurite (Microsoft's
// official emulator).  Same skip-on-no-docker pattern as
// the S3 / GCS bindings.
package azblob_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func openAzblobOnFreshAzurite(t *testing.T) storage.StoragePlugin {
	t.Helper()
	s := sink.UpForTest(t, "azurite")
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %s: %v", s.URL(), err)
	}
	p := &azblob.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("azblob.Open(%s): %v", s.URL(), err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestAzblob_Contract(t *testing.T) {
	contract.Run(t, openAzblobOnFreshAzurite)
}

func TestAzblob_Contract_ParallelPuts(t *testing.T) {
	contract.ParallelPuts(t, openAzblobOnFreshAzurite, 8)
}

func TestAzblob_Contract_ParallelOverwrites(t *testing.T) {
	contract.ParallelOverwrites(t, openAzblobOnFreshAzurite, 8)
}
