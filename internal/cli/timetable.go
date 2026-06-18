// timetable.go — CLI surface for emitting (and optionally applying) pg_timetable schedules.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// newTimetableCmd implements `pg_hardstorage timetable <emit>`.
//
// pg_timetable is the project's recommended PG-native scheduler
// (the design doc explicitly steers operators away from cron when
// they have PG anyway: "Schedule it via pg_timetable or cron at
// whatever cadence matches your egress budget"). This command
// emits ready-to-paste SQL the operator pipes into psql against
// their pg_timetable installation:
//
//	pg_hardstorage timetable emit --repo s3://… --deployment db1 \
//	  | psql -d timetable_admin
//
// The emitted SQL covers the cron-friendly housekeeping commands
// shipped:
//
//	repo scrub --sample-percent 1   hourly
//	wal audit                       hourly
//	anomaly check                   daily
//	wal prune --apply               daily
//	repo gc --apply                 weekly
//	gameday report                  quarterly
//
// Each job uses pg_timetable's PROGRAM kind with a JSONB-shaped
// argv (no shell interpolation hazards). `--include` and
// `--exclude` whitelist / blacklist by job name; `--prefix` lets
// operators namespace the jobs (e.g. one prefix per pg_hardstorage
// fleet sharing the same pg_timetable installation).
func newTimetableCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "timetable <emit>",
		Short: "Generate scheduling SQL for pg_timetable",
	}
	c.AddCommand(newTimetableEmitCmd())
	return c
}

// timetableJob is one row in the emit output. We declare the set
// in code (rather than reading from the user's config) so the
// emitted SQL is the same shape every operator gets — easier to
// review across deployments.
type timetableJob struct {
	Name        string // logical name, gets prefixed with --prefix
	Schedule    string // pg_timetable cron expression
	Description string // for the SQL header comment
	// Args is the argv passed to pg_hardstorage. {{repo}} and
	// {{deployment}} placeholders get replaced from flags.
	Args []string
	// RequiresDeployment marks jobs that need --deployment to be
	// set. Repo-scoped jobs (gc, scrub, replicate) don't.
	RequiresDeployment bool
}

// timetableJobs is the canonical job list. Order = output order.
// Times are spread across the hour to avoid thundering-herd
// effects when many fleets share one pg_timetable instance.
func timetableJobs() []timetableJob {
	return []timetableJob{
		{
			Name:        "repo_scrub_hourly",
			Schedule:    "5 * * * *",
			Description: "Hourly: scrub 1% of chunks for bit-rot detection",
			Args:        []string{"repo", "scrub", "{{repo}}", "--sample-percent", "1", "-o", "json"},
		},
		{
			Name:               "wal_audit_hourly",
			Schedule:           "15 * * * *",
			Description:        "Hourly: audit WAL for LSN gaps",
			Args:               []string{"wal", "audit", "{{deployment}}", "--repo", "{{repo}}", "-o", "json"},
			RequiresDeployment: true,
		},
		{
			Name:               "anomaly_check_daily",
			Schedule:           "10 4 * * *",
			Description:        "Daily 04:10: check the latest backup for size/duration anomalies",
			Args:               []string{"anomaly", "check", "{{deployment}}", "--repo", "{{repo}}", "-o", "json"},
			RequiresDeployment: true,
		},
		{
			Name:               "wal_prune_daily",
			Schedule:           "30 4 * * *",
			Description:        "Daily 04:30: prune WAL older than the oldest kept backup, keep last 14 days",
			Args:               []string{"wal", "prune", "{{deployment}}", "--repo", "{{repo}}", "--apply", "--keep-since", "336h", "-o", "json"},
			RequiresDeployment: true,
		},
		{
			Name:        "repo_gc_weekly",
			Schedule:    "0 5 * * 0",
			Description: "Weekly Sunday 05:00: reclaim orphan chunks (after wal prune)",
			Args:        []string{"repo", "gc", "{{repo}}", "--apply", "-o", "json"},
		},
		{
			Name:        "repo_scrub_quarterly_full",
			Schedule:    "0 6 1 1,4,7,10 *",
			Description: "Quarterly: full scrub (every chunk re-hashed)",
			Args:        []string{"repo", "scrub", "{{repo}}", "--full", "-o", "json"},
		},
		{
			Name:        "gameday_report_quarterly",
			Schedule:    "0 7 1 1,4,7,10 *",
			Description: "Quarterly: emit a gameday report (regulator-friendly chaos rehearsal evidence)",
			Args:        []string{"gameday", "report", "--repo", "{{repo}}", "--since", "0", "-o", "json"},
		},
	}
}

func newTimetableEmitCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		prefix     string
		include    string
		exclude    string
		binary     string
		apply      bool
		pgConn     string
		replace    bool
	)
	c := &cobra.Command{
		Use:   "emit",
		Short: "Print (or directly install) pg_timetable SQL for the cron-friendly housekeeping commands",
		Long: `Emit a SELECT timetable.add_job(...) block per scheduled
operation. Two modes:

  Print to stdout (default):
    pg_hardstorage timetable emit --repo s3://acme/backups --deployment db1 \
        | psql -d timetable_admin

  Apply directly to a pg_timetable database:
    pg_hardstorage timetable emit --repo s3://acme/backups --deployment db1 \
        --apply --pg-connection "postgres://timetable@host/timetable"

Each job uses pg_timetable's PROGRAM kind with a JSONB argv (no
shell escaping hazards). The default cadence matches what the
project's CHANGELOG documents for each command:

    repo scrub --sample-percent 1   hourly
    wal audit                       hourly
    anomaly check                   daily
    wal prune --apply               daily
    repo gc --apply                 weekly
    repo scrub --full               quarterly
    gameday report                  quarterly

Use --include / --exclude to subset; --prefix to namespace the job
names (default: pg_hardstorage_).

When --apply is set, the command:
  1. Connects to --pg-connection and pre-flights the timetable schema.
  2. Wraps everything in a single Go-managed transaction.
  3. (Default) DELETEs existing jobs whose chain_name starts with
     --prefix, so re-applies are idempotent. Pass --replace=false
     to fail-on-duplicate instead.
  4. Calls timetable.add_job for each job with parameter binding
     (no SQL string interpolation of operator-supplied values).

Targets pg_timetable v5; for other versions, the printed SQL is a
starting point operators adapt.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTimetableEmit(cmd, timetableEmitFlags{
				repoURL:    repoURL,
				deployment: deployment,
				prefix:     prefix,
				include:    include,
				exclude:    exclude,
				binary:     binary,
				apply:      apply,
				pgConn:     pgConn,
				replace:    replace,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required; substituted into every job's argv)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&deployment, "deployment", "",
		"deployment name (required for per-deployment jobs: wal audit, wal prune, anomaly check)")
	c.Flags().StringVar(&prefix, "prefix", "pg_hardstorage_",
		"job-name prefix for namespacing in pg_timetable")
	c.Flags().StringVar(&include, "include", "",
		"comma-separated job names (without prefix) to include; empty = all")
	c.Flags().StringVar(&exclude, "exclude", "",
		"comma-separated job names (without prefix) to exclude")
	c.Flags().StringVar(&binary, "binary", "pg_hardstorage",
		"path to the pg_hardstorage binary the scheduled jobs invoke")
	c.Flags().BoolVar(&apply, "apply", false,
		"actually install the jobs into pg_timetable (requires --pg-connection)")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq DSN for the pg_timetable installation (required with --apply)")
	c.Flags().BoolVar(&replace, "replace", true,
		"with --apply: DELETE existing chains whose name starts with --prefix before adding (default true)")
	return c
}

type timetableEmitFlags struct {
	repoURL    string
	deployment string
	prefix     string
	include    string
	exclude    string
	binary     string
	apply      bool
	pgConn     string
	replace    bool
}

func runTimetableEmit(cmd *cobra.Command, f timetableEmitFlags) error {
	d := DispatcherFrom(cmd)
	// Mutual-exclusion + dependency checks for the apply flags.
	if f.apply && f.pgConn == "" {
		return missingFlagErr(cmd, "--pg-connection (with --apply)")
	}
	if !f.apply && f.pgConn != "" {
		return output.NewError("usage.bad_flag",
			"timetable emit: --pg-connection is only meaningful with --apply").Wrap(output.ErrUsage)
	}

	wantInclude := splitCommaTrim(f.include)
	wantExclude := splitCommaTrim(f.exclude)
	includeSet := setOf(wantInclude)
	excludeSet := setOf(wantExclude)

	var (
		jobs    []timetableJob
		skipped []string
	)
	for _, j := range timetableJobs() {
		if len(includeSet) > 0 {
			if _, ok := includeSet[j.Name]; !ok {
				continue
			}
		}
		if _, ok := excludeSet[j.Name]; ok {
			skipped = append(skipped, j.Name)
			continue
		}
		if j.RequiresDeployment && f.deployment == "" {
			skipped = append(skipped, j.Name+" (needs --deployment)")
			continue
		}
		jobs = append(jobs, j)
	}

	// Sort skipped list deterministically for the result body.
	sort.Strings(skipped)

	sql := buildTimetableSQL(jobs, f)
	body := timetableEmitBody{
		Schema:     "pg_hardstorage.timetable.emit.v1",
		Repo:       f.repoURL,
		Deployment: f.deployment,
		Prefix:     f.prefix,
		Binary:     f.binary,
		Jobs:       jobNames(jobs, f.prefix),
		Skipped:    skipped,
		SQL:        sql,
	}

	// Apply path: open the pg_timetable DB and execute the jobs
	// via parameterised add_job calls (NOT the printed SQL). This
	// is safer than running the multi-statement SQL through Exec
	// because:
	//   - Each add_job call is a single parameterised statement
	//     with operator-supplied values bound, not interpolated.
	//   - We control the transaction boundary in Go, so partial
	//     failures roll back atomically.
	//   - Pre-flight checks for the pg_timetable schema fail
	//     early with structured errors.
	if f.apply {
		applied, err := applyTimetableJobs(cmd.Context(), f, jobs)
		if err != nil {
			return err
		}
		body.Applied = true
		body.PGConnection = sanitisePGDSN(f.pgConn)
		body.JobsInserted = applied.inserted
		body.JobsReplaced = applied.replaced
		body.ApplyDurationMS = applied.durationMS
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// applyResult is the local return type for the apply path.
type applyResult struct {
	inserted   int
	replaced   int
	durationMS int64
}

// applyTimetableJobs opens the pg_timetable DB, pre-flights the
// schema, and (in a single transaction) deletes existing chains
// matching the prefix (when --replace) before re-adding.
func applyTimetableJobs(ctx context.Context, f timetableEmitFlags, jobs []timetableJob) (*applyResult, error) {
	c, err := pg.Connect(ctx, f.pgConn, pg.ModeRegular)
	if err != nil {
		// pg.Connect returns structured *output.Error already.
		return nil, err
	}
	defer c.Close(ctx)

	pc := c.PgConn()
	startedAt := time.Now()

	// 1. Pre-flight: pg_timetable schema present.
	res := pc.ExecParams(ctx,
		`SELECT count(*)::int8 FROM pg_namespace WHERE nspname = 'timetable'`,
		nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, output.NewError("timetable.preflight_failed",
			fmt.Sprintf("timetable emit --apply: pre-flight failed: %v", res.Err)).
			Wrap(res.Err)
	}
	if len(res.Rows) != 1 || string(res.Rows[0][0]) == "0" {
		return nil, output.NewError("timetable.not_installed",
			"timetable emit --apply: pg_timetable extension not installed in target DB").
			WithSuggestion(&output.Suggestion{
				Human: "install pg_timetable in the target database first; see https://github.com/cybertec-postgresql/pg_timetable",
			})
	}

	// 2. Open a transaction. We can't use pgconn's Exec for BEGIN
	//    + parameterised statements + COMMIT directly because
	//    pgconn doesn't have a stateful tx API; instead we use
	//    individual ExecParams calls and use BEGIN/COMMIT as
	//    explicit statements. If any add_job fails, we issue
	//    ROLLBACK before returning.
	if err := execSimple(ctx, pc, "BEGIN"); err != nil {
		return nil, output.NewError("timetable.apply_failed",
			fmt.Sprintf("timetable emit --apply: BEGIN: %v", err)).Wrap(err)
	}
	rolledBack := false
	rollback := func() {
		if rolledBack {
			return
		}
		rolledBack = true
		_ = execSimple(ctx, pc, "ROLLBACK")
	}

	// 3. Optional replace: DELETE existing chains with our prefix.
	var replaced int
	if f.replace {
		// pgconn doesn't return affected-row counts in the same
		// shape across versions; we do a count-then-delete pair so
		// we can report a meaningful number.
		countRes := pc.ExecParams(ctx,
			`SELECT count(*)::int8 FROM timetable.chain WHERE chain_name LIKE $1`,
			[][]byte{[]byte(f.prefix + "%")},
			nil, nil, nil).Read()
		if countRes.Err != nil {
			rollback()
			return nil, output.NewError("timetable.apply_failed",
				fmt.Sprintf("timetable emit --apply: count existing chains: %v", countRes.Err)).
				Wrap(countRes.Err)
		}
		if len(countRes.Rows) == 1 {
			fmt.Sscan(string(countRes.Rows[0][0]), &replaced)
		}
		delRes := pc.ExecParams(ctx,
			`DELETE FROM timetable.chain WHERE chain_name LIKE $1`,
			[][]byte{[]byte(f.prefix + "%")},
			nil, nil, nil).Read()
		if delRes.Err != nil {
			rollback()
			return nil, output.NewError("timetable.apply_failed",
				fmt.Sprintf("timetable emit --apply: DELETE existing chains: %v", delRes.Err)).
				Wrap(delRes.Err)
		}
	}

	// 4. Insert each job via timetable.add_job(...) with bound
	//    parameters.
	const addJobSQL = `
SELECT timetable.add_job(
    job_name          => $1,
    job_schedule      => $2,
    job_command       => $3,
    job_parameters    => $4::jsonb,
    job_kind          => 'PROGRAM'::timetable.command_kind,
    job_self_destruct => false,
    job_live          => true
)`
	var inserted int
	for _, j := range jobs {
		argsJSON, err := json.Marshal(substituteJobArgs(j.Args, f))
		if err != nil {
			rollback()
			return nil, output.NewError("internal",
				fmt.Sprintf("timetable emit --apply: marshal args for %s: %v", j.Name, err)).Wrap(err)
		}
		r := pc.ExecParams(ctx, addJobSQL,
			[][]byte{
				[]byte(f.prefix + j.Name),
				[]byte(j.Schedule),
				[]byte(f.binary),
				argsJSON,
			},
			nil, nil, nil).Read()
		if r.Err != nil {
			rollback()
			return nil, output.NewError("timetable.apply_failed",
				fmt.Sprintf("timetable emit --apply: add_job %s: %v",
					f.prefix+j.Name, r.Err)).
				WithSuggestion(&output.Suggestion{
					Human: "the most common cause is a duplicate chain_name; pass --replace to wipe the old chains first",
				}).Wrap(r.Err)
		}
		inserted++
	}

	// 5. COMMIT.
	if err := execSimple(ctx, pc, "COMMIT"); err != nil {
		rollback()
		return nil, output.NewError("timetable.apply_failed",
			fmt.Sprintf("timetable emit --apply: COMMIT: %v", err)).Wrap(err)
	}

	return &applyResult{
		inserted:   inserted,
		replaced:   replaced,
		durationMS: time.Since(startedAt).Milliseconds(),
	}, nil
}

// execSimple runs a parameterless statement against pc. Used for
// BEGIN/COMMIT/ROLLBACK only — every operator-data-bearing
// statement goes through ExecParams with bound parameters.
func execSimple(ctx context.Context, pc *pgconn.PgConn, sql string) error {
	mrr := pc.Exec(ctx, sql)
	_, err := mrr.ReadAll()
	return err
}

// sanitisePGDSN strips the password from a libpq URI for the
// result body. We surface the host:port + database so an operator
// reviewing audit logs can see WHICH pg_timetable was targeted
// without exposing the credential.
func sanitisePGDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// Not a URL DSN (key=value form); strip password tokens
		// best-effort. For simplicity here, just return "<dsn
		// hidden>".
		return "<dsn hidden>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.Redacted()
}

// substituteJobArgs is the apply-path counterpart of
// renderArgsJSON's substitution loop. Returns a []string ready
// for json.Marshal.
func substituteJobArgs(args []string, f timetableEmitFlags) []string {
	out := make([]string, len(args))
	for i, a := range args {
		v := a
		v = strings.ReplaceAll(v, "{{repo}}", f.repoURL)
		v = strings.ReplaceAll(v, "{{deployment}}", f.deployment)
		out[i] = v
	}
	return out
}

// buildTimetableSQL renders the SQL block. Wrapped in BEGIN/COMMIT
// so it applies atomically (an operator can re-run it idempotently
// after editing — pg_timetable's add_job upserts when a job_name
// already exists at the schema level the operator manages, but
// the safe operator-friendly default is to wrap their workflow
// in their own DELETE-and-recreate pattern).
//
// We deliberately emit one INSERT per job rather than a multi-row
// VALUES so an operator copy-pasting just one block works.
func buildTimetableSQL(jobs []timetableJob, f timetableEmitFlags) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- pg_hardstorage timetable jobs (pg_timetable v5)\n")
	fmt.Fprintf(&sb, "-- Generated by pg_hardstorage %s at %s\n",
		version.Version, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "-- Repository: %s\n", f.repoURL)
	if f.deployment != "" {
		fmt.Fprintf(&sb, "-- Deployment: %s\n", f.deployment)
	}
	fmt.Fprintf(&sb, "--\n")
	fmt.Fprintf(&sb, "-- Apply with: pg_hardstorage timetable emit ... | psql -d <pg_timetable-db>\n")
	fmt.Fprintf(&sb, "-- Re-run after editing the schedule list; pg_timetable will fail on duplicate\n")
	fmt.Fprintf(&sb, "-- job_name unless you DELETE first.\n\n")
	fmt.Fprintf(&sb, "BEGIN;\n\n")

	for _, j := range jobs {
		fmt.Fprintf(&sb, "-- %s\n", j.Description)
		fmt.Fprintf(&sb, "SELECT timetable.add_job(\n")
		fmt.Fprintf(&sb, "    job_name          => %s,\n", quoteSQLString(f.prefix+j.Name))
		fmt.Fprintf(&sb, "    job_schedule      => %s,\n", quoteSQLString(j.Schedule))
		fmt.Fprintf(&sb, "    job_command       => %s,\n", quoteSQLString(f.binary))
		fmt.Fprintf(&sb, "    job_parameters    => %s::jsonb,\n", quoteSQLString(renderArgsJSON(j.Args, f)))
		fmt.Fprintf(&sb, "    job_kind          => 'PROGRAM'::timetable.command_kind,\n")
		fmt.Fprintf(&sb, "    job_self_destruct => false,\n")
		fmt.Fprintf(&sb, "    job_live          => true\n")
		fmt.Fprintf(&sb, ");\n\n")
	}
	fmt.Fprintf(&sb, "COMMIT;\n")
	return sb.String()
}

// renderArgsJSON renders Args as a JSON array of strings, with
// the same {{repo}} / {{deployment}} substitution the apply path
// does via substituteJobArgs.
//
// Implementation: delegates to encoding/json.Marshal on the
// substituted []string. We used to hand-build the JSON via a
// custom jsonQuote that only escaped backslash + double-quote;
// that left control characters (\n, \t, NUL etc.) unescaped, so
// an operator passing a multi-line --repo or --deployment value
// produced invalid JSON in the emitted SQL. json.Marshal handles
// every JSON-string escape correctly and still emits a one-line
// readable form for arrays of plain strings — the original
// "manually escape so the SQL stays readable" rationale doesn't
// require a custom quoter.
func renderArgsJSON(args []string, f timetableEmitFlags) string {
	subs := substituteJobArgs(args, f)
	body, err := json.Marshal(subs)
	if err != nil {
		// json.Marshal of []string is total: every Go string
		// (including invalid UTF-8) marshals to valid JSON via
		// � substitution. The error path is unreachable in
		// practice; if a future Go change makes it possible, the
		// fallback is the original semantics — empty array — which
		// produces an obviously-broken SQL block the operator will
		// notice immediately. We don't propagate the error because
		// the emitter's signature is `string` not `(string, error)`
		// and the call sites pipe to fmt.Fprintf.
		return "[]"
	}
	return string(body)
}

// quoteSQLString renders s as a single-quoted SQL string with
// embedded quotes doubled. Doesn't try to handle E'\\…' style
// escapes — every value we emit is plain ASCII.
func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func setOf(xs []string) map[string]struct{} {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

func jobNames(jobs []timetableJob, prefix string) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = prefix + j.Name
	}
	return out
}

// timetableEmitBody is the v1-stable Result body. SQL is the
// primary payload; the metadata (Repo, Deployment, Jobs, Skipped)
// gives JSON consumers a structured view + lets the text-mode
// renderer print a one-line summary above the SQL. Apply-path
// fields (Applied, PGConnection, JobsInserted, JobsReplaced,
// ApplyDurationMS) are populated only when --apply was used.
type timetableEmitBody struct {
	Schema     string   `json:"schema"`
	Repo       string   `json:"repo"`
	Deployment string   `json:"deployment,omitempty"`
	Prefix     string   `json:"prefix"`
	Binary     string   `json:"binary"`
	Jobs       []string `json:"jobs"`
	Skipped    []string `json:"skipped,omitempty"`
	SQL        string   `json:"sql"`

	// Apply-path fields. Zero-valued when --apply was not set.
	Applied         bool   `json:"applied,omitempty"`
	PGConnection    string `json:"pg_connection,omitempty"` // sanitised (no password)
	JobsInserted    int    `json:"jobs_inserted,omitempty"`
	JobsReplaced    int    `json:"jobs_replaced,omitempty"`
	ApplyDurationMS int64  `json:"apply_duration_ms,omitempty"`
}

// WriteText renders the emit-or-apply outcome to w — the structured summary
// in apply mode and the raw SQL in preview mode.
func (b timetableEmitBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Applied {
		// Apply mode: print the structured summary, NOT the SQL.
		// The SQL is still in the JSON body for audit purposes;
		// text mode shows what happened against the live DB.
		fmt.Fprintf(bw, "✓ pg_timetable jobs applied to %s\n", b.PGConnection)
		fmt.Fprintf(bw, "  Inserted:    %d\n", b.JobsInserted)
		if b.JobsReplaced > 0 {
			fmt.Fprintf(bw, "  Replaced:    %d (existing chains with prefix %q wiped)\n",
				b.JobsReplaced, b.Prefix)
		}
		fmt.Fprintf(bw, "  Duration:    %d ms\n", b.ApplyDurationMS)
		if len(b.Skipped) > 0 {
			fmt.Fprintf(bw, "  Skipped:     %s\n", strings.Join(b.Skipped, ", "))
		}
		fmt.Fprintln(bw, "  Re-run with --apply to refresh after editing the schedule list.")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	// Print mode: SQL on stdout for piping into psql.
	if len(b.Skipped) > 0 {
		fmt.Fprintf(bw, "-- Skipped jobs: %s\n", strings.Join(b.Skipped, ", "))
	}
	fmt.Fprintf(bw, "%s", b.SQL)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
