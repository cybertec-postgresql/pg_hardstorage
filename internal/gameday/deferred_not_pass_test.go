package gameday

// deferred_not_pass_test.go — a scenario that does not drive its fault
// must not report a pass.
//
// agent_kill and patroni_failover both used to end with:
//
//	r.Evidence = append(r.Evidence, Event{Kind: "deferred", Message:
//	    "runtime drive ... lands alongside ..."})
//	r.Pass = true
//
// So `gameday run agent_kill` exited 0, `gameday report` incremented
// Passes, and the JSON was indistinguishable from a scenario that
// killed an agent and watched it recover. patroni_failover is declared
// tier L4 — the tier an auditor reads as "we tested catastrophic
// failover". Nothing was killed, nothing was measured.
//
// This is the same defect class as a chaos phase that skips on an unset
// env var and gets logged as a pass: the run reports success, and
// success is exactly what "did nothing" looks like from outside.
//
// The rule enforced here is mechanical and applies to every scenario,
// present and future: evidence tagged "deferred" means the runtime
// drive is missing, and a run that did not drive anything is not a
// pass. Dry-run is exempt — there the scenario reports a plan, which is
// what it says it is doing.

import (
	"context"
	"testing"
)

func TestScenarios_DeferredIsNeverAPass(t *testing.T) {
	scenarios := List()
	if len(scenarios) == 0 {
		t.Fatal("no scenarios registered — this guard would hold vacuously")
	}

	checked := 0
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			res, err := Run(context.Background(), sc.Name, RunOptions{})
			if err != nil {
				// A scenario that needs a repo/backend refuses without
				// one; that is a real refusal, not a silent pass.
				t.Skipf("scenario refused to run without options: %v", err)
			}
			if res == nil {
				t.Fatalf("scenario %q returned a nil result and no error", sc.Name)
			}
			checked++

			var deferredEvidence bool
			for _, ev := range res.Evidence {
				if ev.Kind == "deferred" {
					deferredEvidence = true
					break
				}
			}

			if deferredEvidence && res.Pass {
				t.Errorf("scenario %q recorded `deferred` evidence — its runtime drive is not "+
					"implemented — yet reported pass=true.\n\n"+
					"`gameday run %s` then exits 0 and `gameday report` counts a success, "+
					"which is indistinguishable from a scenario that actually ran and held. "+
					"Set Deferred + Pass=false instead; the CLI maps Deferred to "+
					"notimpl.scenario so an operator sees 'not implemented' rather than "+
					"'invariant violated'.", sc.Name, sc.Name)
			}
			if deferredEvidence && !res.Deferred {
				t.Errorf("scenario %q recorded `deferred` evidence but did not set "+
					"Result.Deferred; the CLI cannot tell it apart from a genuine "+
					"invariant failure and will report verify.failed (exit 9)", sc.Name)
			}
			if res.Deferred && res.Failure == "" {
				t.Errorf("scenario %q is deferred but carries no Failure text; the operator "+
					"gets a non-zero exit with no explanation", sc.Name)
			}
		})
	}
	if checked == 0 {
		t.Fatal("every scenario skipped — this guard asserted nothing")
	}
}

// TestScenarios_DryRunStillPasses pins the exemption, so a later change
// that makes deferred scenarios fail everywhere does not quietly break
// the documented `--dry-run` contract (dry-run reports a plan and
// passes; that is what it claims to do).
func TestScenarios_DryRunStillPasses(t *testing.T) {
	for _, sc := range List() {
		t.Run(sc.Name, func(t *testing.T) {
			res, err := Run(context.Background(), sc.Name, RunOptions{DryRun: true})
			if err != nil {
				t.Skipf("scenario refused to dry-run: %v", err)
			}
			if !res.Pass {
				t.Errorf("scenario %q failed its dry run: %s — a dry run reports the plan it "+
					"would execute and must pass", sc.Name, res.Failure)
			}
		})
	}
}
