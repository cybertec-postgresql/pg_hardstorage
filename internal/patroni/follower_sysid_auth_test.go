package patroni_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// TestFollower_SystemIDProbeUsesConfiguredAuth pins that the
// system-identifier probe goes through the configured patroni.Client
// (basic auth, custom transport) rather than a bare http.Client. On a
// Patroni whose REST API requires auth, the bare client got 401 on
// every tick, the probe error aborted each poll, and leader-following
// stalled forever.
func TestFollower_SystemIDProbeUsesConfiguredAuth(t *testing.T) {
	const sysID = "7000000000000000001"
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "patroni" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body := clusterWithLeader("node-1", "host-1", 5432, 1)
		body["system_identifier"] = sysID
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := patroni.NewClient(srv.URL, patroni.WithAuth("patroni", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan patroni.LeaderChange, 8)
	pollErrs := make(chan error, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:           c,
		Interval:         25 * time.Millisecond,
		ExpectedSystemID: sysID,
		OnEvent:          func(ev patroni.LeaderChange) { events <- ev },
		OnPollError: func(err error) {
			select {
			case pollErrs <- err:
			default:
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		if ev.New == nil || ev.New.Name != "node-1" {
			t.Errorf("expected node-1 leader; got %+v", ev.New)
		}
	case err := <-pollErrs:
		t.Fatalf("poll failed (probe ignored configured auth?): %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for leader event")
	}
}
