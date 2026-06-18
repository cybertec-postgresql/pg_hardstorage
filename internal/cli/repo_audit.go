// repo_audit.go — 'repo audit' CLI verb: comprehensive read-only fleet-wide state report.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repoaudit"
)

// newRepoAuditCmd implements `pg_hardstorage repo audit <url>` —
// the comprehensive read-only repository state report.
//
// Different from the other repo subcommands:
//
//   - `repo usage`  — object count + bytes per category (storage)
//   - `repo check`  — repository invariants (HSREPO present, etc.)
//   - `repo scrub`  — bit-rot detection (chunk integrity)
//   - `repo gc`     — chunk garbage collection
//
// `repo audit` is the single-command "tell me everything about my
// repo state" view: per-deployment lifecycle, fleet-wide KEK-ref
// breakdown, schema-version distribution, replica completeness,
// audit-chain summary, storage usage, and WORM mode — all in one
// pass. The natural target for compliance audits, capacity-planning
// reports, and "what's going on with my fleet?" sweeps.
//
// Read-only by construction: safe against a read-only or WORM-locked
// repo, in production, at any cadence.
func newRepoAuditCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		skipStor   bool
		skipChain  bool
		skipAppr   bool
	)
	c := &cobra.Command{
		Use:   "audit <url>",
		Short: "Comprehensive read-only repository state report",
		Long: `repo audit walks every deployment + every visible manifest in the
repo and produces a comprehensive state report covering:

  - per-deployment lifecycle (active / tombstoned / held counts,
    oldest / newest backup, latest backup metadata, encryption
    posture, KEK refs, schema versions, PG-version spread,
    timeline spread)
  - fleet-wide rollups (KEK ref → manifest count, schema-version
    distribution, replica completeness)
  - audit-chain summary (event count, last-anchor age,
    head-pointer presence)
  - storage usage (objects / bytes per category — same shape as
    repo usage)
  - approval-request lifecycle counts
  - WORM mode + retention from HSREPO

The report is a snapshot of FACTS, not a verdict — it surfaces
the data an operator needs to answer "are all my fleets'
backups encrypted under the expected KEK?", "how many manifests
are still on the old schema?", "which deployments are missing
replica copies?" — without prescribing what's "broken". For
operational health checks (something is actively wrong), use
` + "`pg_hardstorage doctor`" + `; for per-backup integrity, use
` + "`pg_hardstorage verify`" + `.

Read-only by construction; safe at any cadence.

Optional opt-outs cut runtime on huge fleets:
  --no-storage     skip the per-category storage walk
  --no-chain       skip the audit-chain summary
  --no-approvals   skip the approval-request lifecycle scan
`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo audit: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runRepoAudit(cmd, repoURL, deployment, skipStor, skipChain, skipAppr)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict the per-deployment section to one deployment (fleet rollups still cover all deployments)")
	c.Flags().BoolVar(&skipStor, "no-storage", false,
		"skip the per-category storage usage scan (cheaper on huge repos)")
	c.Flags().BoolVar(&skipChain, "no-chain", false,
		"skip the audit-chain summary (cheaper on long chains)")
	c.Flags().BoolVar(&skipAppr, "no-approvals", false,
		"skip the approval-request lifecycle scan")
	return c
}

func runRepoAudit(cmd *cobra.Command, repoURL, deployment string, skipStor, skipChain, skipAppr bool) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the first positional <url>)")
	}
	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	meta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	rep, err := repoaudit.Audit(cmd.Context(), sp, meta, repoURL, repoaudit.Options{
		Verifier:         verifier,
		DeploymentFilter: deployment,
		SkipStorage:      skipStor,
		SkipAuditChain:   skipChain,
		SkipApprovals:    skipAppr,
	})
	if err != nil {
		return output.NewError("repo.audit_failed",
			fmt.Sprintf("repo audit: %v", err)).Wrap(err)
	}

	body := repoAuditBody{Report: rep}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// repoAuditBody wraps the domain Report with a stable text-renderer
