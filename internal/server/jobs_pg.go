// jobs_pg.go — PGBackend: PostgreSQL-backed JobBackend (FOR UPDATE SKIP LOCKED for multi-instance HA).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSchemaVersion is the on-disk schema version. Stored in
// phs.schema_meta so a future migration ladder can detect
// the current version and apply the right upgrades.
const PGSchemaVersion = 1

// pgSchemaDDL is the bootstrap DDL applied on backend Open. Idempotent
// (every CREATE uses IF NOT EXISTS) so the operator can safely run
// multiple control-plane instances against the same database; only
// the first one's CREATE actually does work, the rest are no-ops.
//
// Schema is `phs` (the project abbreviation), keeping the bookkeeping
// in its own namespace so a single PG can host pg_hardstorage data
// alongside other application data without name collisions, and so
// `SELECT FROM phs.jobs` is the documented psql query operators
// reach for when triaging.
//
// We can NOT use the project's full name (`pg_hardstorage`) as the
// schema because PostgreSQL reserves the `pg_` prefix for system
// schemas and rejects user-created ones with SQLSTATE 42939
// ("unacceptable schema name").  The extension at
// ext/pg_hardstorage_extension/ uses `pg_hardstorage` as its schema
// because PG's CREATE EXTENSION mechanism allows it; raw CREATE
// SCHEMA does not.
const pgSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS phs;

