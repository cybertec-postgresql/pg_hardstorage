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
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// nonDriveKinds are evidence kinds meaning the drive or the measurement
// did NOT happen. Evidence tagged with any of them must never accompany
// Pass=true.
//
// This is a SET rather than the single literal "deferred" because that
// is how the tier-L4 hollow pass got in: runPatroniFailover's unmeasured
// branch tagged its evidence "unmeasured" and set Pass=true, and the
// guard below — written for exactly that shape — could not see the
// synonym. TestScenarios_NoUndeclaredNonDriveKinds keeps this set
// honest by failing on any kind the package emits that is in neither
// list.
var nonDriveKinds = map[string]bool{
	"deferred":      true,
	"unmeasured":    true,
	"skipped":       true,
	"notimpl":       true,
	"misconfigured": true,
}

// driveKinds report work the drill actually did or found.
var driveKinds = map[string]bool{
	"arranged": true, "cleanup": true, "cleanup_failed": true,
	"fault": true, "fault_active": true, "fault_cleared": true,
	"fault_observed": true, "info": true, "invariant": true,
	"note": true, "observed": true, "plan": true,
	"recovered": true, "refused": true,
}

// TestScenarios_DeferredIsNeverAPass sweeps every registered scenario.
//
// Its REACH IS LIMITED and that limit is why the patroni_failover
// hollow pass survived. It drives each scenario with empty RunOptions,
// so a scenario needing a driver or a repository refuses immediately
// and is skipped — meaning this guard never sees any branch that runs
// AFTER the fault is driven. runPatroniFailover's unmeasured branch is
// one of those, and no sweep with empty options can reach it.
//
// So this is the outer net, not the whole net. Post-drive branches have
// to be pinned per scenario with a fake driver — see
// TestPatroniFailover_UnmeasuredIsDeferredNotAPass, which is what
// actually catches that shape. Do not read a pass here as "every
// scenario's deferral is handled".
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
			var whichKind string
			for _, ev := range res.Evidence {
				if nonDriveKinds[ev.Kind] {
					deferredEvidence = true
					whichKind = ev.Kind
					break
				}
			}

			if deferredEvidence && res.Pass {
				t.Errorf("scenario %q recorded %q evidence — its runtime drive or measurement "+
					"did not happen — yet reported pass=true.\n\n"+
					"`gameday run %s` then exits 0 and `gameday report` counts a success, "+
					"which is indistinguishable from a scenario that actually ran and held. "+
					"Set Deferred + Pass=false instead; the CLI maps Deferred to "+
					"notimpl.scenario so an operator sees 'not implemented' rather than "+
					"'invariant violated'.", sc.Name, whichKind, sc.Name)
			}
			if deferredEvidence && !res.Deferred {
				t.Errorf("scenario %q recorded %q evidence but did not set "+
					"Result.Deferred; the CLI cannot tell it apart from a genuine "+
					"invariant failure and will report verify.failed (exit 9)", sc.Name, whichKind)
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

// TestScenarios_NoUndeclaredNonDriveKinds closes the hole that let the
// bug above exist.
//
// TestScenarios_DeferredIsNeverAPass keys on the exact evidence kind
// "deferred". runPatroniFailover's unmeasured branch used the kind
// "unmeasured" and set Pass=true, so the guard never fired — a hollow
// pass in a tier-L4 scenario, sitting next to the guard written to
// prevent exactly that, invisible because of one word.
//
// A guard keyed on a magic string only works if every synonym is known
// to it. This test reads the evidence kinds the package actually emits
// and requires each one to be classified: either it means work HAPPENED
// (and a pass is meaningful), or it means work DID NOT happen (and the
// scenario must defer). A new kind fails here until somebody decides
// which it is, rather than silently defaulting to "pass is fine".
func TestScenarios_NoUndeclaredNonDriveKinds(t *testing.T) {
	kinds, err := evidenceKindsInPackage()
	if err != nil {
		t.Fatalf("scan evidence kinds: %v", err)
	}
	if len(kinds) == 0 {
		t.Fatal("found no evidence kinds in the package — the scan broke and this guard " +
			"asserts nothing")
	}
	t.Logf("classified %d evidence kind(s): %v", len(kinds), kinds)

	for _, k := range kinds {
		if !driveKinds[k] && !nonDriveKinds[k] {
			t.Errorf("evidence kind %q is not classified as either a drive kind or a "+
				"non-drive kind.\n\n"+
				"TestScenarios_DeferredIsNeverAPass only recognises \"deferred\". If %q "+
				"means the scenario did not actually drive or measure its fault, it must "+
				"use \"deferred\" (and set Result.Deferred with Pass=false) so that guard "+
				"can see it. If it reports real work, add it to the drive list here.", k, k)
		}
	}
}

// evidenceKindsInPackage reads every `Kind: "..."` literal in the
// package's non-test sources.
func evidenceKindsInPackage() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`Kind:\s*"([a-z_]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(name)
		if rerr != nil {
			return nil, rerr
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
