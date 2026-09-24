package cli

import (
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// The safety stack promises "audit on every gate", but runLlmChat never
// set the gate / anomaly callbacks, so execute_command's allow and
// refusal decisions reached no audit trail.  bindGateAudit must wire
// both, and they must fire from a real execute_command run.
func TestBindGateAudit_EveryGateOutcomeEmitted(t *testing.T) {
	skill := &skills.Skill{Name: "ops", Context: skills.ContextSpec{
		AvailableTools:  []string{"execute_command"},
		AllowedExecutes: []string{"pg_hardstorage repo"},
	}}
	_, state, err := resolveExecMode("advise+execute", skill)
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		action string
		body   map[string]any
	}
	var got []ev
	bindGateAudit(state, func(a string, b map[string]any) { got = append(got, ev{a, b}) })
	if state.auditCallback == nil || state.anomalyCallback == nil {
		t.Fatal("callbacks not bound")
	}

	root := &cobra.Command{Use: "pg_hardstorage"}
	repo := &cobra.Command{Use: "repo"}
	repo.AddCommand(&cobra.Command{Use: "gc <url>", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(repo)
	exec := &tools.ExecuteCommand{
		Mode: state.mode, Policy: state.policy, Preview: state.preview,
		Anomaly: state.anomaly, Tree: cmdtree.Walk(root),
		Runner: &tools.CLIRunner{Path: "/x", Runner: func(context.Context, []string) ([]byte, []byte, int, error) {
			return []byte(`{}`), nil, 0, nil
		}},
		AuditCallback:   state.auditCallback,
		AnomalyCallback: state.anomalyCallback,
	}
	cmd := "pg_hardstorage repo gc s3://r/"

	// 1. No preview → refusal, audited.
	if _, err := exec.Run(context.Background(), map[string]any{"command": cmd}); err != nil {
		t.Fatal(err)
	}
	// 2. Previewed but "gc" never raised by the operator → anomaly severe, audited.
	state.preview.Add(cmd)
	if _, err := exec.Run(context.Background(), map[string]any{"command": cmd}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %+v, want gate(refused), gate(allowed), anomaly", got)
	}
	if got[0].action != "llm.execute_gate" || got[0].body["allowed"] != false || got[0].body["gate"] != "no_preview" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].action != "llm.execute_gate" || got[1].body["allowed"] != true {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[2].action != "llm.execute_anomaly" || got[2].body["score"] != safety.ScoreSevere.String() {
		t.Errorf("event 2 = %+v", got[2])
	}
}

func TestBindGateAudit_NilStateNoop(t *testing.T) {
	bindGateAudit(nil, func(string, map[string]any) { t.Error("emit on nil state") })
}