CREATE TABLE IF NOT EXISTS phs.schema_meta (
    schema_name   TEXT PRIMARY KEY,
    version       INTEGER NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS phs.jobs (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    deployment      TEXT NOT NULL,
    repo_url        TEXT NOT NULL DEFAULT '',
    args            JSONB,
    state           TEXT NOT NULL,
    assigned_to     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    progress        JSONB NOT NULL DEFAULT '[]'::jsonb,
    result          JSONB,
    failure         TEXT NOT NULL DEFAULT ''
);

-- claim() picks the oldest queued job matching agent's deployment +
-- kind filters; this index makes that walk an index scan + LIMIT 1
-- + FOR UPDATE SKIP LOCKED.
CREATE INDEX IF NOT EXISTS jobs_state_created
    ON phs.jobs (state, created_at);

-- per-deployment list filters (the operator's "what's queued for
-- db1?" query) ride this index.
CREATE INDEX IF NOT EXISTS jobs_deployment_state
    ON phs.jobs (deployment, state);

-- SweepAbandoned scans Running jobs by StartedAt; without this
-- index a fleet with thousands of completed jobs would scan all of
-- them every sweep interval.
CREATE INDEX IF NOT EXISTS jobs_running_started
    ON phs.jobs (started_at)
    WHERE state = 'running';
`

// PGBackend is the PostgreSQL-backed JobBackend. It holds a pgxpool
// (connection pool) — multi-control-plane HA works because:
//
//   - Claim uses FOR UPDATE SKIP LOCKED, so two control planes
//     racing for the same queued job get different rows (or one
//     gets nothing).
//   - The schema's primary key + state column are the source of
//     truth; in-memory caches on individual control planes don't
//     diverge.
//   - Connection pooling handles the per-control-plane fan-in
//     without us building a custom pool.
//
// Schema bootstrap runs at Open. Subsequent Open calls (e.g. a
// second control plane joining) are no-ops because every CREATE
// uses IF NOT EXISTS.
type PGBackend struct {
	pool *pgxpool.Pool
}

// OpenPGBackend connects to the supplied DSN, runs the schema
// bootstrap, and returns a ready backend. Caller closes via the
// returned backend's Close().
func OpenPGBackend(ctx context.Context, dsn string) (*PGBackend, error) {
	if dsn == "" {
		return nil, errors.New("pgbackend: empty DSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgbackend: connect: %w", err)
	}
	if err := bootstrapPGSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &PGBackend{pool: pool}, nil
}

// bootstrapPGSchema applies the DDL + records the version.
func bootstrapPGSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, pgSchemaDDL); err != nil {
		return fmt.Errorf("pgbackend: bootstrap DDL: %w", err)
	}
	// Record version. ON CONFLICT DO UPDATE so a future migration
	// can bump it; the ladder lands here.
	_, err := pool.Exec(ctx, `
        INSERT INTO phs.schema_meta (schema_name, version)
        VALUES ('jobs', $1)
        ON CONFLICT (schema_name) DO UPDATE
            SET version = EXCLUDED.version,
                applied_at = now()
            WHERE schema_meta.version <> EXCLUDED.version
    `, PGSchemaVersion)
	if err != nil {
		return fmt.Errorf("pgbackend: record schema version: %w", err)
	}
	return nil
}

// Close implements JobBackend.
func (b *PGBackend) Close() error {
	if b.pool != nil {
		b.pool.Close()
	}
	return nil
}

// Pool exposes the underlying pgxpool for tests + advanced operators
// who want to run their own SQL against the schema (e.g. an out-of-
// band backfill, or a bespoke retention sweep). Production callers
// should go through the JobBackend methods — this is an escape hatch.
func (b *PGBackend) Pool() *pgxpool.Pool { return b.pool }

// Enqueue implements JobBackend.
func (b *PGBackend) Enqueue(ctx context.Context, opts EnqueueOptions) (*Job, error) {
	if opts.Kind == "" {
		return nil, errors.New("jobs: Kind is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("jobs: Deployment is required")
	}
	now := time.Now().UTC()
	id := newJobID()
	args, err := jsonOrNull(opts.Args)
	if err != nil {
		return nil, err
	}
	_, err = b.pool.Exec(ctx, `
        INSERT INTO phs.jobs
            (id, kind, deployment, repo_url, args, state, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
    `, id, string(opts.Kind), opts.Deployment, opts.RepoURL, args, string(JobQueued), now)
	if err != nil {
		return nil, fmt.Errorf("pgbackend: insert: %w", err)
	}
	return b.Get(ctx, id)
}

// Get implements JobBackend.
func (b *PGBackend) Get(ctx context.Context, id string) (*Job, error) {
	row := b.pool.QueryRow(ctx, `
        SELECT id, kind, deployment, repo_url, args,
               state, assigned_to, created_at, updated_at,
               started_at, completed_at, progress, result, failure
        FROM phs.jobs
        WHERE id = $1
    `, id)
	j, err := scanJob(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("pgbackend: get: %w", err)
	}
	return j, nil
}

// List implements JobBackend.
func (b *PGBackend) List(ctx context.Context, opts ListOptions) ([]Job, error) {
	// Build the WHERE incrementally. Avoids string concat for the
	// non-filter case and keeps the parameter list aligned with $N
	// placeholders.
	var (
		where []string
		args  []any
	)
	if opts.State != "" {
		args = append(args, string(opts.State))
		where = append(where, fmt.Sprintf("state = $%d", len(args)))
	}
	if opts.Kind != "" {
		args = append(args, string(opts.Kind))
		where = append(where, fmt.Sprintf("kind = $%d", len(args)))
	}
	if opts.Deployment != "" {
		args = append(args, opts.Deployment)
		where = append(where, fmt.Sprintf("deployment = $%d", len(args)))
	}
	q := `
        SELECT id, kind, deployment, repo_url, args,
               state, assigned_to, created_at, updated_at,
               started_at, completed_at, progress, result, failure
        FROM phs.jobs
    `
	if len(where) > 0 {
		q += "WHERE " + strings.Join(where, " AND ") + "\n"
	}
	q += "ORDER BY created_at DESC"
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := b.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgbackend: list: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("pgbackend: list scan: %w", err)
		}
		out = append(out, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgbackend: list rows: %w", err)
	}
	return out, nil
}

// Claim implements JobBackend. The whole reason this backend exists:
// FOR UPDATE SKIP LOCKED on the inner SELECT means two control
// planes racing for the same queued job get different rows. The
// query is one round-trip, so claim latency under contention is
// roughly one network hop + the WHERE-index scan cost.
func (b *PGBackend) Claim(ctx context.Context, opts ClaimOptions) (*Job, error) {
	if opts.AgentID == "" {
		return nil, errors.New("jobs: AgentID is required for claim")
	}
	deployments := opts.Deployments
	if deployments == nil {
		deployments = []string{}
	}
	kinds := make([]string, len(opts.Kinds))
	for i, k := range opts.Kinds {
		kinds[i] = string(k)
	}
	now := time.Now().UTC()

	// Uncapped: a single-statement claim, no serialization needed.
	if opts.MaxConcurrent <= 0 {
		return b.claimRow(ctx, b.pool, opts, deployments, kinds, now)
	}

	// Capped: the running-count check ($5) and the claim must be atomic
	// w.r.t. other claims, or two control planes can each observe a
	// pre-claim count below the cap and BOTH admit, overshooting it
	// (race-condition audit #5). Serialize capped claims with a
	// transaction-scoped advisory lock so the count is always current.
	// The cap is global, so a single global lock key suffices; it
	// auto-releases at commit, and claims are infrequent enough that
	// serializing the decision is cheap.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgbackend: claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", claimAdvisoryLockKey); err != nil {
		return nil, fmt.Errorf("pgbackend: claim: advisory lock: %w", err)
	}
	j, err := b.claimRow(ctx, tx, opts, deployments, kinds, now)
	if err != nil {
		return nil, err // rolled back by defer; no rows changed on ErrNoJobs
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("pgbackend: claim: commit: %w", err)
	}
	return j, nil
}

// claimAdvisoryLockKey is the transaction-advisory-lock key that
// serializes concurrency-capped claims into a hard global cap.
const claimAdvisoryLockKey int64 = 0x7068735f6a6f62 // "phs_job"

// pgQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so the claim
// statement can run either standalone (uncapped) or inside the advisory-
// locked transaction (capped).
type pgQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// claimRow runs the two-stage claim UPDATE: the inner SELECT picks the
// oldest matching queued row (FOR UPDATE SKIP LOCKED so concurrent claims
// get different rows) only while the running count is below the cap ($5);
// the outer UPDATE transitions it; RETURNING yields the row in one
// round-trip.
func (b *PGBackend) claimRow(ctx context.Context, q pgQuerier, opts ClaimOptions, deployments, kinds []string, now time.Time) (*Job, error) {
	row := q.QueryRow(ctx, `
        UPDATE phs.jobs
           SET state = 'running',
               assigned_to = $1,
               started_at = $2,
               updated_at = $2
         WHERE id = (
             SELECT id FROM phs.jobs
              WHERE state = 'queued'
                AND ($3::text[] = '{}'::text[] OR deployment = ANY($3))
                AND ($4::text[] = '{}'::text[] OR kind = ANY($4))
                AND ($5 <= 0 OR (
                    SELECT count(*) FROM phs.jobs WHERE state = 'running'
                ) < $5)
              ORDER BY created_at
              LIMIT 1
              FOR UPDATE SKIP LOCKED
         )
        RETURNING id, kind, deployment, repo_url, args,
                  state, assigned_to, created_at, updated_at,
                  started_at, completed_at, progress, result, failure
    `, opts.AgentID, now, deployments, kinds, opts.MaxConcurrent)
	j, err := scanJob(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoJobs
		}
		return nil, fmt.Errorf("pgbackend: claim: %w", err)
	}
	return j, nil
}

// AppendProgress implements JobBackend.
func (b *PGBackend) AppendProgress(ctx context.Context, id string, ev ProgressEvent) error {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("pgbackend: marshal progress: %w", err)
	}
	// jsonb || jsonb appends; we cast the new event to a single-
	// element array to match. The state guard is a CASE that flips
	// to NULL on mismatch, which the trailing WHERE filters out.
	cmd, err := b.pool.Exec(ctx, `
        UPDATE phs.jobs
           SET progress = progress || $1::jsonb,
               updated_at = $2
         WHERE id = $3
           AND state = 'running'
    `, "["+string(body)+"]", ev.At, id)
	if err != nil {
		return fmt.Errorf("pgbackend: append progress: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		// Either the job doesn't exist or it's not running; do a
		// follow-up Get to disambiguate so the caller gets the
		// right sentinel.
		j, err := b.Get(ctx, id)
		if err != nil {
			return err // ErrJobNotFound on missing
		}
		_ = j
		return ErrJobNotRunning
	}
	return nil
}

// Complete implements JobBackend. Idempotent: re-completing a
// terminal job returns the existing row.
func (b *PGBackend) Complete(ctx context.Context, id string, opts CompleteOptions) (*Job, error) {
	now := time.Now().UTC()
	state := JobCompleted
	failure := ""
	if !opts.Success {
		state = JobFailed
		failure = opts.Failure
		if failure == "" {
			failure = "agent reported failure with no message"
		}
	}
	result, err := jsonOrNull(opts.Result)
	if err != nil {
		return nil, err
	}
	row := b.pool.QueryRow(ctx, `
        UPDATE phs.jobs
           SET state = CASE WHEN state IN ('completed', 'failed', 'cancelled') THEN state ELSE $1 END,
               result = CASE WHEN state IN ('completed', 'failed', 'cancelled') THEN result ELSE $2 END,
               failure = CASE WHEN state IN ('completed', 'failed', 'cancelled') THEN failure ELSE $3 END,
               completed_at = COALESCE(completed_at, $4),
               updated_at = $4
         WHERE id = $5
        RETURNING id, kind, deployment, repo_url, args,
                  state, assigned_to, created_at, updated_at,
                  started_at, completed_at, progress, result, failure
    `, string(state), result, failure, now, id)
	j, err := scanJob(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("pgbackend: complete: %w", err)
	}
	// Claim-fence (race-condition audit #3): the CASE above preserves a
	// terminal state, so a SUCCESS report that comes back Failed/Cancelled
	// means our claim was reclaimed (swept abandoned) or cancelled while
	// we ran. Surface it rather than report a phantom success.
	if opts.Success && (j.State == JobFailed || j.State == JobCancelled) {
		return nil, ErrClaimLost
	}
	return j, nil
}

// Cancel implements JobBackend.
func (b *PGBackend) Cancel(ctx context.Context, id, reason string) (*Job, error) {
	now := time.Now().UTC()
	failure := "cancelled: " + reason
	row := b.pool.QueryRow(ctx, `
        UPDATE phs.jobs
           SET state = CASE WHEN state IN ('completed', 'failed', 'cancelled') THEN state ELSE 'cancelled' END,
               failure = CASE WHEN state IN ('completed', 'failed', 'cancelled') THEN failure ELSE $1 END,
               completed_at = COALESCE(completed_at, $2),
               updated_at = $2
         WHERE id = $3
        RETURNING id, kind, deployment, repo_url, args,
                  state, assigned_to, created_at, updated_at,
                  started_at, completed_at, progress, result, failure
    `, failure, now, id)
	j, err := scanJob(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("pgbackend: cancel: %w", err)
	}
	return j, nil
}

// SweepAbandoned implements JobBackend. Single-statement reclamation:
// the WHERE filters by state + liveness; the SET transitions to failed.
//
// Liveness keys on updated_at — the LAST time the agent reported activity
// (every AppendProgress bumps it) — NOT started_at. Keying on started_at
// reclaimed any job simply running longer than the deadline, even one
// posting progress every second: a >6h backup of a large cluster was
// declared "abandoned" while healthy, a second agent then claimed the
// same job (duplicate concurrent backup), and the original's Complete
// failed with ErrClaimLost — discarding hours of finished work. updated_at
// reclaims only jobs whose agent genuinely STOPPED reporting (which the
// failure message already claims), regardless of how long the job runs.
func (b *PGBackend) SweepAbandoned(ctx context.Context, deadline time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-deadline)
	cmd, err := b.pool.Exec(ctx, `
        UPDATE phs.jobs
           SET state = 'failed',
               failure = $1,
               completed_at = now() AT TIME ZONE 'UTC',
               updated_at   = now() AT TIME ZONE 'UTC'
         WHERE state = 'running'
           AND started_at IS NOT NULL
           AND updated_at < $2
    `, fmt.Sprintf("abandoned: agent stopped reporting (claim deadline %s elapsed)", deadline), cutoff)
	if err != nil {
		return 0, fmt.Errorf("pgbackend: sweep: %w", err)
	}
	return int(cmd.RowsAffected()), nil
}

// scanJob unmarshals one row into a Job. Both pgx.Row and pgx.Rows
// satisfy the same subset of the interface (Scan); the helper's
// receiver is whichever the caller has.
func scanJob(row interface {
	Scan(...any) error
}) (*Job, error) {
	var (
		j           Job
		argsBody    []byte
		progBody    []byte
		resultBody  []byte
		startedAt   *time.Time
		completedAt *time.Time
		state       string
		kind        string
	)
	if err := row.Scan(
		&j.ID, &kind, &j.Deployment, &j.RepoURL, &argsBody,
		&state, &j.AssignedTo, &j.CreatedAt, &j.UpdatedAt,
		&startedAt, &completedAt, &progBody, &resultBody, &j.Failure,
	); err != nil {
		return nil, err
	}
	j.Kind = JobKind(kind)
	j.State = JobState(state)
	j.StartedAt = startedAt
	j.CompletedAt = completedAt
	if len(argsBody) > 0 {
		if err := json.Unmarshal(argsBody, &j.Args); err != nil {
			return nil, fmt.Errorf("pgbackend: unmarshal args: %w", err)
		}
	}
	if len(progBody) > 0 {
		if err := json.Unmarshal(progBody, &j.Progress); err != nil {
			return nil, fmt.Errorf("pgbackend: unmarshal progress: %w", err)
		}
	}
	if len(resultBody) > 0 {
		if err := json.Unmarshal(resultBody, &j.Result); err != nil {
			return nil, fmt.Errorf("pgbackend: unmarshal result: %w", err)
		}
	}
	return &j, nil
}

// jsonOrNull marshals m to JSON, or returns nil when m is empty so
// the column stores SQL NULL rather than `{}` — distinguishes
// "operator passed an empty body" from "operator passed nothing."
func jsonOrNull(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}
