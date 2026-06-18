// Package agent — controlplane.go is the agent-side dispatch client.
//
// When `pg_hardstorage agent --control-plane <url>` runs, the agent
// drops the local schedule and instead:
//
//  1. Heartbeats every ~10s with its registered deployments.
//  2. Polls every ~5s for newly-claimable jobs.
//
// Both intervals are jittered (±DefaultJitterFraction, with the first
// tick spread across the whole interval) so a fleet of agents that
// started together doesn't hit the control plane in synchronized
// bursts. See ControlPlaneClient.JitterFraction.
//  3. On claim, executes the job (v0.1: backup only) by invoking
//     the same primitives the local-schedule path uses — same
//     backup runner, same retention runner.
//  4. Streams progress events back as they fire and posts a
//     terminal `complete` once the runner returns.
//
// Concurrency: one in-flight job at a time. Multi-job concurrency
// would require coordinating per-deployment locks (the existing
// schedule engine has them; the polling client doesn't reuse the
// engine because the engine's tick model is tied to declarative
// schedules, not opportunistic polls).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
)

// DefaultJitterFraction is the ±fraction applied to the heartbeat and
// poll intervals when ControlPlaneClient.JitterFraction is unset. It
// spreads a fleet's requests so agents that started together don't
// hammer the control plane in synchronized bursts every interval. 0.2
// keeps the mean cadence (the jitter is symmetric) while decorrelating
// the fleet over a handful of cycles. Each agent process auto-seeds the
// global RNG independently, so no per-agent seeding is needed.
const DefaultJitterFraction = 0.2

// ControlPlaneClient polls a control plane for jobs.
type ControlPlaneClient struct {
	BaseURL string
	Token   string
	AgentID string
	Host    string
	Version string

	// Deployments declares which deployment names this agent is
	// eligible to run jobs for. The control plane filters claims
	// against this list — an agent that doesn't manage db1 won't
	// be assigned a db1 job.
	Deployments []string

	// HTTPClient is the HTTP client used for every request. nil
	// defaults to a 30s-timeout client; tests substitute a
	// stub Transport.
	HTTPClient *http.Client

	// HeartbeatInterval / PollInterval default to 10s / 5s when
	// zero. Operator override via the corresponding flags.
	HeartbeatInterval time.Duration
	PollInterval      time.Duration

	// JitterFraction is the ±fraction applied to each interval so a
	// fleet that started together doesn't hit the control plane in
	// synchronized bursts. Zero → DefaultJitterFraction; a negative
	// value disables jitter (exact intervals, for deterministic
	// load tests). The first tick of each timer is additionally
	// spread uniformly across the whole interval so the very first
	// cycle is decorrelated too.
	JitterFraction float64

	// JobExecutor runs a claimed job. The default executor refuses
	// every job with a structured `notimpl` failure, which is the
	// honest v0.1 default — execution wiring lands as the agent's
	// in-process runner is exposed for control-plane use.
	//
	// Tests substitute a fake executor; production callers wire
	// this against internal/backup/runner once the runner exposes
	// a "run one named-deployment backup" entry point.
	JobExecutor JobExecutor
}

// JobExecutor runs one claimed job. ProgressFn fires for each event
// the executor wants to surface to the control plane; the client
// forwards it through POST /v1/jobs/<id>/progress. Returns the
// terminal Result + nil on success, or an error message on failure.
type JobExecutor interface {
	Execute(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error)
}

// ControlPlaneJob is the job shape the client receives from the
// control plane. Mirrors internal/server/jobs.go's Job — duplicated
// here to avoid the agent depending on the server package (the agent
// might be talking to a control plane that's a different version).
type ControlPlaneJob struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Deployment string         `json:"deployment"`
	RepoURL    string         `json:"repo_url"`
	Args       map[string]any `json:"args,omitempty"`
}

// notImplExecutor is the default JobExecutor — refuses every job.
// Operators wiring real execution pass a real JobExecutor when
// constructing the client.
type notImplExecutor struct{}

