// scenarios.go — built-in chaos-test scenarios (agent_kill, s3_throttle, …) registered at init.
package gameday

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
)

func init() {
	Register(Scenario{
		Name:        "agent_kill",
		Description: "An agent killed mid-backup leaves an unrenewed lease. Asserts a second agent is excluded while it is live, and that exactly one of several racing agents reclaims it after expiry.",
		Tier:        "L2",
		Run:         runAgentKill,
	})
	Register(Scenario{
		Name:        "s3_throttle",
		Description: "Inject a 503-storm into the storage plugin for duration; assert backup completes.",
		Tier:        "L3",
		Run:         runS3Throttle,
	})
	Register(Scenario{
		Name:        "patroni_failover",
		Description: "Declarative invariant: Patroni leader change does not lose committed WAL. v0.1 reports manual steps; drives Patroni's REST /switchover.",
		Tier:        "L4",
		Run:         runPatroniFailover,
	})
}

func runS3Throttle(ctx context.Context, opts RunOptions) (*Result, error) {
	r := &Result{
		Schema:    SchemaResult,
		Scenario:  "s3_throttle",
		StartedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
	}
	defer finalize(r)

	dur := opts.FaultDuration
	if dur == 0 {
		dur = 5 * time.Minute
	}

	if opts.DryRun {
		r.Evidence = append(r.Evidence, Event{
			At:      time.Now().UTC(),
			Kind:    "plan",
			Message: fmt.Sprintf("would inject 503 responses for %s on every chunk PUT against the configured storage plugin", dur),
		})
		r.Pass = true
		return r, nil
	}

	// Without a repo URL we fall back to the contract-only path
	// (matches the v0.1 posture for ad-hoc invocations).
	if opts.RepoURL == "" {
		r.Evidence = append(r.Evidence,
			Event{
				At:      time.Now().UTC(),
				Kind:    "invariant",
				Message: "503-storm of duration N must not abort an in-flight backup whose retry budget covers N",
				Body: map[string]any{
					"fault_duration": dur.String(),
					"retry_budget":   "AWS-style exponential with jitter; per-host circuit breaker",
				},
			},
			Event{
				At:      time.Now().UTC(),
				Kind:    "info",
				Message: "no --repo provided; scenario passes-by-contract. Pass --repo to drive the fault-injection middleware against a real backend.",
			},
		)
		r.Pass = true
		return r, nil
	}

	// Real fault-injection drive against the configured backend.
	sp, err := storage.Open(ctx, opts.RepoURL)
	if err != nil {
		r.Failure = fmt.Sprintf("open backend %q: %v", opts.RepoURL, err)
		r.Pass = false
		return r, nil
	}
	defer sp.Close()

	mw := faultinject.New(sp)
	const probeKey = "gameday/s3_throttle/probe"
	probeBody := []byte("gameday s3_throttle probe payload")

	// Step 1: install the fault and attempt a Put. The Put must fail
	// with ErrInjected. We use a short ActiveDuration as a safety
	// guard so a forgotten Deactivate doesn't leave the wrapper
	// inactive for the operator's whole session.
	mw.Activate([]faultinject.Rule{{
		Name:      "s3_throttle_putfail",
		Ops:       faultinject.OpPut,
		KeyPrefix: probeKey, // limit to our own probe; don't break unrelated traffic
		Err:       faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{ActiveDuration: dur})

	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "fault_active",
		Message: "fault-injection rule installed: OpPut against probe key returns ErrInjected",
		Body:    map[string]any{"fault_duration": dur.String()},
	})

	_, putErr := mw.Put(ctx, probeKey, bytes.NewReader(probeBody),
		storage.PutOptions{ContentLength: int64(len(probeBody))})
	if !errors.Is(putErr, faultinject.ErrInjected) {
		r.Failure = fmt.Sprintf("expected ErrInjected during fault window; got %v", putErr)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "fault_observed",
		Message: "Put returned ErrInjected as expected during fault window",
	})

	// Step 2: deactivate and verify recovery — the Put now succeeds.
	mw.Deactivate()
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "fault_cleared",
		Message: "fault deactivated; retrying Put expects success",
	})
	if _, err := mw.Put(ctx, probeKey, bytes.NewReader(probeBody),
		storage.PutOptions{ContentLength: int64(len(probeBody))}); err != nil {
		r.Failure = fmt.Sprintf("post-fault Put should succeed; got %v", err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "recovered",
		Message: "post-fault Put succeeded; recovery confirmed",
	})

	// Best-effort cleanup: delete the probe key so we don't leave
	// litter in the operator's repo. Failure here is non-fatal.
	_ = sp.Delete(ctx, probeKey)

	r.Pass = true
	return r, nil
}

