// hold.go — CLI surface for legal-hold markers on backups (add/remove/list/purge-expired).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

// newHoldCmd implements `pg_hardstorage hold` — the legal-hold
// command tree. A held backup is invisible to retention's
// soft-delete sweep regardless of policy outcome. Used for:
//
//   - regulatory holds ("preserve everything in this date range
//     pending litigation")
//   - operator-driven preservation ("I'm still debugging from
//     this; please don't reap it")
//   - SOC-2 / HIPAA evidence preservation
//
// Subcommands:
//
//	hold add <deployment> <backup-id> [--holder X] [--reason ...]
//	hold remove <deployment> <backup-id>
//	hold list [<deployment>]
//
// Holds are markers BESIDE the manifest (`<key>.hold` sibling),
// same shape as tombstones — see internal/backup/hold.go for the
// store-level API and rationale.
//
// What's deliberately NOT here:
//
//   - n-of-m approval (a follow-up — `hold remove` will
//     gain a confirmation flow once the approval scaffolding lands).
//   - Time-bounded holds with auto-expiry (a enhancement
//     to the marker schema; today's holds are forever-until-removed).
func newHoldCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hold",
		Short: "Legal hold — pin backups against retention sweeps",
		Long: `A held backup is excluded from ` + "`pg_hardstorage rotate`" + `'s
soft-delete sweep, regardless of the retention policy outcome.
The hold is recorded as a sibling marker file in the repository,
so it survives agent restarts and is visible across operators.

Removing a hold is the explicit ` + "`hold remove`" + ` operation; nothing
else clears it.`,
	}
	c.AddCommand(newHoldAddCmd())
	c.AddCommand(newHoldRemoveCmd())
	c.AddCommand(newHoldListCmd())
	c.AddCommand(newHoldPurgeExpiredCmd())
	return c
}

