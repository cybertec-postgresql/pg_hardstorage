package compose_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/compose"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

func TestAllocatePorts_Standalone(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "a", OS: "ubuntu:24.04", PG: "17", Count: 1},
		{Name: "b", OS: "debian:12", PG: "16", Count: 2},
	}}
	pm := compose.AllocatePorts(f, 20000)
	// Sorted alpha: a (1 port), then b (2 ports).
	if pm["a"] != 20000 {
		t.Errorf("a: got %d want 20000", pm["a"])
	}
	if pm["b-c0"] != 20001 {
		t.Errorf("b-c0: got %d want 20001", pm["b-c0"])
	}
	if pm["b-c1"] != 20002 {
		t.Errorf("b-c1: got %d want 20002", pm["b-c1"])
	}
}

func TestAllocatePorts_PatroniExpandsNodes(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "p", OS: "debian:12", PG: "17", Count: 1,
			Role: "patroni-cluster", Nodes: 3},
	}}
	pm := compose.AllocatePorts(f, 20000)
	for i, want := range map[string]int{
		"p-c0-node0": 20000, "p-c0-node1": 20001, "p-c0-node2": 20002,
	} {
		if got := pm[i]; got != want {
			t.Errorf("%s: got %d want %d (full map: %v)", i, got, want, pm)
		}
	}
}

func TestAllocatePorts_DefaultBase(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	pm := compose.AllocatePorts(f, 0) // 0 → 15432 default
	if pm["x"] != 15432 {
		t.Errorf("default base: got %d want 15432", pm["x"])
	}
}

func TestPortFor_Standalone(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "a", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	pm := compose.AllocatePorts(f, 20000)
	port, err := compose.PortFor(pm, f.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if port != 20000 {
		t.Errorf("port: got %d want 20000", port)
	}
}

func TestPortFor_PatroniGetsNode0(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "p", OS: "debian:12", PG: "17", Count: 1,
			Role: "patroni-cluster", Nodes: 3},
	}}
	pm := compose.AllocatePorts(f, 30000)
	port, err := compose.PortFor(pm, f.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if port != 30000 {
		t.Errorf("Patroni lead port: got %d want 30000 (node0)", port)
	}
}

func TestPortFor_UnknownCell(t *testing.T) {
	f := &config.Fleet{Entries: []config.FleetEntry{
		{Name: "a", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	pm := compose.AllocatePorts(f, 20000)
	other := config.FleetEntry{Name: "not-in-map", OS: "ubuntu:24.04", PG: "17", Count: 1}
	if _, err := compose.PortFor(pm, other); err == nil {
		t.Errorf("expected error for unknown cell")
	}
}

func TestFirstContainer_NamingMatchesCompose(t *testing.T) {
	cases := []struct {
		entry config.FleetEntry
		want  string
	}{
		{config.FleetEntry{Name: "a", Count: 1}, "a"},
		{config.FleetEntry{Name: "a", Count: 3}, "a-c0"},
		{config.FleetEntry{Name: "p", Count: 1, Role: "patroni-cluster", Nodes: 3}, "p-c0-node0"},
	}
	for _, tt := range cases {
		t.Run(tt.want, func(t *testing.T) {
			if got := compose.FirstContainer(tt.entry); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}
