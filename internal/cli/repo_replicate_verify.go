// repo_replicate_verify.go — CLI surface for cross-repo replication-completeness verification.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRepoReplicateVerifyCmd implements `repo replicate verify`.
//
// The read-side complement to `repo replicate`.  Operationally
// answers: "is my replica actually consistent with the primary?".
// Walks the source repo's manifests + chunks, asserts each is
// present at the replica, and (in --deep mode) compares content
// byte-for-byte.
//
// Three verdicts:
//
//   - **consistent** — every primary key is present at the
//     replica with matching content.  No action needed.
//   - **drifted**    — every key is present, but at least one
//     has mismatched content.  Operators re-replicate to
//     refresh the affected keys.
//   - **broken**     — at least one key is absent from the
//     replica.  The replica can't serve the affected backups;
//     operators investigate why the original replicate missed
//     them (transient error, replicate worker stopped early,
//     bucket policy regression).
//
// Read-only against both repos.  Safe at any cadence; the
// default Stat-only path is cheap enough for per-PR / per-CI
// runs against production-shaped fleets.
func newRepoReplicateVerifyCmd() *cobra.Command {
	var (
		from       string
		to         string
		deployment string
		includeWAL bool
		deep       bool
		format     string
	)
	c := &cobra.Command{
		Use:   "verify --from <primary-url> --to <replica-url>",
		Short: "Verify a replica repo is consistent with its source",
		Long: `repo replicate verify walks the source repository's manifests +
chunks and asserts each is present at the replica with matching
content.  The output is one of three verdicts:

  consistent  — replica matches primary
  drifted     — present-but-mismatched content (re-replicate)
  broken      — missing keys (re-replicate AND investigate)

Default is Stat-only: O(N keys) Stat calls; detects "missing"
+ size-mismatch.  ` + "`--deep`" + ` adds a body-fetch + byte-compare
pass on every considered key, catching same-size-different-bytes
drift.  Slow; suitable for periodic deep scrub, not per-PR.

Read-only against both repos.  Both ` + "`--from`" + ` and ` + "`--to`" + ` MUST
already exist.

Output formats:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade GFM rendering.

Exit-code mapping:
  0  — verdict=consistent
  9  — verdict=drifted OR broken (verify-failed namespace).
       The body still renders to stdout for diagnosis; the
       error summary on stderr names the verdict.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepoReplicateVerify(cmd, repoReplicateVerifyFlags{
				from:       from,
				to:         to,
				deployment: deployment,
				includeWAL: includeWAL,
				deep:       deep,
				format:     format,
			})
		},
	}
	c.Flags().StringVar(&from, "from", "", "primary (source) repository URL (required)")
	_ = c.MarkFlagRequired("from")
	c.Flags().StringVar(&to, "to", "", "replica (destination) repository URL (required)")
	_ = c.MarkFlagRequired("to")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict the walk to one deployment (default: all)")
	c.Flags().BoolVar(&includeWAL, "include-wal", false,
		"also verify wal/<deployment>/... segment manifests")
	c.Flags().BoolVar(&deep, "deep", false,
		"fetch + byte-compare both sides (catches same-size drift; slow)")
	c.Flags().StringVar(&format, "format", "json",
		"output format: json | markdown")
	return c
}

type repoReplicateVerifyFlags struct {
	from       string
	to         string
	deployment string
	includeWAL bool
	deep       bool
	format     string
}

func runRepoReplicateVerify(cmd *cobra.Command, f repoReplicateVerifyFlags) error {
	d := DispatcherFrom(cmd)
	if f.from == f.to {
		return output.NewError("usage.bad_flag",
			"repo replicate verify: --from and --to are the same URL (nothing to verify)").
			Wrap(output.ErrUsage)
	}
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("repo replicate verify: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	_, srcSP, err := openRepo(cmd.Context(), f.from)
	if err != nil {
		return err
	}
	defer srcSP.Close()
	_, dstSP, err := openRepo(cmd.Context(), f.to)
	if err != nil {
		return err
	}
	defer dstSP.Close()

	res, err := repo.VerifyReplicate(cmd.Context(), srcSP, dstSP, repo.ReplicateVerifyOptions{
		Deployment: f.deployment,
		IncludeWAL: f.includeWAL,
		Deep:       f.deep,
	})
	if err != nil {
		return output.NewError("repo.replicate_verify_failed",
			fmt.Sprintf("repo replicate verify: %v", err)).Wrap(err)
	}
	res.SourceURL = f.from
	res.DestURL = f.to

	body := replicateVerifyBody{
		Result: res,
		format: f.format,
	}
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}

	// Verdict mapping: drifted + broken both trip
	// verify.replica_inconsistent → ExitVerifyFailed.  consistent
	// exits 0.  The body has already rendered to stdout (dual-
	// stream pattern); the error summary on stderr names the
	// verdict.
	if res.Verdict != repo.VerdictConsistent {
		return output.NewError("verify.replica_inconsistent",
			fmt.Sprintf("repo replicate verify: %s — %d missing, %d drifted",
				res.Verdict,
				res.ManifestsMissing+res.ChunksMissing+res.WALManifestsMissing,
				res.ManifestsContentDrift+res.ChunksContentDrift)).
			WithSuggestion(&output.Suggestion{
				Human:   "review the failures slice + run `repo replicate --from " + f.from + " --to " + f.to + "` to repair",
				Command: "pg_hardstorage repo replicate --from " + f.from + " --to " + f.to,
			})
	}
	return nil
}

type replicateVerifyBody struct {
	Result *repo.ReplicateVerifyResult
	format string
}

// MarshalJSON emits the embedded repo.ReplicateVerifyResult so the JSON
// contract stays the domain v1 shape.
func (b replicateVerifyBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Result)
}

// WriteText renders the verification report to w, choosing markdown when
// format is "markdown" and the compact summary otherwise.
func (b replicateVerifyBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return writeReplicateVerifyMarkdown(w, b.Result)
	}
	return writeReplicateVerifyCompact(w, b.Result)
}

// writeReplicateVerifyMarkdown renders the report as a forensics-
// grade GFM document.
func writeReplicateVerifyMarkdown(w io.Writer, r *repo.ReplicateVerifyResult) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "# pg_hardstorage repo replicate verify")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Source | `%s` |\n", r.SourceURL)
	fmt.Fprintf(bw, "| Destination | `%s` |\n", r.DestURL)
	if r.Deployment != "" {
		fmt.Fprintf(bw, "| Deployment | `%s` |\n", r.Deployment)
	}
	if r.Deep {
		fmt.Fprintln(bw, "| Mode | deep (body byte-compare) |")
	} else {
		fmt.Fprintln(bw, "| Mode | Stat-only |")
	}
	if r.IncludeWAL {
		fmt.Fprintln(bw, "| WAL | included |")
	}
	fmt.Fprintf(bw, "| Started at | %s |\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)

	icon := "✓"
	descr := "consistent"
	switch r.Verdict {
	case repo.VerdictDrifted:
		icon = "·"
		descr = "drifted"
	case repo.VerdictBroken:
		icon = "✗"
		descr = "broken"
	}
	fmt.Fprintf(bw, "## Verdict: %s %s\n\n", icon, strings.ToUpper(descr))

	fmt.Fprintln(bw, "## Counters")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Class | Considered | Present | Missing | Content drift |")
	fmt.Fprintln(bw, "| --- | ---: | ---: | ---: | ---: |")
	fmt.Fprintf(bw, "| Manifests | %d | %d | %d | %d |\n",
		r.ManifestsConsidered, r.ManifestsPresent,
		r.ManifestsMissing, r.ManifestsContentDrift)
	fmt.Fprintf(bw, "| Chunks | %d | %d | %d | %d |\n",
		r.ChunksConsidered, r.ChunksPresent,
		r.ChunksMissing, r.ChunksContentDrift)
	if r.IncludeWAL {
		fmt.Fprintf(bw, "| WAL manifests | %d | %d | %d | — |\n",
			r.WALManifestsConsidered, r.WALManifestsPresent,
			r.WALManifestsMissing)
	}
	fmt.Fprintln(bw)

	if len(r.Failures) > 0 {
		fmt.Fprintln(bw, "## Failures")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Kind | Key | Backup | Reason |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- |")
		for _, f := range r.Failures {
			bid := "—"
			if f.BackupID != "" {
				bid = "`" + f.BackupID + "`"
			}
			fmt.Fprintf(bw, "| `%s` | `%s` | %s | %s |\n",
				f.Kind, f.Key, bid, fallbackOrEmDash(f.Reason))
		}
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "_To repair drift / missing keys, re-run `repo replicate --from <src> --to <dst>`._")
	} else if r.Verdict == repo.VerdictConsistent {
		fmt.Fprintln(bw, "_No failures — the replica is consistent with the primary._")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

// writeReplicateVerifyCompact renders the compact human-readable
// view for `-o text` without --format markdown.
func writeReplicateVerifyCompact(w io.Writer, r *repo.ReplicateVerifyResult) error {
	bw := &strings.Builder{}
	icon := "✓"
	switch r.Verdict {
	case repo.VerdictDrifted:
		icon = "·"
	case repo.VerdictBroken:
		icon = "✗"
	}
	fmt.Fprintf(bw, "repo replicate verify — %s → %s\n", r.SourceURL, r.DestURL)
	fmt.Fprintf(bw, "  %s Verdict: %s\n", icon, strings.ToUpper(string(r.Verdict)))
	if r.Deployment != "" {
		fmt.Fprintf(bw, "  Deployment: %s\n", r.Deployment)
	}
	if r.Deep {
		fmt.Fprintln(bw, "  Mode:       deep (body byte-compare)")
	} else {
		fmt.Fprintln(bw, "  Mode:       Stat-only")
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "Manifests:     %d considered, %d present, %d missing, %d drifted\n",
		r.ManifestsConsidered, r.ManifestsPresent,
		r.ManifestsMissing, r.ManifestsContentDrift)
	fmt.Fprintf(bw, "Chunks:        %d considered, %d present, %d missing, %d drifted\n",
		r.ChunksConsidered, r.ChunksPresent,
		r.ChunksMissing, r.ChunksContentDrift)
	if r.IncludeWAL {
		fmt.Fprintf(bw, "WAL manifests: %d considered, %d present, %d missing\n",
			r.WALManifestsConsidered, r.WALManifestsPresent, r.WALManifestsMissing)
	}
	if len(r.Failures) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Failures (first 10):")
		max := 10
		if len(r.Failures) < max {
			max = len(r.Failures)
		}
		for i := 0; i < max; i++ {
			f := r.Failures[i]
			fmt.Fprintf(bw, "  %s — %s\n", f.Kind, f.Key)
		}
		if len(r.Failures) > max {
			fmt.Fprintf(bw, "  … (+%d more)\n", len(r.Failures)-max)
		}
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full report.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func fallbackOrEmDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
