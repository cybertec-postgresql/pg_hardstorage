// Contract suite for the GCS plugin.  Same shape as the S3
// binding: each sub-case brings up a fresh
// fake-gcs-server container, opens a fresh plugin against
// it, and runs the shared contract.Run cases.
//
// Skips on hosts without a reachable Docker daemon.  CI
// environments that promise Docker availability set
// PG_HARDSTORAGE_DEMAND_DOCKER=1 to flip skip → fail.
package gcs_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func openGCSOnFreshFake(t *testing.T) storage.StoragePlugin {
	t.Helper()
	s := sink.UpForTest(t, "gcs-fake")
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %s: %v", s.URL(), err)
	}
	p := &gcs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("gcs.Open(%s): %v", s.URL(), err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// GCS contract suite — exercises the gcs plugin against
// fake-gcs-server in `-backend memory -data /data` mode.
//
// Backend-choice history: the original `-filesystem-root
// /data` mode had a subtle JSON-vs-XML API divergence
// (objects PUT via the JSON API weren't visible to JSON-
// API Stat / List / Delete / Rename, only to XML-API
// Get) which made 6/11 contract cases fail.  Switching to
// the memory backend with `-data` startup-seed keeps a
// single in-process map for both API surfaces; both agree
// on what exists.  Bucket pre-creation still uses the
// same `<bucket>/` subdir under the seed dir.

func TestGCS_Contract(t *testing.T) {
	contract.Run(t, openGCSOnFreshFake)
}

func TestGCS_Contract_ParallelPuts(t *testing.T) {
	contract.ParallelPuts(t, openGCSOnFreshFake, 8)
}

func TestGCS_Contract_ParallelOverwrites(t *testing.T) {
	contract.ParallelOverwrites(t, openGCSOnFreshFake, 8)
}