// Execute always fails with a not-wired sentinel; callers should
// supply a real JobExecutor.
func (notImplExecutor) Execute(_ context.Context, j *ControlPlaneJob, _ func(map[string]any)) (map[string]any, error) {
	return nil, fmt.Errorf("control-plane agent: in-process %s execution is not yet wired; job %s recorded as failed", j.Kind, j.ID)
}

// Run starts the heartbeat + poll loop. Blocks until ctx cancels.
// Cancellation triggers a final goodbye-heartbeat (non-blocking;
// best-effort) and clean exit.
func (c *ControlPlaneClient) Run(ctx context.Context) error {
	if c.BaseURL == "" {
		return errors.New("controlplane: BaseURL is required")
	}
	if c.AgentID == "" {
		return errors.New("controlplane: AgentID is required")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.JobExecutor == nil {
		c.JobExecutor = notImplExecutor{}
	}
	// Jitter: 0 → default; negative → disabled (exact intervals).
	c.JitterFraction = resolveJitterFraction(c.JitterFraction)

	// Timers (not tickers) so each interval can be re-jittered on every
	// fire — this continuously decorrelates the fleet, and survives a
	// wave of agents restarting together (a fixed phase offset would
	// re-synchronize them). The first tick is spread across the whole
	// interval so even the first cycle isn't a thundering herd.
	hbTimer := time.NewTimer(firstInterval(c.HeartbeatInterval, c.JitterFraction))
	defer hbTimer.Stop()
	pollTimer := time.NewTimer(firstInterval(c.PollInterval, c.JitterFraction))
	defer pollTimer.Stop()

	// First-time heartbeat fires immediately so the control plane
	// learns about us before the first tick.
	if err := c.heartbeat(ctx); err != nil {
		// Non-fatal — control plane may come up later. Log to
		// stderr via fmt; the agent's structured-event bus would
		// be a wire.
		fmt.Fprintf(stderrSink, "controlplane: initial heartbeat failed: %v\n", err)
		metrics.ControlPlaneError("heartbeat")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hbTimer.C:
			if err := c.heartbeat(ctx); err != nil {
				fmt.Fprintf(stderrSink, "controlplane: heartbeat: %v\n", err)
				metrics.ControlPlaneError("heartbeat")
			}
			hbTimer.Reset(jitteredInterval(c.HeartbeatInterval, c.JitterFraction))
		case <-pollTimer.C:
			job, err := c.claim(ctx)
			if err != nil {
				if !errors.Is(err, errNoJobs) {
					fmt.Fprintf(stderrSink, "controlplane: claim: %v\n", err)
					metrics.ControlPlaneError("claim")
				}
				pollTimer.Reset(jitteredInterval(c.PollInterval, c.JitterFraction))
				continue
			}
			c.runOne(ctx, job)
			pollTimer.Reset(jitteredInterval(c.PollInterval, c.JitterFraction))
		}
	}
}

// resolveJitterFraction applies the default/disable convention: 0 →
// DefaultJitterFraction, negative → 0 (disabled), positive → as-is.
func resolveJitterFraction(f float64) float64 {
	switch {
	case f == 0:
		return DefaultJitterFraction
	case f < 0:
		return 0
	default:
		return f
	}
}

// firstInterval returns the delay before a timer's FIRST fire. With
// jitter it is drawn uniformly from [0, base) so a fleet that started
// together spreads its first cycle across the whole interval; without
// jitter it is exactly base.
func firstInterval(base time.Duration, fraction float64) time.Duration {
	if fraction <= 0 || base <= 0 {
		return base
	}
	return time.Duration(float64(base) * rand.Float64())
}

