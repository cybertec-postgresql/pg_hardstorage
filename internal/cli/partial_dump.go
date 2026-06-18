// partial_dump.go — CLI surface for SQL-dumping per-table data from a backup via a sandbox PG.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial/sandbox"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// newPartialDumpCmd implements `pg_hardstorage partial dump`. Closes
// the operator's "last shell command" that the partial-restore
// workflow has had since it shipped: the binary now does the full
// pipeline (full restore → start sandbox PG → pg_dump → cleanup).
//
// Why this lives at `partial dump` instead of extending `partial
// restore`: the existing `partial restore` is the FILE-extraction
// half (selective heap files into a target dir, no PG instance
// involved). `partial dump` is the SQL-extraction half — needs a
// running PG, runs pg_dump, emits SQL. Different output shapes
// (PGDATA vs SQL stream), different operational requirements
// (PG binaries on PATH), so two commands.
//
// Operator-facing shape:
//
//	pg_hardstorage partial dump db1 \
//	    --repo s3://acme/backups \
//	    --backup latest \
//	    --tables public.users,public.events \
//	    --output dump.sql
//
// The flow:
//  1. `pg_hardstorage restore` into a temp dir (full restore).
//  2. Spin up a local PG against that dir (unix socket, no TCP).
//  3. pg_dump --table=... and stream SQL to --output (or stdout).
//  4. pg_ctl stop, cleanup the temp dir.
//
// Requires `pg_ctl` and `pg_dump` on PATH. The CLI pre-flights this
// before doing the (potentially long) restore.
func newPartialDumpCmd() *cobra.Command {
	var (
		repoURL          string
		backupID         string
		tables           string
		outFile          string
		database         string
		dataOnly         bool
		stagingDir       string
		skipVersionCheck bool
		kmsConfig        map[string]string
	)
	c := &cobra.Command{
		Use:   "dump <deployment>",
		Short: "Restore + start sandbox PG + pg_dump SQL for selected tables",
		Long: `Run a full pipeline:
  1. Restore the backup into a temp directory.
  2. Start a local sandbox PG instance against that data dir
     (unix socket only — no TCP, no port collisions).
  3. Run pg_dump for the requested tables.
  4. Stop PG and remove the temp dir.

The SQL is written to --sql-file (or stdout if --sql-file is empty).
Operator-friendly redirection: pipe stdout into a file or psql
directly. (--sql-file avoids shadowing the global -o/--output JSON
output flag; the SQL stream is logically separate from the
structured Result envelope which still rides on stderr/stdout per
-o.)

--data-only emits only INSERT/COPY statements (no DDL).

Requires pg_ctl and pg_dump on PATH (use --staging-dir to choose
where the temporary restore lives — default is system temp).

This is the SQL-emitting half of partial restore the plan
documented as deferred. It pairs with ` + "`partial restore`" + `
(file-extraction half) — operators who want SQL run dump; those
who want a PGDATA tree run restore.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPartialDump(cmd, partialDumpFlags{
				deployment:       args[0],
				repoURL:          repoURL,
				backupID:         backupID,
				tables:           tables,
				output:           outFile,
				database:         database,
				dataOnly:         dataOnly,
				stagingDir:       stagingDir,
				skipVersionCheck: skipVersionCheck,
				kmsConfig:        kmsConfig,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&backupID, "backup", "latest",
		"backup ID, or `latest`")
	c.Flags().StringVar(&tables, "tables", "",
		"comma-separated qualified tables (required, e.g. public.users,public.events)")
	c.Flags().StringVar(&database, "database", "postgres",
		"database containing the requested tables; pg_dump connects to exactly one database, so a table in another database needs its name here (issue #97)")
	c.Flags().StringVar(&outFile, "sql-file", "",
		"write the dumped SQL here; empty streams to stdout (named --sql-file to avoid shadowing the global --output flag)")
	c.Flags().BoolVar(&dataOnly, "data-only", false,
		"pass --data-only to pg_dump (no DDL, just INSERT/COPY)")
	c.Flags().StringVar(&stagingDir, "staging-dir", "",
		"directory the temporary restore lives under (default: system temp)")
	c.Flags().StringToStringVar(&kmsConfig, "kms-config", nil,
		"cloud KMS provider config for a cloud-KMS-encrypted backup (e.g. region=eu-central-1,endpoint=...); empty uses ambient credentials")
	c.Flags().BoolVar(&skipVersionCheck, "skip-version-check", false,
		"bypass the data-dir vs pg_ctl major-version pre-flight (only for operators running heterogeneous fleets where compatibility was validated some other way)")
	return c
}

type partialDumpFlags struct {
	deployment       string
	repoURL          string
	backupID         string
	tables           string
	output           string
	database         string
	dataOnly         bool
	stagingDir       string
	skipVersionCheck bool
	kmsConfig        map[string]string
}

func runPartialDump(cmd *cobra.Command, f partialDumpFlags) error {
	d := DispatcherFrom(cmd)
	tlist := splitCommaTrim(f.tables)
	if len(tlist) == 0 {
		return output.NewError("usage.missing_flag",
			"partial dump: --tables is required (comma-separated, qualified)").
			Wrap(output.ErrUsage)
	}

	// Pre-flight: pg_ctl and pg_dump on PATH. We do this BEFORE the
	// (potentially long) restore so an operator missing the binaries
	// hits a fast clear error.
	pgCtl, pgDump, err := sandbox.DiscoverPGTools()
	if err != nil {
		return output.NewError("preflight.pg_tools_missing",
			fmt.Sprintf("partial dump: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "install postgresql client tools (pg_ctl + pg_dump) on the agent host; on Debian/Ubuntu: apt install postgresql-client",
			}).Wrap(err)
	}

	// 1. Restore the backup into a staging dir we own + clean up.
	stagingRoot := f.stagingDir
	if stagingRoot == "" {
		stagingRoot = os.TempDir()
	}
	stagingDir, err := os.MkdirTemp(stagingRoot, "pg_hardstorage-dump-*")
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("partial dump: mkdir staging: %v", err)).Wrap(err)
	}
	// Clean up unconditionally — even on success the staging dir
	// is transient. PG insists on data-dir mode 0700.
	defer os.RemoveAll(stagingDir)
	if err := os.Chmod(stagingDir, 0o700); err != nil {
		return output.NewError("internal",
			fmt.Sprintf("partial dump: chmod staging: %v", err)).Wrap(err)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	// Resolve --backup latest if needed.
	backupID := f.backupID
	if backupID == "" || backupID == "latest" {
		_, sp, err := openRepo(cmd.Context(), f.repoURL)
		if err != nil {
			return err
		}
		store := backup.NewManifestStore(sp)
		latest, err := resolveLatestBackupID(cmd.Context(), store, f.deployment, verifier)
		sp.Close()
		if err != nil {
			return output.NewError("notfound.backup",
				fmt.Sprintf("partial dump: %v", err)).Wrap(err)
		}
		backupID = latest
	}

	// Restore into the staging dir. Suppress events in JSON mode
	// so the output is one Result document.
	suppressEvents := d.Renderer().Name() == "json"
	rres, err := restore.Restore(cmd.Context(), restore.Options{
		RepoURL:        f.repoURL,
		Deployment:     f.deployment,
		BackupID:       backupID,
		TargetDir:      stagingDir,
		Verifier:       verifier,
		AllowOverwrite: true, // staging dir is empty but PG creates a few files at start
		KEKForRef:      keystore.KEKResolver(p.Keyring.Value),
		UnwrapDEK:      keystore.DEKResolver(p.Keyring.Value, stringMapToAny(f.kmsConfig)),
		OnEvent: func(e *output.Event) {
			if suppressEvents {
				return
			}
			_ = d.Event(cmd.Context(), e)
		},
	})
	if err != nil {
		return output.NewError("partial.dump_restore_failed",
			fmt.Sprintf("partial dump: restore step: %v", err)).Wrap(err)
	}

	// 2. Spin up the sandbox. The data-dir-vs-binary major-version
	// pre-flight runs inside Start; --skip-version-check is the
	// operator-facing escape hatch for heterogeneous fleets.
	sb, err := sandbox.Start(cmd.Context(), sandbox.Options{
		DataDir:          stagingDir,
		PGCtlPath:        pgCtl,
		PGDumpPath:       pgDump,
		Database:         f.database,
		SkipVersionCheck: f.skipVersionCheck,
	})
	if err != nil {
		// Surface the version-mismatch case with a structured code
		// + actionable suggestion so the operator gets the fix
		// without reading the sandbox package source.
		if errors.Is(err, sandbox.ErrPGVersionMismatch) {
			// preflight.* namespace → ExitPreflight (per output/exitcode.go).
			// This is genuinely a pre-flight check — the operator's
			// host doesn't have the binary set the backup needs —
			// not a runtime backup-state failure.
			return output.NewError("preflight.pg_version_mismatch",
				fmt.Sprintf("partial dump: %v", err)).
				WithSuggestion(&output.Suggestion{
					Human:   "install the matching PostgreSQL major version (the one the backup was taken from), or pass --skip-version-check if you've validated compatibility some other way",
					Command: "apt install postgresql-<major>",
				}).
				Wrap(err)
		}
		return output.NewError("partial.dump_sandbox_failed",
			fmt.Sprintf("partial dump: sandbox start: %v", err)).Wrap(err)
	}
	defer func() {
		// Stop with a separate context so a cancelled cmd.Context
		// doesn't leave PG running (pg_ctl stop -m fast is idempotent
		// and shouldn't take long).
		stopCtx, cancel := context.WithTimeout(context.Background(), sandbox.DefaultShutdownTimeout)
		defer cancel()
		_ = sb.Stop(stopCtx)
	}()

	// 3. pg_dump.
	startedDump := time.Now()
	var sqlBytes int64
	dumpDest, dumpClose, err := openDumpOutput(f.output)
	if err != nil {
		return err
	}
	// dumpClose is responsible for fsync(file)+SyncDir(parent) when
	// dumpDest is a real file; for stdout it's a no-op.  The deferred
	// best-effort close here covers the error-return paths; the
	// happy path calls it explicitly so we can surface a sync error.
	defer dumpClose()
	counter := &countingWriter{w: dumpDest}
	if err := sb.Dump(cmd.Context(), counter, tlist, f.dataOnly); err != nil {
		return output.NewError("partial.dump_pg_dump_failed",
			fmt.Sprintf("partial dump: pg_dump: %v (sandbox PG log: %s)",
				err, sb.LogFile())).Wrap(err)
	}
	if err := dumpClose(); err != nil {
		return output.NewError("partial.dump_sync_failed",
			fmt.Sprintf("partial dump: fsync output: %v", err)).Wrap(err)
	}
	sqlBytes = counter.n

	// Empty-dump guard (issue #97).  pg_dump always emits a header
	// preamble for any real table, so zero bytes means it matched no
	// tables — almost always because the requested table lives in a
	// database other than --database.  Older pg_dump builds exit 0 in
	// this case and we'd otherwise report success while leaving an
	// empty SQL file on disk.  Fail loudly and remove the empty file so
	// the operator isn't left with a silent, misleading artefact.
	if sqlBytes == 0 {
		dbName := f.database
		if dbName == "" {
			dbName = "postgres"
		}
		if f.output != "" && f.output != "-" {
			_ = os.Remove(f.output)
		}
		return output.NewError("partial.dump_no_tables",
			fmt.Sprintf("partial dump: pg_dump produced no output — table(s) %s not found in database %q",
				strings.Join(tlist, ", "), dbName)).
			WithSuggestion(&output.Suggestion{
				Human: "the requested table lives in a different database; pass --database <name> (partial dump connects to a single database, default \"postgres\")",
			})
	}

	body := partialDumpBody{
		Schema:       "pg_hardstorage.partial.dump.v1",
		Deployment:   f.deployment,
		BackupID:     rres.BackupID,
		Tables:       tlist,
		Output:       f.output,
		BytesEmitted: sqlBytes,
		DataOnly:     f.dataOnly,
		DurationMS:   time.Since(startedDump).Milliseconds(),
		StagingDir:   stagingDir,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// openDumpOutput returns the writer for the SQL stream + closes it
// when the run ends. Empty path returns os.Stdout (which we don't
// close). Path = "-" is also stdout (cron-friendly redirection).
// openDumpOutput returns the writer for the SQL stream + a closer
// that performs fsync(file) + SyncDir(parent) before close.  The
// closer is idempotent — call it from both the happy path (to
// surface a sync error) and a deferred best-effort path.  For
// stdout it's a no-op closer.
func openDumpOutput(path string) (io.Writer, func() error, error) {
	noop := func() error { return nil }
	if path == "" || path == "-" {
		return os.Stdout, noop, nil
	}
	// MkdirAll on the parent so operators don't have to pre-create
	// dirs. Then create-or-truncate the file.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, output.NewError("internal",
				fmt.Sprintf("partial dump: mkdir output parent: %v", err)).Wrap(err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, output.NewError("internal",
			fmt.Sprintf("partial dump: open output: %v", err)).Wrap(err)
	}
	var closed bool
	closer := func() error {
		if closed {
			return nil
		}
		closed = true
		// Sync data; close; sync parent dir.  fsutil.SyncFile +
		// SyncDir guarantee the operator's SQL dump survives a
		// crash immediately after the command prints "✓ done".
		if syncErr := fsutil.SyncFile(f); syncErr != nil {
			_ = f.Close()
			return syncErr
		}
		if closeErr := f.Close(); closeErr != nil {
			return closeErr
		}
		return fsutil.SyncDir(filepath.Dir(path))
	}
	return f, closer, nil
}

// countingWriter wraps an io.Writer and tracks bytes seen. Used so
// the result body reports SQL size without having to re-read the
// output file.
type countingWriter struct {
	w io.Writer
	n int64
}

// Write forwards p to the underlying writer and accumulates the byte total.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// partialDumpBody is the v1-stable Result body.
type partialDumpBody struct {
	Schema       string   `json:"schema"`
	Deployment   string   `json:"deployment"`
	BackupID     string   `json:"backup_id"`
	Tables       []string `json:"tables"`
	Output       string   `json:"output,omitempty"`
	BytesEmitted int64    `json:"bytes_emitted"`
	DataOnly     bool     `json:"data_only,omitempty"`
	DurationMS   int64    `json:"duration_ms"`
	StagingDir   string   `json:"staging_dir,omitempty"`
}

// WriteText renders the dump outcome — output destination, byte size, and
// duration — as human-readable text to w.
func (b partialDumpBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	dest := b.Output
	if dest == "" {
		dest = "<stdout>"
	}
	fmt.Fprintf(bw, "partial dump — %s/%s\n", b.Deployment, b.BackupID)
	fmt.Fprintf(bw, "  Tables:        %s\n", strings.Join(b.Tables, ", "))
	fmt.Fprintf(bw, "  Output:        %s\n", dest)
	fmt.Fprintf(bw, "  Bytes emitted: %s\n", humanBytes(b.BytesEmitted))
	if b.DataOnly {
		fmt.Fprintln(bw, "  Mode:          --data-only")
	}
	fmt.Fprintf(bw, "  Duration:      %d ms\n", b.DurationMS)
	fmt.Fprintln(bw, "  ✓ sandbox PG cleaned up; staging dir removed")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
