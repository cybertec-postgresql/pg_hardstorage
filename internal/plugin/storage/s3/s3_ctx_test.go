package s3_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// External-review-pass-4: top-of-method ctx.Err() check matches the
// fs plugin's posture. The AWS SDK respects ctx through every call,
// so an in-flight S3 request IS interruptible. The pre-call check
// is a microsecond optimisation that bails before the SDK's
// resolve-endpoint + sign-request + DNS work — and also documents
// the consistent defensive pattern.
func TestS3_PreCancelledCtx_AllMethodsHonour(t *testing.T) {
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	urlStr := "s3://test-bucket/?endpoint=" + fake.URL + "&region=us-east-1&path_style=true"
	u, _ := url.Parse(urlStr)
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("Put", func(t *testing.T) {
		_, err := p.Put(ctx, "k", bytes.NewReader([]byte("x")), storage.PutOptions{})
		if err == nil {
			t.Error("Put should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Get", func(t *testing.T) {
		_, err := p.Get(ctx, "k")
		if err == nil {
			t.Error("Get should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Stat", func(t *testing.T) {
		_, err := p.Stat(ctx, "k")
		if err == nil {
			t.Error("Stat should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Delete", func(t *testing.T) {
		err := p.Delete(ctx, "k")
		if err == nil {
			t.Error("Delete should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("RenameIfNotExists", func(t *testing.T) {
		err := p.RenameIfNotExists(ctx, "src", "dst")
		if err == nil {
			t.Error("RenameIfNotExists should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("List", func(t *testing.T) {
		var got error
		for _, err := range p.List(ctx, "") {
			if err != nil {
				got = err
				break
			}
		}
		if got == nil {
			t.Error("List should yield ctx error on pre-cancelled ctx")
		}
	})

	// CRITICAL: the fake httptest server should NOT have received
	// ANY request from any of the above pre-cancelled calls. If it
	// did, the early-bail check isn't doing its job.
	if hits.Load() != 0 {
		t.Errorf("S3 fake server received %d HTTP request(s) under pre-cancelled ctx; should be 0",
			hits.Load())
	}
}