// jitteredInterval scales base by a factor uniform in [1-fraction,
// 1+fraction], so the mean cadence is preserved while successive
// intervals are decorrelated. A non-positive fraction returns base
// unchanged.
func jitteredInterval(base time.Duration, fraction float64) time.Duration {
	if fraction <= 0 || base <= 0 {
		return base
	}
	if fraction > 1 {
		fraction = 1
	}
	scale := 1 + fraction*(2*rand.Float64()-1)
	d := time.Duration(float64(base) * scale)
	if d <= 0 {
		// Degenerate (fraction≈1 and the draw≈0) — never schedule a
		// non-positive interval; fall back to the base cadence.
		return base
	}
	return d
}

// heartbeat posts /v1/agents/heartbeat. Failure is non-fatal — the
// loop continues and tries again on the next tick.
func (c *ControlPlaneClient) heartbeat(ctx context.Context) error {
	body := map[string]any{
		"id":          c.AgentID,
		"host":        c.Host,
		"version":     c.Version,
		"deployments": c.Deployments,
	}
	_, err := c.post(ctx, "/v1/agents/heartbeat", body, http.StatusOK)
	return err
}

// claim posts /v1/jobs/claim. Returns errNoJobs when the server
// surfaces 404 + notfound.no_jobs; that's the documented "no work,
// poll again later" path.
func (c *ControlPlaneClient) claim(ctx context.Context) (*ControlPlaneJob, error) {
	body := map[string]any{
		"agent_id":    c.AgentID,
		"deployments": c.Deployments,
		"kinds":       []string{"backup"}, // v0.1: agent-side execution covers backups
	}
	resp, err := c.post(ctx, "/v1/jobs/claim", body, http.StatusOK)
	if err != nil {
		// Distinguish "no jobs" from other failures via the response
		// body's structured code.
		if resp != nil && bytes.Contains(resp, []byte("notfound.no_jobs")) {
			return nil, errNoJobs
		}
		return nil, err
	}
	// Decode envelope.
	var env struct {
		Result *ControlPlaneJob `json:"result"`
	}
	if jerr := json.Unmarshal(resp, &env); jerr != nil {
		return nil, fmt.Errorf("controlplane: claim: parse: %w (body: %s)", jerr, resp)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("controlplane: claim: empty result")
	}
	return env.Result, nil
}

// runOne executes one claimed job through the configured executor,
// streaming progress events back as they fire and posting a
// terminal /complete on return.
func (c *ControlPlaneClient) runOne(ctx context.Context, job *ControlPlaneJob) {
	progress := func(body map[string]any) {
		ev := map[string]any{
			"at":   time.Now().UTC().Format(time.RFC3339Nano),
			"op":   "agent.progress",
			"body": body,
		}
		if _, err := c.post(ctx, "/v1/jobs/"+job.ID+"/progress", ev, http.StatusAccepted); err != nil {
			// Progress posts are best-effort; a failure here doesn't
			// abort the job.
			fmt.Fprintf(stderrSink, "controlplane: progress: %v\n", err)
			metrics.ControlPlaneError("progress")
		}
	}

	result, runErr := c.JobExecutor.Execute(ctx, job, progress)
	completeBody := map[string]any{
		"success": runErr == nil,
		"result":  result,
	}
	if runErr != nil {
		completeBody["failure"] = runErr.Error()
	}
	// previously a single best-effort POST.  A transient
	// network blip on /complete left the job stuck in Running until
	// SweepAbandoned kicked in (window: many minutes, configurable).
	// The agent already finished the work; the only thing the control
	// plane is missing is the success/failure payload.  Retry with
	// exponential backoff so transient failures don't strand jobs.
	c.postCompleteWithRetry(ctx, job.ID, completeBody)
}

