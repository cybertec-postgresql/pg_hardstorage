// status.go — 'status' CLI verb: last-backup state for one or all deployments.
package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRealStatusCmd implements `status [<deployment>]`.
func newRealStatusCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "status [<deployment>]",
		Short:        "Show last-backup state for one or all deployments",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deployment := ""
			if len(args) == 1 {
				deployment = args[0]
			}
			return runStatus(cmd, deployment, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runStatus(cmd *cobra.Command, deployment, repoURL string) error {
	d := DispatcherFrom(cmd)

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)

	var deployments []string
	if deployment != "" {
		deployments = []string{deployment}
	} else {
		deployments, err = store.Deployments(cmd.Context())
		if err != nil {
			return fmt.Errorf("status: enumerate deployments: %w", err)
		}
	}

	body := statusBody{Generated: time.Now().UTC(), Repo: repoURL}

	//: per-deployment logical-stream registry (no PG round-trip;
	// just reads the local state-file registry). Operators wanting
	// real lag run `pg_hardstorage logical status <name>
	// --pg-connection ...` which connects to PG.
	streamsByDep := loadStreamsByDeployment()

	for _, dep := range deployments {
		ds := summarizeDeployment(cmd.Context(), store, dep, verifier)
		if names := streamsByDep[dep]; len(names) > 0 {
			sort.Strings(names)
			ds.LogicalStreams = names
		}
		body.Deployments = append(body.Deployments, ds)
	}

	//: repo-level audit-anchor freshness + pending-approvals
	// count. Both are best-effort: failures surface as fields rather
	// than aborting the whole command. Same posture as doctor's
	// per-repo checks.
	body.AuditAnchor = computeAnchorStatus(cmd.Context(), sp, repoMeta)
	body.PendingApprovals = countPendingApprovals(cmd.Context(), sp)

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// loadStreamsByDeployment groups the registered logical-decoding
// streams by deployment name. Returns nil when the registry's
// state file is absent (a clean install with no streams). A read
// error returns nil too — status is read-only and shouldn't block
// on registry trouble.
func loadStreamsByDeployment() map[string][]string {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil
	}
	mgr := logical.NewManager(filepath.Join(p.State.Value, "logical_streams.json"))
	streams, err := mgr.List()
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, s := range streams {
		out[s.Deployment] = append(out[s.Deployment], s.Name)
	}
	return out
}

