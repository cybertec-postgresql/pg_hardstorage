package azblob

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	az "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// retentionFailServer is a minimal Blob service: block uploads and the
// commit succeed, the immutability-policy call fails (after onPolicy),
// and DELETE answers deleteStatus.
func retentionFailServer(t *testing.T, onPolicy func(), deleteStatus int, deletes *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		comp := r.URL.Query().Get("comp")
		switch {
		case r.Method == http.MethodPut && comp == "immutabilityPolicies":
			if onPolicy != nil {
				onPolicy()
			}
			w.Header().Set("x-ms-error-code", "AuthorizationPermissionMismatch")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>AuthorizationPermissionMismatch</Code><Message>no</Message></Error>`)
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"0x1"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			if deleteStatus != http.StatusAccepted {
				w.Header().Set("x-ms-error-code", "ServerBusy")
				w.WriteHeader(deleteStatus)
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>ServerBusy</Code><Message>backend unavailable</Message></Error>`)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
}

func newFakeAzPlugin(t *testing.T, srv *httptest.Server) *Plugin {
	t.Helper()
	cli, err := az.NewClientWithNoCredential(srv.URL+"/", &az.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: srv.Client(),
		Retry:     policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatalf("NewClientWithNoCredential: %v", err)
	}
	return &Plugin{serviceURL: srv.URL, container: "c", client: cli}
}

// TestPut_RetentionFailure_CleanupSurvivesCancelledCtx: see the gcs
// twin. The rollback DELETE reused the (cancelled) caller context and
// never left the process, so an UNLOCKED blob stayed at the chunk key
// for the next IfNotExists Put to dedup against.
func TestPut_RetentionFailure_CleanupSurvivesCancelledCtx(t *testing.T) {
	var deletes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := retentionFailServer(t, cancel, http.StatusAccepted, &deletes)
	defer srv.Close()
	p := newFakeAzPlugin(t, srv)

	_, err := p.Put(ctx, "chunks/aa/aabb", strings.NewReader("data"), storage.PutOptions{
		IfNotExists: true, RetainUntil: time.Now().Add(time.Hour), RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatal("Put succeeded although retention failed")
	}
	if deletes.Load() == 0 {
		t.Fatal("rollback DELETE never reached the server: cleanup reused the cancelled context, leaving an unlocked blob")
	}
}

// TestPut_RetentionFailure_FailedCleanupIsReported: a failed rollback
// must be visible in the error rather than discarded.
func TestPut_RetentionFailure_FailedCleanupIsReported(t *testing.T) {
	var deletes atomic.Int32
	srv := retentionFailServer(t, nil, http.StatusServiceUnavailable, &deletes)
	defer srv.Close()
	p := newFakeAzPlugin(t, srv)

	_, err := p.Put(context.Background(), "chunks/aa/aabb", strings.NewReader("data"), storage.PutOptions{
		IfNotExists: true, RetainUntil: time.Now().Add(time.Hour), RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatal("Put succeeded although retention failed")
	}
	if deletes.Load() == 0 {
		t.Fatal("no rollback attempted")
	}
	if !strings.Contains(err.Error(), "ServerBusy") {
		t.Fatalf("failed rollback not surfaced; err = %v", err)
	}
}
