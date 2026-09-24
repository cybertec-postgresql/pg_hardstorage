package scenario_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

const strictHead = `
schema: pg_hardstorage.scenario.v1
name: strict
topology:
  provider: local-docker
steps:
`

// Scenario-level decoding rejects unknown keys, but a step decoded its
// payload through yaml.Node.Decode, which ignores KnownFields: a typoed
// step key was dropped without a word. Two shipped scenarios wrote
// `assert: {asserts: [...]}` — decoded as an assert step with ZERO
// assertions, which then reported "0/0 asserts passed".
func TestParse_StepRejectsUnknownKeys(t *testing.T) {
	for name, step := range map[string]string{
		"typo in take_backup": "  - take_backup: { deployment: db1, tpye: full }\n",
		"assert mapping form": "  - assert:\n      asserts:\n        - lsn_at_least: \"0/0\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := scenario.Parse([]byte(strictHead + step))
			if err == nil {
				t.Fatal("an unknown step key parsed silently")
			}
		})
	}
}

func TestParse_StepKnownKeysStillParse(t *testing.T) {
	s, err := scenario.Parse([]byte(strictHead + "  - take_backup: { deployment: db1, type: full }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Steps[0].Deployment != "db1" || s.Steps[0].Type != "full" {
		t.Errorf("step fields lost: %+v", s.Steps[0])
	}
}

// An assert step that checks nothing is a scenario bug, not a pass.
func TestParse_EmptyAssertStepRejected(t *testing.T) {
	_, err := scenario.Parse([]byte(strictHead + "  - assert: []\n"))
	if err == nil || !strings.Contains(err.Error(), "assert") {
		t.Fatalf("an assert step with no assertions must not parse: %v", err)
	}
}
