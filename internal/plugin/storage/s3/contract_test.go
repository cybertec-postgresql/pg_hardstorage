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
// sink.UpForTest decides: the suite SKIPS under `go test -short` and
// when Docker isn't available (no daemon, no `docker` on PATH) — the
// contract is still defined, just not exercised in this environment.
// CI that requires the gate sets `PG_HARDSTORAGE_DEMAND_DOCKER=1`,
// which runs it under -short too and turns a missing daemon into a
// failure. Container start is bounded, and a sink that keeps failing
// to start fails the remaining sub-cases at once.
package s3_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// openS3OnFreshMinIO is the PluginOpener the contract
// harness consumes.  Each call brings up a fresh MinIO
// container (so sub-cases don't share state), opens the
// s3 plugin against it, and registers t.Cleanup hooks for
// teardown.  AWS_* env vars are set with t.Setenv so the
// AWS SDK's default credential chain finds them — that's
// scoped to this sub-test and reverts automatically.
func openS3OnFreshMinIO(t *testing.T) storage.StoragePlugin {
	t.Helper()
	s := sink.UpForTest(t, "s3-minio")

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
