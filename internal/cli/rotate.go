// rotate.go — 'rotate' CLI verb: retention-policy rotation with safe-by-default dry-run.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRotateCmd implements `pg_hardstorage rotate [<deployment>]`.
//
// Default behaviour is dry-run: the command renders the policy
// decision (Keep / Delete with reasons) but doesn't soft-delete
// anything. --apply opts in to actually marking manifests for
// deletion. This is the safe-by-default posture every other
// destructive op honours.
//
// Without a positional <deployment>, rotation runs across every
// deployment in the repo — the typical "scheduled retention sweep"
// invocation.
func newRotateCmd() *cobra.Command {
	var opts rotateOpts
	c := &cobra.Command{
		Use:   "rotate [<deployment>]",
		Short: "Apply retention policy to a deployment (or all)",
		Long: `Classify each backup as kept or to-be-soft-deleted per the chosen
retention policy, then optionally apply the decision.

Three policies ship today:

  gfs (default): grandfather-father-son.
    --keep-daily 7      keep one backup per UTC calendar day, last 7
    --keep-weekly 4     keep one backup per ISO calendar week, last 4
    --keep-monthly 12   keep one backup per UTC calendar month, last 12
    --keep-yearly 5     keep one backup per UTC calendar year, last 5

  simple:
    --keep-for 30d      keep every backup younger than this duration

  count:
    --keep-fulls 14     keep the most recent N full backups

The newest backup is ALWAYS kept regardless of policy output —
operators must never end up with zero backups because of a misset
flag. This is hardcoded; --keep-* lower bounds don't override it.

Default mode is --dry-run; pass --apply to actually mark manifests.
Soft-deleted manifests get a tombstone marker beside them; the
manifest body and its chunks are NOT removed in this slice (the
chunk-GC pass that reaps unreferenced bytes lands).`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.deployment = args[0]
			}
			return runRotate(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&opts.policy, "policy", "gfs",
		"retention policy: gfs|simple|count")
	c.Flags().IntVar(&opts.keepDaily, "keep-daily", 7, "GFS: backups kept per UTC day")
	c.Flags().IntVar(&opts.keepWeekly, "keep-weekly", 4, "GFS: backups kept per ISO week")
	c.Flags().IntVar(&opts.keepMonthly, "keep-monthly", 12, "GFS: backups kept per UTC month")
	c.Flags().IntVar(&opts.keepYearly, "keep-yearly", 5, "GFS: backups kept per UTC year")
	c.Flags().DurationVar(&opts.keepFor, "keep-for", 30*24*time.Hour, "simple: keep every backup younger than this")
	c.Flags().IntVar(&opts.keepFulls, "keep-fulls", 14, "count: keep the N most recent fulls")
	c.Flags().BoolVar(&opts.apply, "apply", false, "actually soft-delete (default: dry-run)")
	return c
}

type rotateOpts struct {
	deployment string
	repoURL    string
	policy     string

	keepDaily   int
	keepWeekly  int
	keepMonthly int
	keepYearly  int
	keepFor     time.Duration
	keepFulls   int

	apply bool
}

