# gameday/

Scheduled chaos drills: kill the agent mid-backup, simulate an S3 503 storm,
force a Patroni failover. Empirical evidence that the system recovers without
operator intervention.

## What lives here

A `Scenario` is one scripted fault + invariant. Each `Run` returns a structured
`Result` with pass / fail and the evidence captured. Results are recorded in the
audit chain so an auditor sees "quarterly DR drill passed on date X" without
trusting an operator's notebook.

v0.1 scenarios:

- `agent_kill` — `SIGKILL` the local agent; assert self-supervised recovery
  within `recover_within`
- `s3_throttle` — wrap the storage plugin with a fault-injecting middleware
  that returns 503 for `duration`; assert the operation completes under the
  retry budget
- `patroni_failover` — declarative-only in v0.1; records the intended
  invariant + manual steps

## Key files

- `gameday.go` — `Scenario`, `Run`, `Report` (recent results from audit log),
  `List`
- `scenarios.go` — the three v0.1 scenarios + scenario registry
- `gameday_test.go` — scenario execution, pass/fail evidence shape, audit
  recording

## Read next

- `../recovery/README.md` — recovery-readiness scorecards and runbook
  generation
- `../audit/README.md` — every drill result is hash-chained
- `../testkit/inject/` — the fault-injection primitives gameday scenarios
  reuse
- `../README.md` — parent index

## Don't put X here

- Production fault-injection — gameday is opt-in and scheduled;
  production-grade chaos belongs elsewhere.
- Live monitoring / SLO burn — that's the future `internal/slo/` surface.
