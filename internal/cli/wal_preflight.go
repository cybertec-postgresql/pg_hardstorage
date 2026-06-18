// wal_preflight.go — 'wal preflight' CLI verb + in-process preflight gate before `wal stream`.
package cli

// wal_preflight.go — `pg_hardstorage wal preflight <deployment>`
// and the in-process preflight that runs before `wal stream`.
//
// Preflight is the operator-facing primitive that catches WAL-
// streamer misconfiguration BEFORE we open a replication
// connection.  Three callers:
//
//   - `wal preflight <deployment>` — standalone validation, used
//     in setup runbooks and CI gates.
//   - `wal stream <deployment>` — gates the stream startup on a
//     clean preflight unless --skip-preflight is set.
//   - external automation that wants the structured findings —
//     all callers go through replication.Preflight, which returns
//     PreflightFinding values that can be JSON-rendered verbatim.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

// newWalPreflightCmd implements `pg_hardstorage wal preflight
// <deployment>`.  Standalone equivalent of the gate that runs
// inside `wal stream` — operators call it before configuring a
// streamer to confirm the PG instance is ready, and CI pipelines
// gate deployment promotions on a clean preflight.
func newWalPreflightCmd() *cobra.Command {
	var opts walPreflightOptions
	c := &cobra.Command{
		Use:   "preflight <deployment>",
		Short: "Validate PostgreSQL configuration for WAL streaming",
		Long: `Run the WAL-stream readiness checks against the source PostgreSQL.

Checks (fatal first):
  - wal_level >= replica
  - max_replication_slots > current count
  - max_wal_senders > current active count
  - the connecting role has the REPLICATION attribute (when --role given)
  - max_slot_wal_keep_size > 0 (warning: caps slot retention, can lose WAL)
  - max_slot_wal_keep_size unbounded (info: surfaces the disk-fill risk)
  - idle_replication_slot_timeout = 0 (warning, PG 17+)

Exits 0 when no fatal findings.  Use --json for the structured form
suitable for piping into automation.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.deployment = args[0]
			return runWalPreflight(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.pgConn, "pg-connection", "",
		"libpq connection string for the source PostgreSQL (required)")
	_ = c.MarkFlagRequired("pg-connection")
	c.Flags().StringVar(&opts.role, "role", "",
		"role the streamer connects as; if empty, the role is inferred from --pg-connection (`user=` parameter)")
	return c
}

type walPreflightOptions struct {
	deployment string
	pgConn     string
	role       string
}

func runWalPreflight(cmd *cobra.Command, opts walPreflightOptions) error {
	d := DispatcherFrom(cmd)

	role := opts.role
	if role == "" {
		role = inferRoleFromDSN(opts.pgConn)
	}

	res, err := runPreflight(cmd.Context(), opts.pgConn, role, walStreamAppName(opts.deployment))
	if err != nil {
		return output.NewError("wal.preflight_failed",
			fmt.Sprintf("wal preflight: %v", err)).Wrap(err)
	}

	body := walPreflightResultBody{
		Deployment:   opts.deployment,
		Role:         role,
		PgVersionNum: res.PgVersionNum,
		Findings:     res.Findings,
	}
	if res.HasFatal() {
		// Surface as an error so the exit code is non-zero, but
		// keep the structured result attached so JSON consumers
		// see all the findings.
		return output.NewError("wal.preflight_fatal",
			"wal preflight: at least one check failed (see findings)").
			WithSuggestion(&output.Suggestion{
				Human: "address each fatal finding's `suggestion` field, then re-run",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// walPreflightResultBody is the JSON shape `wal preflight` returns.
// Mirrors the structure of replication.PreflightResult plus the
// scenario-level metadata the operator wants in the same payload.
type walPreflightResultBody struct {
	Deployment   string                         `json:"deployment"`
	Role         string                         `json:"role,omitempty"`
	PgVersionNum int                            `json:"pg_version_num,omitempty"`
	Findings     []replication.PreflightFinding `json:"findings"`
}

// runPreflight opens a regular-mode connection and runs
// replication.Preflight.  Lifted out of the cobra closure so
// `wal stream` can call it on the same path.
func runPreflight(ctx context.Context, dsn, role, appName string) (*replication.PreflightResult, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return nil, fmt.Errorf("open regular conn: %w", err)
	}
	defer c.Close(ctx)
	return replication.Preflight(ctx, c, role, appName)
}

// walStreamAppName is the application_name the WAL streamer's
// replication connection uses — identical to the replication-slot
// name, so a single identifier is what an operator would put in
// synchronous_standby_names. PG slot/identifier rules are [a-z0-9_],
// so deployment hyphens are translated to underscores.
func walStreamAppName(deployment string) string {
	return "pg_hardstorage_" + strings.ReplaceAll(deployment, "-", "_")
}

// inferRoleFromDSN returns the `user=` value from the DSN, or the
// userinfo from a URL-form connection string, or empty when
// neither is present.  Used so `wal preflight` and the in-stream
// preflight can default the role check without forcing the
// operator to repeat the username on the CLI.
//
// Supports both libpq forms:
//   - keyword form: `host=... user=foo dbname=...`
//   - URI form:     `postgres://foo@host/db`
func inferRoleFromDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		if u.User != nil {
			return u.User.Username()
		}
		return ""
	}
	for _, kv := range strings.Fields(dsn) {
		if strings.HasPrefix(kv, "user=") {
			return strings.TrimPrefix(kv, "user=")
		}
	}
	return ""
}
