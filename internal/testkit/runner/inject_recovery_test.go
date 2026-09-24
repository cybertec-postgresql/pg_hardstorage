package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// An inject step whose recovery failed emitted recovery_failed and then
// PASSED: the scenario carried on against a cell still under the fault
// and blamed whatever failed next.
func TestRunInject_RecoveryFailureFailsTheStep(t *testing.T) {
	agent := &inject.FakeTarget{NameStr: "agent-0", RoleStr: "agent",
		ExecFunc: func(argv []string) ([]byte, error) {
			if strings.Contains(strings.Join(argv, " "), "-D") {
				return nil, errors.New("iptables: rule vanished")
			}
			return nil, nil
		}}
	state := &runState{targets: inject.NewStaticTargetSet([]inject.Target{agent}, 1)}
	st := scenario.Step{Kind: "inject", Action: "network_block(target=10.0.0.1)"}
	res := runInject(context.Background(), st, 0, state, io.Discard)
	if res.Pass {
		t.Fatalf("the fault could not be reverted; the step must fail: %s", res.Message)
	}
	if !strings.Contains(res.Message, "recovery") {
		t.Errorf("message should name the failed recovery: %s", res.Message)
	}
}
