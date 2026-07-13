// init.go — 'init' CLI verb: Day-0 setup wizard from fresh install to first verified backup.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newInitCmd implements the Day-0 setup wizard.
//
// The wizard's job is to connect "fresh install" to "first verified
// backup committed" in five minutes:
//
//  1. Probe the operator's PG (replication-mode connect, IDENTIFY_SYSTEM)
//  2. Initialise the repository (idempotent — repo.Init handles re-runs)
//  3. Generate the signing keypair on first run (keystore handles it)
//  4. Write pg_hardstorage.yaml with the deployment block
//  5. Optionally take the first backup right now
//  6. Print the recommended next steps (start the agent, set a
//     schedule, configure sinks)
//
// Modes:
//
//   - Interactive (default, requires a TTY): prompts for each value
//     with sensible defaults shown in brackets.
//
//   - Non-interactive (--yes plus enough flags): every prompt is
//     auto-answered from flags. Required: --pg-connection, --repo.
//     Optional: --deployment, --retention, --skip-backup.
//
//   - Quick (--quick): fast-track for single-host evaluation. Auto-detects
//     the local PostgreSQL socket at /var/run/postgresql or PGHOST/PGPORT,
//     uses a file:// repo in /var/backups/pg_hardstorage, encrypts by
//     default, takes a backup, and prints next steps. This is the
//     single-command "I want a backup running, now" mode.
//
// We refuse to run prompts when stdin isn't a terminal — never hang
// a CI pipeline waiting for input.
func newInitCmd() *cobra.Command {
	var opts initOpts
	c := &cobra.Command{
		Use:   "init",
		Short: "Day-0 setup wizard — connect, init repo, take first backup",
		Long: `Interactive setup wizard.

Walks through:
  1. PG connection probe
  2. Repository initialisation
  3. Signing keypair generation (first run only)
  4. Writing pg_hardstorage.yaml with one deployment configured
  5. Optionally taking the first backup right now

Non-interactive scripted mode: pass --yes plus --pg-connection and
--repo. Other prompts auto-answer from flags or accept their defaults.

Quick-start single command: pass --quick for auto-detection of a local
PostgreSQL 18 socket.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.pgConn, "pg-connection", "",
		"libpq connection string for PostgreSQL")
	c.Flags().StringVar(&opts.repoURL, "repo", "",
		"repository URL (file://, s3://, ...)")
	c.Flags().StringVar(&opts.deployment, "deployment", "db1",
		"name for this deployment")
	c.Flags().BoolVar(&opts.yes, "yes", false,
		"non-interactive mode: accept all defaults and skip prompts")
	c.Flags().BoolVar(&opts.quick, "quick", false,
		"auto-detect a local PostgreSQL socket, use a file:// repo, accept all defaults, take a backup — zero questions")
	c.Flags().BoolVar(&opts.skipBackup, "skip-backup", false,
		"skip taking the first backup (config-only init)")
	c.Flags().StringVar(&opts.scheduleBackup, "schedule-backup", "every 6h",
		"backup schedule expression (e.g. 'every 6h', 'daily_at 04:00', 'off')")
	c.Flags().StringVar(&opts.scheduleRotate, "schedule-rotate", "daily_at 04:00",
		"retention rotation schedule (or 'off')")
	c.Flags().BoolVar(&opts.encrypt, "encrypt", true,
		"generate a local KEK so future backups encrypt by default")
	return c
}

type initOpts struct {
	pgConn         string
	repoURL        string
	deployment     string
	yes            bool
	quick          bool
	skipBackup     bool
	scheduleBackup string
	scheduleRotate string
	encrypt        bool
}

// runInit is the wizard body. We deliberately keep the steps linear
// and easy to read top-to-bottom — the operator should be able to
// trace what just happened by reading the source if they're paranoid.
func runInit(cmd *cobra.Command, opts initOpts) error {
	d := DispatcherFrom(cmd)

	if opts.quick {
		opts.yes = true
		if opts.pgConn == "" {
			opts.pgConn = resolveQuickPGConn()
		}
		if opts.repoURL == "" {
			opts.repoURL = quickDefaultRepoURL()
		}
	}

	prompter := newPrompter(cmd.InOrStdin(), cmd.OutOrStderr(), opts.yes)

	emit := func(severity output.Severity, op string, body map[string]any) {
		_ = d.Event(cmd.Context(), output.NewEvent(severity, "init", op).WithBody(body))
	}

	// 1. Resolve paths.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	// 2. Gather inputs (interactive prompts or flags).
	pgConn, err := prompter.askLine("PostgreSQL connection (libpq URI)", opts.pgConn, validateNonEmpty)
	if err != nil {
		return err
	}
	repoURL, err := prompter.askLine("Repository URL (file:///, s3://, ...)", opts.repoURL, validateNonEmpty)
	if err != nil {
		return err
	}
	deployment, err := prompter.askLine("Deployment name", opts.deployment, validateNonEmpty)
	if err != nil {
		return err
	}

	// 3. Probe PG. Fast — fail early before we touch the repo.
	emit(output.SeverityInfo, "probe.starting", map[string]any{"deployment": deployment})
	identity, pgVersion, err := probeForInit(cmd.Context(), pgConn)
	if err != nil {
		return output.NewError("init.probe_failed",
			fmt.Sprintf("init: cannot probe PostgreSQL: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "verify the connection string, that the user has the REPLICATION attribute, and that pg_hba.conf permits replication from this host",
			}).Wrap(err)
	}
	emit(output.SeverityNotice, "probe.ok", map[string]any{
		"pg_version": pgVersion,
		"system_id":  identity.SystemID,
		"timeline":   identity.Timeline,
	})

	// 4. Initialise the repository. The wizard is idempotent on
	//    repo existence: a standalone `repo init` may have been
	//    run first, or this is a re-run of `init` itself. In
	//    either case fall through to repo.Open to load the
	//    existing metadata. (repo.Init itself is intentionally
	//    strict — it backs the `repo init` CLI which maps
	//    ErrAlreadyExists to the conflict.repo_exists exit code.)
	var repoID string
	initRes, err := repo.Init(cmd.Context(), repo.InitOptions{URL: repoURL})
	switch {
	case err == nil:
		repoID = initRes.ID
	case errors.Is(err, repo.ErrAlreadyExists):
		meta, _, openErr := repo.Open(cmd.Context(), repoURL)
		if openErr != nil {
			return output.NewError("init.repo_init_failed",
				fmt.Sprintf("init: repo exists but cannot open: %v", openErr)).Wrap(openErr)
		}
		repoID = meta.ID
	default:
		return output.NewError("init.repo_init_failed",
			fmt.Sprintf("init: initialise repo: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "pick a directory you can write to, e.g. --repo file://$HOME/pg_hardstorage-repo (or fix permissions on the chosen path) and re-run",
			}).Wrap(err)
	}
	emit(output.SeverityNotice, "repo.ready", map[string]any{
		"url":     repoURL,
		"repo_id": repoID,
	})

	// 5. Generate / load the signing keypair.
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("init.keystore_failed",
			fmt.Sprintf("init: signing key: %v", err)).Wrap(err)
	}
	_ = verifier // unused below; kept so the call's audit-trail intent is clear

	// 5b. Generate / load the KEK if encryption is requested. Default
	//     is encrypt=true (the SPEC's "encryption is on by default"
	//     posture); operators can override with --encrypt=false.
	wantEncrypt := opts.encrypt
	if !opts.yes {
		wantEncrypt = prompter.askYes("Generate a KEK so backups are encrypted?", opts.encrypt)
	}
	var kekConfig *runner.EncryptionConfig
	kekGenerated := false
	if wantEncrypt {
		kek, generated, err := keystore.LoadOrGenerateKEK(p.Keyring.Value)
		if err != nil {
			return output.NewError("init.kek_failed",
				fmt.Sprintf("init: load/generate KEK: %v", err)).Wrap(err)
		}
		kekGenerated = generated
		kekConfig = &runner.EncryptionConfig{KEK: kek, KEKRef: keystore.KEKRefLocal}
		emit(output.SeverityNotice, "kek.ready", map[string]any{
			"path":      filepath.Join(p.Keyring.Value, keystore.KEKFileName),
			"generated": generated,
		})
	}

	// 6. Write the config (additive merge: existing deployments
	//    preserved, this one upserted).
	if err := writeInitConfig(p, deployment, pgConn, repoURL, opts.scheduleBackup, opts.scheduleRotate); err != nil {
		return err
	}
	emit(output.SeverityNotice, "config.written", map[string]any{
		"deployment": deployment,
	})

	// 7. Optionally take the first backup.
	var firstBackup *runner.Result
	if !opts.skipBackup && prompter.askYes("Take a backup right now?", true) {
		emit(output.SeverityInfo, "backup.starting", map[string]any{"deployment": deployment})
		res, err := runner.Take(cmd.Context(), runner.TakeOptions{
			PGConnString: pgConn,
			RepoURL:      repoURL,
			Deployment:   deployment,
			Signer:       signer,
			Verifier:     verifier,
			Fast:         true, // operator is watching; CHECKPOINT now
			Encryption:   kekConfig,
		})
		if err != nil {
			return output.NewError("init.first_backup_failed",
				fmt.Sprintf("init: first backup: %v", err)).Wrap(err)
		}
		firstBackup = res
		emit(output.SeverityNotice, "backup.ok", map[string]any{
			"backup_id":   res.BackupID,
			"duration_ms": res.Duration.Milliseconds(),
		})
	}

	// 8. Final result document. Operator-readable; structured for
	//    JSON consumers.
	body := initResultBody{
		Deployment:     deployment,
		PGConnection:   pgConn,
		RepoURL:        repoURL,
		PGVersion:      pgVersion,
		SystemID:       identity.SystemID,
		Timeline:       uint32(identity.Timeline),
		ConfigPath:     initConfigPath(p),
		KeyringPath:    p.Keyring.Value,
		FirstBackup:    shapeFirstBackup(firstBackup),
		ScheduleBackup: opts.scheduleBackup,
		ScheduleRotate: opts.scheduleRotate,
		Encryption:     wantEncrypt,
		KEKGenerated:   kekGenerated,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// probeForInit opens a replication-mode connection to validate the
// DSN, runs IDENTIFY_SYSTEM, and probes the PG major version (via a
// regular-mode connection — replication-mode can't run SHOW).
func probeForInit(ctx context.Context, dsn string) (pg.SystemIdentity, int, error) {
	repl, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return pg.SystemIdentity{}, 0, fmt.Errorf("open replication connection: %w", err)
	}
	defer repl.Close(ctx)
	identity, err := pg.IdentifySystem(ctx, repl)
	if err != nil {
		return pg.SystemIdentity{}, 0, fmt.Errorf("IDENTIFY_SYSTEM: %w", err)
	}

	regular, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return identity, 0, fmt.Errorf("open regular connection: %w", err)
	}
	defer regular.Close(ctx)
	v, err := pg.QueryVersion(ctx, regular)
	if err != nil {
		// Probe failure is non-fatal for the wizard — we have
		// IDENTIFY_SYSTEM already; PG version is informational.
		return identity, 0, nil
	}
	return identity, v.Major, nil
}

// writeInitConfig merges the new deployment into the existing
// pg_hardstorage.yaml (or creates it). Existing deployments and
// sinks are preserved; the new deployment with this name is upserted.
//
// We deliberately re-emit the FULL config so the file is always
// canonical and human-readable, rather than appending fragments.
func writeInitConfig(p *paths.Paths, deployment, pgConn, repoURL, schedBackup, schedRotate string) error {
	loaded, err := config.Load(p)
	if err != nil {
		return output.NewError("init.config_parse_failed",
			fmt.Sprintf("init: parse existing config: %v", err)).Wrap(err)
	}
	cfg := config.Config{}
	if loaded != nil {
		cfg = loaded.Config
	}
	if cfg.Schema == "" {
		cfg.Schema = config.Schema
	}
	if cfg.Deployments == nil {
		cfg.Deployments = map[string]config.DeploymentConfig{}
	}

	// Preserve any existing extras for this deployment (tenant,
	// retention overrides) — we only set fields the wizard owns.
	dep := cfg.Deployments[deployment]
	dep.PGConnection = pgConn
	dep.Repo = repoURL
	dep.Schedule.Backup = parseSchedExpr(schedBackup)
	dep.Schedule.Rotate = parseSchedExpr(schedRotate)
	cfg.Deployments[deployment] = dep

	body, err := config.Marshal(&cfg)
	if err != nil {
		return output.NewError("init.config_marshal_failed", err.Error()).Wrap(err)
	}
	configPath := initConfigPath(p)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return output.NewError("init.config_dir_failed",
			fmt.Sprintf("init: mkdir %s: %v", filepath.Dir(configPath), err)).Wrap(err)
	}
	// fsutil.WriteFileAtomic: the config file is the entire output
	// of the wizard — losing it after the agent prints "✓ done"
	// would silently strand the operator with a half-set-up
	// deployment.
	if err := fsutil.WriteFileAtomic(configPath, body, 0o600); err != nil {
		return output.NewError("init.config_write_failed",
			fmt.Sprintf("init: write %s: %v", configPath, err)).Wrap(err)
	}
	return nil
}

// initConfigPath is the canonical config-file location for `init`.
func initConfigPath(p *paths.Paths) string {
	return filepath.Join(p.Config.Value, "pg_hardstorage.yaml")
}

// parseSchedExpr maps wizard-friendly shorthand to a config.ScheduleSpec.
// Accepts:
//
//	"off"            (zero spec — task disabled)
//	"every <dur>"    -> Spec.Every
//	"daily_at HH:MM" -> Spec.DailyAt
//
// Anything else is treated as Every (operators typing a bare "6h"
// get the natural interpretation). The grammar is intentionally
// forgiving; the strict validator runs at agent-start.
func parseSchedExpr(s string) config.ScheduleSpec {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "off") {
		return config.ScheduleSpec{}
	}
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "every ") {
		return config.ScheduleSpec{Every: strings.TrimSpace(s[len("every "):])}
	}
	if strings.HasPrefix(low, "daily_at ") {
		return config.ScheduleSpec{DailyAt: strings.TrimSpace(s[len("daily_at "):])}
	}
	if strings.HasPrefix(low, "at ") {
		return config.ScheduleSpec{At: strings.TrimSpace(s[len("at "):])}
	}
	return config.ScheduleSpec{Every: s}
}

// shapeFirstBackup pulls a small subset of runner.Result for the
// init Result body. We intentionally don't expose every field —
// `pg_hardstorage list <deployment>` is the canonical surface for
// detailed backup metadata.
func shapeFirstBackup(r *runner.Result) *firstBackupSummary {
	if r == nil {
		return nil
	}
	return &firstBackupSummary{
		BackupID:     r.BackupID,
		LogicalBytes: r.LogicalBytes,
		PrimaryKey:   r.PrimaryKey,
		DurationMS:   r.Duration.Milliseconds(),
	}
}

// initResultBody is the wizard's exit document.
type initResultBody struct {
	Deployment     string              `json:"deployment"`
	PGConnection   string              `json:"pg_connection"`
	RepoURL        string              `json:"repo_url"`
	PGVersion      int                 `json:"pg_version,omitempty"`
	SystemID       string              `json:"system_id"`
	Timeline       uint32              `json:"timeline"`
	ConfigPath     string              `json:"config_path"`
	KeyringPath    string              `json:"keyring_path"`
	FirstBackup    *firstBackupSummary `json:"first_backup,omitempty"`
	ScheduleBackup string              `json:"schedule_backup,omitempty"`
	ScheduleRotate string              `json:"schedule_rotate,omitempty"`
	Encryption     bool                `json:"encryption_enabled"`
	KEKGenerated   bool                `json:"kek_generated"`
}

type firstBackupSummary struct {
	BackupID     string `json:"backup_id"`
	LogicalBytes int64  `json:"logical_bytes"`
	PrimaryKey   string `json:"primary_key"`
	DurationMS   int64  `json:"duration_ms"`
}

// WriteText renders the init body as the human-friendly summary the
// SPEC's Day-0 example calls out.
func (b initResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "✓ pg_hardstorage initialized")
	fmt.Fprintf(bw, "  Deployment:   %s\n", b.Deployment)
	if b.PGVersion > 0 {
		fmt.Fprintf(bw, "  PostgreSQL:   %d\n", b.PGVersion)
	}
	fmt.Fprintf(bw, "  Cluster ID:   %s\n", b.SystemID)
	fmt.Fprintf(bw, "  Timeline:     %d\n", b.Timeline)
	fmt.Fprintf(bw, "  Repository:   %s\n", b.RepoURL)
	fmt.Fprintf(bw, "  Config:       %s\n", b.ConfigPath)
	fmt.Fprintf(bw, "  Keyring:      %s\n", b.KeyringPath)
	if b.Encryption {
		if b.KEKGenerated {
			fmt.Fprintf(bw, "  Encryption:   AES-256-GCM (KEK generated at %s/kek.bin)\n", b.KeyringPath)
		} else {
			fmt.Fprintf(bw, "  Encryption:   AES-256-GCM (using existing KEK)\n")
		}
	} else {
		fmt.Fprintf(bw, "  Encryption:   off\n")
	}
	if b.FirstBackup != nil {
		fmt.Fprintln(bw, "")
		fmt.Fprintf(bw, "  First backup: %s\n", b.FirstBackup.BackupID)
		fmt.Fprintf(bw, "    logical:    %s\n", humanBytes(b.FirstBackup.LogicalBytes))
		fmt.Fprintf(bw, "    duration:   %dms\n", b.FirstBackup.DurationMS)
	}
	fmt.Fprintln(bw, "")
	fmt.Fprintln(bw, "Next steps:")
	fmt.Fprintf(bw, "  1. Start the agent (drives scheduled backups + retention):\n")
	fmt.Fprintf(bw, "       pg_hardstorage agent\n")
	fmt.Fprintf(bw, "  2. Continuously archive WAL for PITR (connection + repo come from the config init just wrote):\n")
	fmt.Fprintf(bw, "       pg_hardstorage wal stream %s\n", b.Deployment)
	fmt.Fprintf(bw, "  3. Inspect deployment health:\n")
	fmt.Fprintf(bw, "       pg_hardstorage doctor %s", b.Deployment)
	_, err := io.WriteString(w, bw.String())
	return err
}

// prompter encapsulates the interactive-or-not input strategy.
//
// In --yes mode every prompt resolves to its default (or to the
// flag-supplied value) without touching stdin.
type prompter struct {
	r          *bufio.Reader
	w          io.Writer
	autoAccept bool
}

func newPrompter(in io.Reader, w io.Writer, autoAccept bool) *prompter {
	return &prompter{r: bufio.NewReader(in), w: w, autoAccept: autoAccept}
}

// askLine prompts for a free-form string. dflt is shown in [brackets].
// validate may reject the input and re-prompt; nil means "any non-
// empty answer." In --yes mode we use dflt without asking; if dflt
// is empty AND no value is required, that's an error.
func (p *prompter) askLine(label, dflt string, validate func(string) error) (string, error) {
	if p.autoAccept {
		if dflt == "" {
			return "", output.NewError("init.missing_input",
				fmt.Sprintf("init: %q is required in --yes mode", label)).Wrap(output.ErrUsage)
		}
		if validate != nil {
			if err := validate(dflt); err != nil {
				return "", output.NewError("init.bad_default",
					fmt.Sprintf("init: default for %q failed validation: %v", label, err)).Wrap(output.ErrUsage)
			}
		}
		return dflt, nil
	}
	for {
		hint := ""
		if dflt != "" {
			hint = " [" + dflt + "]"
		}
		fmt.Fprintf(p.w, "  ? %s%s: ", label, hint)
		line, err := p.r.ReadString('\n')
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return "", err
		}
		line = strings.TrimSpace(line)
		// EOF with no pending input: stdin is closed (Ctrl-D, or a
		// script/CI pipe with no answers). Re-prompting would spin a
		// tight busy-loop forever, flooding the terminal — abort with
		// a structured error instead.
		if eof && line == "" && dflt == "" {
			fmt.Fprintln(p.w)
			return "", output.NewError("init.stdin_closed",
				fmt.Sprintf("init: stdin closed while waiting for %q — cannot prompt", label)).
				WithSuggestion(&output.Suggestion{
					Human: "run init interactively, or supply the answers via flags (--pg-connection, --repo, --deployment) with --yes for non-interactive use",
				}).Wrap(output.ErrUsage)
		}
		if line == "" {
			line = dflt
		}
		if validate != nil {
			if err := validate(line); err != nil {
				fmt.Fprintf(p.w, "    %v\n", err)
				if eof {
					// Nothing more will arrive; don't loop on a
					// failed validation of the same dead input.
					return "", output.NewError("init.stdin_closed",
						fmt.Sprintf("init: stdin closed and the last input for %q failed validation", label)).
						Wrap(output.ErrUsage)
				}
				continue
			}
		}
		return line, nil
	}
}

// askYes prompts with [Y/n] / [y/N] depending on dflt. In --yes
// mode returns dflt without asking.
func (p *prompter) askYes(label string, dflt bool) bool {
	if p.autoAccept {
		return dflt
	}
	hint := "[y/N]"
	if dflt {
		hint = "[Y/n]"
	}
	fmt.Fprintf(p.w, "  ? %s %s: ", label, hint)
	line, _ := p.r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return dflt
	}
	return line == "y" || line == "yes"
}

// quickDefaultRepoURL picks the --quick default repository location.
// Root gets the traditional system path; everyone else gets a
// user-writable directory — the previous hardcoded
// /var/backups/pg_hardstorage made "zero questions" --quick fail with
// a permission error for every non-root evaluator.
func quickDefaultRepoURL() string {
	if os.Geteuid() == 0 {
		return "file:///var/backups/pg_hardstorage"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return "file://" + filepath.Join(home, ".local", "share", "pg_hardstorage", "repo")
	}
	// No resolvable home (rare: stripped env) — fall back to CWD.
	return "file://" + filepath.Join(mustGetwd(), "pg_hardstorage-repo")
}

// mustGetwd returns the working directory or "." — never panics; the
// caller only builds a default suggestion path from it.
func mustGetwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// validateNonEmpty rejects empty strings.
func validateNonEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}

// resolveQuickPGConn auto-detects a local PostgreSQL 18 connection from
// the unix socket directory or PGHOST/PGPORT when --quick is set. Falls
// back to the standard UNIX socket at /var/run/postgresql/ if nothing
// is configured.
func resolveQuickPGConn() string {
	if host := os.Getenv("PGHOST"); host != "" {
		port := os.Getenv("PGPORT")
		if port == "" {
			port = "5432"
		}
		return fmt.Sprintf("postgres://%s:%s/?sslmode=disable", host, port)
	}
	sockDir := "/var/run/postgresql"
	if os.Getenv("PGHOST") == "" {
		if _, err := os.Stat(sockDir); err == nil {
			return fmt.Sprintf("postgres:///?host=%s&sslmode=disable", sockDir)
		}
	}
	return "postgres:///?sslmode=disable&host=/var/run/postgresql"
}
