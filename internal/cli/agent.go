// agent.go — 'agent' CLI verb: long-lived supervised process driving backups + control-plane jobs.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// newAgentCmd implements `pg_hardstorage agent`. It loads the config,
// builds a Schedule.Engine with one Task per (deployment, action),
// and runs the engine until SIGINT/SIGTERM.
//
// Why an in-process scheduler rather than systemd timers?
//
//   - Composes cleanly with our config layout (one config file
//     declares both backup AND retention schedules per deployment).
//   - Single process = single set of credentials, single audit
//     stream, single source of "what's currently running."
//   - Crash-only design fits — restart the agent and every task is
//     immediately due (since v0.1 stores LastRun in-memory only).
//
// systemd is the right wrapper around this binary, not a replacement
// for the scheduler.
func newAgentCmd() *cobra.Command {
	var (
		dryRun              bool
		controlPlane        string
		controlPlaneToken   string
		controlPlaneAgentID string
		metricsListen       string
	)
	c := &cobra.Command{
		Use:   "agent",
		Short: "Run the host agent (loads config, runs scheduled backups + retention)",
		Long: `Long-running process that schedules and executes the recurring
backup and retention work declared in pg_hardstorage.yaml.

Default mode (no --control-plane): runs the local schedule engine.
--control-plane mode: heartbeats + polls the control plane for jobs;
the polling protocol is fully wired, in-process job execution lands
in.

The agent installs a SIGINT/SIGTERM handler that cancels the engine
cleanly. In-flight tasks finish their current step before the agent
exits.

--dry-run prints the schedule that WOULD run (next-due time per task)
and exits, without starting the engine. Ignored in --control-plane
mode.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if controlPlane != "" {
				return runAgentControlPlane(cmd, controlPlane, controlPlaneToken, controlPlaneAgentID, metricsListen)
			}
			return runAgent(cmd, dryRun, metricsListen)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the schedule and exit without running tasks")
	c.Flags().StringVar(&metricsListen, "metrics-listen", "",
		"bind address for the Prometheus /metrics endpoint (e.g. 127.0.0.1:9187); empty disables it")
	c.Flags().StringVar(&controlPlane, "control-plane", "",
		"control-plane base URL (e.g. https://control:8443); switches the agent into polling mode")
	c.Flags().StringVar(&controlPlaneToken, "control-plane-token-file", "",
		"file containing the bearer token for control-plane requests")
	c.Flags().StringVar(&controlPlaneAgentID, "agent-id", "",
		"agent identity (default: hostname)")
	return c
}

// runAgentControlPlane is the --control-plane mode. Loads the config
// (so we can advertise managed deployments), constructs a
// ControlPlaneClient, and blocks until ctx cancels.
func runAgentControlPlane(cmd *cobra.Command, baseURL, tokenFile, agentID, metricsListen string) error {
	d := DispatcherFrom(cmd)

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		return output.NewError("config.load_failed",
			fmt.Sprintf("agent: load config: %v", err)).Wrap(err)
	}

	deployments := []string{}
	if loaded != nil {
		for name := range loaded.Config.Deployments {
			deployments = append(deployments, name)
		}
		sort.Strings(deployments)
	}

	if agentID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "agent-unknown"
		}
		agentID = host
	}
	host, _ := os.Hostname()

	var token string
	if tokenFile != "" {
		body, err := os.ReadFile(tokenFile)
		if err != nil {
			return output.NewError("config.bad_token_file",
				fmt.Sprintf("agent: read --control-plane-token-file: %v", err)).Wrap(err)
		}
		token = strings.TrimSpace(string(body))
	}

	// Load the keystore so the JobExecutor can sign manifests for
	// claimed jobs. Mirrors the local-schedule path's keystoreFor() —
	// same operator-readable keyring directory, same load-or-generate
	// semantics.
	signer, verifier, kerr := keystoreFor(p)
	if kerr != nil {
		return kerr
	}

	var deps map[string]config.DeploymentConfig
	if loaded != nil {
		deps = loaded.Config.Deployments
	}
	//: route by Kind. The agent claims any kind it advertises,
	// and the router dispatches to the per-Kind executor. Backup,
	// restore, and verify all wired; future kinds (logical
	// restore, partial restore) drop in via additional map entries.
	backupExec := agent.NewBackupExecutor(deps, signer, verifier)
	restoreExec := agent.NewRestoreExecutor(deps, verifier, p.Keyring.Value)
	verifyExec := agent.NewVerifyExecutor(deps, verifier, p.Keyring.Value)
	executor := agent.NewRouterExecutor(map[string]agent.JobExecutor{
		"backup":  backupExec,
		"restore": restoreExec,
		"verify":  verifyExec,
	})

	client := &agent.ControlPlaneClient{
		BaseURL:     baseURL,
		Token:       token,
		AgentID:     agentID,
		Host:        host,
		Version:     version.Version,
		Deployments: deployments,
		JobExecutor: executor,
	}
	agent.SetStderrSink(os.Stderr)

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Optional Prometheus surface for this agent process.  The backup /
	// verify pipelines this agent drives record into the process-wide
	// registry; the listener makes those counters scrapable.
	defer startMetricsListener(ctx, d, metricsListen)()

	_ = d.Event(ctx, output.NewEvent(output.SeverityInfo, "agent", "control_plane.starting").
		WithBody(map[string]any{
			"base_url":    baseURL,
			"agent_id":    agentID,
			"deployments": deployments,
		}))

	runErr := client.Run(ctx)
	if errors.Is(runErr, context.Canceled) {
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
			"agent_id":   agentID,
			"base_url":   baseURL,
			"clean_stop": true,
		}))
	}
	return output.NewError("agent.controlplane_failed",
		fmt.Sprintf("agent control-plane: %v", runErr)).Wrap(runErr)
}

func runAgent(cmd *cobra.Command, dryRun bool, metricsListen string) error {
	d := DispatcherFrom(cmd)

	// Windows: the agent runs as a foreground process —
	// scheduling + signal handling work the same way they
	// do on Linux, but there is no Windows Service Control
	// Manager integration yet (issue #11).  Warn so an
	// operator who launched `pg_hardstorage agent` from a
	// PowerShell prompt knows the process will exit when
	// the console session ends.  See
	// docs/how-to/windows-install.md for the NSSM recipe
	// that wraps the agent into an auto-start service.
	if runtime.GOOS == "windows" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: pg_hardstorage agent on Windows runs in the foreground; "+
				"no Windows Service Control Manager integration yet. "+
				"Use NSSM/WinSW for unattended scheduling — see "+
				"docs/how-to/windows-install.md.")
	}

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		return output.NewError("config.load_failed",
			fmt.Sprintf("agent: load config: %v", err)).Wrap(err)
	}
	if loaded == nil || len(loaded.Config.Deployments) == 0 {
		return output.NewError("config.no_deployments",
			"agent: no deployments configured (add a `deployments:` block to pg_hardstorage.yaml)").
			WithSuggestion(&output.Suggestion{
				Human: "see `pg_hardstorage init` for the Day-0 walkthrough",
			})
	}

	signer, verifier, err := keystoreFor(p)
	if err != nil {
		return err
	}

	engine := schedule.New(
		schedule.WithOnStart(func(name string, due time.Time) {
			_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "agent", "task.started").
				WithBody(map[string]any{"task": name, "due_at": due.UTC().Format(time.RFC3339)}))
		}),
		schedule.WithOnFinish(func(name string, due time.Time, dur time.Duration, runErr error) {
			body := map[string]any{
				"task":        name,
				"duration_ms": dur.Milliseconds(),
			}
			sev := output.SeverityNotice
			if runErr != nil {
				sev = output.SeverityError
				body["error"] = runErr.Error()
			}
			_ = d.Event(cmd.Context(), output.NewEvent(sev, "agent", "task.finished").WithBody(body))
		}),
	)

	taskCount, addErrs := buildAgentTasks(engine, loaded.Config.Deployments, signer, verifier)
	for _, e := range addErrs {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "agent", "task.add_failed").
			WithBody(map[string]any{
				"deployment": e.Deployment,
				"task":       e.Task,
				"error":      e.Err.Error(),
			}))
	}
	if taskCount == 0 {
		return output.NewError("config.no_tasks",
			"agent: no scheduled tasks (every deployment is missing a schedule block)").
			WithSuggestion(&output.Suggestion{
				Human: "add `schedule.backup` and/or `schedule.rotate` blocks under each deployment",
			})
	}

	if dryRun {
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(buildDryRunBody(engine, loaded.Config.Deployments)))
	}

	// Real run: SIGINT/SIGTERM cancels.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Optional Prometheus surface for this agent process (no-op when the
	// flag is empty).  Started before the engine so a scrape during the
	// first backup already sees data.
	defer startMetricsListener(ctx, d, metricsListen)()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	//: also run any registered logical-decoding streams in
	// parallel goroutines, supervised by logical.Runner. The
	// schedule engine and the logical runner share the same ctx
	// — SIGINT cancels both.
	logicalMgr, lerr := logicalManager()
	if lerr != nil {
		return lerr
	}
	registeredStreams, _ := logicalMgr.List()
	var logicalDone chan struct{}
	if len(registeredStreams) > 0 {
		runner := &logical.Runner{
			Manager: logicalMgr,
			ConnectionFor: func(s *logical.Stream) string {
				if dep, ok := loaded.Config.Deployments[s.Deployment]; ok {
					return dep.PGConnection
				}
				return ""
			},
			OnEvent: func(ev *output.Event) { _ = d.Event(ctx, ev) },
		}
		logicalDone = make(chan struct{})
		go func() {
			defer close(logicalDone)
			_ = runner.Run(ctx)
		}()
	}

	//+: spawn one Patroni leader-follow coordinator per
	// deployment that has Patroni configured. Each runs in its
	// own goroutine; all share ctx so a SIGINT cancels every
	// follower along with the schedule engine + logical runner.
	// Failures during coordinator startup are logged and the
	// agent continues — a misconfigured Patroni URL on one
	// deployment shouldn't take the whole agent down.
	followerDones, followerCount, ferr := startPatroniFollowers(ctx, d, loaded.Config.Deployments)
	if ferr != nil {
		// Hard failure (rare — only validation issues we couldn't
		// surface as per-deployment events). Surface and continue;
		// the agent shouldn't refuse to start because Patroni
		// integration is misconfigured for one deployment.
		_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "agent", "patroni.startup_partial").
			WithBody(map[string]any{"error": ferr.Error()}))
	}

	_ = d.Event(ctx, output.NewEvent(output.SeverityNotice, "agent", "starting").
		WithBody(map[string]any{
			"task_count":    taskCount,
			"stream_count":  len(registeredStreams),
			"patroni_count": followerCount,
		}))

	runErr := engine.Run(ctx)
	// Wait for the logical runner to drain before returning, so a
	// shutdown report reflects the actual stop time.
	if logicalDone != nil {
		<-logicalDone
	}
	// Same wait for each Patroni follower coordinator. They block
	// in their own Run() until ctx is cancelled; we drain them
	// before the agent's "stopped" event fires so the report is
	// accurate.
	for _, done := range followerDones {
		<-done
	}
	if errors.Is(runErr, context.Canceled) {
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
			"clean_stop": true,
			"task_count": taskCount,
		}))
	}
	if errors.Is(runErr, schedule.ErrNoTasks) {
		// Defensive — buildAgentTasks already enforces this, but the
		// engine's own error is the canonical "the slate was empty"
		// signal.
		return output.NewError("config.no_tasks",
			"agent: schedule engine started with zero tasks").Wrap(runErr)
	}
	return runErr
}

// keystoreFor resolves the signing keypair the same way the backup /
// rotate / restore commands do. Centralised so the agent's task
// closures can hand it to runner.Take and ManifestStore.
func keystoreFor(p *paths.Paths) (*backup.Signer, *backup.Verifier, error) {
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return nil, nil, output.NewError("internal",
			fmt.Sprintf("agent: signing key: %v", err)).Wrap(err)
	}
	return signer, verifier, nil
}

// agentTaskAddError is the structured failure for "couldn't build a
// task from this deployment's schedule." We accumulate them so one
// bad config doesn't kill the agent before it ever starts running.
type agentTaskAddError struct {
	Deployment string
	Task       string
	Err        error
}

// buildAgentTasks walks deployments and registers backup / rotate
// tasks. Returns (count_added, []errors).
//
// Each task's Run closure captures its own deployment / task / config.
// Tasks fire serially within the engine, so two tasks targeting the
// same repo prefix never race.
func buildAgentTasks(engine *schedule.Engine, deps map[string]config.DeploymentConfig, signer *backup.Signer, verifier *backup.Verifier) (int, []agentTaskAddError) {
	count := 0
	var errs []agentTaskAddError

	// Sort deployment names for deterministic task order in dry-run
	// output and engine internals.
	names := make([]string, 0, len(deps))
	for k := range deps {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		dep := deps[name]
		if !dep.Schedule.Backup.IsZero() {
			task, err := buildBackupTask(name, dep, signer, verifier)
			if err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "backup", Err: err})
			} else if err := engine.Add(task); err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "backup", Err: err})
			} else {
				count++
			}
		}
		if !dep.Schedule.Rotate.IsZero() {
			task, err := buildRotateTask(name, dep, verifier)
			if err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "rotate", Err: err})
			} else if err := engine.Add(task); err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "rotate", Err: err})
			} else {
				count++
			}
		}
		if !dep.Schedule.AuditAnchor.IsZero() {
			task, err := buildAnchorTask(name, dep)
			if err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "audit-anchor", Err: err})
			} else if err := engine.Add(task); err != nil {
				errs = append(errs, agentTaskAddError{Deployment: name, Task: "audit-anchor", Err: err})
			} else {
				count++
			}
		}
	}
	return count, errs
}

// buildBackupTask produces a schedule.Task that runs runner.Take.
// Validates required deployment fields (PGConnection, Repo) up
// front so the engine never registers a task that's guaranteed to
// fail at firing time.
func buildBackupTask(name string, dep config.DeploymentConfig, signer *backup.Signer, verifier *backup.Verifier) (*schedule.Task, error) {
	if dep.PGConnection == "" {
		return nil, errors.New("missing pg_connection")
	}
	if dep.Repo == "" {
		return nil, errors.New("missing repo")
	}
	sched, err := schedule.Parse(schedule.Spec(dep.Schedule.Backup))
	if err != nil {
		return nil, err
	}
	return &schedule.Task{
		Name:     "backup:" + name,
		Schedule: sched,
		Run: func(ctx context.Context) error {
			_, err := runner.Take(ctx, runner.TakeOptions{
				PGConnString: dep.PGConnection,
				RepoURL:      dep.Repo,
				Deployment:   name,
				Tenant:       dep.Tenant,
				Signer:       signer,
				Verifier:     verifier,
			})
			return err
		},
	}, nil
}

// buildAnchorTask produces a schedule.Task that publishes the audit
// chain head into the repo's transparency-log namespace. Idempotent
// — the StorageBackedLog derives a deterministic LogID from the
// chain-head hash + sequence, so re-anchoring the same head returns
// the same ID without creating a duplicate entry. That's what makes
// "anchor every 30 minutes" safe even when the chain hasn't moved.
//
// Anchoring is best-effort: a failed anchor doesn't fail any other
// scheduled task. The schedule engine surfaces the error via its
// onFinish callback (event "task.finished" with err set), which the
// agent's existing event plumbing forwards to monitoring.
//
// Empty audit chain (a fresh repo with no events yet) is also
// best-effort: Anchor returns a structured error in that case which
// the engine surfaces as a Notice-level finish — operators see
// "task ran, no work to do" rather than a hard failure.
func buildAnchorTask(name string, dep config.DeploymentConfig) (*schedule.Task, error) {
	if dep.Repo == "" {
		return nil, errors.New("missing repo")
	}
	sched, err := schedule.Parse(schedule.Spec(dep.Schedule.AuditAnchor))
	if err != nil {
		return nil, err
	}
	publisherID, _ := os.Hostname()
	return &schedule.Task{
		Name:     "audit-anchor:" + name,
		Schedule: sched,
		Run: func(ctx context.Context) error {
			repoMeta, sp, err := repo.Open(ctx, dep.Repo)
			if err != nil {
				return fmt.Errorf("open repo: %w", err)
			}
			defer sp.Close()
			store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
			log := audit.NewStorageBackedLogWithRetention(sp, repoMeta.WORM)
			// Anchor every shard. An empty repo yields no anchors and no
			// error, so the engine doesn't escalate on a quiet repo.
			_, err = store.AnchorAll(ctx, log, publisherID)
			return err
		},
	}, nil
}

// buildRotateTask produces a schedule.Task that runs the retention
// policy and applies it. v0.1: the rotate task uses the configured
// policy or GFS defaults; soft-deletes are real (not dry-run) since
// scheduled runs are inherently authoritative.
func buildRotateTask(name string, dep config.DeploymentConfig, verifier *backup.Verifier) (*schedule.Task, error) {
	if dep.Repo == "" {
		return nil, errors.New("missing repo")
	}
	policy, err := buildRetentionPolicy(dep.Retention)
	if err != nil {
		return nil, err
	}
	sched, err := schedule.Parse(schedule.Spec(dep.Schedule.Rotate))
	if err != nil {
		return nil, err
	}
	return &schedule.Task{
		Name:     "rotate:" + name,
		Schedule: sched,
		Run: func(ctx context.Context) error {
			_, sp, err := repo.Open(ctx, dep.Repo)
			if err != nil {
				return fmt.Errorf("open repo: %w", err)
			}
			defer sp.Close()
			store := backup.NewManifestStore(sp)

			var manifests []*backup.Manifest
			for m, err := range store.List(ctx, name, verifier) {
				if err != nil {
					return fmt.Errorf("list: %w", err)
				}
				manifests = append(manifests, m)
			}

			decision := policy.Apply(time.Now().UTC(), manifests)
			for _, m := range decision.Delete {
				reason := strings.Join(decision.Reasons[m.BackupID], ",")
				if reason == "" {
					reason = "policy=" + decision.PolicyName
				}
				if err := store.SoftDelete(ctx, name, m.BackupID, decision.PolicyName, reason); err != nil {
					// Hold-protection: a held manifest is
					// retention-immune. Skip cleanly so an
					// active hold doesn't break the agent's
					// scheduled rotation. Other errors
					// still bubble up.
					if errors.Is(err, backup.ErrManifestHeld) {
						continue
					}
					return fmt.Errorf("soft-delete %s: %w", m.BackupID, err)
				}
			}
			return nil
		},
	}, nil
}

// buildRetentionPolicy translates the YAML retention block to a
// concrete retention.Policy. Defaults to GFS with the spec's standard
// numbers (7/4/12/5) when the operator doesn't set anything.
func buildRetentionPolicy(c config.RetentionConfig) (retention.Policy, error) {
	policy := strings.ToLower(c.Policy)
	switch policy {
	case "", "gfs":
		return retention.GFSPolicy{
			KeepDaily:   defaultIfZero(c.KeepDaily, 7),
			KeepWeekly:  defaultIfZero(c.KeepWeekly, 4),
			KeepMonthly: defaultIfZero(c.KeepMonthly, 12),
			KeepYearly:  defaultIfZero(c.KeepYearly, 5),
		}, nil
	case "simple":
		dur := 30 * 24 * time.Hour
		if c.KeepFor != "" {
			parsed, err := time.ParseDuration(c.KeepFor)
			if err != nil {
				return nil, fmt.Errorf("retention.keep_for %q: %w", c.KeepFor, err)
			}
			dur = parsed
		}
		return retention.SimplePolicy{KeepFor: dur}, nil
	case "count":
		return retention.CountPolicy{KeepFulls: defaultIfZero(c.KeepFulls, 14)}, nil
	}
	return nil, fmt.Errorf("retention.policy %q (allowed: gfs, simple, count)", policy)
}

func defaultIfZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// buildDryRunBody renders the engine's task list for the --dry-run path.
type dryRunBody struct {
	TaskCount        int                   `json:"task_count"`
	Tasks            []schedule.TaskStatus `json:"tasks"`
	PatroniFollowers []dryRunPatroni       `json:"patroni_followers,omitempty"`
}

// dryRunPatroni summarises one deployment's Patroni follower
// configuration for the --dry-run report. Operators verifying
// their config see exactly what will get spawned at start.
//
// Slot mode reflects the+ Mechanism 2/3 split:
//   - Slots is set with a single entry → single-slot Mechanism 2
//   - Slots has 2+ entries → multi-slot Mechanism 3 (dual-slot
//     etc.) with each role visible
//
// We always emit Slots (not the legacy Slot single-string field)
// so the JSON shape stays uniform; Slot is preserved as a
// human-friendly fallback only when explicitly named.
type dryRunPatroni struct {
	Deployment string              `json:"deployment"`
	URL        string              `json:"url"`
	Slot       string              `json:"slot,omitempty"`
	Slots      []dryRunPatroniSlot `json:"slots,omitempty"`
	Interval   string              `json:"interval,omitempty"`
}

// dryRunPatroniSlot mirrors PatroniSlot in YAML shape.
type dryRunPatroniSlot struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func buildDryRunBody(e *schedule.Engine, deps map[string]config.DeploymentConfig) dryRunBody {
	tasks := e.Tasks()
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].Name < tasks[j].Name
	})

	// Patroni followers — sorted by deployment name so dry-run
	// output is stable across runs.
	names := make([]string, 0, len(deps))
	for name := range deps {
		if deps[name].Patroni.IsEnabled() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var fols []dryRunPatroni
	for _, name := range names {
		dep := deps[name]
		entry := dryRunPatroni{
			Deployment: name,
			URL:        dep.Patroni.URL,
			Interval:   dep.Patroni.Interval,
		}
		if len(dep.Patroni.Slots) > 0 {
			// Multi-slot Mechanism 3: emit the full slots array.
			for _, s := range dep.Patroni.Slots {
				entry.Slots = append(entry.Slots, dryRunPatroniSlot{
					Name: s.Name,
					Role: s.Role,
				})
			}
		} else {
			// Single-slot Mechanism 2: synthesise the default name
			// when not set, surface in the legacy Slot field.
			slot := dep.Patroni.Slot
			if slot == "" {
				slot = "pg_hardstorage_" + name
			}
			entry.Slot = slot
		}
		fols = append(fols, entry)
	}
	return dryRunBody{TaskCount: len(tasks), Tasks: tasks, PatroniFollowers: fols}
}

// WriteText renders dryRunBody for text mode.
func (b dryRunBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Agent dry-run: %d task(s)\n", b.TaskCount)
	for _, ts := range b.Tasks {
		fmt.Fprintf(bw, "  %s\n", ts.Name)
		fmt.Fprintf(bw, "    schedule: %s\n", ts.Description)
		fmt.Fprintf(bw, "    next due: %s\n", ts.NextDue.UTC().Format(time.RFC3339))
	}
	if len(b.PatroniFollowers) > 0 {
		fmt.Fprintf(bw, "Patroni followers: %d\n", len(b.PatroniFollowers))
		for _, f := range b.PatroniFollowers {
			fmt.Fprintf(bw, "  %s\n", f.Deployment)
			fmt.Fprintf(bw, "    url:      %s\n", f.URL)
			if len(f.Slots) > 0 {
				fmt.Fprintf(bw, "    slots:\n")
				for _, s := range f.Slots {
					fmt.Fprintf(bw, "      - name: %s, role: %s\n", s.Name, s.Role)
				}
			} else {
				fmt.Fprintf(bw, "    slot:     %s\n", f.Slot)
			}
			if f.Interval != "" {
				fmt.Fprintf(bw, "    interval: %s\n", f.Interval)
			}
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// startPatroniFollowers spawns one follower.Coordinator goroutine
// per Patroni-configured deployment. Returns the per-deployment
// done-channels (the agent waits on these before its final
// "stopped" event so shutdown timing is accurate), the count of
// goroutines started (for the agent's "starting" event body),
// and a non-nil error only when a HARD startup failure couldn't
// be surfaced as a per-deployment event (rare; almost everything
// is per-deployment-best-effort because a misconfigured Patroni
// URL on one deployment shouldn't take the whole agent down).
//
// The repo + storage + timeline-store wiring needs the same
// pattern as buildAgentTasks: open the storage plugin once per
// deployment so the goroutine can write timeline-history files
// to the right repo. We open lazily inside the goroutine so an
// unreachable repo doesn't block the agent's startup.
func startPatroniFollowers(ctx context.Context, d *output.Dispatcher, deps map[string]config.DeploymentConfig) ([]chan struct{}, int, error) {
	// Sort the deployment names so event ordering is deterministic
	// in tests + log streams.
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}
	sort.Strings(names)

	var dones []chan struct{}
	count := 0
	for _, name := range names {
		dep := deps[name]
		if !dep.Patroni.IsEnabled() {
			continue
		}
		if dep.Repo == "" {
			_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "agent", "patroni.skipped_no_repo").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{
					"hint": "deployment has patroni.url but no repo: configured; the timeline-history capture writes into the repo, so we skip this deployment's follower",
				}))
			continue
		}
		client, err := patroni.NewClient(dep.Patroni.URL,
			patroniClientOpts(dep.Patroni)...)
		if err != nil {
			_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.client_init_failed").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{
					"url":   dep.Patroni.URL,
					"error": err.Error(),
				}))
			continue
		}

		interval, ierr := parsePatroniInterval(dep.Patroni.Interval)
		if ierr != nil {
			_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.bad_interval").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{
					"interval": dep.Patroni.Interval,
					"error":    ierr.Error(),
				}))
			continue
		}

		// Resolve slot configuration. Two modes:
		//   - Slots set → multi-slot Mechanism 3
		//   - Slot or empty → single-slot Mechanism 2
		// Mutually-exclusive; reject both-set as a config bug
		// before doing any side-effecting work.
		if dep.Patroni.Slot != "" && len(dep.Patroni.Slots) > 0 {
			_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.slot_config_conflict").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{
					"hint": "patroni.slot and patroni.slots are mutually exclusive — pick single-slot Mechanism 2 (slot:) OR multi-slot Mechanism 3 (slots: [...])",
				}))
			continue
		}
		var slotName string
		var slots []follower.SlotSpec
		if len(dep.Patroni.Slots) > 0 {
			// Multi-slot Mechanism 3 path.
			slots = make([]follower.SlotSpec, 0, len(dep.Patroni.Slots))
			for i, s := range dep.Patroni.Slots {
				role, rerr := parseSlotRole(s.Role)
				if rerr != nil {
					_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.bad_slot_role").
						WithSubject(output.Subject{Deployment: name}).
						WithBody(map[string]any{
							"index": i,
							"name":  s.Name,
							"role":  s.Role,
							"error": rerr.Error(),
						}))
					continue
				}
				if s.Name == "" {
					_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.bad_slot_name").
						WithSubject(output.Subject{Deployment: name}).
						WithBody(map[string]any{
							"index": i,
							"hint":  "patroni.slots[*].name is required",
						}))
					continue
				}
				slots = append(slots, follower.SlotSpec{Name: s.Name, Role: role})
			}
			if len(slots) == 0 {
				// Every entry was rejected — skip this deployment.
				continue
			}
		} else {
			slotName = dep.Patroni.Slot
			if slotName == "" {
				slotName = "pg_hardstorage_" + name
			}
		}

		// Open the storage plugin ONCE per deployment so the
		// timeline.Store has stable backing across leader changes.
		// If the repo is unreachable at startup, log + skip; the
		// agent itself stays up. The follower can be re-tried by
		// restarting the agent after the repo is reachable again.
		_, sp, oerr := repo.Open(ctx, dep.Repo)
		if oerr != nil {
			_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.repo_open_failed").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{
					"repo":  dep.Repo,
					"error": oerr.Error(),
				}))
			continue
		}
		ts := timeline.New(sp)
		gs := gapstate.New(sp)

		// DSNFor: the agent has the libpq DSN for the deployment
		// in PGConnection. Patroni reports the new leader's
		// host:port; we need to splice them into the existing DSN
		// so user/password/sslmode/etc. carry over. patroniDSNFor
		// handles the splice; failure to parse the existing DSN
		// surfaces as a structured error event during the first
		// reconcile, not at startup.
		dsnFor := patroniDSNFor(dep.PGConnection)

		// LastConfirmedLSN: derive from the repo's WAL inventory
		// for the new leader's TLI. The Coordinator calls this
		// once per reconcile, so the cost (one List walk over
		// wal/<deployment>/<tli>/) lands only on leader-change
		// events — not on every Patroni poll. A zero return
		// (no segments archived yet for this TLI) is the
		// "first-time bootstrap" signal EnsureSlot expects.
		//
		// We capture sp + name in the closure rather than
		// re-opening the repo per call. The plug.Close() in
		// the goroutine below releases the SP at agent
		// shutdown.
		spForCoord := sp
		deploymentForCoord := name
		coord, cerr := follower.New(follower.Options{
			Client:        client,
			SlotName:      slotName,
			Slots:         slots,
			Deployment:    name,
			TimelineStore: ts,
			GapStore:      gs,
			DSNFor:        dsnFor,
			Interval:      interval,
			OnEvent:       func(ev *output.Event) { _ = d.Event(ctx, ev) },
			LastConfirmedLSN: func(leader patroni.LeaderEndpoint) pglogrepl.LSN {
				lsn, _, err := inventory.HighestArchivedLSN(ctx, spForCoord, deploymentForCoord, leader.Timeline)
				if err != nil {
					// Repo unreachable / List error → degrade
					// to "first-time bootstrap" rather than
					// crash the reconcile. EnsureSlot handles
					// zero correctly; the Coordinator's gap
					// calc skips when lastConfirmed=0.
					_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "agent", "patroni.lsn_lookup_failed").
						WithSubject(output.Subject{Deployment: deploymentForCoord, Timeline: leader.Timeline}).
						WithBody(map[string]any{"error": err.Error()}))
					return 0
				}
				return lsn
			},
		})
		if cerr != nil {
			_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.coordinator_init_failed").
				WithSubject(output.Subject{Deployment: name}).
				WithBody(map[string]any{"error": cerr.Error()}))
			_ = sp.Close()
			continue
		}

		done := make(chan struct{})
		dones = append(dones, done)
		count++
		// One goroutine per follower. ctx cancel propagates from
		// the agent's signal handler.
		go func(c *follower.Coordinator, deployment string, plug storage.StoragePlugin) {
			defer close(done)
			defer func() { _ = plug.Close() }()
			if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				_ = d.Event(ctx, output.NewEvent(output.SeverityError, "agent", "patroni.coordinator_failed").
					WithSubject(output.Subject{Deployment: deployment}).
					WithBody(map[string]any{"error": err.Error()}))
			}
		}(coord, name, sp)
	}
	return dones, count, nil
}

// patroniClientOpts builds the variadic options for
// patroni.NewClient from a deployment's PatroniConfig. Today
// only basic-auth lands; future fields (TLS pinning, custom
// HTTP client) drop in here.
func patroniClientOpts(cfg config.PatroniConfig) []patroni.ClientOption {
	var opts []patroni.ClientOption
	if cfg.User != "" || cfg.Password != "" {
		opts = append(opts, patroni.WithAuth(cfg.User, cfg.Password))
	}
	return opts
}

// parseSlotRole maps a YAML role string to the Coordinator's
// SlotRole constant. Case-insensitive on purpose — operators
// writing YAML by hand sometimes capitalize "Leader" or
// "Replica".
func parseSlotRole(s string) (follower.SlotRole, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "leader", "primary":
		return follower.SlotRoleLeader, nil
	case "replica", "standby":
		return follower.SlotRoleReplica, nil
	}
	return "", fmt.Errorf("role %q must be 'leader' or 'replica'", s)
}

// parsePatroniInterval parses the YAML duration string. Empty
// → zero (the Coordinator falls through to its default). Bad
// strings surface a structured error so the operator sees the
// typo at startup instead of mid-incident.
func parsePatroniInterval(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// patroniDSNFor returns a DSNFor closure that splices Patroni's
// reported leader host:port into the deployment's libpq DSN.
// The original DSN's user/password/dbname/sslmode/connect_timeout
// flow through unchanged — only the host + port are replaced.
//
// libpq supports two DSN forms: URI (postgres://user:pass@host:port/db?...)
// and key-value (host=h port=p user=u dbname=d sslmode=disable).
// We support both; the splice picks the URI path if the input
// looks URI-shaped, else the key-value path.
//
// On parse failure the closure returns "" — the Coordinator
// surfaces a structured dsn_build_failed event when that happens,
// so we don't propagate the parse error here.
func patroniDSNFor(originalDSN string) func(host string, port int) string {
	return func(host string, port int) string {
		return spliceDSNHostPort(originalDSN, host, port)
	}
}

// spliceDSNHostPort is the worker. Exposed at package scope so
// the unit tests can drive it without standing up a Coordinator.
func spliceDSNHostPort(dsn, host string, port int) string {
	if dsn == "" || host == "" || port == 0 {
		return ""
	}
	// URI form: postgres:// or postgresql:// prefix.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		u.Host = fmt.Sprintf("%s:%d", host, port)
		return u.String()
	}
	// Key-value form: rebuild with host/port replaced. A naive
	// strings.Fields split corrupts DSNs with quoted values that
	// contain spaces (e.g. password='a b') — it would split the value
	// across two tokens and drop the rest. Parse with a libpq-aware
	// tokenizer that respects single-quoted values, carry through every
	// pair except host / hostaddr / port, then re-serialize.
	pairs, ok := parseKeyValueDSN(dsn)
	if !ok {
		return ""
	}
	out := make([]string, 0, len(pairs)+2)
	for _, kv := range pairs {
		switch kv.key {
		case "host", "hostaddr", "port":
			continue
		}
		out = append(out, kv.key+"="+quoteDSNValue(kv.value))
	}
	out = append(out, fmt.Sprintf("host=%s", host))
	out = append(out, fmt.Sprintf("port=%d", port))
	return strings.Join(out, " ")
}

// dsnKV is one parsed key=value pair from a libpq keyword/value DSN.
type dsnKV struct {
	key   string
	value string
}

// parseKeyValueDSN parses a libpq keyword/value connection string into
// ordered key=value pairs, respecting single-quoted values (which may
// contain spaces) and backslash escapes inside quotes. It follows the
// libpq grammar closely enough for the DSNs we splice on a Patroni
// leader change: whitespace-separated `key = value` pairs, optional
// spaces around '=', and single-quoted values with \' and \\ escapes.
// Returns ok=false on a malformed string (unterminated quote, missing
// value) so the caller can fail closed rather than emit a corrupt DSN.
func parseKeyValueDSN(dsn string) ([]dsnKV, bool) {
	var pairs []dsnKV
	i, n := 0, len(dsn)
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	for {
		for i < n && isSpace(dsn[i]) {
			i++
		}
		if i >= n {
			break
		}
		// Read the key up to '=' or whitespace.
		start := i
		for i < n && dsn[i] != '=' && !isSpace(dsn[i]) {
			i++
		}
		key := dsn[start:i]
		if key == "" {
			return nil, false
		}
		for i < n && isSpace(dsn[i]) {
			i++
		}
		if i >= n || dsn[i] != '=' {
			return nil, false
		}
		i++ // consume '='
		for i < n && isSpace(dsn[i]) {
			i++
		}
		// Read the value: single-quoted (may contain spaces + escapes)
		// or a bare token terminated by whitespace.
		var val strings.Builder
		if i < n && dsn[i] == '\'' {
			i++ // opening quote
			closed := false
			for i < n {
				c := dsn[i]
				if c == '\\' && i+1 < n {
					val.WriteByte(dsn[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return nil, false // unterminated quote
			}
		} else {
			for i < n && !isSpace(dsn[i]) {
				if dsn[i] == '\\' && i+1 < n {
					val.WriteByte(dsn[i+1])
					i += 2
					continue
				}
				val.WriteByte(dsn[i])
				i++
			}
		}
		pairs = append(pairs, dsnKV{key: key, value: val.String()})
	}
	return pairs, true
}

// quoteDSNValue renders a value for a libpq keyword/value DSN,
// single-quoting (and escaping) it when it is empty or contains
// whitespace / quote / backslash characters — so a round-tripped value
// with spaces (password='a b') survives re-serialization intact.
func quoteDSNValue(v string) string {
	needQuote := v == ""
	if !needQuote {
		for i := 0; i < len(v); i++ {
			switch v[i] {
			case ' ', '\t', '\n', '\r', '\'', '\\':
				needQuote = true
			}
			if needQuote {
				break
			}
		}
	}
	if !needQuote {
		return v
	}
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(v); i++ {
		if v[i] == '\'' || v[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('\'')
	return b.String()
}
