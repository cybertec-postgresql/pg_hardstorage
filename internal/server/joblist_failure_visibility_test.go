package server_test

// A job backend that cannot be read must not report an empty queue.
//
// JobRegistry.List was the one method here that folded its backend
// error into its value — every sibling (Enqueue, Get, Claim,
// AppendProgress, Complete, Cancel) returns an error. On MemoryBackend
// List cannot fail, so the shape was invisible; on PGBackend it is a
// query, and a failed query became "no jobs".
//
// That reached two places, and the second is the worse one:
//
//   - GET /v1/jobs answered 200 with {"jobs": [], "count": 0}. A caller
//     cannot tell "the queue is empty" from "I could not read it".
//
//   - the metrics scrape publishes a census that is deliberately seeded
//     to zero for every state, so an idle control plane still emits the
//     full series set. Combined with a swallowed error, a backend
//     outage published exactly the same all-zero census as a healthy
//     idle one — so an alert on "queued jobs climbing", or on "nothing
//     has run in an hour", stayed quiet precisely when the control
//     plane could not see its own queue.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

var errJobBackendDown = errors.New("job backend unavailable")

// errListBackend fails List; everything else behaves normally.
type errListBackend struct{ *server.MemoryBackend }

func (errListBackend) List(context.Context, server.ListOptions) ([]server.Job, error) {
	return nil, errJobBackendDown
}

func TestJobRegistryList_SurfacesTheBackendError(t *testing.T) {
	r := server.NewJobRegistryWithBackend(errListBackend{server.NewMemoryBackend()})
	out, err := r.List(server.ListOptions{})
	if err == nil {
		t.Fatal("List reported success for a backend that failed; the caller then " +
			"treats an unreadable queue as an empty one")
	}
	if !errors.Is(err, errJobBackendDown) {
		t.Errorf("error does not wrap the backend's: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d jobs alongside an error", len(out))
	}
}

// The healthy path is unchanged.
func TestJobRegistryList_HealthyBackendStillLists(t *testing.T) {
	r := server.NewJobRegistry()
	if _, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"}); err != nil {
		t.Fatal(err)
	}
	out, err := r.List(server.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d jobs, want 1", len(out))
	}
}

// GET /v1/jobs must not answer 200 with an empty list.
func TestHandleJobsList_BackendFailureIsNotAnEmptyQueue(t *testing.T) {
	s, err := server.NewWithJobs(server.Config{
		Listen:           "127.0.0.1:0",
		HeartbeatTimeout: 30 * time.Second,
	}, server.NewJobRegistryWithBackend(errListBackend{server.NewMemoryBackend()}))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("GET /v1/jobs answered 200 for an unreadable backend:\n%s\n\n"+
			"A client polling for work reads that as an empty queue and may enqueue "+
			"a duplicate, or conclude a running backup has finished.", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal.job_list_failed") {
		t.Errorf("response carries no structured code a client can branch on:\n%s", rec.Body.String())
	}
}