// postCompleteWithRetry is the resilient /complete poster.  It tries
// up to completeMaxAttempts times with exponential backoff,
// honouring ctx cancellation between attempts.  After the final
// failure it surfaces the error to stderrSink — at that point the
// agent has done everything it can and SweepAbandoned will reclaim.
//
// We deliberately do NOT use c.HTTPClient's per-request timeout as
// the only retry budget: a 30-second timeout times completeMaxAttempts
// could hold up the next job claim by minutes.  The retry loop
// caps total wallclock at completeRetryBudget.
func (c *ControlPlaneClient) postCompleteWithRetry(ctx context.Context, jobID string, body map[string]any) {
	deadline := time.Now().Add(completeRetryBudget)
	delay := completeInitialBackoff
	var lastErr error
retryLoop:
	for attempt := 1; attempt <= completeMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break retryLoop
		}
		// Cap each attempt's deadline so a hung server doesn't burn
		// the whole budget in one go.  The remaining budget is at
		// least completeInitialBackoff (the loop exit condition).
		attemptCtx, cancel := context.WithTimeout(ctx, completePerAttemptTimeout)
		_, err := c.post(attemptCtx, "/v1/jobs/"+jobID+"/complete", body, http.StatusOK)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		if attempt == completeMaxAttempts || time.Now().Add(delay).After(deadline) {
			break retryLoop
		}
		// Sleep with ctx cancellation honoured.  An unlabelled
		// `break` inside the select would only break the select
		// (not the for-loop), so a SIGINT during backoff was
		// previously ignored — the loop would spin to
		// completeMaxAttempts.  Audit fix: labelled break.
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			lastErr = ctx.Err()
			break retryLoop
		}
		delay *= 2
		if delay > completeMaxBackoff {
			delay = completeMaxBackoff
		}
	}
	fmt.Fprintf(stderrSink, "controlplane: complete %q failed after retries: %v (SweepAbandoned will reclaim the job; check the control plane log for the abandoned reason)\n", jobID, lastErr)
	metrics.ControlPlaneError("complete")
}

// /complete retry budget.  The values reflect "transient failures
// (one or two seconds) get retried; persistent failures fall through
// to SweepAbandoned within ~30s instead of stranding the job until
// the next sweep cycle (minutes)."
const (
	completeMaxAttempts       = 5
	completeInitialBackoff    = 500 * time.Millisecond
	completeMaxBackoff        = 5 * time.Second
	completeRetryBudget       = 30 * time.Second
	completePerAttemptTimeout = 10 * time.Second
)

// post is the shared HTTP helper. Sends body as JSON; returns the
// response body bytes on success (status code matches wantStatus),
// or (body, error) on mismatch — the caller can inspect body for
// structured-error codes.
func (c *ControlPlaneClient) post(ctx context.Context, path string, body any, wantStatus int) ([]byte, error) {
	js, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("controlplane: encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(js))
	if err != nil {
		return nil, fmt.Errorf("controlplane: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controlplane: %s %s: %w", req.Method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != wantStatus {
		return respBody, fmt.Errorf("controlplane: %s %s: status=%d body=%s",
			req.Method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// errNoJobs is the sentinel for "claim returned 404 / no work" so
// the polling loop doesn't log it as a real failure.
var errNoJobs = errors.New("controlplane: no eligible jobs")

// stderrSink is the destination for non-fatal log lines. Pulled into
// a package-level var so tests can substitute a buffer; production
// keeps it pointing at os.Stderr via init.
var stderrSink io.Writer = stderrSinkInit()

func stderrSinkInit() io.Writer {
	// io.Discard would be wrong — we want a real stderr in production.
	// We resolve via a small init so tests can rebind without
	// shadowing the package-level decl.
	return discard{}
}

// discard is an io.Writer that drops bytes — used at package init so
// tests don't see stray "controlplane:" lines on stderr unless they
// rebind stderrSink.
type discard struct{}

// Write implements io.Writer by silently discarding p.
func (discard) Write(p []byte) (int, error) { return len(p), nil }

// SetStderrSink swaps the package-level stderr destination. The CLI
// command sets this to os.Stderr so production runs surface
// non-fatal heartbeat / progress errors; tests leave it at discard
// or substitute a *bytes.Buffer to assert.
func SetStderrSink(w io.Writer) { stderrSink = w }
