// readyz_cache_test.go — perf audit #6: the unauthenticated
// /v1/readyz must not pay the full repo.Open cost on every probe.
// The last fully-ready probe result is served from cache for the
// TTL; a degraded result is never cached, so a recovering repo is
// detected on the next probe.
//
// repo.Open is counted through a storage scheme that wraps a real
// fs plugin (each repo.Open performs exactly one storage Open).
// The scheme registry is process-global, so each run registers a
// unique scheme (same posture as storage_test.go's registry tests).

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

var readyzSchemeSeq atomic.Int64

// countingFS wraps fs.Plugin and counts Open calls.
type countingFS struct {
	fs.Plugin
	opens *atomic.Int64
}

func (c *countingFS) Open(ctx context.Context, cfg storage.StorageConfig) error {
	c.opens.Add(1)
	return c.Plugin.Open(ctx, cfg)
}

// registerCountingScheme registers a unique scheme backed by an fs
// plugin sharing *opens and returns its repo URL for dir.
func registerCountingScheme(t *testing.T, dir string, opens *atomic.Int64) string {
	t.Helper()
	scheme := fmt.Sprintf("readyz-count-%d", readyzSchemeSeq.Add(1))
	storage.Register(scheme, func() storage.StoragePlugin {
		return &countingFS{opens: opens}
	})
	return scheme + "://" + dir
}

func probeReadyz(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/readyz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestReadyz_ReadyResultCachedWithinTTL: the second probe within
// the TTL must not Open the repo again.
func TestReadyz_ReadyResultCachedWithinTTL(t *testing.T) {
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	var opens atomic.Int64
	s, err := New(Config{Listen: "127.0.0.1:0", Repos: []string{registerCountingScheme(t, root, &opens)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if w := probeReadyz(t, s); w.Code != http.StatusOK {
		t.Fatalf("first probe: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("first probe: storage opens = %d, want 1", got)
	}

	if w := probeReadyz(t, s); w.Code != http.StatusOK {
		t.Fatalf("second probe: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("second probe within the %s TTL: storage opens = %d, want 1 — the ready result must be served from cache",
			s.readyzTTL, got)
	}
}

// TestReadyz_DegradedResultNeverCached: a failing repo must be
// re-probed on every request (caching the failure would delay
// recovery detection by the whole TTL).
func TestReadyz_DegradedResultNeverCached(t *testing.T) {
	root := t.TempDir() // empty: no HSREPO → repo.Open fails
	var opens atomic.Int64
	s, err := New(Config{Listen: "127.0.0.1:0", Repos: []string{registerCountingScheme(t, root, &opens)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 1; i <= 2; i++ {
		w := probeReadyz(t, s)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("probe %d: status %d, want 503 (body=%s)", i, w.Code, w.Body.String())
		}
		if got := opens.Load(); got != int64(i) {
			t.Fatalf("probe %d: storage opens = %d, want %d — degraded results must never be cached", i, got, i)
		}
	}
}

// TestReadyz_CacheExpiresAfterTTL: after the TTL elapses, the next
// probe re-Opens every repo (a repo created since the last probe
// must still be reported — and so must one that broke since).
func TestReadyz_CacheExpiresAfterTTL(t *testing.T) {
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	var opens atomic.Int64
	s, err := New(Config{Listen: "127.0.0.1:0", Repos: []string{registerCountingScheme(t, root, &opens)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.readyzTTL = 40 * time.Millisecond

	if w := probeReadyz(t, s); w.Code != http.StatusOK {
		t.Fatalf("first probe: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	time.Sleep(80 * time.Millisecond)
	if w := probeReadyz(t, s); w.Code != http.StatusOK {
		t.Fatalf("post-TTL probe: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("post-TTL probe: storage opens = %d, want 2 — the cache must expire after the TTL", got)
	}
}