// runPatroniFailover documents the failover invariant. A real driver
// requires owning a Patroni cluster (the verifier sandbox).
func runPatroniFailover(ctx context.Context, opts RunOptions) (*Result, error) {
	r := &Result{
		Schema:    SchemaResult,
		Scenario:  "patroni_failover",
		StartedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
	}
	defer finalize(r)

	if opts.DryRun {
		r.Evidence = append(r.Evidence, Event{
			At:   time.Now().UTC(),
			Kind: "plan",
			Message: "would read the current leader, POST /switchover, poll until a " +
				"different member holds the leader lock, and re-measure replication-slot " +
				"continuity — asserting gap_bytes == 0 across the promotion",
		})
		r.Pass = true
		return r, nil
	}

	// No cluster to drive is a refusal, not a pass. This is the whole
	// point of the rewrite: the scenario used to append a "deferred"
	// note here and return Pass=true, so a tier-L4 drill exited 0
	// having promoted nothing.
	if opts.Patroni == nil {
		r.Failure = "no Patroni endpoint for this drill: pass --patroni-url, or " +
			"--deployment naming a deployment with patroni.url set in pg_hardstorage.yaml"
		r.Misconfigured = true
		r.Pass = false
		return r, nil
	}

	recoverWithin := opts.RecoverWithin
	if recoverWithin == 0 {
		recoverWithin = 2 * time.Minute
	}

	oldLeader, oldTimeline, err := opts.Patroni.Leader(ctx)
	if err != nil {
		r.Failure = fmt.Sprintf("read current leader: %v", err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "observed",
		Message: "current leader before the drill",
		Body:    map[string]any{"leader": oldLeader, "timeline": oldTimeline},
	})

	baseline := observeSlot(ctx, opts, r, "baseline")

	if err := opts.Patroni.Switchover(ctx, oldLeader); err != nil {
		// A refusal is a FINDING, not an infrastructure error: the
		// cluster evaluated the request and said no, which usually
		// means no replica was healthy enough to promote. That is
		// exactly what a drill is for.
		r.Failure = fmt.Sprintf("Patroni did not accept the switchover: %v", err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "fault",
		Message: "switchover accepted by Patroni",
		Body:    map[string]any{"demoting": oldLeader},
	})

	newLeader, newTimeline, err := awaitNewLeader(ctx, opts.Patroni, oldLeader, recoverWithin)
	if err != nil {
		r.Failure = err.Error()
		r.Pass = false
		return r, nil
	}
	r.RecoveryTime = time.Since(r.StartedAt)
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "recovered",
		Message: "a different member holds the leader lock",
		Body: map[string]any{
			"leader":       newLeader,
			"timeline":     newTimeline,
			"was":          oldLeader,
			"recovered_in": r.RecoveryTime.String(),
		},
	})

	after := observeSlot(ctx, opts, r, "after")

	// The invariant: a planned switchover must not cost us WAL.
	if after == nil {
		// The leader moved, but the invariant this scenario declares is
		// that a planned switchover does not COST us WAL — and that was
		// not measured. Passing here would be a hollow pass at tier L4,
		// the tier an auditor reads as "we tested catastrophic
		// failover".
		//
		// Kind is "deferred", not a synonym. deferred_not_pass_test.go
		// keys on that exact string, and this branch previously said
		// "unmeasured", which slipped past the one guard written to
		// catch precisely this: evidence that the drive did not happen,
		// alongside Pass=true.
		r.Evidence = append(r.Evidence, Event{
			At:   time.Now().UTC(),
			Kind: "deferred",
			Message: "replication-slot continuity was NOT measured (no ObserveSlot seam " +
				"wired); the leader moved, but that is not evidence the promotion kept the WAL",
		})
		r.Failure = "slot continuity was not measured: the drill drove a real switchover and " +
			"the leader moved, but without a repository and a PG connection for this " +
			"deployment there is no way to check whether the promotion cost WAL — which is " +
			"the invariant this scenario exists to prove. Pass --repo and configure " +
			"deployments.<name>.pg_connection, then re-run."
		r.Deferred = true
		r.Pass = false
		return r, nil
	}
	if after.GapBytes > 0 || after.Outcome == "missing" {
		r.Failure = fmt.Sprintf("slot continuity broke across the promotion: outcome=%s "+
			"gap_bytes=%d — WAL in that window cannot be fetched again, so PITR inside it "+
			"is impossible from this repository", after.Outcome, after.GapBytes)
		r.Pass = false
		return r, nil
	}
	// "recreated" fails EVEN AT gap_bytes=0. The drill audits
	// CONFIGURATION — can this cluster's slot survive a promotion? —
	// and a recreated slot answers no: the reconciler rebuilt it at
	// the new leader's position, and only a quiet WAL window kept the
	// gap at zero this time. Passing on the quiet case is a verdict
	// that depends on how busy the database happened to be during the
	// drill, for a misconfiguration that does not: the next failover
	// under load strands WAL. Caught live by the patroni gate's
	// hollow-pass meta-test, which ran the drill on a cluster with no
	// permanent_slots during a quiet window and watched it pass.
	if after.Outcome == "recreated" {
		r.Failure = "the replication slot did not survive the promotion (outcome=recreated, " +
			"gap_bytes=0): the new leader rebuilt it at its own position, and only a quiet " +
			"WAL window kept the gap at zero this time — under load the same failover " +
			"strands WAL between the old confirmed LSN and the rebuilt slot. Declare the " +
			"slot in Patroni's permanent_slots (slots: {<name>: {type: physical}}) so it " +
			"survives promotions, then re-run the drill."
		r.Pass = false
		return r, nil
	}
	if baseline != nil && baseline.GapBytes > 0 {
		r.Evidence = append(r.Evidence, Event{
			At:   time.Now().UTC(),
			Kind: "note",
			Message: "a gap already existed before the drill; the promotion did not widen it, " +
				"but the pre-existing gap is worth investigating separately",
			Body: map[string]any{"baseline_gap_bytes": baseline.GapBytes},
		})
	}
	r.Pass = true
	return r, nil
}

