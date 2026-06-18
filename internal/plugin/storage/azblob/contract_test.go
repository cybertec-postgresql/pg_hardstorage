// Contract suite for the Azure Blob plugin.  Drives the
// shared invariant set against Azurite (Microsoft's
// official emulator).  Same skip-on-no-docker pattern as
// the S3 / GCS bindings.
package azblob_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker not on PATH but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker not on PATH; skipping Azure-via-Azurite contract suite")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker daemon not reachable but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker daemon not reachable; skipping")
	}
}

func openAzblobOnFreshAzurite(t *testing.T) storage.StoragePlugin {
	t.Helper()
	requireDocker(t)
	s, err := sink.New("azurite")
	if err != nil {
		t.Fatalf("sink.New(azurite): %v", err)
	}
	if err := s.Up(context.Background()); err != nil {
		t.Fatalf("sink.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Down(context.Background()) })
	for k, v := range s.EnvForAgent() {
		t.Setenv(k, v)
	}
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
