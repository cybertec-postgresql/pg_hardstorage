package scenario_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

func TestParse_Minimal(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.scenario.v1
name: minimal
tier: L1

topology:
  provider: local-docker
  pg_version: "17"

steps:
  - assert:
    - count_exact: { table: users, value: 100 }
    - lsn_at_least: "0/1000000"

asserts:
  - count_range: { table: users, min: 99, max: 101 }
`)
	s, err := scenario.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "minimal" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Topology.Provider != "local-docker" {
		t.Errorf("Provider = %q", s.Topology.Provider)
	}
	if len(s.Steps) != 1 || s.Steps[0].Kind != "assert" {
		t.Errorf("Steps = %+v", s.Steps)
	}
	if len(s.Steps[0].Asserts) != 2 {
		t.Errorf("step asserts = %d", len(s.Steps[0].Asserts))
	}
	if len(s.Asserts) != 1 {
		t.Errorf("scenario asserts = %d", len(s.Asserts))
	}
}

func TestParse_RejectsBadSchema(t *testing.T) {
	body := []byte(`
schema: wrong
name: x
topology: { provider: local-docker }
steps:
  - assert: [{ count_exact: { table: u, value: 1 } }]
`)
	_, err := scenario.Parse(body)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema-rejection error; got %v", err)
	}
}

func TestParse_DropSlotStep(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.scenario.v1
name: drop-slot-test
tier: L4
topology:
  provider: local-docker
  pg_version: "17"
steps:
  - drop_slot: {}
  - drop_slot:
      slot: my_custom_slot
`)
	s, err := scenario.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(s.Steps))
	}
	if s.Steps[0].Kind != "drop_slot" {
		t.Errorf("Steps[0].Kind = %q, want drop_slot", s.Steps[0].Kind)
	}
	if s.Steps[0].Slot != "" {
		t.Errorf("Steps[0].Slot = %q, want empty (default)", s.Steps[0].Slot)
	}
	if s.Steps[1].Kind != "drop_slot" {
		t.Errorf("Steps[1].Kind = %q", s.Steps[1].Kind)
	}
	if s.Steps[1].Slot != "my_custom_slot" {
		t.Errorf("Steps[1].Slot = %q, want my_custom_slot", s.Steps[1].Slot)
	}
}

func TestParse_RejectsMissingTopology(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.scenario.v1
name: x
topology: { provider: "" }
steps:
  - assert: [{ count_exact: { table: u, value: 1 } }]
`)
	_, err := scenario.Parse(body)
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("expected provider-rejection error; got %v", err)
	}
}