// computeAnchorStatus is the same probe doctor runs, packaged for
// the status body. Empty chain is healthy; chain-with-no-anchor is
// stale; chain-with-stale-anchor reports BehindEvents.
func computeAnchorStatus(ctx context.Context, sp storage.StoragePlugin, repoMeta *repo.Metadata) anchorStatus {
	a := anchorStatus{}
	chainKeys := 0
	for info, err := range sp.List(ctx, "audit/") {
		if err != nil {
			a.ProbeError = err.Error()
			return a
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		if info.Key == audit.HeadKey {
			continue
		}
		if strings.HasPrefix(info.Key, audit.AnchorPrefix) {
			continue
		}
		// Per-shard head pointers (audit/shards/<shard>/_head.json) are
		// perf caches, not chain events — counting them inflated the
		// event count by one per shard. Exclude them like the global
		// HeadKey above.
		if strings.HasPrefix(info.Key, "audit/shards/") && strings.HasSuffix(info.Key, "/_head.json") {
			continue
		}
		chainKeys++
	}
	a.ChainEventCount = chainKeys

	log := audit.NewStorageBackedLogWithRetention(sp, repoMeta.WORM)
	latest, err := log.LatestAnchor(ctx)
	if err != nil {
		a.ProbeError = err.Error()
		return a
	}
	if latest == nil {
		// No anchor at all; chain emptiness decides healthy / not.
		a.Fresh = chainKeys == 0
		return a
	}
	a.Present = true
	a.HeadSequence = latest.HeadSequence
	// Freshness compares the anchor's head sequence against the CURRENT
	// head sequence of the SAME shard it witnesses, read from that
	// shard's authoritative head pointer — NOT the chainKeys-1 event
	// count, which is wrong under WORM retention pruning and across
	// shards (see doctor.go readAuditHeadSequence + the anchor-freshness
	// notes there). Mirror that logic.
	headSeq, ok := readAuditHeadSequence(ctx, sp, latest.Shard)
	if !ok {
		// No readable head pointer — falling back to the retention-
		// truncated count is the false positive we're avoiding. Treat
		// the present anchor as fresh rather than emit a guess.
		a.Fresh = true
		return a
	}
	if latest.HeadSequence >= headSeq {
		a.Fresh = true
		return a
	}
	a.BehindEvents = int(headSeq - latest.HeadSequence)
	return a
}

// countPendingApprovals walks the approvals/ prefix and counts
// requests in StatusPending. Approved / expired / revoked requests
// don't count — operators want "what still needs sign-off?" Errors
// during the walk return 0 + best-effort.
func countPendingApprovals(ctx context.Context, sp storage.StoragePlugin) int {
	store := approval.NewStore(sp)
	pending, err := store.List(ctx, approval.ListFilters{Status: approval.StatusPending})
	if err != nil {
		return 0
	}
	return len(pending)
}

// summarizeDeployment scans every committed backup for one deployment
// and produces a deploymentStatus snapshot. Errors on individual
// manifests are counted in Skipped rather than aborting the rollup.
//
// Walks ListIncludingTombstoned so the rollup sees both live AND
// tombstoned manifests in a single pass — counts both into BackupCount
// and TombstonedCount respectively. The latest-backup pointer is
// always picked from the LIVE set (a tombstoned manifest isn't
// "the most recent backup" for status purposes).
func summarizeDeployment(ctx context.Context, store *backup.ManifestStore, deployment string, verifier *backup.Verifier) deploymentStatus {
	s := deploymentStatus{Deployment: deployment, Healthy: true}
	var latest *backup.Manifest
	for entry, err := range store.ListIncludingTombstoned(ctx, deployment, verifier) {
		if err != nil {
			s.SkippedCount++
			s.Healthy = false
			continue
		}
		if entry.Tombstoned {
			s.TombstonedCount++
			continue
		}
		s.BackupCount++
		if latest == nil || entry.Manifest.StoppedAt.After(latest.StoppedAt) {
			latest = entry.Manifest
		}
	}
	if latest != nil {
		s.LatestBackupID = latest.BackupID
		s.LatestStopLSN = latest.StopLSN
		s.LatestTimeline = latest.Timeline
		s.LatestType = string(latest.Type)
		s.LatestStoppedAt = latest.StoppedAt
		age := time.Since(latest.StoppedAt)
		s.LatestAgeMS = age.Milliseconds()
		s.LatestAgeHuman = humanDuration(age)
	}

	// Hold counts. Cheap List walk over the deployment's
	// hold tree, classifying by ActiveAt(now). A failure
	// here is non-fatal — we record the count we got and
	// move on (a torn hold body shouldn't break status).
	if holds, herr := store.ListHolds(ctx, deployment); herr == nil {
		now := time.Now().UTC()
		for _, h := range holds {
			if h.ActiveAt(now) {
				s.ActiveHolds++
			} else {
				s.ExpiredHolds++
			}
		}
	}
	return s
}

type deploymentStatus struct {
	Deployment      string    `json:"deployment"`
	BackupCount     int       `json:"backup_count"`
	SkippedCount    int       `json:"skipped_count,omitempty"`
	Healthy         bool      `json:"healthy"`
	LatestBackupID  string    `json:"latest_backup_id,omitempty"`
	LatestType      string    `json:"latest_type,omitempty"`
	LatestStoppedAt time.Time `json:"latest_stopped_at,omitempty"`
	LatestStopLSN   string    `json:"latest_stop_lsn,omitempty"`
	LatestTimeline  uint32    `json:"latest_timeline,omitempty"`
	LatestAgeMS     int64     `json:"latest_age_ms,omitempty"`
	LatestAgeHuman  string    `json:"latest_age_human,omitempty"`

	// Lifecycle counts surface the soft-delete + hold posture at
	// a glance — "how much retained data is on a held manifest?",
	// "how many tombstoned backups are sitting around waiting for
	// chunk-GC?", "are there expired holds I should clean up?".
	// All omitempty so a deployment with none of them keeps the
	// JSON shape minimal (24-month-compat regression-tested).
	TombstonedCount int `json:"tombstoned_count,omitempty"`
	ActiveHolds     int `json:"active_holds,omitempty"`
	ExpiredHolds    int `json:"expired_holds,omitempty"`

	// LogicalStreams names every registered logical-decoding stream
	// for this deployment. Empty when none configured. Real-time
	// lag is a separate concern (`pg_hardstorage logical status
	// <name> --pg-connection ...`) so status stays fast.
	LogicalStreams []string `json:"logical_streams,omitempty"`
}

// anchorStatus summarises the repo's transparency-log anchor
// freshness — the same probe doctor's repo section runs.
type anchorStatus struct {
	Present         bool   `json:"present"`
	Fresh           bool   `json:"fresh"`
	ChainEventCount int    `json:"chain_event_count"`
	HeadSequence    int64  `json:"head_sequence,omitempty"`
	BehindEvents    int    `json:"behind_events,omitempty"`
	ProbeError      string `json:"probe_error,omitempty"`
}

type statusBody struct {
	Generated        time.Time          `json:"generated"`
	Repo             string             `json:"repo"`
	Deployments      []deploymentStatus `json:"deployments"`
	AuditAnchor      anchorStatus       `json:"audit_anchor"`
	PendingApprovals int                `json:"pending_approvals"`
}

// WriteText renders a per-deployment table plus the repo-level
// anchor + approvals summary at the bottom. Stable ordering across
// runs.
func (b statusBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Deployments) == 0 {
		fmt.Fprintln(bw, "No deployments visible in this repository.")
	} else {
		deps := make([]deploymentStatus, len(b.Deployments))
		copy(deps, b.Deployments)
		sort.Slice(deps, func(i, j int) bool { return deps[i].Deployment < deps[j].Deployment })

		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  DEPLOYMENT\tBACKUPS\tHEALTH\tLATEST\tAGE\tSTOP LSN\tTLI\tSTREAMS")
		for _, dep := range deps {
			health := "✓"
			if !dep.Healthy {
				health = fmt.Sprintf("✗ (%d skipped)", dep.SkippedCount)
			}
			latestID := dep.LatestBackupID
			if latestID == "" {
				latestID = "-"
			}
			age := dep.LatestAgeHuman
			if age == "" {
				age = "-"
			}
			stopLSN := dep.LatestStopLSN
			if stopLSN == "" {
				stopLSN = "-"
			}
			tli := "-"
			if dep.LatestTimeline > 0 {
				tli = fmt.Sprintf("%d", dep.LatestTimeline)
			}
			streams := "-"
			if len(dep.LogicalStreams) > 0 {
				streams = fmt.Sprintf("%d", len(dep.LogicalStreams))
			}
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				dep.Deployment, dep.BackupCount, health, latestID, age, stopLSN, tli, streams)
			// Lifecycle continuation line: only when there's
			// something to show. Keeps the default table
			// narrow but surfaces tombstoned/holds when
			// they exist.
			lifecycle := lifecycleSummary(dep)
			if lifecycle != "" {
				// NO tab characters here: a tab-free line is a single
				// trailing cell, which tabwriter excludes from column
				// sizing. With the trailing tabs this 60+-char note
				// became column 1's width and blew the whole table
				// ~50 columns wide (plus a line of trailing spaces).
				fmt.Fprintf(tw, "    └─ %s\n", lifecycle)
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	// Repo-level footer. Compact one-liners so a glance answers
	// the "is anything pending sign-off / out of date?" question.
	fmt.Fprintf(bw, "\n  Audit anchor: %s\n", b.AuditAnchor.summary())
	if b.PendingApprovals > 0 {
		fmt.Fprintf(bw, "  Pending approvals: %d (run `pg_hardstorage approval list --status pending`)\n",
			b.PendingApprovals)
	} else {
		fmt.Fprintln(bw, "  Pending approvals: 0")
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// lifecycleSummary returns the per-deployment soft-delete + hold
// continuation-line text. Empty when none of the relevant
// counts are non-zero (so deployments with a clean lifecycle
// posture don't get a noisy second line).
func lifecycleSummary(s deploymentStatus) string {
	parts := []string{}
	if s.TombstonedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tombstoned (chunks reclaimed by next `repo gc --apply`)", s.TombstonedCount))
	}
	if s.ActiveHolds > 0 {
		parts = append(parts, fmt.Sprintf("%d active hold(s)", s.ActiveHolds))
	}
	if s.ExpiredHolds > 0 {
		parts = append(parts, fmt.Sprintf("%d expired hold(s) — `hold remove` to clean up", s.ExpiredHolds))
	}
	return strings.Join(parts, "; ")
}

// summary returns the one-line operator-readable form of an
// anchorStatus.
func (a anchorStatus) summary() string {
	if a.ProbeError != "" {
		return "✗ probe failed: " + a.ProbeError
	}
	if !a.Present {
		if a.ChainEventCount == 0 {
			return "✓ chain empty (no anchor needed yet)"
		}
		return fmt.Sprintf("⚠ none (%d event(s) un-anchored — run `pg_hardstorage audit anchor`)", a.ChainEventCount)
	}
	if a.Fresh {
		return fmt.Sprintf("✓ fresh (sequence %d)", a.HeadSequence)
	}
	return fmt.Sprintf("⚠ stale: %d event(s) behind (run `pg_hardstorage audit anchor`)", a.BehindEvents)
}

// humanDuration formats d as a coarse human string. Goal is "what
// would a tired operator at 3am want to read in a glance?". We round
// generously: anything under a minute is "Ns"; minutes / hours / days
// follow.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
