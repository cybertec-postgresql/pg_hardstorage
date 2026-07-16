// repo_scrub.go — CLI surface for the repo-wide chunk-sampling scrub.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRepoScrubCmd implements `pg_hardstorage repo scrub <url>`. The
// periodic-maintenance side of chunk integrity verification.
//
// Two framings of the same primitive (repo.Scrub):
//
//   - `repair scrub` is the diagnostic. "I think something is
//     corrupt; sample 1k chunks and tell me what's broken." Default
//     limit is 1000; surface findings as a verification failure.
//
//   - `repo scrub` is the maintenance. "I run this from cron once
//     an hour at 1% sample; once a quarter at 100%." Default
//     sample percent is 1; result body emphasises throughput
//     (bytes scanned, duration) so operators can size their scrub
//     budget. Findings still flip the exit code to
//     ExitVerifyFailed so the cron job alarms.
//
// Mismatches are also written to the audit chain on success — bit-rot
// is rare enough that the chain entry is signal, not noise. A
// successful "no mismatches" scrub does NOT write an audit entry
// (would be too chatty for a cron-driven check).
func newRepoScrubCmd() *cobra.Command {
	var (
		repoURL       string
		samplePercent int
		fullScan      bool
	)
	c := &cobra.Command{
		Use:   "scrub <url>",
		Short: "Re-hash chunks to detect bit-rot (periodic maintenance)",
		Long: `scrub samples N% of the repo's referenced chunks and re-hashes
each one against its key, surfacing any mismatch as a "your storage
backend has corrupted bytes" finding.

The --sample-percent default is 1 (1% per run) which is the
operator-friendly cadence for an hourly cron job. For exhaustive
quarterly checks pass --full (equivalent to --sample-percent 100).

Scrub mismatches map to ExitVerifyFailed (9) so a cron-wired scrub
alarms when integrity slips. Findings are also captured in the
hash-chained audit log.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Positional-or-flag, matching the sibling repo verbs
			// (repo check, repo gc): --repo is an accepted alternative
			// to the positional <url>; a positional that disagrees
			// with --repo is a conflict rather than a silent override.
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo scrub: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runRepoScrub(cmd, repoURL, samplePercent, fullScan)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	c.Flags().IntVar(&samplePercent, "sample-percent", 1,
		"percent of referenced chunks to sample (1-100); default 1 for hourly cron, 100 for quarterly full scan")
	c.Flags().BoolVar(&fullScan, "full", false,
		"shorthand for --sample-percent 100 (scan every referenced chunk)")
	return c
}

func runRepoScrub(cmd *cobra.Command, urlArg string, samplePercent int, fullScan bool) error {
	d := DispatcherFrom(cmd)
	if urlArg == "" {
		return output.NewError("usage.missing_arg",
			"repo scrub: <url> is required").Wrap(output.ErrUsage)
	}
	if fullScan {
		samplePercent = 100
	}
	if samplePercent < 1 || samplePercent > 100 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("repo scrub: --sample-percent must be in [1,100]; got %d", samplePercent)).Wrap(output.ErrUsage)
	}

	repoMeta, sp, err := openRepo(cmd.Context(), urlArg)
	if err != nil {
		return err
	}
	defer sp.Close()

	refs, err := repo.CollectReferences(cmd.Context(), sp)
	if err != nil {
		return output.NewError("repo.scrub.collect_refs_failed",
			fmt.Sprintf("repo scrub: collect references: %v", err)).Wrap(err)
	}

	// Convert sample-percent to a chunk-count limit. limit=0 means
	// "everything" — what --full requests.
	limit := 0
	if samplePercent < 100 {
		limit = refs.Len() * samplePercent / 100
		if limit < 1 {
			limit = 1
		}
	}

	// Scrub manifest-aware: build the per-manifest CAS so ENCRYPTED
	// chunks decrypt with the right DEK before the plaintext-hash
	// round-trip. The previous bare casdefault.New(sp) had no
	// decryptor, so every encrypted chunk failed to decrypt and was
	// bucketed as a "mismatch" — repo scrub reported 100% corruption
	// on any encrypted repo (the default after `init`) and exited 9.
	// This is the same machinery `repair scrub` uses.
	startedAt := time.Now().UTC()
	agg, refsTotal, err := scrubManifestAware(cmd.Context(), sp, limit)
	stoppedAt := time.Now().UTC()
	if err != nil {
		return output.NewError("repo.scrub.failed",
			fmt.Sprintf("repo scrub: %v", err)).Wrap(err)
	}

	body := repoScrubBody{
		ReferencedTotal: refsTotal,
		Sampled:         agg.Sampled,
		OK:              agg.OK,
		MismatchCount:   len(agg.Mismatches),
		BytesScanned:    agg.Bytes,
		SamplePercent:   samplePercent,
		StartedAt:       startedAt,
		StoppedAt:       stoppedAt,
		DurationMS:      stoppedAt.Sub(startedAt).Milliseconds(),
	}
	for _, h := range agg.Mismatches {
		body.Mismatches = append(body.Mismatches, h.String())
	}

	if len(agg.Mismatches) > 0 {
		// Audit emission for mismatches. Bit-rot is rare enough
		// that the chain entry is signal — operators investigating
		// "when did we first see corruption?" can walk the chain
		// for repo.scrub.mismatch events.
		audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
			Action:    "repo.scrub.mismatch",
			Subject:   audit.Subject{Repo: urlArg},
			Timestamp: time.Now().UTC(),
			Body: map[string]any{
				"sample_percent":   samplePercent,
				"sampled":          agg.Sampled,
				"referenced_total": refs.Len(),
				"mismatch_count":   len(agg.Mismatches),
				"first_mismatch":   body.Mismatches[0],
				"bytes_scanned":    agg.Bytes,
				"duration_ms":      body.DurationMS,
			},
		})
		return output.NewError("verify.scrub_mismatch",
			fmt.Sprintf("repo scrub: %d chunk(s) failed verification (sampled %d of %d referenced)",
				len(agg.Mismatches), agg.Sampled, refsTotal)).
			WithSuggestion(&output.Suggestion{
				Human:   "the storage backend has corrupted bytes for the listed chunks. Heal from a replica with `pg_hardstorage repair scrub --heal --replica <replica-url>`.",
				Command: fmt.Sprintf("pg_hardstorage repair scrub --repo %s --heal --replica <replica-url>", urlArg),
				DocURL:  "docs/runbooks/scrub-mismatch.md",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// repoScrubBody is the v1-stable result. Distinct from the
// repair-scrub body so monitoring tools can target the right
// schema for cron-driven scrubs.
type repoScrubBody struct {
	ReferencedTotal int       `json:"referenced_total"`
	Sampled         int       `json:"sampled"`
	OK              int       `json:"ok"`
	MismatchCount   int       `json:"mismatch_count"`
	BytesScanned    int64     `json:"bytes_scanned"`
	Mismatches      []string  `json:"mismatches,omitempty"`
	SamplePercent   int       `json:"sample_percent"`
	StartedAt       time.Time `json:"started_at"`
	StoppedAt       time.Time `json:"stopped_at"`
	DurationMS      int64     `json:"duration_ms"`
}

// WriteText renders the scrub result — sample size, mismatch list, and
// throughput figures — as human-readable text to w.
func (b repoScrubBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repo scrub — %d%% sample\n", b.SamplePercent)
	fmt.Fprintf(bw, "  Referenced chunks: %d\n", b.ReferencedTotal)
	fmt.Fprintf(bw, "  Sampled:           %d\n", b.Sampled)
	fmt.Fprintf(bw, "  OK:                %d\n", b.OK)
	fmt.Fprintf(bw, "  Mismatches:        %d\n", b.MismatchCount)
	fmt.Fprintf(bw, "  Bytes scanned:     %s\n", humanBytes(b.BytesScanned))
	fmt.Fprintf(bw, "  Duration:          %d ms\n", b.DurationMS)
	if len(b.Mismatches) > 0 {
		fmt.Fprintf(bw, "  ✗ FAILED HASHES:\n")
		for _, h := range b.Mismatches {
			fmt.Fprintf(bw, "    %s\n", h)
		}
	} else {
		fmt.Fprintln(bw, "  ✓ no integrity findings")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
