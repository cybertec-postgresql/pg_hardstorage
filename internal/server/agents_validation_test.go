package server_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestHeartbeat_RejectsBadFields pins input-validation audit #5: the
// agent heartbeat bounds each field's length and rejects control
// characters, so an authenticated agent can't bloat the registry/responses
// or inject control chars into echoed fields.
func TestHeartbeat_RejectsBadFields(t *testing.T) {
	r := server.NewAgentRegistry(0)
	long := strings.Repeat("a", 257)

	cases := []struct {
		name string
		req  server.HeartbeatRequest
	}{
		{"missing id", server.HeartbeatRequest{ID: "", Host: "h"}},
		{"missing host", server.HeartbeatRequest{ID: "a", Host: ""}},
		{"id too long", server.HeartbeatRequest{ID: long, Host: "h"}},
		{"host too long", server.HeartbeatRequest{ID: "a", Host: long}},
		{"version too long", server.HeartbeatRequest{ID: "a", Host: "h", Version: long}},
		{"control char in id", server.HeartbeatRequest{ID: "a\x00b", Host: "h"}},
		{"control char in host", server.HeartbeatRequest{ID: "a", Host: "h\nx"}},
		{"control char in deployment", server.HeartbeatRequest{ID: "a", Host: "h", Deployments: []string{"ok", "bad\x07"}}},
		{"empty deployment in list", server.HeartbeatRequest{ID: "a", Host: "h", Deployments: []string{""}}},
	}
	for _, c := range cases {
		if _, err := r.Heartbeat(c.req); err == nil {
			t.Errorf("%s: Heartbeat should be rejected", c.name)
		}
	}
}

// TestHeartbeat_AcceptsValidFields: a normal heartbeat still round-trips.
func TestHeartbeat_AcceptsValidFields(t *testing.T) {
	r := server.NewAgentRegistry(0)
	a, err := r.Heartbeat(server.HeartbeatRequest{
		ID:          "agent-1",
		Host:        "host.example.internal",
		Version:     "0.4.2",
		Deployments: []string{"db1", "db2"},
	})
	if err != nil {
		t.Fatalf("valid heartbeat rejected: %v", err)
	}
	if a.ID != "agent-1" || a.Host != "host.example.internal" {
		t.Errorf("round-trip mismatch: %+v", a)
	}
	// A field exactly at the limit is allowed.
	if _, err := r.Heartbeat(server.HeartbeatRequest{ID: strings.Repeat("a", 256), Host: "h"}); err != nil {
		t.Errorf("a 256-byte id (at the limit) should be allowed; got %v", err)
	}
}
