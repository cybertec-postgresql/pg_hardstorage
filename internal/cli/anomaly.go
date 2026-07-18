// anomaly.go — 'anomaly check' CLI verb: flags outliers against backup-size/duration baselines.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/anomaly"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newAnomalyCmd implements `pg_hardstorage anomaly check <deployment>`.
//
// The plan calls anomaly detection out as a bonus feature: "Anomaly
// detection on backup size/duration/page-churn baselines." This is
// the skeleton — the math primitive ships in
// `internal/anomaly`, the CLI wraps it around the existing
// ManifestStore so an operator can ask "is my latest backup
// unusual?" against the rolling baseline of priors.
//
// Operator-facing shape:
//
//	pg_hardstorage anomaly check db1 \
//	    --repo s3://acme-pg-backups \
//	    [--threshold 3.0] [--window 10] [--min-samples 3]
//	    [--all]
//
// Default mode scores ONLY the latest backup against its priors —
// the natural cron-driven check ("did the most recent backup look
// weird?"). `--all` scores every backup in the deployment against
// its rolling-window predecessors, useful for one-time historical
// audits and for backfilling an anomaly story for a pre-existing
// fleet.
//
// Findings flip the exit code to ExitVerifyFailed (9) so cron-wired
// runs alarm when a backup looks unusual. An audit-chain entry is
// appended on every flag, matching the pattern repo-scrub uses for
// integrity findings.
func newAnomalyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "anomaly <subcommand>",
		Short: "Inspect backup metric baselines for outliers",
		Long: `anomaly compares each backup's logical bytes, duration, file
count, and unique-chunk count against the rolling baseline of
same-type prior backups for the same deployment. A z-score above
the threshold (default 3.0) flags a metric as unusual.

Subcommands:
  check <deployment>   Score the latest (or all, with --all) backup(s).

The math is intentionally simple: rolling mean + sample stddev over
the most-recent N priors of the same backup type, no seasonal
adjustment. Backup metrics aren't seasonal in any meaningful way
(a 04:00 backup and a 16:00 backup should look the same), and
clever forecasting here is just an opportunity to be wrong.

Findings flip the exit code to 9 (ExitVerifyFailed) so cron-driven
checks alarm. An audit-chain entry is written on every flag —
bit-rot-style: rare enough to be signal, not noise.`,
	}
	c.AddCommand(newAnomalyCheckCmd())
	return c
}

