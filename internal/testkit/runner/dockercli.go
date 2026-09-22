// dockercli.go — bounded wrappers for the short docker CLI calls the
// runner makes to look around and to collect forensics.
//
// A scenario run inherits cobra's context, which carries no deadline:
// the per-step `timeout:` budgets are enforced inside the step
// implementations, not by the context handed to them. That is fine for
// calls that do real work, which the step budgets already cover, and
// wrong for the two classes here.
//
// It has already cost a full day. A `scenario run` sat wedged for
// 1553 minutes with this stack:
//
//	os/exec.(*Cmd).Wait
//	runner.findDockerLeaderContainer  steps.go:356
//	runner.runSeed                    steps.go:244
//
// — blocked in `docker ps`, a call that answers in milliseconds
// against a healthy daemon and never returns against a wedged one.
// `exec.CommandContext` was already in use there and bought nothing,
// because the context it was given can never expire. The process held
// its container and its log file until it was killed by hand.
//
// So: give these calls a bound of their own.
package runner

import (
	"context"
	"os/exec"
	"time"
)

// dockerCLITimeout bounds an informational or diagnostic docker
// invocation — `ps`, `logs --tail`, `rm -f`, `image inspect`. Every
// one of them talks to the local daemon and returns metadata it
// already holds; none copies data or waits on a workload. Twenty
// seconds is far past the point where a healthy daemon has answered
// and far short of a CI job's patience.
//
// This is deliberately NOT applied to `docker exec` calls that run a
// query, a pgbench, or a restore. Those are real work with legitimate
// multi-minute durations, and they are governed by the step's own
// `timeout:` budget.
// A var rather than a const so the tests can shrink it; nothing in
// production reassigns it.
var dockerCLITimeout = 20 * time.Second

// dockerCLIWaitDelay bounds how long Wait may block *after* the
// context has expired and the child has been killed.
//
// Without it the deadline is decorative. exec.CommandContext kills the
// process it started and nothing else, while Output/CombinedOutput go
// on reading the pipes until every holder of the write end closes it —
// and a killed `docker` leaves its inherited descendants holding that
// end. The first version of this file set a 250 ms timeout in a test
// and watched the call block past 15 s anyway.
//
// WaitDelay is the supported way out: after the delay, Wait closes the
// pipes itself, returns what it has, and reports exec.ErrWaitDelay.
// The callers here want the partial output, so the error is either
// ignored (dockerDiag) or turned into a step failure (dockerInfo) —
// both correct for "docker would not answer".
var dockerCLIWaitDelay = 5 * time.Second

// dockerInfo runs a short docker query that the caller's context
// should still be able to cancel early — a lookup on the happy path,
// where a cancelled run means nobody wants the answer any more.
//
// The returned error is whatever exec reports; a timeout surfaces as
// the context's error, which reads as "docker ps: signal: killed" at
// the call site. That is the intended outcome: a wedged daemon fails
// the step instead of hanging it.
func dockerInfo(ctx context.Context, args ...string) ([]byte, error) {
	tctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "docker", args...)
	cmd.WaitDelay = dockerCLIWaitDelay
	return cmd.Output()
}

// dockerDiag runs a forensic docker call on a failure or cleanup path
// and deliberately does NOT take the caller's context.
//
// These sites exist precisely because something went wrong, and the
// run's context is frequently already cancelled by the time they
// execute — scraping `docker logs` under a dead context would return
// nothing and throw away the only explanation of the failure the
// operator is going to get. They need to outlive the run, but they
// still must not outlive it *forever*, which is the state this file
// exists to prevent.
//
// CombinedOutput because the interesting half of `docker logs` output
// is on stderr. Errors are ignored by every caller by design: a
// missing container is a normal outcome here (the thing may have died
// before it was named), and the forensics are best-effort.
func dockerDiag(args ...string) ([]byte, error) {
	tctx, cancel := context.WithTimeout(context.Background(), dockerCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "docker", args...)
	cmd.WaitDelay = dockerCLIWaitDelay
	return cmd.CombinedOutput()
}