// newHoldPurgeExpiredCmd implements `pg_hardstorage hold
// purge-expired [<deployment>]`. Bulk cleanup of expired
// hold markers — the operational follow-up to the
// `hold.expired_present` doctor signal.
//
// Two scopes:
//   - hold purge-expired             # fleet-wide
//   - hold purge-expired <deployment># scoped to one
//
// Two modes:
//   - --dry-run         # preview only, no mutations
//   - --yes             # actually remove (audit-emitted)
//
// Indefinite holds are NEVER touched — the legal-hold default
// is preserved. Active bounded holds (future ExpiresAt) are
// also untouched. Only past-expiry markers are reaped.
func newHoldPurgeExpiredCmd() *cobra.Command {
	var (
		repoURL string
		dryRun  bool
		yes     bool
	)
	c := &cobra.Command{
		Use:   "purge-expired [<deployment>]",
		Short: "Remove every hold marker whose ExpiresAt is in the past",
		Long: `purge-expired sweeps expired hold markers. Indefinite
holds (no ExpiresAt) are NEVER touched — they're the legal-hold
default. Active bounded holds (future ExpiresAt) are also untouched.
Only past-expiry markers are reaped.

Pairs with the ` + "`" + `hold.expired_present` + "`" + ` doctor signal: doctor
flags expired markers as an operational hygiene issue; this
command cleans them up in one operation.

  pg_hardstorage hold purge-expired                 # fleet-wide
  pg_hardstorage hold purge-expired db1             # scoped to db1
  pg_hardstorage hold purge-expired --dry-run       # preview, no mutations
  pg_hardstorage hold purge-expired --yes           # actually remove

Each removal is audit-emitted (` + "`" + `hold.purge_expired` + "`" + ` action) so
the chain has a per-marker record. --dry-run emits nothing —
preview is a read-only operation.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := ""
			if len(args) == 1 {
				scope = args[0]
			}
			return runHoldPurgeExpired(cmd, scope, repoURL, dryRun, yes)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"preview the markers that would be removed (no mutations, no audit emits)")
	c.Flags().BoolVar(&yes, "yes", false,
		"confirm bulk removal — required unless --dry-run is set")
	return c
}

func runHoldPurgeExpired(cmd *cobra.Command, scope, repoURL string, dryRun, yes bool) error {
	d := DispatcherFrom(cmd)
	if !dryRun && !yes {
		return output.NewError("aborted.confirmation_required",
			"hold purge-expired: refusing bulk removal without --yes").
			WithSuggestion(&output.Suggestion{
				Human: "preview first with --dry-run to see what would be removed; re-run with --yes once you're sure. The removal is audit-emitted, so a recoverable history is preserved either way.",
			})
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if !dryRun {
		if err := assertRepoWritable(cmd.Context(), sp, "hold purge-expired"); err != nil {
			return err
		}
	}

	store := backup.NewManifestStore(sp)
	removed, perr := store.PurgeExpiredHolds(cmd.Context(), scope, dryRun)
	if perr != nil {
		// Mid-walk failure leaves partial state visible.
		// Surface the error along with the count we DID
		// remove so the operator can re-run cleanly (the
		// operation is idempotent).
		return output.NewError("hold.purge_failed",
			fmt.Sprintf("hold purge-expired: %v (%d removed before failure)", perr, len(removed))).
			WithSuggestion(&output.Suggestion{
				Human: "the operation is idempotent — re-run with the same arguments after fixing the underlying error to drain the rest.",
			}).Wrap(perr)
	}

	// Audit emit one event per removal (NOT a single bulk
	// event with a slice). Per-marker records make forensic
	// review of "who reaped what when" trivial. Skipped on
	// dry-run (no state change).
	if !dryRun && len(removed) > 0 {
		auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
		for _, r := range removed {
			ev := &audit.Event{
				Action: "hold.purge_expired",
				Subject: audit.Subject{
					Deployment: r.Deployment,
					BackupID:   r.BackupID,
					Repo:       repoURL,
				},
				Body: map[string]any{
					"holder":     r.Holder,
					"reason":     r.Reason,
					"held_at":    r.HeldAt.Format(time.RFC3339),
					"expired_at": r.ExpiredAt.Format(time.RFC3339),
				},
			}
			// Best-effort audit emit — a chain failure
			// shouldn't undo the removal (the marker is
			// already gone).
			_ = auditStore.Append(cmd.Context(), ev)
		}
	}

	rows := make([]holdPurgeRow, 0, len(removed))
	for _, r := range removed {
		rows = append(rows, holdPurgeRow{
			Deployment: r.Deployment,
			BackupID:   r.BackupID,
			Holder:     r.Holder,
			Reason:     r.Reason,
			HeldAt:     r.HeldAt.UTC().Format(time.RFC3339),
			ExpiredAt:  r.ExpiredAt.UTC().Format(time.RFC3339),
		})
	}
	// Stable order by (deployment, backup_id) for deterministic
	// JSON across runs.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Deployment != rows[j].Deployment {
			return rows[i].Deployment < rows[j].Deployment
		}
		return rows[i].BackupID < rows[j].BackupID
	})
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(holdPurgeBody{
		Scope:   scope,
		DryRun:  dryRun,
		Count:   len(rows),
		Removed: rows,
	}))
}

func newHoldAddCmd() *cobra.Command {
	var (
		repoURL string
		holder  string
		reason  string
		until   string
	)
	c := &cobra.Command{
		Use:   "add <deployment> <backup-id>",
		Short: "Place a legal hold on a backup",
		Long: `add places a hold marker on the named backup. Held manifests
refuse ` + "`" + `backup delete` + "`" + ` and are skipped by retention; the only
way to remove a hold is the explicit ` + "`" + `hold remove` + "`" + ` operation.

By default holds are indefinite (the legal-hold default — a
regulatory hold sticks around until explicitly released). Pass
--until to set an auto-expiry time; the marker stays on disk
for audit but no longer protects the manifest after that
moment. Useful for time-bounded debugging windows
("--until 14d") or finite litigation periods
("--until 2027-01-01").

--until accepts:
  - duration shorthand: "30d", "2w", "12h", "45m"
  - absolute time:      "2027-01-01T00:00:00Z" (RFC3339) or
                        "2027-01-01" (date only, midnight UTC)`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHoldAdd(cmd, args[0], args[1], repoURL, holder, reason, until)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&holder, "holder", "",
		"who placed the hold (free-form; appears in the marker for audit)")
	c.Flags().StringVar(&reason, "reason", "",
		"why the hold was placed (free-form; appears in the marker for audit)")
	c.Flags().StringVar(&until, "until", "",
		"auto-expire the hold at this time (e.g. '30d', '2w', '2027-01-01'); empty = indefinite")
	return c
}

func runHoldAdd(cmd *cobra.Command, deployment, backupID, repoURL, holder, reason, until string) error {
	d := DispatcherFrom(cmd)

	// Parse --until BEFORE opening the repo so a typo on the
	// expiry doesn't burn a network round-trip + return a
	// post-hoc usage error.
	var expiresAt time.Time
	if until != "" {
		t, perr := parseUntil(until, time.Now().UTC())
		if perr != nil {
			return output.NewError("usage.bad_until",
				fmt.Sprintf("hold add: --until %q: %v", until, perr)).Wrap(output.ErrUsage)
		}
		if !t.After(time.Now().UTC()) {
			return output.NewError("usage.bad_until",
				fmt.Sprintf("hold add: --until %q resolves to %s, which is not in the future",
					until, t.Format(time.RFC3339))).
				WithSuggestion(&output.Suggestion{
					Human: "use a future time (e.g. '30d' from now, or an absolute date in the future); a past expiry would be inert from creation.",
				}).Wrap(output.ErrUsage)
		}
		expiresAt = t
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	if err := store.PutHoldUntil(cmd.Context(), deployment, backupID, holder, reason, expiresAt); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("hold add: backup %s/%s not found", deployment, backupID)).
				WithSuggestion(&output.Suggestion{
					Human:   "list available backups with `pg_hardstorage list " + deployment + "`",
					Command: "pg_hardstorage list " + deployment + " --repo " + repoURL,
				}).Wrap(err)
		}
		return output.NewError("hold.put_failed",
			fmt.Sprintf("hold add: %v", err)).Wrap(err)
	}

	// Audit the legal-hold placement (observability audit #3): placing a
	// hold is a compliance-relevant action, so it must leave a chain
	// record — previously only the automatic purge-expired path did.
	addEv := &audit.Event{
		Action: "hold.add",
		Actor:  holder,
		Subject: audit.Subject{
			Deployment: deployment,
			BackupID:   backupID,
			Repo:       repoURL,
		},
		Body: map[string]any{
			"holder": holder,
			"reason": reason,
		},
	}
	if !expiresAt.IsZero() {
		addEv.Body["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), addEv)

	body := holdMutationBody{
		Deployment: deployment,
		BackupID:   backupID,
		Action:     "added",
		Holder:     holder,
		Reason:     reason,
	}
	if !expiresAt.IsZero() {
		exp := expiresAt
		body.ExpiresAt = &exp
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// parseUntil parses --until argument into a time.Time. Accepts
// duration shorthand ("30d", "2w", "12h", "45m") and the same
// absolute formats naturaltime.Parse handles (RFC3339, date-
// only, etc).
//
// Calendar-month and -year units are deliberately omitted —
// "1m" is ambiguous (minute or month?) and we want a single
// unambiguous spelling per unit. Operators wanting "approximately
// one year" use "365d" or an absolute date.
func parseUntil(s string, now time.Time) (time.Time, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return time.Time{}, errors.New("empty")
	}
	// Duration shorthand: <N>{d|w|h|m|s}. Reject ambiguous
	// "<N>m" interpretations as "month" (use "30d" / "365d" /
	// absolute date for those).
	if d, ok, err := parseUntilDuration(in); ok {
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	// Fall back to the absolute-time formats that naturaltime
	// already understands (RFC3339, date-only, etc).
	t, err := naturaltime.Parse(in, now)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// parseUntilDuration parses "<N><unit>" where unit ∈ {d,w,h,m,s}.
// Returns ok=false when the input doesn't match the shorthand
// shape (so the caller falls back to absolute-time parsing).
func parseUntilDuration(s string) (time.Duration, bool, error) {
	if len(s) < 2 {
		return 0, false, nil
	}
	unit := s[len(s)-1]
	num := s[:len(s)-1]
	mult := time.Duration(0)
	switch unit {
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	case 'h':
		mult = time.Hour
	case 'm':
		mult = time.Minute
	case 's':
		mult = time.Second
	default:
		return 0, false, nil
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, false, nil
	}
	if n <= 0 {
		return 0, true, fmt.Errorf("duration must be positive (got %d)", n)
	}
	return time.Duration(n) * mult, true, nil
}

func newHoldRemoveCmd() *cobra.Command {
	var (
		repoURL string
		yes     bool
	)
	c := &cobra.Command{
		Use:          "remove <deployment> <backup-id>",
		Short:        "Release a legal hold (the only way to clear one)",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHoldRemove(cmd, args[0], args[1], repoURL, yes)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&yes, "yes", false,
		"confirm removal (required — releasing a hold is auditable)")
	return c
}

func runHoldRemove(cmd *cobra.Command, deployment, backupID, repoURL string, yes bool) error {
	d := DispatcherFrom(cmd)
	if !yes {
		return output.NewError("aborted.confirmation_required",
			fmt.Sprintf("hold remove: refusing to release hold on %s/%s without --yes", deployment, backupID)).
			WithSuggestion(&output.Suggestion{
				Human: "releasing a hold is auditable — re-run with --yes once you're sure. Use `pg_hardstorage hold list` to see who placed the hold and why.",
			})
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	// Capture the hold's holder/reason BEFORE removing it so the audit
	// record can say whose hold was released and why.
	var prevHolder, prevReason string
	if h, gerr := store.GetHold(cmd.Context(), deployment, backupID); gerr == nil && h != nil {
		prevHolder, prevReason = h.Holder, h.Reason
	}
	if err := store.RemoveHold(cmd.Context(), deployment, backupID); err != nil {
		return output.NewError("hold.remove_failed",
			fmt.Sprintf("hold remove: %v", err)).Wrap(err)
	}

	// Audit the hold release (observability audit #3) — the command's own
	// help already promises "releasing a hold is auditable", but it
	// emitted no record. Now it does.
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action: "hold.remove",
		Actor:  prevHolder,
		Subject: audit.Subject{
			Deployment: deployment,
			BackupID:   backupID,
			Repo:       repoURL,
		},
		Body: map[string]any{
			"holder": prevHolder,
			"reason": prevReason,
		},
	})

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(holdMutationBody{
		Deployment: deployment,
		BackupID:   backupID,
		Action:     "removed",
	}))
}

func newHoldListCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "list [<deployment>]",
		Short:        "List active legal holds",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := ""
			if len(args) == 1 {
				scope = args[0]
			}
			return runHoldList(cmd, scope, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runHoldList(cmd *cobra.Command, scope, repoURL string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	holds, err := store.ListHolds(cmd.Context(), scope)
	if err != nil {
		return output.NewError("hold.list_failed",
			fmt.Sprintf("hold list: %v", err)).Wrap(err)
	}

	now := time.Now().UTC()
	rows := make([]holdListRow, 0, len(holds))
	for _, h := range holds {
		row := holdListRow{
			Deployment: h.Deployment,
			BackupID:   h.BackupID,
			Holder:     h.Holder,
			Reason:     h.Reason,
			HeldAt:     h.HeldAt.UTC().Format(time.RFC3339),
		}
		if h.ExpiresAt != nil {
			row.ExpiresAt = h.ExpiresAt.UTC().Format(time.RFC3339)
			row.Active = h.ActiveAt(now)
			row.Expired = !row.Active
		} else {
			row.Active = true // indefinite holds are always active
		}
		rows = append(rows, row)
	}
	// Stable order: deployment then backup_id.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Deployment != rows[j].Deployment {
			return rows[i].Deployment < rows[j].Deployment
		}
		return rows[i].BackupID < rows[j].BackupID
	})

	body := holdListBody{
		Scope: scope,
		Holds: rows,
		Count: len(rows),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// filterHeld removes any backup IDs from `decisionDelete` that have
// an active hold. Used by the rotate path so retention respects holds
// without retention itself having to know about them. Returns the
// kept-after-filter slice plus the deployment+backup-ids that were
// blocked, so the rotate report can surface them as a separate stat.
func filterHeld(ctx context.Context, store *backup.ManifestStore, deployment string, decisionDelete []*backup.Manifest) ([]*backup.Manifest, []string, error) {
	if len(decisionDelete) == 0 {
		return decisionDelete, nil, nil
	}
	keep := make([]*backup.Manifest, 0, len(decisionDelete))
	var held []string
	for _, m := range decisionDelete {
		// IsActivelyHeld respects ExpiresAt: an expired hold
		// no longer protects the manifest, so retention can
		// reap it. The marker stays on disk for audit until
		// an operator runs `hold remove` explicitly.
		ok, err := store.IsActivelyHeld(ctx, deployment, m.BackupID)
		if err != nil {
			return nil, nil, fmt.Errorf("hold check %s: %w", m.BackupID, err)
		}
		if ok {
			held = append(held, m.BackupID)
			continue
		}
		keep = append(keep, m)
	}
	return keep, held, nil
}

// Result body shapes — stable per the v1 schema commitment.

type holdMutationBody struct {
	Deployment string     `json:"deployment"`
	BackupID   string     `json:"backup_id"`
	Action     string     `json:"action"` // added | removed
	Holder     string     `json:"holder,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// WriteText renders the hold-add or hold-remove confirmation as human-readable
// text to w.
func (b holdMutationBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	switch b.Action {
	case "added":
		fmt.Fprintf(bw, "✓ Hold placed on %s/%s\n", b.Deployment, b.BackupID)
		if b.Holder != "" {
			fmt.Fprintf(bw, "  Holder:  %s\n", b.Holder)
		}
		if b.Reason != "" {
			fmt.Fprintf(bw, "  Reason:  %s\n", b.Reason)
		}
		if b.ExpiresAt != nil {
			fmt.Fprintf(bw, "  Expires: %s (auto-deactivates at this instant; remove explicitly to clean up the marker)\n",
				b.ExpiresAt.UTC().Format("2006-01-02 15:04:05 MST"))
		}
	case "removed":
		fmt.Fprintf(bw, "✓ Hold released on %s/%s", b.Deployment, b.BackupID)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type holdListRow struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	Holder     string `json:"holder,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HeldAt     string `json:"held_at"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Active     bool   `json:"active"`
	Expired    bool   `json:"expired,omitempty"`
}

type holdListBody struct {
	Scope string        `json:"scope,omitempty"` // empty = fleet-wide
	Count int           `json:"count"`
	Holds []holdListRow `json:"holds"`
}

// WriteText renders the active-hold listing as a tabular summary to w.
func (b holdListBody) WriteText(w io.Writer) error {
	if len(b.Holds) == 0 {
		_, err := fmt.Fprintln(w, "no active holds")
		return err
	}
	bw := &strings.Builder{}
	if b.Scope == "" {
		fmt.Fprintf(bw, "%d active hold(s) (fleet-wide)\n", b.Count)
	} else {
		fmt.Fprintf(bw, "%d active hold(s) on %s\n", b.Count, b.Scope)
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tBACKUP ID\tHELD AT\tEXPIRES\tHOLDER\tREASON")
	for _, h := range b.Holds {
		expCol := defaultIfEmpty(h.ExpiresAt, "(indefinite)")
		if h.Expired {
			expCol += " [EXPIRED]"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			h.Deployment, h.BackupID, h.HeldAt, expCol,
			defaultIfEmpty(h.Holder, "-"),
			defaultIfEmpty(h.Reason, "-"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// holdPurgeBody is the v1-stable result for hold purge-expired.
// DryRun reflects the mode; Count is the number of rows; the
// Removed slice is the per-marker forensic record. Empty
// Removed (nothing expired) is rendered as a friendly empty
// message in text mode rather than a no-row table.
type holdPurgeBody struct {
	Scope   string         `json:"scope,omitempty"` // empty = fleet-wide
	DryRun  bool           `json:"dry_run,omitempty"`
	Count   int            `json:"count"`
	Removed []holdPurgeRow `json:"removed"`
}

type holdPurgeRow struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	Holder     string `json:"holder,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HeldAt     string `json:"held_at"`
	ExpiredAt  string `json:"expired_at"`
}

// WriteText renders the purge-expired result as human-readable text to w,
// distinguishing dry-run from a real purge.
func (b holdPurgeBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Count == 0 {
		// No expired markers — clean. Distinguish dry-run
		// from real for clarity.
		switch {
		case b.DryRun:
			fmt.Fprintln(bw, "no expired hold markers (dry-run)")
		default:
			fmt.Fprintln(bw, "no expired hold markers — nothing to purge")
		}
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	verb := "✓ Removed"
	if b.DryRun {
		verb = "Would remove"
	}
	scope := b.Scope
	if scope == "" {
		scope = "(fleet-wide)"
	}
	fmt.Fprintf(bw, "%s %d expired hold marker(s) — %s\n", verb, b.Count, scope)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tBACKUP ID\tHELD AT\tEXPIRED AT\tHOLDER\tREASON")
	for _, r := range b.Removed {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			r.Deployment, r.BackupID, r.HeldAt, r.ExpiredAt,
			defaultIfEmpty(r.Holder, "-"),
			defaultIfEmpty(r.Reason, "-"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if b.DryRun {
		fmt.Fprintln(bw, "Re-run with --yes to perform the removal (each marker is audit-emitted).")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func defaultIfEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
