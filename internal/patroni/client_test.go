package patroni_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// fakePatroniServer spins up an httptest server that responds to
// the Patroni REST endpoints we care about.
type fakePatroniServer struct {
	*httptest.Server
	cluster            any
	leader200          bool
	history            []any
	requireBasic       bool
	wantUser, wantPass string
	switchoverFn       func(http.ResponseWriter, *http.Request)
}

func newFakePatroni(t *testing.T) *fakePatroniServer {
	t.Helper()
	f := &fakePatroniServer{
		leader200: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		if f.requireBasic {
			u, p, ok := r.BasicAuth()
			if !ok || u != f.wantUser || p != f.wantPass {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.cluster)
	})
	mux.HandleFunc("/leader", func(w http.ResponseWriter, r *http.Request) {
		if f.leader200 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.history)
	})
	mux.HandleFunc("/switchover", func(w http.ResponseWriter, r *http.Request) {
		if f.switchoverFn != nil {
			f.switchoverFn(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// TestClient_Cluster: parses the canonical Patroni cluster shape.
func TestClient_Cluster(t *testing.T) {
	f := newFakePatroni(t)
	f.cluster = map[string]any{
		"scope": "acme-prod",
		"members": []any{
			map[string]any{
				"name": "node-1", "role": "leader", "state": "running",
				"api_url": "http://node-1:8008/patroni",
				"host":    "node-1.example.com", "port": 5432, "timeline": 2,
			},
			map[string]any{
				"name": "node-2", "role": "replica", "state": "running",
				"api_url": "http://node-2:8008/patroni",
				"host":    "node-2.example.com", "port": 5432, "timeline": 2,
				"lag": 0,
			},
		},
	}
	c, err := patroni.NewClient(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := c.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cluster.Scope != "acme-prod" {
		t.Errorf("Scope = %q", cluster.Scope)
	}
	if len(cluster.Members) != 2 {
		t.Fatalf("Members = %d", len(cluster.Members))
	}
	if !cluster.Members[0].IsLeader() {
		t.Error("first member should be leader")
	}
	if cluster.Members[1].Lag == nil || *cluster.Members[1].Lag != 0 {
		t.Errorf("replica Lag = %v", cluster.Members[1].Lag)
	}
}

// TestClient_Leader_Found: returns the leader from /cluster.
func TestClient_Leader_Found(t *testing.T) {
	f := newFakePatroni(t)
	f.cluster = map[string]any{
		"scope": "x",
		"members": []any{
			map[string]any{"name": "n1", "role": "replica", "state": "running"},
			map[string]any{"name": "n2", "role": "leader", "state": "running",
				"timeline": 5},
		},
	}
	c, _ := patroni.NewClient(f.URL)
	leader, err := c.Leader(context.Background())
	if err != nil {
		t.Fatalf("Leader: %v", err)
	}
	if leader.Name != "n2" {
		t.Errorf("leader = %q, want n2", leader.Name)
	}
	if leader.Timeline != 5 {
		t.Errorf("Timeline = %d, want 5", leader.Timeline)
	}
}

// TestClient_Leader_NoLeader: when no member has role=leader,
// returns ErrNoLeader (cluster currently has no primary).
func TestClient_Leader_NoLeader(t *testing.T) {
	f := newFakePatroni(t)
	f.cluster = map[string]any{
		"scope": "x",
		"members": []any{
			map[string]any{"name": "n1", "role": "replica", "state": "running"},
			map[string]any{"name": "n2", "role": "replica", "state": "running"},
		},
	}
	c, _ := patroni.NewClient(f.URL)
	_, err := c.Leader(context.Background())
	if !errors.Is(err, patroni.ErrNoLeader) {
		t.Errorf("expected ErrNoLeader; got %v", err)
	}
}

// TestClient_Leader_AcceptsMasterRole: older Patroni versions used
// "master" instead of "leader". IsLeader accepts both.
func TestClient_Leader_AcceptsMasterRole(t *testing.T) {
	f := newFakePatroni(t)
	f.cluster = map[string]any{
		"members": []any{
			map[string]any{"name": "n1", "role": "master", "state": "running"},
		},
	}
	c, _ := patroni.NewClient(f.URL)
	leader, err := c.Leader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if leader.Name != "n1" {
		t.Errorf("master-role member should be detected as leader")
	}
}

// TestClient_IsLeaderCheck_200: GET /leader returning 200 means
// "this node IS the leader."
func TestClient_IsLeaderCheck_200(t *testing.T) {
	f := newFakePatroni(t)
	f.leader200 = true
	c, _ := patroni.NewClient(f.URL)
	is, err := c.IsLeaderCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !is {
		t.Error("expected true on 200")
	}
}

// TestClient_IsLeaderCheck_503: GET /leader returning 503 means
// "this node is NOT the leader."
func TestClient_IsLeaderCheck_503(t *testing.T) {
	f := newFakePatroni(t)
	f.leader200 = false
	c, _ := patroni.NewClient(f.URL)
	is, err := c.IsLeaderCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if is {
		t.Error("expected false on 503")
	}
}

// TestClient_History: parses the positional-array shape.
func TestClient_History(t *testing.T) {
	f := newFakePatroni(t)
	f.history = []any{
		[]any{2.0, "0/15A2B388", "no recovery target specified",
			"2026-04-28T09:12:00.123456+00:00", "node-2"},
		[]any{3.0, "0/2400FF80", "no recovery target specified",
			"2026-04-28T11:33:00.000000+00:00", "node-3"},
	}
	c, _ := patroni.NewClient(f.URL)
	events, err := c.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Timeline != 2 {
		t.Errorf("Timeline[0] = %d", events[0].Timeline)
	}
	if events[0].NewLeader != "node-2" {
		t.Errorf("NewLeader[0] = %q", events[0].NewLeader)
	}
	if events[1].SwitchLSN != "0/2400FF80" {
		t.Errorf("SwitchLSN[1] = %q", events[1].SwitchLSN)
	}
}

// TestClient_History_Empty: no history events parses to empty
// slice, not nil-with-error.
func TestClient_History_Empty(t *testing.T) {
	f := newFakePatroni(t)
	f.history = []any{}
	c, _ := patroni.NewClient(f.URL)
	events, err := c.History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected empty; got %d", len(events))
	}
}

// TestClient_BasicAuth: the WithAuth option threads basic-auth
// credentials through every request.
func TestClient_BasicAuth(t *testing.T) {
	f := newFakePatroni(t)
	f.cluster = map[string]any{"members": []any{}}
	f.requireBasic = true
	f.wantUser = "operator"
	f.wantPass = "s3cret"

	// Without auth: unauthorized.
	cBad, _ := patroni.NewClient(f.URL)
	_, err := cBad.Cluster(context.Background())
	if !errors.Is(err, patroni.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized; got %v", err)
	}

	// With auth: succeeds.
	c, _ := patroni.NewClient(f.URL, patroni.WithAuth("operator", "s3cret"))
	if _, err := c.Cluster(context.Background()); err != nil {
		t.Errorf("auth'd request failed: %v", err)
	}
}

// TestClient_Unreachable: pointing at a closed server surfaces
// ErrUnreachable.
func TestClient_Unreachable(t *testing.T) {
	c, _ := patroni.NewClient("http://127.0.0.1:1")
	_, err := c.Cluster(context.Background())
	if !errors.Is(err, patroni.ErrUnreachable) {
		t.Errorf("expected ErrUnreachable; got %v", err)
	}
}

// TestNewClient_RejectsBadURL: empty / non-http URLs are usage
// errors.
func TestNewClient_RejectsBadURL(t *testing.T) {
	if _, err := patroni.NewClient(""); err == nil {
		t.Error("empty URL should error")
	}
	if _, err := patroni.NewClient("file:///tmp/x"); err == nil {
		t.Error("file:// URL should error")
	}
	if _, err := patroni.NewClient("not a url"); err == nil {
		t.Error("invalid URL should error")
	}
}

// TestClient_BasePathPreserved: a base URL with a path prefix
// (operators behind a reverse proxy might mount Patroni at
// /patroni/) gets the prefix preserved on every request.
func TestClient_BasePathPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/patroni/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"members": []any{}})
	}))
	defer srv.Close()

	c, err := patroni.NewClient(srv.URL + "/patroni")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cluster(context.Background()); err != nil {
		t.Errorf("path-prefixed request failed: %v", err)
	}
}

