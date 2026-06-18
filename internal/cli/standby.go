// standby.go — CLI surface for provisioning and managing PG standby instances.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/standby"
)

// newRealStandbyCmd implements `pg_hardstorage standby
// <create|list|destroy>`.
//
// What ships in v0.1:
//   - Provision a standby data dir from a backup (restore + write
//     standby.signal + restore_command pointing at our wal-fetch shim).
//   - Track standbys in a state file under paths.State()/standbys.json.
//   - Destroy clears the entry and (with --remove-target) wipes the
//     data dir.
//
// What v0.1 does NOT do:
//   - Start the PG process. The Result body emits the recommended
//     systemd / pg_ctl invocation; the operator owns the lifecycle.
//   - Promote, fail over, or coordinate with the source. Standby is
//     a read-only follower; promotion is `pg_promote()` initiated by
//     the operator.
func newRealStandbyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "standby <create|list|destroy>",
		Short: "Hot-standby restore — read-only replica fed entirely from the backup pipeline",
		Long: `Manage hot-standby PostgreSQL instances fed by the backup pipeline.

A standby is a configured PGDATA dir with standby.signal +
restore_command pointing at our 'wal fetch' shim. PG starts in
recovery mode, applies the restored backup, and continuously pulls
new WAL from the repository — without touching the source PG.

v0.1 provisions the data dir; the operator starts PG via systemd
or pg_ctl. The Result body emits the recommended invocation.

The state file lives under paths.State()/standbys.json — same
back-compat commitment (24-month) as the manifest schema.`,
	}
	c.AddCommand(
		newStandbyCreateCmd(),
		newStandbyListCmd(),
		newStandbyDestroyCmd(),
	)
	return c
}

