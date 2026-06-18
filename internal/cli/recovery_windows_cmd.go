// recovery_windows_cmd.go — CLI surface for the recovery-windows coverage report.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

// newRecoveryWindowsCmd implements `recovery windows <deployment>`
// — the PITR windows enumeration command.
func newRecoveryWindowsCmd() *cobra.Command {
	var (
		repoURL          string
		format           string
		includeOlderThan string
	)
	c := &cobra.Command{
		Use:   "windows <deployment>",
		Short: "List every PITR window available for a deployment",
		Long: `Walks every committed backup for the deployment and reports
its PITR window:

  - EarliestRestoreLSN = the backup's stop_lsn
  - LatestRestoreLSN   = highest archived WAL LSN on the
                         backup's timeline (empty if no WAL is
                         archived; PITR isn't possible past the
                         base in that case)
  - Gaps               = persisted gapstate records + manifest-
                         embedded wal_gaps overlapping the window

The --include-older-than flag bounds the walk: useful for
"show me windows from the last 30 days".

Output formats:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade GFM Markdown rendering.

Read-only; safe at any cadence.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecoveryWindows(cmd, args[0], recoveryWindowsFlags{
				repoURL:          repoURL,
				format:           format,
				includeOlderThan: includeOlderThan,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&format, "format", "json",
		"output format for the report body: json | markdown")
	c.Flags().StringVar(&includeOlderThan, "include-older-than", "",
		"limit the walk to backups within this duration (e.g. 30d, 90d); empty = all")
	return c
}

type recoveryWindowsFlags struct {
	repoURL          string
	format           string
	includeOlderThan string
}

func runRecoveryWindows(cmd *cobra.Command, deployment string, f recoveryWindowsFlags) error {
	d := DispatcherFrom(cmd)
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery windows: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	includeOlder, err := parseRecoveryDuration(f.includeOlderThan)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery windows: --include-older-than: %v", err)).Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	r, err := recovery.Windows(cmd.Context(), sp, deployment, recovery.WindowsOptions{
		Verifier:         verifier,
		IncludeOlderThan: includeOlder,
	})
	if err != nil {
		return output.NewError("recovery.windows_failed",
			fmt.Sprintf("recovery windows: %v", err)).Wrap(err)
	}
	r.URL = f.repoURL

	body := windowsReportBody{WindowsReport: r, format: f.format}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type windowsReportBody struct {
	*recovery.WindowsReport
	format string
}

// MarshalJSON emits the embedded recovery.WindowsReport so the JSON contract
// stays the domain v1 shape.
func (b windowsReportBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.WindowsReport)
}

// WriteText renders the windows report to w, choosing markdown when format
// is "markdown" and the compact summary otherwise.
func (b windowsReportBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return recovery.RenderWindowsMarkdown(w, b.WindowsReport)
	}
	return writeWindowsSummary(w, b.WindowsReport)
}

func writeWindowsSummary(w io.Writer, r *recovery.WindowsReport) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "recovery windows — %s/%s\n", r.URL, r.Deployment)
	fmt.Fprintf(bw, "  Windows: %d\n", r.Coverage.WindowCount)
	if !r.Coverage.EarliestRecoverableTime.IsZero() {
		fmt.Fprintf(bw, "  Earliest: %s\n", r.Coverage.EarliestRecoverableTime.Format(time.RFC3339))
		fmt.Fprintf(bw, "  Latest:   %s\n", r.Coverage.LatestRecoverableTime.Format(time.RFC3339))
	}
	if r.Coverage.WindowsWithGaps > 0 {
		fmt.Fprintf(bw, "  ✗ Windows with WAL gaps: %d (total %d bytes)\n",
			r.Coverage.WindowsWithGaps, r.Coverage.TotalGapBytes)
	}
	fmt.Fprintln(bw)
	if len(r.Windows) == 0 {
		fmt.Fprintln(bw, "(no windows)")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	for i, win := range r.Windows {
		walMark := "no-WAL"
		if win.HasArchivedWAL {
			walMark = "WAL→" + win.LatestRestoreLSN
		}
		gapMark := ""
		if g := len(win.Gaps) + len(win.WALGapsFromManifest); g > 0 {
			gapMark = fmt.Sprintf(" ✗gap×%d", g)
		}
		fmt.Fprintf(bw, "  %d. %s  TLI=%d  %s..%s  %s%s\n",
			i+1, win.BackupID, win.Timeline,
			win.EarliestRestoreLSN, win.LatestRestoreLSN,
			walMark, gapMark)
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full report.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
