package s3_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// openWithHandler opens the plugin against an arbitrary handler, for
// requests the shared fakeS3 does not model.
func openWithHandler(t *testing.T, h http.Handler) storage.StoragePlugin {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	u, err := url.Parse(fmt.Sprintf("s3://test-bucket/pfx?endpoint=%s&region=us-east-1&path_style=true", srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := &s3.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// The stored lock carries milliseconds (Ceph RGW keeps them); they must
// survive the read-back, or a lock 52 ms past the deadline would compare
// as shorter than it is.
func TestS3_Retention_ReadsLock(t *testing.T) {
	var gotPath string
	sp := openWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["retention"]; !ok || r.Method != http.MethodGet {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<Retention><Mode>GOVERNANCE</Mode>` +
			`<RetainUntilDate>2026-11-09T08:00:00.052Z</RetainUntilDate></Retention>`))
	}))

	until, mode, err := storage.RetentionOf(context.Background(), sp, "chunks/sha256/16/d1/16d1.chk")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 9, 8, 0, 0, 52_000_000, time.UTC)
	if !until.Equal(want) || mode != storage.WORMGovernance {
		t.Errorf("Retention = %v %q, want %v governance", until, mode, want)
	}
	if gotPath != "/test-bucket/pfx/chunks/sha256/16/d1/16d1.chk" {
		t.Errorf("request path = %q, want the prefixed key", gotPath)
	}
}

// An object without a lock (or a denied read) is an error, never a zero
// lock that a caller might misread.
func TestS3_Retention_NoLockIsError(t *testing.T) {
	sp := openWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchObjectLockConfiguration</Code></Error>`))
	}))
	if _, _, err := storage.RetentionOf(context.Background(), sp, "k"); err == nil {
		t.Fatal("expected an error for an object without a lock")
	}
}
