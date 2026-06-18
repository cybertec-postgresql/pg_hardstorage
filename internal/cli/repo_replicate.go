// repo_replicate.go — CLI surface for source-to-destination repo replication.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRepoReplicateCmd implements `pg_hardstorage repo replicate`.
//
// The plan calls this out by name (resilience principle 8: "Backups
// of backups (cross-region)"). ships the one-shot operator
// command driving the existing repo.Replicate primitive. A long-
// running goroutine that watches the manifest stream is a
// follow-up — first we make the bytes flow correctly, then we make
// them flow continuously.
//
// Operator-facing shape:
//
//	pg_hardstorage repo replicate \
//	    --from s3://primary-bucket \
//	    --to   s3://replica-bucket-eu \
//	    [--include-wal] [--dry-run]
//
// Idempotent — wire it into pg_timetable / cron / a sidecar Job and
// run as often as your egress budget tolerates. The dry-run mode
// reports what would copy without writing, useful for sizing the
// next run.
func newRepoReplicateCmd() *cobra.Command {
	var (
		from         string
		to           string
		includeWAL   bool
		dryRun       bool
		maxMbps      float64
		scheduleExpr string
	)
	c := &cobra.Command{
		Use:   "replicate --from <src-url> --to <dst-url>",
		Short: "Copy committed manifests + chunks from a source repo to a replica repo",
		Long: `replicate copies every committed (non-tombstoned) manifest from
--from to --to, plus every chunk those manifests reference.

Idempotent: chunks and manifests already at --to are skipped via
Stat / IfNotExists, so re-running the same replicate is safe and
cheap.

The destination repo MUST already exist (use 'repo init <to>' first).
Replicate only ADDS to the destination — it never initialises one and
never prunes (use 'repo gc' on the destination if you want to trim).

Tombstoned backups are not replicated. The replica is for surviving
loss of the primary, not for resurrecting deleted backups.

By default WAL segment manifests (and their chunks) are NOT copied.
Pass --include-wal for full WAL redundancy in the replica region.

Bandwidth shaping (mutually exclusive):
  --max-mbps N         constant cap of N megabits per second
  --schedule <expr>    time-of-day windows; e.g.
                       "Mon-Fri,09:00-18:00=50mbps;Sat-Sun,00:00-23:59=200mbps"
                       Times are UTC. Outside any declared window
                       traffic is unbounded. Rates are 0/<N>mbps/<N>kbps.

Both forms wrap the destination's storage plugin with a token-
bucket. The schedule is consulted per-acquire so a window
transition takes effect mid-run.

This is the implementation: a one-shot command. Schedule it via
pg_timetable or cron at whatever cadence matches your egress budget
and target RPO for the replica region.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoReplicate(cmd, from, to, includeWAL, dryRun, maxMbps, scheduleExpr)
		},
	}
	c.Flags().StringVar(&from, "from", "", "Source repo URL (must already exist)")
	_ = c.MarkFlagRequired("from")
	c.Flags().StringVar(&to, "to", "", "Destination repo URL (must already exist; bootstrap with 'repo init')")
	_ = c.MarkFlagRequired("to")
	c.Flags().BoolVar(&includeWAL, "include-wal", false,
		"Also replicate wal/<deployment>/<tli>/ segment manifests and their chunks")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"Compute the work but write nothing to the destination")
	c.Flags().Float64Var(&maxMbps, "max-mbps", 0,
		"Cap upload bandwidth to N megabits per second (default 0 = unbounded). Token-bucket shared across the run.")
	c.Flags().StringVar(&scheduleExpr, "schedule", "",
		"Time-of-day windows for bandwidth shaping; e.g. \"Mon-Fri,09:00-18:00=50mbps\". Mutually exclusive with --max-mbps.")
	// `replicate verify` is the read-side complement: walks both
	// repos + asserts the replica is consistent with the primary.
	c.AddCommand(newRepoReplicateVerifyCmd())
	return c
}

func runRepoReplicate(cmd *cobra.Command, from, to string, includeWAL, dryRun bool, maxMbps float64, scheduleExpr string) error {
	d := DispatcherFrom(cmd)
	// Progress events are for a human watching a long run; suppress them
	// under -o json so scripted consumers get a clean single-object result
	// body (the established dual-stream convention). Metrics fire regardless.
	emitProgress := func(e *output.Event) {
		if d.Renderer().Name() != "json" {
			_ = d.Event(cmd.Context(), e)
		}
	}
	if from == to {
		return output.NewError("usage.bad_flag",
			"repo replicate: --from and --to must differ").Wrap(output.ErrUsage)
	}
	if maxMbps < 0 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("repo replicate: --max-mbps must be >= 0; got %v", maxMbps)).Wrap(output.ErrUsage)
	}
	if maxMbps > 0 && scheduleExpr != "" {
		return output.NewError("usage.bad_flag",
			"repo replicate: --max-mbps and --schedule are mutually exclusive").Wrap(output.ErrUsage)
	}
	var schedule *throttle.Schedule
	if scheduleExpr != "" {
		s, err := throttle.ParseSchedule(scheduleExpr)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("repo replicate: --schedule: %v", err)).Wrap(output.ErrUsage)
		}
		schedule = s
	}

	_, srcSP, err := openRepo(cmd.Context(), from)
	if err != nil {
		return err
	}
	defer srcSP.Close()

	dstMeta, dstSP, err := openRepo(cmd.Context(), to)
	if err != nil {
		return err
	}
	defer dstSP.Close()

	// We're writing to dst; refuse if it's read-only mode. (The
	// source is read-only as far as Replicate is concerned, so the
	// source's mode is irrelevant — read-only-source is fine.)
	if !dryRun {
		if err := assertRepoWritable(cmd.Context(), dstSP, "repo replicate (writes to destination)"); err != nil {
			return err
		}
	}

	// Wrap the destination plugin with a bandwidth throttle when
	// --max-mbps or --schedule is set. The throttle is transparent
	// (passes through every method); it only meters bytes flowing
	// through Put. The source plugin stays unwrapped — we throttle
	// the WRITE side because that's where the egress charge lives.
	dstWriter := dstSP
	switch {
	case maxMbps > 0:
		bps := int64(maxMbps * 1_000_000 / 8) // megabits → bytes/sec
		dstWriter = throttle.New(dstSP, bps)
	case schedule != nil:
		// Burst sized to the schedule's peak BPS so a window
		// transition into a high-rate period doesn't pay an
		// unfair stall. We compute peak by walking the windows.
		var peakBPS int64
		for _, w := range schedule.Windows {
			if w.BPS > peakBPS {
				peakBPS = w.BPS
			}
		}
		dstWriter = throttle.New(dstSP, 0,
			throttle.WithSchedule(schedule),
			throttle.WithBurst(peakBPS))
	}

	// Observability (audit #1): a long-running cross-region copy used to
	// be opaque — no start signal, no progress, no metrics. Emit a start
	// event, a throttled per-step progress event so an operator can see it
	// advancing (vs. stuck), and run metrics at the end.
	emitProgress(output.NewEvent(output.SeverityInfo, "repo", "replicate.started").
		WithBody(map[string]any{"source": from, "dest": to, "dry_run": dryRun, "include_wal": includeWAL}))

	var progressN int
	res, err := repo.Replicate(cmd.Context(), srcSP, dstWriter, repo.ReplicateOptions{
		DryRun:     dryRun,
		IncludeWAL: includeWAL,
		// Apply the DESTINATION repo's WORM policy to every replicated
		// object, so a compliance-configured DR replica is actually
		// immutable instead of freely deletable.
		DstWORM: dstMeta.WORM,
		OnProgress: func(ev repo.ReplicateProgress) {
			progressN++
			if progressN%50 == 0 {
				emitProgress(output.NewEvent(output.SeverityInfo, "repo", "replicate.progress").
					WithBody(map[string]any{"stage": ev.Stage, "processed": progressN, "current": ev.Current}))
			}
		},
	})
	if err != nil {
		metrics.ReplicateRun("failure")
		return output.NewError("repo.replicate.failed",
			fmt.Sprintf("repo replicate: %v", err)).Wrap(err)
	}
	res.SourceURL = from
	res.DestURL = to

	// Run metrics + a completion event. result is "incomplete" when any
	// object failed (the per-key detail is in the result body).
	result := "success"
	if res.ManifestsFailed > 0 || res.ChunksFailed > 0 || res.WALManifestsFailed > 0 {
		result = "incomplete"
	}
	metrics.ReplicateRun(result)
	metrics.AddReplicateCopied(res.ManifestsCopied, res.ChunksCopied, res.WALManifestsCopied, res.BytesCopied)
	emitProgress(output.NewEvent(output.SeverityInfo, "repo", "replicate.completed").
		WithBody(map[string]any{
			"result":           result,
			"manifests_copied": res.ManifestsCopied,
			"chunks_copied":    res.ChunksCopied,
			"bytes_copied":     res.BytesCopied,
		}))

	body := repoReplicateBody{
		ReplicateResult: *res,
		MaxMbps:         maxMbps,
		Schedule:        scheduleExpr,
	}

	// Failures in the result body don't flip the exit code on their
	// own — replicate is best-effort and reports per-key issues for
	// the operator to investigate. A *total* manifest-failure count
	// equal to ManifestsConsidered is the strongest signal we can
	// emit; treat it as a hard error so cron-wired runs alarm.
	if res.ManifestsConsidered > 0 && res.ManifestsFailed == res.ManifestsConsidered {
		return output.NewError("repo.replicate.all_manifests_failed",
			fmt.Sprintf("repo replicate: every manifest failed (%d/%d) — check destination credentials, network, and disk space",
				res.ManifestsFailed, res.ManifestsConsidered)).
			WithSuggestion(&output.Suggestion{
				Human: "the JSON body has the per-key error detail; common causes are wrong dst credentials and dst disk full",
			})
	}

	// Render the body first (counters for monitoring) — dual-stream
	// pattern, same as `repo replicate verify`.
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}

	// Then signal INCOMPLETENESS via a non-zero exit. Previously any
	// partial failure exited 0, so a scripted `repo replicate && rm
	// source` would delete the source after an incomplete copy — a
	// silent loss of whatever didn't replicate (data-loss audit round 4
	// #2). Best-effort CONTINUE still applies (we copied what we could);
	// the exit just reflects that the destination is not a complete
	// replica. Replication is idempotent, so the operator re-runs until
	// it exits clean before trusting the DR copy. Dry-runs never trip
	// this (they don't write).
	if !dryRun && (res.ManifestsFailed > 0 || res.ChunksFailed > 0 || res.ChunksMissing > 0 || res.WALManifestsFailed > 0 || res.WALAuxFailed > 0) {
		return output.NewError("repo.replicate.incomplete",
			fmt.Sprintf("repo replicate: destination is INCOMPLETE — manifests_failed=%d chunks_failed=%d chunks_missing=%d wal_manifests_failed=%d wal_aux_failed=%d; do NOT delete the source until a re-run exits clean",
				res.ManifestsFailed, res.ChunksFailed, res.ChunksMissing, res.WALManifestsFailed, res.WALAuxFailed)).
			WithSuggestion(&output.Suggestion{
				Human: "replication is idempotent — re-run `repo replicate` until it exits 0, then confirm with `repo replicate verify` before retiring the source.",
			})
	}
	return nil
}

// repoReplicateBody is the v1-stable Result body. Cron-friendly:
// every counter the operator might graph is a top-level integer,
// duration_ms is a fixed unit, and started/stopped_at are RFC 3339.
type repoReplicateBody struct {
	repo.ReplicateResult
	// MaxMbps is the constant bandwidth cap that was active during
	// this run, in megabits per second. Zero means unbounded.
	MaxMbps float64 `json:"max_mbps,omitempty"`
	// Schedule is the time-of-day window expression used during
	// this run; mutually exclusive with MaxMbps. Empty when
	// unbounded or when --max-mbps was used.
	Schedule string `json:"schedule,omitempty"`
}

// WriteText renders the replicate result — per-object counters, throughput
// cap, and schedule — as human-readable text to w.
func (r repoReplicateBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repo replicate — %s → %s\n", r.SourceURL, r.DestURL)
	if r.DryRun {
		fmt.Fprintln(bw, "  (dry-run — nothing written to destination)")
	}
	if r.MaxMbps > 0 {
		fmt.Fprintf(bw, "  Bandwidth cap: %g Mbps (token-bucket on destination)\n", r.MaxMbps)
	}
	if r.Schedule != "" {
		fmt.Fprintf(bw, "  Schedule:      %s (UTC)\n", r.Schedule)
	}
	fmt.Fprintf(bw, "  Manifests: %d copied · %d skipped · %d tombstoned · %d failed (%d considered)\n",
		r.ManifestsCopied, r.ManifestsSkipped, r.ManifestsTombstoned, r.ManifestsFailed, r.ManifestsConsidered)
	fmt.Fprintf(bw, "  Chunks:    %d copied · %d skipped · %d missing · %d failed (%d considered)\n",
		r.ChunksCopied, r.ChunksSkipped, r.ChunksMissing, r.ChunksFailed, r.ChunksConsidered)
	if r.IncludeWAL {
		fmt.Fprintf(bw, "  WAL:       %d copied · %d skipped · %d failed (%d considered)\n",
			r.WALManifestsCopied, r.WALManifestsSkipped, r.WALManifestsFailed, r.WALManifestsConsidered)
		fmt.Fprintf(bw, "  WAL aux:   %d copied · %d skipped · %d failed (%d considered) [.history/.backup/.partial]\n",
			r.WALAuxCopied, r.WALAuxSkipped, r.WALAuxFailed, r.WALAuxConsidered)
	}
	fmt.Fprintf(bw, "  Bytes copied: %s\n", humanBytes(r.BytesCopied))
	fmt.Fprintf(bw, "  Duration:     %s\n", time.Duration(r.DurationMS)*time.Millisecond)
	if r.ManifestsFailed == 0 && r.ChunksFailed == 0 && r.ChunksMissing == 0 {
		fmt.Fprintln(bw, "  ✓ replication clean")
	} else {
		fmt.Fprintln(bw, "  ✗ replication had findings — see JSON body for details")
	}
	if len(r.Failures) > 0 {
		fmt.Fprintf(bw, "  First %d failure(s):\n", len(r.Failures))
		for _, f := range r.Failures {
			fmt.Fprintf(bw, "    %s — %s\n", f.Key, f.Err)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
