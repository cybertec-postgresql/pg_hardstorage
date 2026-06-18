// db_extension.go — CLI surface for installing the in-DB pg_hardstorage SQL views.
package cli

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/dbext"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newDbCmd implements `pg_hardstorage db <install-extension|...>`.
//
// Today the only subcommand is `install-extension` — the SPEC's
// "in-database SQL views" item.  The command runs the bundled
// extension SQL against a connected PostgreSQL cluster, exposing
// `pg_hardstorage.backups`, `.health`, `.rpo` views plus the
// upsert helpers the agent uses to refresh them.
//
// We intentionally do NOT depend on libpq directly here — we
// shell out to `psql` (which is already a build dependency of
// every operator's PG environment) and pipe the SQL on stdin.
// This avoids:
//
//   - Pulling in pgx / lib/pq as a CLI-time dependency.
//   - Reimplementing PG client-side authentication
//     (.pgpass, peer, trust, GSS, ...) — psql already does it.
//   - Producing a binary that's harder to FIPS-build.
//
// The trade-off is that operators need psql on the PATH.  In
// practice every PG host has it; managed-PG users (RDS, Cloud
// SQL) already invoke psql for setup.
func newDbCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "db <install-extension|uninstall-extension>",
		Short: "In-database integration (SQL views, upsert helpers)",
	}
	c.AddCommand(
		newDbInstallExtensionCmd(),
		newDbUninstallExtensionCmd(),
	)
	return c
}

