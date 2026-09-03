package server_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// mustList is the test-side of JobRegistry.List gaining an error
// return. The error used to be folded into an empty slice, which is
// exactly what made a backend outage look like an empty queue.
func mustList(t *testing.T, r *server.JobRegistry, opts server.ListOptions) []server.Job {
	t.Helper()
	out, err := r.List(opts)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return out
}

func mustListLen(t *testing.T, r *server.JobRegistry, opts server.ListOptions) int {
	t.Helper()
	return len(mustList(t, r, opts))
}
