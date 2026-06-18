// capacity_preflight.go — CLI surface for the pre-backup capacity guard.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/capacity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// newCapacityPreflightCmd implements
// `pg_hardstorage capacity preflight <repo> [--projected-bytes N|--from-deployment X]
// [--safety-factor 1.1]`.
//
// Standalone gate that the operator can call before running a
// real backup to confirm the repo has sufficient free space.
// Three input modes for the projection:
//
//	--projected-bytes  explicit byte count (operator knows
//	                   what they're projecting).
//
//	--from-deployment  use the deployment's latest committed
//	                   manifest's logical bytes as the
//	                   projection. The natural default for
//	                   "will my next backup fit?".
//
// Default safety factor is 1.10 (the SPEC's resilience
// requirement). Operators with very tight repos can dial it
// down; safety-conscious ops crank it up.
//
// Object-store backends report Unsupported (silent pass) —
// the operator's quota is out-of-band and we can't probe it.
func newCapacityPreflightCmd() *cobra.Command {
	var (
		repoURL        string
		projectedBytes int64
		fromDeployment string
		safetyFactor   float64
	)
	c := &cobra.Command{
		Use:   "preflight <repo>",
		Short: "Check whether the repo has free space for a projected backup",
		Long: `preflight asks "would a backup of N bytes succeed against this
repo's current free space?" and returns a structured verdict.

Three verdicts:

  pass                free space ≥ projected × safety_factor
  insufficient_space  free space < projected × safety_factor
  unsupported         backend can't probe (object stores; silent
                      pass — the operator's quota is out-of-band)

Projection modes:

  --projected-bytes N      explicit byte count
  --from-deployment NAME   use the deployment's latest committed
                           manifest's logical bytes (the natural
                           default for "will my next backup fit?")

Pre-flight is fail-open on a probe failure (statfs returning an
error) — a flaky probe shouldn't refuse an otherwise-OK backup.
The structured verdict surfaces the probe error in Note when
relevant.`,
		Args:         cobra.RangeArgs(0, 1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"capacity preflight: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runCapacityPreflight(cmd, capacityPreflightOptions{
				repoURL:        repoURL,
				projectedBytes: projectedBytes,
				fromDeployment: fromDeployment,
				safetyFactor:   safetyFactor,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <repo> also accepted)")
	c.Flags().Int64Var(&projectedBytes, "projected-bytes", 0,
		"explicit projected backup size in bytes (mutually exclusive with --from-deployment)")
	c.Flags().StringVar(&fromDeployment, "from-deployment", "",
		"derive projected size from the deployment's latest committed manifest (logical bytes)")
	c.Flags().Float64Var(&safetyFactor, "safety-factor", capacity.DefaultSafetyFactor,
		"multiplier on projected size for the required-free-space threshold (default 1.1 = 110%)")
	return c
}

type capacityPreflightOptions struct {
	repoURL        string
	projectedBytes int64
	fromDeployment string
	safetyFactor   float64
}

func runCapacityPreflight(cmd *cobra.Command, opts capacityPreflightOptions) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if opts.repoURL == "" {
		return missingFlagErr(cmd, "--repo (or a positional <url>)")
	}
	if opts.projectedBytes > 0 && opts.fromDeployment != "" {
		return output.NewError("usage.bad_flag",
			"capacity preflight: --projected-bytes and --from-deployment are mutually exclusive").Wrap(output.ErrUsage)
	}
	if opts.projectedBytes <= 0 && opts.fromDeployment == "" {
		return output.NewError("usage.missing_flag",
			"capacity preflight: must pass --projected-bytes or --from-deployment").Wrap(output.ErrUsage)
	}

	_, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	projected := opts.projectedBytes
	if projected <= 0 {
		// --from-deployment: derive from the deployment's
		// latest manifest's logical bytes.
		verifier, vErr := loadVerifier()
		if vErr != nil {
			return vErr
		}
		projected, err = projectedBytesFromDeployment(cmd.Context(), sp, opts.fromDeployment, verifier)
		if err != nil {
			return err
		}
	}

	res, err := capacity.Preflight(cmd.Context(), sp, capacity.PreflightOptions{
		ProjectedBytes: projected,
		SafetyFactor:   opts.safetyFactor,
	})
	if err != nil {
		// Probe failed. Fail-open per the documented posture
		// — surface as a structured warning rather than a
		// hard refusal. Caller (operator) sees both the
		// probe error and that the gate didn't fire.
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(capacityPreflightBody{
			Repo:           opts.repoURL,
			Verdict:        string(capacity.PreflightUnsupported),
			ProjectedBytes: projected,
			SafetyFactor:   capacity.DefaultSafetyFactor,
			ProbeError:     err.Error(),
			Note:           "free-space probe failed; pre-flight is fail-open",
		}))
	}

	body := capacityPreflightBody{
		Repo:           opts.repoURL,
		Verdict:        string(res.Verdict),
		ProjectedBytes: res.ProjectedBytes,
		RequiredBytes:  res.RequiredBytes,
		TotalBytes:     res.TotalBytes,
		AvailableBytes: res.AvailableBytes,
		SafetyFactor:   res.SafetyFactor,
		Note:           res.Note,
	}
	if res.Verdict == capacity.PreflightInsufficientSpace {
		// Pre-flight failed → structured error so the exit
		// code reflects the refusal. preflight.* maps to
		// ExitPreflight (4) per the v1 contract.
		errResult := output.NewError("preflight.repo_full",
			fmt.Sprintf("capacity preflight: repo %s has %d bytes available; %d required (projected %d × %.2f safety)",
				opts.repoURL, res.AvailableBytes, res.RequiredBytes, res.ProjectedBytes, res.SafetyFactor)).
			WithSuggestion(&output.Suggestion{
				Human: "free up repo space (run `repo gc --apply` to reclaim chunks unreferenced by retention) or move to a larger volume; the safety margin can be lowered with --safety-factor as a temporary measure.",
			})
		// Surface the body in the error path too — operators
		// piping JSON expect the verdict's structured shape.
		return errResult
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// projectedBytesFromDeployment returns the deployment's latest
// committed manifest's logical bytes — the natural projection
// for "would the next backup fit?". Empty deployment → error.
// No backups → error (the operator hasn't set up a baseline
// yet; explicit --projected-bytes is the right call).
func projectedBytesFromDeployment(ctx context.Context, sp storage.StoragePlugin, deployment string, verifier *backup.Verifier) (int64, error) {
	if deployment == "" {
		return 0, output.NewError("usage.missing_arg",
			"capacity preflight: --from-deployment requires a non-empty name").Wrap(output.ErrUsage)
	}
	store := backup.NewManifestStore(sp)
	var latest *backup.Manifest
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			continue
		}
		if m == nil {
			continue
		}
		if latest == nil || m.StoppedAt.After(latest.StoppedAt) {
			latest = m
		}
	}
	if latest == nil {
		return 0, output.NewError("notfound.backup",
			fmt.Sprintf("capacity preflight: deployment %q has no committed backups; pass --projected-bytes explicitly", deployment)).
			WithSuggestion(&output.Suggestion{
				Human:   "without history we have no projection. Take one backup with --projected-bytes guarded by your own size estimate, then subsequent --from-deployment runs work.",
				Command: "pg_hardstorage capacity preflight --repo <url> --projected-bytes <N>",
			})
	}
	var total int64
	for _, f := range latest.Files {
		total += f.Size
	}
	return total, nil
}

