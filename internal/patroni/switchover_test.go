package patroni_test

// switchover_test.go — the only mutating Patroni call we make.
//
// Switchover moves a production primary. The failure modes that matter
// are not "did we build the JSON right" but "did we correctly tell a
// REFUSAL apart from a broken cluster": Patroni answers 412 when no
// candidate is healthy enough or when the named leader is not actually
// the leader, and a game-day drill must report that as a finding about
// the cluster rather than as an infrastructure error.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// switchoverServer records the request and replies with the given
// status/body.
func switchoverServer(t *testing.T, status int, body string, seen *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/switchover" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			*seen = m
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSwitchover_AcceptedNamesTheLeader(t *testing.T) {
	var seen map[string]any
	srv := switchoverServer(t, http.StatusOK, "Successfully switched over to node-2", &seen)
	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Switchover(context.Background(),
		patroni.SwitchoverRequest{Leader: "node-1"}); err != nil {
		t.Fatalf("Switchover: %v", err)
	}
	if seen["leader"] != "node-1" {
		t.Errorf("request leader = %v, want node-1 — Patroni rejects a mismatched leader, "+
			"which is the guard against racing a failover that already happened", seen["leader"])
	}
	if _, present := seen["candidate"]; present {
		t.Error("candidate was sent; leaving it out lets Patroni pick the healthiest " +
			"replica, and a drill that hand-picks the target tests our choice not the cluster")
	}
}

func TestSwitchover_AcceptedWithCandidate(t *testing.T) {
	var seen map[string]any
	srv := switchoverServer(t, http.StatusAccepted, "", &seen)
	c, _ := patroni.NewClient(srv.URL)
	if err := c.Switchover(context.Background(), patroni.SwitchoverRequest{
		Leader: "node-1", Candidate: "node-3",
	}); err != nil {
		t.Fatalf("Switchover: %v", err)
	}
	if seen["candidate"] != "node-3" {
		t.Errorf("candidate = %v, want node-3", seen["candidate"])
	}
}

// TestSwitchover_RefusalIsItsOwnError is the distinction the drill
// depends on. 412 means the cluster evaluated the request and said no —
// usually "no candidate is healthy enough to promote", which is a
// finding about the cluster, not a failure to reach it.
func TestSwitchover_RefusalIsItsOwnError(t *testing.T) {
	srv := switchoverServer(t, http.StatusPreconditionFailed,
		"switchover is not possible: cluster does not have members able to take over leader role", nil)
	c, _ := patroni.NewClient(srv.URL)
	err := c.Switchover(context.Background(), patroni.SwitchoverRequest{Leader: "node-1"})
	if !errors.Is(err, patroni.ErrSwitchoverRefused) {
		t.Fatalf("err = %v, want ErrSwitchoverRefused", err)
	}
	if errors.Is(err, patroni.ErrUnreachable) {
		t.Error("a refusal must not read as unreachable: the cluster answered, and " +
			"reporting it as a network problem sends the operator to the wrong place")
	}
	if !strings.Contains(err.Error(), "able to take over") {
		t.Errorf("Patroni's own explanation was dropped from %q; it is the most useful "+
			"part of the message", err)
	}
}

func TestSwitchover_UnauthorizedIsDistinct(t *testing.T) {
	srv := switchoverServer(t, http.StatusUnauthorized, "", nil)
	c, _ := patroni.NewClient(srv.URL)
	err := c.Switchover(context.Background(), patroni.SwitchoverRequest{Leader: "node-1"})
	if !errors.Is(err, patroni.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized — mutating endpoints usually need auth "+
			"while the read-only ones do not, so this is a common first-run failure", err)
	}
	if errors.Is(err, patroni.ErrSwitchoverRefused) {
		t.Error("401 reported as a refusal; the operator would go looking for an unhealthy " +
			"replica instead of adding credentials")
	}
}

func TestSwitchover_RequiresTheLeaderName(t *testing.T) {
	srv := switchoverServer(t, http.StatusOK, "", nil)
	c, _ := patroni.NewClient(srv.URL)
	if err := c.Switchover(context.Background(), patroni.SwitchoverRequest{}); err == nil {
		t.Fatal("Switchover accepted an empty leader name; Patroni requires it and rejects " +
			"a mismatch, so sending nothing would race a failover that already happened")
	}
}

func TestSwitchover_UnreachableEndpoint(t *testing.T) {
	c, _ := patroni.NewClient("http://127.0.0.1:1")
	err := c.Switchover(context.Background(), patroni.SwitchoverRequest{Leader: "node-1"})
	if !errors.Is(err, patroni.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}