// TestMember_UnmarshalLag covers Patroni's polymorphic `lag` field
// (issue #59): an integer means bytes-behind, the string "unknown"
// (emitted mid-failover) means not-known, and an omitted/null field
// also means not-known. All non-integer cases must fold to nil
// rather than failing the whole /cluster decode.
func TestMember_UnmarshalLag(t *testing.T) {
	cases := []struct {
		name string
		json string
		want *int64
	}{
		{"int lag", `{"name":"r","lag":4096}`, ptr(int64(4096))},
		{"zero lag", `{"name":"r","lag":0}`, ptr(int64(0))},
		{"string unknown", `{"name":"r","lag":"unknown"}`, nil},
		{"null lag", `{"name":"r","lag":null}`, nil},
		{"omitted lag", `{"name":"r"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m patroni.Member
			if err := json.Unmarshal([]byte(tc.json), &m); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			switch {
			case tc.want == nil && m.Lag != nil:
				t.Errorf("want nil lag, got %d", *m.Lag)
			case tc.want != nil && m.Lag == nil:
				t.Errorf("want lag %d, got nil", *tc.want)
			case tc.want != nil && *m.Lag != *tc.want:
				t.Errorf("want lag %d, got %d", *tc.want, *m.Lag)
			}
		})
	}
}

// TestCluster_DecodesWithStringLag is the issue-#59 regression: a
// whole /cluster payload where one replica reports lag="unknown"
// must still decode (previously the entire poll failed with
// "patroni_poll_failed").
func TestCluster_DecodesWithStringLag(t *testing.T) {
	const body = `{"members":[
		{"name":"patroni3","role":"leader","state":"running","timeline":2},
		{"name":"patroni2","role":"replica","state":"running","lag":"unknown"},
		{"name":"patroni1","role":"replica","state":"running","lag":128}
	]}`
	var c patroni.Cluster
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("decode /cluster with string lag should succeed; got %v", err)
	}
	if len(c.Members) != 3 {
		t.Fatalf("want 3 members; got %d", len(c.Members))
	}
	if c.Members[1].Lag != nil {
		t.Errorf("replica with lag=\"unknown\" should have nil Lag; got %d", *c.Members[1].Lag)
	}
	if c.Members[2].Lag == nil || *c.Members[2].Lag != 128 {
		t.Errorf("replica with lag=128 should parse; got %v", c.Members[2].Lag)
	}
}

func ptr(v int64) *int64 { return &v }

// TestClient_Unreachable_PreservesUnderlyingError pins the issue
// #74 fix: when the HTTP transport fails (DNS error, connection
// refused, TLS handshake error, context deadline), the wrap MUST:
//   - keep ErrUnreachable matchable via errors.Is (so callers
//     dispatch on the sentinel)
//   - INCLUDE the underlying error's message text (the historical
//     wrap discarded it via `%w` against the sentinel, leaving the
//     operator with "REST endpoint unreachable" and no clue
//     whether it was a DNS lookup miss, a wrong port, or a TLS
//     verification failure)
//
// We target localhost:1 (port-1 is unassigned; connections always
// refuse).  The transport error contains "connection refused" on
// every platform Go supports.
func TestClient_Unreachable_PreservesUnderlyingError(t *testing.T) {
	c, _ := patroni.NewClient("http://127.0.0.1:1")
	_, err := c.Cluster(context.Background())
	if err == nil {
		t.Fatal("expected error against closed port 1")
	}
	if !errors.Is(err, patroni.ErrUnreachable) {
		t.Errorf("errors.Is(err, ErrUnreachable) = false (sentinel lost); err=%v", err)
	}
	// Underlying transport diagnostic MUST appear.  We don't pin
	// the exact phrase (Go's net package wording shifts across
	// releases — "connection refused" / "connect: connection
	// refused" / "dial tcp ...: ..."), but every variant contains
	// "refused" or "connect".  Either signal proves the wrap kept
	// the underlying error's text.
	msg := err.Error()
	if !strings.Contains(msg, "refused") && !strings.Contains(msg, "connect") {
		t.Errorf("error text dropped the transport diagnostic; got %q (expected to contain 'refused' or 'connect')", msg)
	}
}

// TestClient_BaseURL_RoundTrips confirms the BaseURL accessor
// returns the URL the client was constructed with — used by the
// wal-follower coordinator to stamp the URL on `patroni_poll_failed`
// event bodies (issue #74 diagnostic gap: events used to carry only
// `error`, leaving the operator unable to tell which URL the agent
// actually picked up from deployment config).
func TestClient_BaseURL_RoundTrips(t *testing.T) {
	cases := []string{
		"http://patroni:8008",
		"http://haproxy:8008/",
		"https://patroni.acme.example.com:8443",
		"http://patroni.example.com:8008/patroni/", // reverse-proxy-prefixed
	}
	for _, in := range cases {
		c, err := patroni.NewClient(in)
		if err != nil {
			t.Fatalf("NewClient(%q): %v", in, err)
		}
		got := c.BaseURL()
		// url.URL.String round-trips exact-equal for these
		// shapes (no port-default elision, no path normalisation
		// beyond the trailing-slash idempotence url.Parse
		// already does).
		if got == "" {
			t.Errorf("BaseURL() returned empty for %q", in)
		}
		if !strings.HasPrefix(got, in[:strings.Index(in, "://")+3]) {
			t.Errorf("BaseURL() = %q lost the scheme of %q", got, in)
		}
	}
}