func newStandbyCreateCmd() *cobra.Command {
	var (
		repoURL    string
		backupID   string
		target     string
		force      bool
		deployment string
	)
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Provision a hot-standby data dir from a backup",
		Long: `Restore the named backup into --target, configure standby.signal
+ restore_command, and record the standby in the state file.

The operator starts PG separately:

  systemd-run --user --unit pg_hardstorage-standby-<name> \
              postgres -D <target>

or with a stock systemd PG package:

  pg_ctl -D <target> start

PG comes up in hot-standby mode — readable, never auto-promoted.
Promote with pg_promote() inside PG when the operator decides to
cut over.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStandbyCreate(cmd, standbyCreateOptions{
				name:       args[0],
				repoURL:    repoURL,
				backupID:   backupID,
				target:     target,
				force:      force,
				deployment: deployment,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&deployment, "deployment", "",
		"source deployment name (required)")
	_ = c.MarkFlagRequired("deployment")
	c.Flags().StringVar(&backupID, "backup", "latest",
		"backup ID, or `latest` for the most recent committed backup")
	c.Flags().StringVar(&target, "target", "",
		"target data directory — must be empty unless --force (required)")
	_ = c.MarkFlagRequired("target")
	c.Flags().BoolVar(&force, "force", false,
		"permit writing into a non-empty --target")
	return c
}

type standbyCreateOptions struct {
	name       string
	repoURL    string
	backupID   string
	target     string
	force      bool
	deployment string
}

func runStandbyCreate(cmd *cobra.Command, opts standbyCreateOptions) error {
	d := DispatcherFrom(cmd)

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}

	mgr, err := managerForCmd()
	if err != nil {
		return err
	}

	res, err := mgr.Create(cmd.Context(), standby.CreateOptions{
		Name:           opts.name,
		Deployment:     opts.deployment,
		RepoURL:        opts.repoURL,
		BackupID:       opts.backupID,
		TargetDir:      opts.target,
		Verifier:       verifier,
		KEKForRef:      resolveKEKForVerify, // shared with verify --full
		UnwrapDEK:      resolveDEKForVerify, // cloud-KMS DEK unwrap (issue #102)
		AllowOverwrite: opts.force,
	})
	if err != nil {
		return mapStandbyError("standby create", err)
	}

	binPath, _ := os.Executable()
	body := standbyCreateBody{
		Standby:    res,
		StartCmd:   fmt.Sprintf("pg_ctl -D %s start", res.TargetDir),
		StatusCmd:  fmt.Sprintf("pg_isready -h %s", filepath.Dir(res.TargetDir)),
		FetchUsing: binPath,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newStandbyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List recorded standbys",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := managerForCmd()
			if err != nil {
				return err
			}
			out, err := mgr.List()
			if err != nil {
				return output.NewError("standby.list_failed",
					fmt.Sprintf("standby list: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(standbyListBody{Standbys: out}))
		},
	}
}

func newStandbyDestroyCmd() *cobra.Command {
	var removeTarget bool
	c := &cobra.Command{
		Use:          "destroy <name>",
		Short:        "Remove a standby from the state file (data dir kept by default)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := managerForCmd()
			if err != nil {
				return err
			}
			if err := mgr.Destroy(cmd.Context(), args[0], standby.DestroyOptions{
				RemoveTargetDir: removeTarget,
			}); err != nil {
				return mapStandbyError("standby destroy", err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(standbyDestroyBody{
				Name:          args[0],
				RemovedTarget: removeTarget,
			}))
		},
	}
	c.Flags().BoolVar(&removeTarget, "remove-target", false,
		"also rm -rf the data directory")
	return c
}

// --- helpers ---------------------------------------------------------

func managerForCmd() (*standby.Manager, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("paths.resolve_failed",
			fmt.Sprintf("standby: resolve paths: %v", err)).Wrap(err)
	}
	state := filepath.Join(p.State.String(), "standbys.json")
	bin, _ := os.Executable() // best-effort; "pg_hardstorage" fallback below
	if bin == "" {
		bin = "pg_hardstorage"
	}
	return standby.NewManager(state, bin), nil
}

func mapStandbyError(op string, err error) error {
	switch {
	case errors.Is(err, standby.ErrAlreadyExists):
		return output.NewError("conflict.standby_exists",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	case errors.Is(err, standby.ErrNotFound):
		return output.NewError("notfound.standby",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	}
	return output.NewError("standby.failed",
		fmt.Sprintf("%s: %v", op, err)).Wrap(err)
}

// --- bodies ----------------------------------------------------------

type standbyCreateBody struct {
	Standby    *standby.Standby `json:"standby"`
	StartCmd   string           `json:"start_cmd"`
	StatusCmd  string           `json:"status_cmd"`
	FetchUsing string           `json:"fetch_using"`
}

// WriteText renders the new standby's metadata plus follow-up commands as
// human-readable text to w.
func (b standbyCreateBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ standby %s provisioned\n", b.Standby.Name)
	fmt.Fprintf(bw, "  Deployment:  %s\n", b.Standby.Deployment)
	fmt.Fprintf(bw, "  Backup:      %s\n", b.Standby.BackupID)
	fmt.Fprintf(bw, "  Target dir:  %s\n", b.Standby.TargetDir)
	fmt.Fprintf(bw, "  PG version:  %d\n", b.Standby.PGVersion)
	fmt.Fprintf(bw, "  Created at:  %s\n", b.Standby.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "\nNext step — start PG:\n  %s\n", b.StartCmd)
	fmt.Fprintf(bw, "\nThe standby will keep applying WAL via:\n  %s wal fetch %s ... --repo %s\n",
		b.FetchUsing, b.Standby.Deployment, b.Standby.RepoURL)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type standbyListBody struct {
	Standbys []standby.Standby `json:"standbys"`
}

// WriteText renders the standby list as a tabular summary to w.
func (b standbyListBody) WriteText(w io.Writer) error {
	if len(b.Standbys) == 0 {
		_, err := io.WriteString(w, "no standbys recorded")
		return err
	}
	bw := &strings.Builder{}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEPLOYMENT\tBACKUP\tTARGET\tCREATED")
	for _, s := range b.Standbys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Deployment, s.BackupID, s.TargetDir,
			s.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type standbyDestroyBody struct {
	Name          string `json:"name"`
	RemovedTarget bool   `json:"removed_target"`
}

// WriteText renders the destroy confirmation, noting whether the data
// directory was removed, as a single-line summary to w.
func (b standbyDestroyBody) WriteText(w io.Writer) error {
	state := "(data dir kept)"
	if b.RemovedTarget {
		state = "(data dir removed)"
	}
	_, err := fmt.Fprintf(w, "✓ standby %s destroyed %s", b.Name, state)
	return err
}
