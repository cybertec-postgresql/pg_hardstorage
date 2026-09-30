package gcs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// retentionFailServer accepts the upload, fails the retention PATCH
// (running onPatch first), and answers DELETE with deleteStatus.
func retentionFailServer(t *testing.T, onPatch func(), deleteStatus int, deletes *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Method {
		case http.MethodPost: // upload
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"bucket":"bkt","name":"k","size":"4","generation":"1"}`)
		case http.MethodPatch: // SetRetention
			if onPatch != nil {
				onPatch()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"retention not permitted"}}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(deleteStatus)
			if deleteStatus != http.StatusNoContent {
				_, _ = io.WriteString(w, `{"error":{"code":503,"message":"backend unavailable"}}`)
			}
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
}

// TestPut_RetentionFailure_CleanupSurvivesCancelledCtx: the retention
// call fails because the caller's context was cancelled (a shutdown, a
// backup timeout). The rollback DELETE used that same context, so it
// never left the process, and an UNLOCKED object stayed at the chunk
// key — where the next IfNotExists Put dedups against it as though it
// were a committed, protected chunk.
func TestPut_RetentionFailure_CleanupSurvivesCancelledCtx(t *testing.T) {
	var deletes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := retentionFailServer(t, cancel, http.StatusNoContent, &deletes)
	defer srv.Close()
	p := newFakeGCSPlugin(t, srv)

	_, err := p.Put(ctx, "chunks/aa/aabb", strings.NewReader("data"), storage.PutOptions{
		IfNotExists: true, RetainUntil: time.Now().Add(time.Hour), RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatal("Put succeeded although retention failed")
	}
	if deletes.Load() == 0 {
		t.Fatal("rollback DELETE never reached the server: cleanup reused the cancelled context, leaving an unlocked object")
	}
}

// TestPut_RetentionFailure_FailedCleanupIsReported: when the rollback
// itself fails the error must say so — the object is still there,
// unlocked — instead of silently dropping the cleanup error.
func TestPut_RetentionFailure_FailedCleanupIsReported(t *testing.T) {
	var deletes atomic.Int32
	srv := retentionFailServer(t, nil, http.StatusServiceUnavailable, &deletes)
	defer srv.Close()
	p := newFakeGCSPlugin(t, srv)

	_, err := p.Put(context.Background(), "chunks/aa/aabb", strings.NewReader("data"), storage.PutOptions{
		IfNotExists: true, RetainUntil: time.Now().Add(time.Hour), RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatal("Put succeeded although retention failed")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("failed rollback not surfaced; err = %v", err)
	}
}