func newAnomalyCheckCmd() *cobra.Command {
	var (
		repoURL    string
		threshold  float64
		window     int
		minSamples int
		all        bool
	)
	c := &cobra.Command{
		Use:          "check <deployment>",
		Short:        "Score the latest backup (or all backups with --all) against priors",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnomalyCheck(cmd, args[0], repoURL, threshold, window, minSamples, all)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().Float64Var(&threshold, "threshold", anomaly.DefaultThreshold,
		"|z-score| above which a metric is flagged (default 3.0 = three-sigma)")
	c.Flags().IntVar(&window, "window", anomaly.DefaultWindow,
		"rolling-window size — number of most-recent same-type priors used for the baseline")
	c.Flags().IntVar(&minSamples, "min-samples", anomaly.DefaultMinSamples,
		"refuse to score when fewer than N same-type priors are available")
	c.Flags().BoolVar(&all, "all", false,
		"score every backup against its predecessors instead of just the latest")
	return c
}

func runAnomalyCheck(cmd *cobra.Command, deployment, repoURL string, threshold float64, window, minSamples int, all bool) error {
	d := DispatcherFrom(cmd)
	if !finiteFloat(threshold) || threshold <= 0 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("anomaly check: --threshold must be a finite value > 0; got %v", threshold)).Wrap(output.ErrUsage)
	}
	if minSamples < 2 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("anomaly check: --min-samples must be >= 2; got %d", minSamples)).Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	// Collect all same-deployment manifests as Samples in chronological
	// order. The detector handles type-filtering, window trimming, and
	// self-removal — we just need the chronological stream.
	store := backup.NewManifestStore(sp)
	var samples []anomaly.Sample
	var skipped int
	for m, lerr := range store.List(cmd.Context(), deployment, verifier) {
		if lerr != nil {
			skipped++
			continue
		}
		samples = append(samples, manifestToSample(m))
	}
	if len(samples) == 0 {
		// No backups at all is not a failure — empty deployments are
		// the cold-start case. Surface a structured empty result so
		// scripts can act on it.
		body := anomalyCheckBody{
			Deployment:       deployment,
			Repo:             repoURL,
			Threshold:        threshold,
			Window:           window,
			MinSamples:       minSamples,
			TotalBackups:     0,
			ManifestsSkipped: skipped,
			GeneratedAt:      time.Now().UTC(),
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}

	// ManifestStore.List yields newest-first; we want oldest-first for
	// the per-sample score loop because each Score call uses everything
	// older as the prior pool.
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].StoppedAt.Before(samples[j].StoppedAt)
	})

	det := &anomaly.Detector{
		Threshold:  threshold,
		Window:     window,
		MinSamples: minSamples,
	}

	var reports []*anomaly.Report
	if all {
		// Walk: for each sample, prior is everything older.
		for i, candidate := range samples {
			rep, rerr := det.Score(deployment, samples[:i], candidate)
			if rerr != nil {
				return output.NewError("anomaly.score_failed",
					fmt.Sprintf("anomaly check: %v", rerr)).Wrap(rerr)
			}
			reports = append(reports, rep)
		}
	} else {
		// Default: just the most-recent sample.
		latest := samples[len(samples)-1]
		rep, rerr := det.Score(deployment, samples[:len(samples)-1], latest)
		if rerr != nil {
			return output.NewError("anomaly.score_failed",
				fmt.Sprintf("anomaly check: %v", rerr)).Wrap(rerr)
		}
		reports = append(reports, rep)
	}

	// Build the response body + count flags.
	body := anomalyCheckBody{
		Deployment:       deployment,
		Repo:             repoURL,
		Threshold:        threshold,
		Window:           window,
		MinSamples:       minSamples,
		TotalBackups:     len(samples),
		ManifestsSkipped: skipped,
		Reports:          reports,
		GeneratedAt:      time.Now().UTC(),
	}
	for _, r := range reports {
		if r.AnyFlagged {
			body.FlaggedCount++
		}
	}

	if body.FlaggedCount > 0 {
		// Emit one audit event per flagged report — matches the
		// repo.scrub.mismatch pattern (rare enough to be signal).
		// Failure to append is non-fatal: the finding lives in the
		// structured error (and in the per-Report subject details
		// captured here); the audit emission is the longitudinal
		// "when did anomalies start" record.
		emitAnomalyAudits(cmd.Context(), sp, repoMeta, deployment, repoURL, reports)
		// Render a one-line per-backup summary into the error
		// message so text-mode operators see what flagged without
		// having to consult the audit chain. JSON consumers get the
		// same content via the structured suggestion.
		var lines []string
		for _, r := range reports {
			if !r.AnyFlagged {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: %s",
				r.BackupID, strings.Join(r.Reasons, "; ")))
		}
		summary := strings.Join(lines, "\n")
		return output.NewError("anomaly.detected",
			fmt.Sprintf("anomaly check: %d backup(s) flagged out of %d scored\n%s",
				body.FlaggedCount, len(reports), summary)).
			WithSuggestion(&output.Suggestion{
				Human:  "review the flagged backup(s); a sustained shift in size/duration may be a real workload change rather than a regression",
				DocURL: "docs/runbooks/anomaly-detected.md",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// manifestToSample is the shape-bridge between backup.Manifest and
// anomaly.Sample. Mirrors summarizeManifest in list.go but emits the
// detector's input shape — we don't share `backupSummary` because the
// CLI's summary type and the detector's Sample have different
// long-term ownership (the summary is a CLI rendering shape, the
// Sample is a numerical input).
func manifestToSample(m *backup.Manifest) anomaly.Sample {
	s := anomaly.Sample{
		BackupID:        m.BackupID,
		Type:            string(m.Type),
		StoppedAt:       m.StoppedAt,
		DurationSeconds: m.StoppedAt.Sub(m.StartedAt).Seconds(),
		FileCount:       int64(len(m.Files)),
	}
	uniqueChunks := map[string]struct{}{}
	for _, f := range m.Files {
		s.LogicalBytes += f.Size
		for _, c := range f.Chunks {
			uniqueChunks[c.Hash.String()] = struct{}{}
		}
	}
	s.UniqueChunkCount = int64(len(uniqueChunks))
	return s
}

// emitAnomalyAudits appends one audit-chain entry per flagged
// report. Failures here are intentionally swallowed — the JSON
// result already carries the verdict; the audit emission is signal
// for the operator's longitudinal "when did anomalies start" query.
func emitAnomalyAudits(ctx context.Context, sp storage.StoragePlugin, repoMeta *repo.Metadata, deployment, repoURL string, reports []*anomaly.Report) {
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	for _, r := range reports {
		if !r.AnyFlagged {
			continue
		}
		store.AppendOrLog(ctx, &audit.Event{
			Action:    "anomaly.detected",
			Subject:   audit.Subject{Repo: repoURL, Deployment: deployment, BackupID: r.BackupID},
			Timestamp: time.Now().UTC(),
			Body: map[string]any{
				"backup_id":     r.BackupID,
				"type":          r.Type,
				"baseline_size": r.BaselineSize,
				"threshold":     r.Threshold,
				"window":        r.Window,
				"reasons":       r.Reasons,
				"scores":        r.Scores,
			},
		})
	}
}

// anomalyCheckBody is the Result body. Schema-tagged separately from
// the per-Report shape so a monitoring tool can target either the
// run-level summary OR the report stream.
type anomalyCheckBody struct {
	Schema           string            `json:"schema"`
	Deployment       string            `json:"deployment"`
	Repo             string            `json:"repo,omitempty"`
	Threshold        float64           `json:"threshold"`
	Window           int               `json:"window"`
	MinSamples       int               `json:"min_samples"`
	TotalBackups     int               `json:"total_backups"`
	ManifestsSkipped int               `json:"manifests_skipped,omitempty"`
	FlaggedCount     int               `json:"flagged_count"`
	Reports          []*anomaly.Report `json:"reports,omitempty"`
	GeneratedAt      time.Time         `json:"generated_at"`
}

// initSchema is what callers see in the body — set in WriteText
// indirectly (we set it before marshalling by carrying the constant
// in the JSON tag default).
func (b anomalyCheckBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Schema == "" {
		b.Schema = anomaly.Schema
	}
	fmt.Fprintf(bw, "anomaly check — deployment %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Total backups:   %d\n", b.TotalBackups)
	fmt.Fprintf(bw, "  Threshold:       ±%g (z-score)\n", b.Threshold)
	fmt.Fprintf(bw, "  Window:          %d most-recent same-type priors\n", b.Window)
	fmt.Fprintf(bw, "  Min samples:     %d\n", b.MinSamples)
	if b.ManifestsSkipped > 0 {
		fmt.Fprintf(bw, "  Manifests skipped: %d (failed to load)\n", b.ManifestsSkipped)
	}
	fmt.Fprintf(bw, "  Reports scored:  %d\n", len(b.Reports))
	fmt.Fprintf(bw, "  Flagged:         %d\n", b.FlaggedCount)
	if len(b.Reports) == 0 {
		fmt.Fprintln(bw, "  ✓ no backups to score yet (cold-start deployment)")
	} else if b.FlaggedCount == 0 {
		// Surface skip reasons separately from successful scores —
		// "we couldn't score yet" is different from "we scored and
		// nothing was unusual."
		var skips, clean int
		for _, r := range b.Reports {
			if r.Skipped != "" {
				skips++
			} else {
				clean++
			}
		}
		switch {
		case skips > 0 && clean == 0:
			fmt.Fprintf(bw, "  ⚠ all %d report(s) skipped — baseline still warming up\n", skips)
		case skips > 0:
			fmt.Fprintf(bw, "  ✓ %d clean, %d skipped (baseline warming up)\n", clean, skips)
		default:
			fmt.Fprintln(bw, "  ✓ all backups within baseline")
		}
	} else {
		fmt.Fprintln(bw, "  ✗ flagged backup(s):")
		for _, r := range b.Reports {
			if !r.AnyFlagged {
				continue
			}
			fmt.Fprintf(bw, "    %s (type=%s, baseline=%d):\n",
				r.BackupID, r.Type, r.BaselineSize)
			for _, reason := range r.Reasons {
				fmt.Fprintf(bw, "      - %s\n", reason)
			}
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