// runRotate is the body of the rotate command. Pulled out for
// testing.
func runRotate(cmd *cobra.Command, opts rotateOpts) error {
	d := DispatcherFrom(cmd)

	policy, err := buildPolicy(opts)
	if err != nil {
		return err
	}

	// Resolve verifier the same way restore does — every manifest we
	// touch must have a valid signature, otherwise the safety story
	// (the signed-manifest commitment) is broken.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	_, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("rotate: signing key: %v", err)).Wrap(err)
	}

	repoMeta, sp, err := repo.Open(cmd.Context(), opts.repoURL)
	if err != nil {
		return mapRepoOpenErr(opts.repoURL, err)
	}
	defer sp.Close()
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)

	store := backup.NewManifestStore(sp)

	deployments, err := selectDeployments(cmd.Context(), store, opts.deployment)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	overall := []rotationPerDeployment{}

	for _, dep := range deployments {
		manifests, err := loadDeploymentManifests(cmd.Context(), store, dep, verifier)
		if err != nil {
			return output.NewError("rotate.list_failed",
				fmt.Sprintf("rotate: list %s: %v", dep, err)).Wrap(err)
		}
		decision := policy.Apply(now, manifests)

		// Filter out held backups BEFORE counting "Deleted" so the
		// dry-run report tells the truth: "would delete 5; held 1;
		// effective 4." Without the filter, --apply would silently
		// skip held backups (PutHold guards SoftDelete via the
		// IsHeld check), but the dry-run number would lie.
		filteredDelete, heldIDs, err := filterHeld(cmd.Context(), store, dep, decision.Delete)
		if err != nil {
			return output.NewError("rotate.hold_check_failed",
				fmt.Sprintf("rotate: %v", err)).Wrap(err)
		}

		report := rotationPerDeployment{
			Deployment: dep,
			Policy:     decision.PolicyName,
			Kept:       len(decision.Keep) + len(heldIDs),
			Deleted:    len(filteredDelete),
			Held:       len(heldIDs),
			HeldIDs:    heldIDs,
			Decisions:  shapeDecisions(decision, heldIDs),
		}

		if opts.apply {
			// Batch soft-delete: ONE scan of the deployment's manifests
			// for the whole delete set, not a full re-scan per backup
			// (which made a large rotation O(K·N) — CPU-pathology audit
			// #2). Held backups were already removed by filterHeld, and
			// the policy's finalize guarantees the set is chain-safe, so
			// the batch's chain + hold + race checks normally pass; any
			// refusal (a hold or incremental committed mid-rotation, or
			// a storage error) aborts atomically and the operator
			// re-runs.
			ids := make([]string, 0, len(filteredDelete))
			for _, m := range filteredDelete {
				ids = append(ids, m.BackupID)
			}
			deleted, derr := store.SoftDeleteBatch(cmd.Context(), dep, ids,
				decision.PolicyName, "policy="+decision.PolicyName)
			if derr != nil {
				return output.NewError("rotate.soft_delete_failed",
					fmt.Sprintf("rotate: soft-delete batch for %s: %v", dep, derr)).Wrap(derr)
			}
			report.Applied = len(deleted)
			report.HeldSkipped = 0

			// Audit one record per backup the policy deleted (observability
			// audit #4) — a retention run can soft-delete thousands of
			// backups, and previously left no per-backup trail, unlike
			// `backup delete`. Per-backup records make "which run deleted
			// what, when, under which policy" reconstructable.
			for _, id := range deleted {
				auditStore.AppendOrLog(cmd.Context(), &audit.Event{
					Action: "backup.rotate_delete",
					Subject: audit.Subject{
						Deployment: dep,
						BackupID:   id,
						Repo:       opts.repoURL,
					},
					Body: map[string]any{"policy": decision.PolicyName},
				})
			}
		}

		overall = append(overall, report)
	}

	body := rotateResultBody{
		DryRun:      !opts.apply,
		PolicyName:  policy.Name(),
		Deployments: overall,
		EvaluatedAt: now,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// buildPolicy constructs a retention.Policy from the flag set. Returns
// a structured usage error if --policy is unknown.
func buildPolicy(opts rotateOpts) (retention.Policy, error) {
	switch strings.ToLower(opts.policy) {
	case "gfs":
		return retention.GFSPolicy{
			KeepDaily:   opts.keepDaily,
			KeepWeekly:  opts.keepWeekly,
			KeepMonthly: opts.keepMonthly,
			KeepYearly:  opts.keepYearly,
		}, nil
	case "simple":
		if opts.keepFor <= 0 {
			return nil, output.NewError("usage.bad_flag",
				"rotate: --keep-for must be positive for the simple policy").Wrap(output.ErrUsage)
		}
		return retention.SimplePolicy{KeepFor: opts.keepFor}, nil
	case "count":
		if opts.keepFulls < 0 {
			return nil, output.NewError("usage.bad_flag",
				"rotate: --keep-fulls must be >= 0 for the count policy").Wrap(output.ErrUsage)
		}
		return retention.CountPolicy{KeepFulls: opts.keepFulls}, nil
	default:
		return nil, output.NewError("usage.unknown_policy",
			fmt.Sprintf("rotate: unknown --policy %q (supported: gfs|simple|count)", opts.policy)).
			Wrap(output.ErrUsage)
	}
}

// selectDeployments returns the deployment list to rotate over.
// Empty filter means "every deployment in the repo"; non-empty
// filter means "just this one" (validated to exist).
func selectDeployments(ctx context.Context, store *backup.ManifestStore, filter string) ([]string, error) {
	if filter != "" {
		// We don't strictly need to validate existence — an empty
		// deployment yields an empty Decision and a no-op rotation.
		// But explicit "no such deployment" feedback is friendlier.
		all, err := store.Deployments(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range all {
			if d == filter {
				return []string{filter}, nil
			}
		}
		return nil, output.NewError("notfound.deployment",
			fmt.Sprintf("rotate: no such deployment %q in repo (deployments: %s)",
				filter, strings.Join(all, ", ")))
	}
	return store.Deployments(ctx)
}

// loadDeploymentManifests reads every visible manifest for dep,
// verifying signatures. Tombstoned manifests are already filtered
// by ManifestStore.List.
func loadDeploymentManifests(ctx context.Context, store *backup.ManifestStore, dep string, verifier *backup.Verifier) ([]*backup.Manifest, error) {
	var out []*backup.Manifest
	for m, err := range store.List(ctx, dep, verifier) {
		if err != nil {
			// If a single manifest fails verification, skip it but
			// don't abort — operators want the rest of retention to
			// proceed. We surface the failure via the return error
			// after collecting; will plumb each per-manifest
			// failure through the dispatcher as a warning event.
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// shapeDecisions flattens the Decision into a JSON-friendly slice
// suitable for the Result body. heldIDs are backups the retention
// policy chose to delete but a legal hold protects — they must render
// as "held", NOT "delete", so the per-backup listing agrees with the
// summary's `held: N (excluded from delete)` line instead of stamping
// a legally-held manifest `[del ]`.
func shapeDecisions(d retention.Decision, heldIDs []string) []rotationDecision {
	held := map[string]bool{}
	for _, id := range heldIDs {
		held[id] = true
	}
	out := []rotationDecision{}
	for _, m := range d.Keep {
		out = append(out, rotationDecision{
			BackupID:  m.BackupID,
			Action:    "keep",
			StoppedAt: m.StoppedAt.UTC().Format(time.RFC3339),
			Reasons:   d.Reasons[m.BackupID],
		})
	}
	for _, m := range d.Delete {
		action := "delete"
		if held[m.BackupID] {
			action = "held"
		}
		out = append(out, rotationDecision{
			BackupID:  m.BackupID,
			Action:    action,
			StoppedAt: m.StoppedAt.UTC().Format(time.RFC3339),
		})
	}
	// Sort decisions newest-first by StoppedAt for stable rendering.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StoppedAt > out[j].StoppedAt
	})
	return out
}

// rotateResultBody is the typed Result body for `rotate`. Stable
// per the v1 schema commitment.
type rotateResultBody struct {
	DryRun      bool                    `json:"dry_run"`
	PolicyName  string                  `json:"policy_name"`
	EvaluatedAt time.Time               `json:"evaluated_at"`
	Deployments []rotationPerDeployment `json:"deployments"`
}

type rotationPerDeployment struct {
	Deployment  string             `json:"deployment"`
	Policy      string             `json:"policy"`
	Kept        int                `json:"kept"`
	Deleted     int                `json:"deleted"`
	Held        int                `json:"held,omitempty"`
	HeldIDs     []string           `json:"held_ids,omitempty"`
	Applied     int                `json:"applied,omitempty"`
	HeldSkipped int                `json:"held_skipped,omitempty"`
	Decisions   []rotationDecision `json:"decisions,omitempty"`
}

type rotationDecision struct {
	BackupID  string   `json:"backup_id"`
	Action    string   `json:"action"`
	StoppedAt string   `json:"stopped_at"`
	Reasons   []string `json:"reasons,omitempty"`
}

// WriteText is the text-renderer hook. Compact, scan-friendly.
func (b rotateResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.DryRun {
		fmt.Fprintln(bw, "Rotation plan (dry-run — no manifests soft-deleted)")
	} else {
		fmt.Fprintln(bw, "✓ Rotation applied")
	}
	fmt.Fprintf(bw, "  Policy: %s\n", b.PolicyName)
	for _, dep := range b.Deployments {
		fmt.Fprintf(bw, "\n  %s\n", dep.Deployment)
		fmt.Fprintf(bw, "    keep:    %d\n", dep.Kept)
		fmt.Fprintf(bw, "    delete:  %d\n", dep.Deleted)
		if dep.Held > 0 {
			fmt.Fprintf(bw, "    held:    %d (excluded from delete: %s)\n",
				dep.Held, strings.Join(dep.HeldIDs, ", "))
		}
		if !b.DryRun {
			fmt.Fprintf(bw, "    applied: %d\n", dep.Applied)
		}
		for _, d := range dep.Decisions {
			label := "keep"
			switch d.Action {
			case "delete":
				label = "del "
			case "held":
				label = "held"
			}
			reasons := strings.Join(d.Reasons, ",")
			if reasons != "" {
				reasons = "  (" + reasons + ")"
			}
			fmt.Fprintf(bw, "      [%s] %s @ %s%s\n", label, d.BackupID, d.StoppedAt, reasons)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}
