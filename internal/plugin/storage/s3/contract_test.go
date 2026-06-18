// Contract suite for the S3 plugin.  Drives the same 11
// invariants as the fs binding, but against MinIO via the
// testkit's sink runtime — proving the s3 plugin honours
// the documented contract end-to-end (URL parsing, SigV4
// signing through the AWS SDK, path-style addressing,
// list/get/put/rename semantics).
//
// Cost
// ----
// Each contract sub-case brings up a fresh MinIO container
// in <3 s on a warm Docker daemon.  11 sub-cases ≈ 30-40 s
// total wall-clock.  If MinIO startup ever becomes a
// bottleneck we can refactor to share one container with
// per-sub-case prefix isolation; for now, fresh-per-case
// matches what the fs binding does (t.TempDir()) and avoids
// any cross-case pollution surface.
//
// Skip behaviour
// --------------
// When Docker isn't available (no daemon, no `docker` on
// PATH) the test SKIPS rather than fails — the contract is
// still defined, just not exercised in this environment.
// CI that requires the gate sets `PG_HARDSTORAGE_DEMAND_DOCKER=1`
// to flip the skip into a fatal.
package s3_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// requireDocker skips the calling test when Docker isn't
// reachable.  Honours PG_HARDSTORAGE_DEMAND_DOCKER=1 to
// flip the skip into a fatal — CI environments that
// promise Docker availability set the env var so an
// unexpectedly-skipped suite fails the build.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker not on PATH but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker not on PATH; skipping S3-via-MinIO contract suite")
	}
	// `docker info` is the canonical reachability probe;
	// passing means the daemon answered, failing means
	// either the daemon is down or the user lacks the
	// docker socket permission.
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker daemon not reachable but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker daemon not reachable; skipping")
	}
}

// openS3OnFreshMinIO is the PluginOpener the contract
// harness consumes.  Each call brings up a fresh MinIO
// container (so sub-cases don't share state), opens the
// s3 plugin against it, and registers t.Cleanup hooks for
// teardown.  AWS_* env vars are set with t.Setenv so the
// AWS SDK's default credential chain finds them — that's
// scoped to this sub-test and reverts automatically.
func openS3OnFreshMinIO(t *testing.T) storage.StoragePlugin {
	t.Helper()
	requireDocker(t)

	s, err := sink.New("s3-minio")
	if err != nil {
		t.Fatalf("sink.New(s3-minio): %v", err)
	}
	if err := s.Up(context.Background()); err != nil {
		t.Fatalf("sink.Up: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Down(context.Background())
	})

	// AWS SDK's default credential chain reads these env
	// vars first; t.Setenv scopes them to this sub-test
	// and Go reverts automatically at sub-test exit.
	for k, v := range s.EnvForAgent() {
		t.Setenv(k, v)
	}

	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %s: %v", s.URL(), err)
	}
	p := &s3.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("s3.Open(%s): %v", s.URL(), err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestS3_Contract(t *testing.T) {
	contract.Run(t, openS3OnFreshMinIO)
}

// TestS3_Contract_ParallelPuts is the opt-in
// concurrent-IfNotExists clause.  Bringing up a single
// MinIO and racing 8 goroutines through it exercises
// MinIO's check-then-write atomicity for IfNotExists
// (which it implements via S3's If-None-Match: *).
func TestS3_Contract_ParallelPuts(t *testing.T) {
	contract.ParallelPuts(t, openS3OnFreshMinIO, 8)
}

func TestS3_Contract_ParallelOverwrites(t *testing.T) {
	contract.ParallelOverwrites(t, openS3OnFreshMinIO, 8)
}