func newDbInstallExtensionCmd() *cobra.Command {
	var (
		pgConn   string
		dryRun   bool
		printSQL bool
	)
	c := &cobra.Command{
		Use:   "install-extension",
		Short: "Install the pg_hardstorage in-DB extension (creates schema + tables + views + functions)",
		Long: `Runs the bundled extension SQL against the target PostgreSQL
cluster.  Creates:

  - the pg_hardstorage schema
  - the *_state authoritative tables
  - the .backups / .health / .rpo views
  - the upsert_* helper functions
  - the pg_hardstorage_writer role

Idempotent: re-running is safe (CREATE OR REPLACE semantics).

The command shells out to ` + "`psql`" + ` and pipes the SQL on stdin.
` + "`psql`" + ` must be on the PATH; every PG host has it.

Use --dry-run to print what would be executed without
applying it; --print-sql to write the embedded SQL to stdout
(useful for piping to a different tool).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			sql := dbext.InlineSQL()

			if printSQL {
				_, err := io.WriteString(cmd.OutOrStdout(), sql)
				return err
			}

			// Flag-gated: --pg-connection is needed only for a live run.
			if pgConn == "" && !dryRun {
				return missingFlagErr(cmd, "--pg-connection (or use --dry-run / --print-sql)")
			}
			if dryRun {
				return d.Result(output.NewResult(cmd.CommandPath()).WithBody(dbExtInstallBody{
					DryRun:   true,
					BytesSQL: len(sql),
					Schema:   dbext.SchemaName,
					Views:    dbext.ViewNames,
					Version:  dbext.Version,
				}))
			}

			ps, err := exec.LookPath("psql")
			if err != nil {
				return output.NewError("db.psql_not_found",
					"db install-extension: `psql` not on PATH — install postgresql-client (or use --print-sql to apply manually)").Wrap(err)
			}
			pcmd := exec.CommandContext(cmd.Context(), ps, "-v", "ON_ERROR_STOP=1", "-d", pgConn, "-X", "-q", "-1")
			pcmd.Stdin = strings.NewReader(sql)
			pcmd.Stderr = cmd.ErrOrStderr()
			if err := pcmd.Run(); err != nil {
				return output.NewError("db.install_failed",
					fmt.Sprintf("db install-extension: psql exec: %v", err)).
					WithSuggestion(&output.Suggestion{
						Human:   "verify the connection string + role permissions; rerun with --dry-run to confirm the SQL is well-formed",
						Command: "pg_hardstorage db install-extension --print-sql",
					}).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(dbExtInstallBody{
				BytesSQL: len(sql),
				Schema:   dbext.SchemaName,
				Views:    dbext.ViewNames,
				Version:  dbext.Version,
			}))
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the target PostgreSQL cluster")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"don't apply; report what would be installed")
	c.Flags().BoolVar(&printSQL, "print-sql", false,
		"print the embedded SQL to stdout (no PG connection required)")
	return c
}

func newDbUninstallExtensionCmd() *cobra.Command {
	var (
		pgConn   string
		dropData bool
	)
	c := &cobra.Command{
		Use:   "uninstall-extension",
		Short: "Remove the pg_hardstorage in-DB schema",
		Long: `Drops the pg_hardstorage schema (CASCADE) and the
pg_hardstorage_writer role.  --drop-data is REQUIRED to
acknowledge that committed backup metadata (backups_state,
health_state, rpo_state) will be deleted; without it the
command refuses.

This is the inverse of install-extension.  It does not touch
the repo — backups themselves are unaffected.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			if err := requireFlags(cmd, "pg-connection"); err != nil {
				return err
			}
			if !dropData {
				return output.NewError("usage.confirmation_required",
					"db uninstall-extension: --drop-data is REQUIRED to acknowledge that backup metadata will be removed").
					WithSuggestion(&output.Suggestion{
						Human: "this only removes the in-DB views — repo backups are unaffected; pass --drop-data if that's what you want",
					}).Wrap(output.ErrUsage)
			}

			ps, err := exec.LookPath("psql")
			if err != nil {
				return output.NewError("db.psql_not_found",
					"db uninstall-extension: `psql` not on PATH").Wrap(err)
			}
			drop := `DROP SCHEMA IF EXISTS pg_hardstorage CASCADE;
DROP ROLE IF EXISTS pg_hardstorage_writer;
`
			pcmd := exec.CommandContext(cmd.Context(), ps, "-v", "ON_ERROR_STOP=1", "-d", pgConn, "-X", "-q", "-1")
			pcmd.Stdin = strings.NewReader(drop)
			pcmd.Stderr = cmd.ErrOrStderr()
			if err := pcmd.Run(); err != nil {
				return output.NewError("db.uninstall_failed",
					fmt.Sprintf("db uninstall-extension: psql exec: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(dbExtUninstallBody{
				Schema: dbext.SchemaName,
			}))
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the target PostgreSQL cluster")
	c.Flags().BoolVar(&dropData, "drop-data", false,
		"REQUIRED — acknowledge that pg_hardstorage's in-DB tables will be dropped")
	return c
}

type dbExtInstallBody struct {
	DryRun   bool     `json:"dry_run,omitempty"`
	BytesSQL int      `json:"bytes_sql"`
	Schema   string   `json:"schema"`
	Views    []string `json:"views"`
	Version  string   `json:"version"`
}

// WriteText renders the install result — schema, version, and exposed views —
// as human-readable text to w.
func (b dbExtInstallBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "✓ db install-extension"
	if b.DryRun {
		verb = "✓ db install-extension --dry-run (no changes applied)"
	}
	fmt.Fprintf(bw, "%s\n", verb)
	fmt.Fprintf(bw, "  Schema:    %s\n", b.Schema)
	fmt.Fprintf(bw, "  Version:   %s\n", b.Version)
	fmt.Fprintf(bw, "  Views:     %s\n", strings.Join(b.Views, ", "))
	fmt.Fprintf(bw, "  SQL bytes: %d", b.BytesSQL)
	if !b.DryRun {
		fmt.Fprintf(bw, "\n  Note:      query `SELECT * FROM pg_hardstorage.backups` from any role with USAGE on the schema")
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type dbExtUninstallBody struct {
	Schema string `json:"schema"`
}

// WriteText renders the uninstall confirmation as a single-line summary to w.
func (b dbExtUninstallBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ db uninstall-extension — schema %s dropped (repo backups unaffected)", b.Schema)
	return err
}