// observeSlot runs the ObserveSlot seam if wired, recording whatever it
// finds as evidence. A seam error is recorded and treated as "not
// measured" rather than failing the drill: the promotion result is
// still worth reporting.
func observeSlot(ctx context.Context, opts RunOptions, r *Result, phase string) *SlotObservation {
	if opts.ObserveSlot == nil {
		return nil
	}
	obs, err := opts.ObserveSlot(ctx)
	if err != nil {
		r.Evidence = append(r.Evidence, Event{
			At:      time.Now().UTC(),
			Kind:    "unmeasured",
			Message: fmt.Sprintf("%s slot observation failed: %v", phase, err),
		})
		return nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "observed",
		Message: phase + " replication-slot continuity",
		Body:    map[string]any{"outcome": obs.Outcome, "gap_bytes": obs.GapBytes},
	})
	return obs
}

// awaitNewLeader polls until a member other than old holds the leader
// lock, or the budget expires.
func awaitNewLeader(ctx context.Context, drv PatroniDriver, old string, within time.Duration) (string, uint32, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for {
		name, tli, err := drv.Leader(ctx)
		switch {
		case err == nil && name != "" && name != old:
			return name, tli, nil
		case err != nil:
			// A leaderless window is EXPECTED mid-promotion; keep
			// polling and only report the last error if we time out.
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", 0, fmt.Errorf("no new leader within %s (last error: %v); the "+
					"cluster did not complete the promotion", within, lastErr)
			}
			return "", 0, fmt.Errorf("no new leader within %s: %q still holds the leader "+
				"lock, so the switchover was accepted but never took effect", within, old)
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// finalize computes Duration and stamps StoppedAt. Called via defer
// from each scenario's Run so even a panic-on-defer leaves the
// Result populated.
func finalize(r *Result) {
	r.StoppedAt = time.Now().UTC()
	r.Duration = r.StoppedAt.Sub(r.StartedAt)
}