// hook. The Report itself is the v1 JSON contract; this type only
// adds the presentation layer.
type repoAuditBody struct {
	*repoaudit.Report
}

// MarshalJSON forwards to the underlying Report so the JSON shape is
// the v1 contract. Without this method the wrapper would emit
// {"Report":{...}} — wrong.
func (b repoAuditBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Report)
}

// WriteText renders the report as a human-friendly summary. Layout
// is deterministic — each section is its own paragraph — so an
// operator skimming the output can find a deployment, a KEK, a
// schema-version row by eye.
func (b repoAuditBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	r := b.Report
	fmt.Fprintf(bw, "repo audit — %s\n", r.URL)
	if r.Repo != nil {
		fmt.Fprintf(bw, "  ID:        %s\n", r.Repo.ID)
		if r.Repo.CreatedAt != "" {
			fmt.Fprintf(bw, "  Created:   %s\n", r.Repo.CreatedAt)
		}
		if r.Repo.Mode != "" {
			fmt.Fprintf(bw, "  Mode:      %s\n", r.Repo.Mode)
		}
		if r.Repo.WORMMode != "" {
			fmt.Fprintf(bw, "  WORM:      %s (retention %s)\n",
				r.Repo.WORMMode, durationFromSeconds(r.Repo.WORMRetentionSeconds))
		}
	}
	fmt.Fprintf(bw, "  Audited:   %s in %d ms\n",
		r.GeneratedAt.Format(time.RFC3339), r.DurationMS)
	if r.DeploymentFilter != "" {
		fmt.Fprintf(bw, "  Filter:    deployment %q\n", r.DeploymentFilter)
	}
	fmt.Fprintln(bw)

	// Per-deployment section.
	if len(r.Deployments) == 0 {
		fmt.Fprintln(bw, "Deployments: (none)")
	} else {
		fmt.Fprintln(bw, "Deployments:")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tACTIVE\tTOMB\tHELD\tEXP-HELD\tSIG-FAIL\tPOSTURE\tLATEST")
		for _, d := range r.Deployments {
			latest := "(none)"
			if d.Latest != nil {
				latest = d.Latest.BackupID
			}
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
				d.Name, d.Active, d.Tombstoned, d.Held, d.HeldExpired,
				d.SignatureFailed, d.EncryptionPosture, latest)
		}
		_ = tw.Flush()
		fmt.Fprintln(bw)
		// Per-deployment KEK / schema / timeline detail.
		for _, d := range r.Deployments {
			if len(d.KEKRefs) == 0 && len(d.PGVersions) == 0 && len(d.Timelines) == 0 {
				continue
			}
			fmt.Fprintf(bw, "  %s:\n", d.Name)
			if len(d.KEKRefs) > 0 {
				fmt.Fprintf(bw, "    KEK refs:    %s\n", strings.Join(d.KEKRefs, ", "))
			}
			if len(d.PGVersions) > 0 {
				fmt.Fprintf(bw, "    PG versions: %s\n", joinInts(d.PGVersions))
			}
			if len(d.Timelines) > 0 {
				fmt.Fprintf(bw, "    Timelines:   %s\n", joinUint32s(d.Timelines))
			}
			if len(d.Schemas) > 1 {
				fmt.Fprintf(bw, "    Schemas:     %s (mixed; consider re-anchoring)\n",
					strings.Join(d.Schemas, ", "))
			}
		}
		fmt.Fprintln(bw)
	}

	// Fleet rollups.
	if len(r.KEKRefs) > 0 {
		fmt.Fprintln(bw, "KEK ref breakdown (fleet-wide):")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		// Sort by manifest count desc for the most-significant
		// rollup at the top. The body's slice is sorted by ref
		// alphabetically (stable JSON); we re-sort for the text
		// layout.
		refs := append([]repoaudit.KEKRefSummary(nil), r.KEKRefs...)
		sort.Slice(refs, func(i, j int) bool { return refs[i].ManifestCount > refs[j].ManifestCount })
		for _, k := range refs {
			fmt.Fprintf(tw, "  %s\t%d\n", k.KEKRef, k.ManifestCount)
		}
		_ = tw.Flush()
		fmt.Fprintln(bw)
	}
	if len(r.SchemaVersions) > 0 {
		fmt.Fprintln(bw, "Manifest schema distribution:")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		for _, s := range r.SchemaVersions {
			fmt.Fprintf(tw, "  %s\t%d\n", s.Schema, s.ManifestCount)
		}
		_ = tw.Flush()
		fmt.Fprintln(bw)
	}
	if r.Replicas != nil {
		fmt.Fprintln(bw, "Replica completeness:")
		fmt.Fprintf(bw, "  Primary manifests:        %d\n", r.Replicas.PrimaryManifests)
		fmt.Fprintf(bw, "  Replica manifests:        %d\n", r.Replicas.ReplicaManifests)
		if r.Replicas.UnreplicatedPrimaries > 0 {
			fmt.Fprintf(bw, "  ✗ Unreplicated primaries: %d\n", r.Replicas.UnreplicatedPrimaries)
		}
		if r.Replicas.OrphanedReplicas > 0 {
			fmt.Fprintf(bw, "  ✗ Orphaned replicas:      %d (replica with no matching primary)\n",
				r.Replicas.OrphanedReplicas)
		}
		fmt.Fprintln(bw)
	}
	if r.Approvals != nil {
		fmt.Fprintf(bw, "Approval requests: %d total", r.Approvals.Total)
		if r.Approvals.Pending+r.Approvals.Approved+r.Approvals.Expired+r.Approvals.Revoked > 0 {
			fmt.Fprintf(bw, " (pending=%d approved=%d expired=%d revoked=%d)",
				r.Approvals.Pending, r.Approvals.Approved, r.Approvals.Expired, r.Approvals.Revoked)
		}
		fmt.Fprintln(bw)
		fmt.Fprintln(bw)
	}
	if r.Chain != nil {
		fmt.Fprintln(bw, "Audit chain:")
		fmt.Fprintf(bw, "  Events:           %d\n", r.Chain.EventCount)
		fmt.Fprintf(bw, "  Head pointer:     %v\n", r.Chain.HeadHashAvailable)
		if r.Chain.AnchorCount > 0 {
			fmt.Fprintf(bw, "  Anchors:          %d\n", r.Chain.AnchorCount)
			fmt.Fprintf(bw, "  Last anchored:    %s",
				r.Chain.LastAnchorAt.Format(time.RFC3339))
			if r.Chain.LastAnchorAgeMS > 0 {
				fmt.Fprintf(bw, " (%s ago)",
					(time.Duration(r.Chain.LastAnchorAgeMS) * time.Millisecond).Truncate(time.Second))
			}
			fmt.Fprintln(bw)
		} else if r.Chain.EventCount > 0 {
			fmt.Fprintln(bw, "  Anchors:          (none — `audit anchor` to publish)")
		}
		fmt.Fprintln(bw)
	}
	if r.Storage != nil {
		fmt.Fprintln(bw, "Storage usage:")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  CATEGORY\tOBJECTS\tBYTES")
		for _, c := range r.Storage.Categories {
			fmt.Fprintf(tw, "  %s\t%d\t%s\n", c.Category, c.Objects, humanBytes(c.Bytes))
		}
		fmt.Fprintf(tw, "  --\t--\t--\n")
		fmt.Fprintf(tw, "  TOTAL\t%d\t%s\n", r.Storage.TotalObjects, humanBytes(r.Storage.TotalBytes))
		_ = tw.Flush()
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// durationFromSeconds is a presentation helper. Renders integer
// seconds as a coarse human duration ("30d", "8760h", "0s").
func durationFromSeconds(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int64(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	default:
		return d.String()
	}
}

func joinInts(in []int) string {
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ", ")
}

func joinUint32s(in []uint32) string {
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ", ")
}