// capacityPreflightBody is the v1-stable result for `capacity
// preflight`. Verdict is pass / insufficient_space /
// unsupported.
type capacityPreflightBody struct {
	Repo           string  `json:"repo"`
	Verdict        string  `json:"verdict"`
	ProjectedBytes int64   `json:"projected_bytes"`
	RequiredBytes  int64   `json:"required_bytes,omitempty"`
	TotalBytes     int64   `json:"total_bytes,omitempty"`
	AvailableBytes int64   `json:"available_bytes,omitempty"`
	SafetyFactor   float64 `json:"safety_factor"`
	Note           string  `json:"note,omitempty"`
	ProbeError     string  `json:"probe_error,omitempty"`
}

// WriteText renders the preflight verdict and supporting figures as
// human-readable text to w.
func (b capacityPreflightBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	switch b.Verdict {
	case string(capacity.PreflightPass):
		fmt.Fprintf(bw, "✓ capacity preflight pass — %s\n", b.Repo)
		fmt.Fprintf(bw, "  Projected:    %s\n", humanBytes(b.ProjectedBytes))
		fmt.Fprintf(bw, "  Required:     %s (×%.2f safety)\n", humanBytes(b.RequiredBytes), b.SafetyFactor)
		fmt.Fprintf(bw, "  Available:    %s of %s\n", humanBytes(b.AvailableBytes), humanBytes(b.TotalBytes))
	case string(capacity.PreflightInsufficientSpace):
		fmt.Fprintf(bw, "✗ capacity preflight: insufficient space — %s\n", b.Repo)
		fmt.Fprintf(bw, "  Projected:    %s\n", humanBytes(b.ProjectedBytes))
		fmt.Fprintf(bw, "  Required:     %s (×%.2f safety)\n", humanBytes(b.RequiredBytes), b.SafetyFactor)
		fmt.Fprintf(bw, "  Available:    %s of %s\n", humanBytes(b.AvailableBytes), humanBytes(b.TotalBytes))
	case string(capacity.PreflightUnsupported):
		fmt.Fprintf(bw, "⚠ capacity preflight: unsupported (skipped) — %s\n", b.Repo)
		if b.Note != "" {
			fmt.Fprintf(bw, "  %s\n", b.Note)
		}
		if b.ProbeError != "" {
			fmt.Fprintf(bw, "  probe error: %s\n", b.ProbeError)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
