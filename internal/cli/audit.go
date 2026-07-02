// audit.go — CLI surface for the tamper-evident audit log (append, search, summary, verify, anchor).
package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newAuditCmd implements `pg_hardstorage audit` — the operator-facing
// surface of the hash-chained audit log.
//
// Subcommands:
//
//	audit append <action> [--actor X] [--reason Y] [--body file]
//	audit search [--action X] [--actor Y] [--since 24h] [--until ...] [--limit N]
//	audit verify-chain
//
// `append` is rare-but-supported for two reasons:
//
//  1. Operator-driven action records ("manually rotated KEK out of
//     band; record this in the audit chain") that don't have a
//     code-path emitting the event automatically.
//  2. Test fixtures, which need a way to plant a chain of known
//     events for verify-chain regression tests.
//
// Future code paths (backup commit, hold place, kms rotate, ...)
// will append events automatically; today's CLI surface lets
// operators inspect what's there.
func newAuditCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "audit",
		Short: "Query the hash-chained audit log",
		Long: `Each audit event records an operator-visible action and links
to the prior event via SHA-256 of the prior event's canonical JSON.
Tampering with any historical event invalidates every subsequent
event's prev_hash; ` + "`audit verify-chain`" + ` walks the chain and
reports both kinds of finding (hash mismatch on an event, or chain
break between events).`,
	}
	c.AddCommand(newAuditAppendCmd())
	c.AddCommand(newAuditSearchCmd())
	c.AddCommand(newAuditSummaryCmd())
	c.AddCommand(newAuditVerifyChainCmd())
	c.AddCommand(newAuditAnchorCmd())
	c.AddCommand(newAuditVerifyAnchorCmd())
	c.AddCommand(newAuditExportBundleCmd())
	c.AddCommand(newAuditVerifyBundleCmd())
	return c
}

func newAuditAppendCmd() *cobra.Command {
	var (
		repoURL    string
		actor      string
		tenant     string
		deployment string
		reason     string
	)
	c := &cobra.Command{
		Use:          "append <action>",
		Short:        "Record a new event in the audit chain",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditAppend(cmd, args[0], repoURL, actor, tenant, deployment, reason)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&actor, "actor", "",
		"operator principal (free-form; appears in the event for audit)")
	c.Flags().StringVar(&tenant, "tenant", "",
		"tenant scope (default: deployment's tenant or 'default')")
	c.Flags().StringVar(&deployment, "deployment", "",
		"deployment subject (optional)")
	c.Flags().StringVar(&reason, "reason", "",
		"free-form reason; lands in body.reason")
	return c
}

func runAuditAppend(cmd *cobra.Command, action, repoURL, actor, tenant, deployment, reason string) error {
	d := DispatcherFrom(cmd)
	if action == "" {
		return output.NewError("usage.missing_arg",
			"audit append: action is required").Wrap(output.ErrUsage)
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	ev := &audit.Event{
		Action: action,
		Actor:  actor,
		Tenant: tenant,
		Subject: audit.Subject{
			Deployment: deployment,
			Tenant:     tenant,
			Repo:       repoURL,
		},
	}
	if reason != "" {
		ev.Body = map[string]any{"reason": reason}
	}
	if err := store.Append(cmd.Context(), ev); err != nil {
		return output.NewError("audit.append_failed",
			fmt.Sprintf("audit append: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(auditAppendBody{
		ID:        ev.ID,
		Sequence:  ev.Sequence,
		Action:    ev.Action,
		Hash:      ev.Hash,
		PrevHash:  ev.PrevHash,
		Timestamp: ev.Timestamp.UTC().Format(time.RFC3339),
	}))
}

func newAuditSearchCmd() *cobra.Command {
	var (
		repoURL       string
		action        string
		actionPrefix  string
		actor         string
		actorContains string
		tenant        string
		deployment    string
		backupID      string
		since         string
		until         string
		limit         int
		reverse       bool
	)
	c := &cobra.Command{
		Use:   "search",
		Short: "Query audit events with filters",
		Long: `search walks the audit chain and returns events matching
the supplied filters. Filters AND-combine: an event must match
every non-zero criterion to be included.

Common forensic queries:

  # Everything in the last 24h, newest first
  audit search --repo <url> --since 24h --reverse

  # Every backup-namespace action against db1
  audit search --repo <url> --deployment db1 --action-prefix backup.

  # Just the deletes of one specific backup
  audit search --repo <url> --backup-id db1.full.20260501T0900Z

  # Anything done by an operator whose email contains "ops@"
  audit search --repo <url> --actor-contains ops@

--action vs --action-prefix: the former is the strict match
("backup.delete" only); the latter is the namespace-loose match
("backup." captures backup.create, backup.delete, backup.undelete).
Pass both for the strictest possible match (the strict --action
must share the loose --action-prefix or no events match).

--reverse + --limit returns the N most-recent matching events,
which is the natural "what happened recently?" pagination.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditSearch(cmd, auditSearchOpts{
				repoURL:       repoURL,
				action:        action,
				actionPrefix:  actionPrefix,
				actor:         actor,
				actorContains: actorContains,
				tenant:        tenant,
				deployment:    deployment,
				backupID:      backupID,
				since:         since,
				until:         until,
				limit:         limit,
				reverse:       reverse,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&action, "action", "", "filter on exact action match (e.g. 'backup.create')")
	c.Flags().StringVar(&actionPrefix, "action-prefix", "", "filter on action namespace prefix (e.g. 'backup.' captures backup.*)")
	c.Flags().StringVar(&actor, "actor", "", "filter on exact actor match")
	c.Flags().StringVar(&actorContains, "actor-contains", "", "filter on substring match against actor (partial-principal lookup)")
	c.Flags().StringVar(&tenant, "tenant", "", "filter on exact tenant match")
	c.Flags().StringVar(&deployment, "deployment", "", "filter on exact deployment match against the event Subject")
	c.Flags().StringVar(&backupID, "backup-id", "", "filter on exact backup-id match against the event Subject")
	c.Flags().StringVar(&since, "since", "",
		"include events at or after this time (RFC3339 absolute or duration like 24h, 7d)")
	c.Flags().StringVar(&until, "until", "",
		"include events strictly before this time (same format as --since)")
	c.Flags().IntVar(&limit, "limit", 0,
		"cap the number of returned events (0 = no cap); combine with --reverse for the N most-recent")
	c.Flags().BoolVar(&reverse, "reverse", false,
		"yield newest-first (commit order otherwise)")
	return c
}

// auditSearchOpts groups the search-flag bag so adding fields
// later doesn't churn the signature of every caller.
type auditSearchOpts struct {
	repoURL       string
	action        string
	actionPrefix  string
	actor         string
	actorContains string
	tenant        string
	deployment    string
	backupID      string
	since         string
	until         string
	limit         int
	reverse       bool
}

func runAuditSearch(cmd *cobra.Command, opts auditSearchOpts) error {
	d := DispatcherFrom(cmd)
	since, err := parseSinceUntil(opts.since)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit search: --since: %v", err)).Wrap(output.ErrUsage)
	}
	until, err := parseSinceUntil(opts.until)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit search: --until: %v", err)).Wrap(output.ErrUsage)
	}

	repoMeta, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	events, err := store.Search(cmd.Context(), audit.ListFilters{
		Action:        opts.action,
		ActionPrefix:  opts.actionPrefix,
		Actor:         opts.actor,
		ActorContains: opts.actorContains,
		Tenant:        opts.tenant,
		Deployment:    opts.deployment,
		BackupID:      opts.backupID,
		Since:         since,
		Until:         until,
		Limit:         opts.limit,
		Reverse:       opts.reverse,
	})
	if err != nil {
		return output.NewError("audit.search_failed",
			fmt.Sprintf("audit search: %v", err)).Wrap(err)
	}

	rows := make([]auditSearchRow, 0, len(events))
	for _, ev := range events {
		rows = append(rows, auditSearchRow{
			ID:         ev.ID,
			Sequence:   ev.Sequence,
			Timestamp:  ev.Timestamp.UTC().Format(time.RFC3339),
			Action:     ev.Action,
			Actor:      ev.Actor,
			Tenant:     ev.Tenant,
			Deployment: ev.Subject.Deployment,
			BackupID:   ev.Subject.BackupID,
			Hash:       ev.Hash,
			PrevHash:   ev.PrevHash,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(auditSearchBody{
		Count:  len(rows),
		Events: rows,
	}))
}

// newAuditSummaryCmd implements `pg_hardstorage audit summary` —
// the compliance-friendly counts-by-action rollup. Same filter
// shape as `audit search`; emits per-action counts + a total
// rather than the events themselves. Useful for monthly
// reports ("our 30-day backup-delete count went from 2 to
// 47 — what changed?") and for spot-checks during incident
// review.
func newAuditSummaryCmd() *cobra.Command {
	var (
		repoURL       string
		actionPrefix  string
		actor         string
		actorContains string
		tenant        string
		deployment    string
		backupID      string
		since         string
		until         string
	)
	c := &cobra.Command{
		Use:   "summary",
		Short: "Aggregate audit-event counts by action",
		Long: `summary is the count-by-action rollup over the same
filter surface as ` + "`audit search`" + `. Useful for compliance reports
and incident review:

  # Every action's 30-day count for db1
  audit summary --repo <url> --since 720h --deployment db1

  # All backup-namespace actions in the last week
  audit summary --repo <url> --since 7d --action-prefix backup.

  # Per-tenant deletion volume
  audit summary --repo <url> --since 720h --action backup.delete

The result is a map of action → count plus a total, rendered as
a JSON body or a tabular text view.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditSummary(cmd, auditSummaryOpts{
				repoURL:       repoURL,
				actionPrefix:  actionPrefix,
				actor:         actor,
				actorContains: actorContains,
				tenant:        tenant,
				deployment:    deployment,
				backupID:      backupID,
				since:         since,
				until:         until,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&actionPrefix, "action-prefix", "", "filter on action namespace prefix (e.g. 'backup.')")
	c.Flags().StringVar(&actor, "actor", "", "filter on exact actor match")
	c.Flags().StringVar(&actorContains, "actor-contains", "", "filter on substring match against actor")
	c.Flags().StringVar(&tenant, "tenant", "", "filter on exact tenant match")
	c.Flags().StringVar(&deployment, "deployment", "", "filter on exact deployment match")
	c.Flags().StringVar(&backupID, "backup-id", "", "filter on exact backup-id match")
	c.Flags().StringVar(&since, "since", "",
		"include events at or after this time (RFC3339 absolute or duration like 24h, 7d)")
	c.Flags().StringVar(&until, "until", "",
		"include events strictly before this time")
	return c
}

type auditSummaryOpts struct {
	repoURL       string
	actionPrefix  string
	actor         string
	actorContains string
	tenant        string
	deployment    string
	backupID      string
	since         string
	until         string
}

func runAuditSummary(cmd *cobra.Command, opts auditSummaryOpts) error {
	d := DispatcherFrom(cmd)
	since, err := parseSinceUntil(opts.since)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit summary: --since: %v", err)).Wrap(output.ErrUsage)
	}
	until, err := parseSinceUntil(opts.until)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit summary: --until: %v", err)).Wrap(output.ErrUsage)
	}
	repoMeta, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	counts, total, err := store.SummaryByAction(cmd.Context(), audit.ListFilters{
		ActionPrefix:  opts.actionPrefix,
		Actor:         opts.actor,
		ActorContains: opts.actorContains,
		Tenant:        opts.tenant,
		Deployment:    opts.deployment,
		BackupID:      opts.backupID,
		Since:         since,
		Until:         until,
	})
	if err != nil {
		return output.NewError("audit.summary_failed",
			fmt.Sprintf("audit summary: %v", err)).Wrap(err)
	}

	rows := make([]auditSummaryRow, 0, len(counts))
	for action, count := range counts {
		rows = append(rows, auditSummaryRow{Action: action, Count: count})
	}
	// Sort: highest count first; ties broken alphabetically
	// for determinism. Operators reading "what happened most"
	// expect the most-frequent action up top.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Action < rows[j].Action
	})
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(auditSummaryBody{
		Total:   total,
		Counts:  rows,
		Filters: opts.summaryFilters(),
	}))
}

// summaryFilters builds the result-body's "filters" echo block —
// the exact filters that produced the rollup, so a downstream
// consumer can interpret a non-zero result without re-parsing
// flags. Empty filters are omitted (omitempty on the wrapping
// struct fields).
func (o auditSummaryOpts) summaryFilters() auditSummaryFilters {
	return auditSummaryFilters{
		ActionPrefix:  o.actionPrefix,
		Actor:         o.actor,
		ActorContains: o.actorContains,
		Tenant:        o.tenant,
		Deployment:    o.deployment,
		BackupID:      o.backupID,
		Since:         o.since,
		Until:         o.until,
	}
}

// parseSinceUntil accepts an empty string (zero time), a duration
// (interpreted as "N ago", e.g. "24h" → time.Now()-24h), or an
// absolute RFC3339 timestamp. The duration form is the more common
// operator-facing usage.
func parseSinceUntil(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	// The flag help advertises day-granularity forms ("7d") but
	// time.ParseDuration only knows up to hours, so it rejects any "d".
	// Rewrite a leading day count into hours before parsing so both
	// "7d" and combined forms like "7d12h" work as documented.
	if d, ok := parseDurationWithDays(s); ok {
		return time.Now().UTC().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp or duration like 24h / 7d; got %q", s)
}

// parseDurationWithDays extends time.ParseDuration with a day unit: a
// leading "<int>d" is converted to the equivalent hours and any
// remaining duration ("12h30m") is appended. Returns ok=false when the
// input isn't a recognisable duration (so the caller can fall back to
// timestamp parsing).
func parseDurationWithDays(s string) (time.Duration, bool) {
	// Plain time.ParseDuration first — the common case (24h, 90m).
	if d, err := time.ParseDuration(s); err == nil {
		return d, true
	}
	// Look for a "<digits>d" prefix.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != 'd' {
		return 0, false
	}
	days, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	dayPart := time.Duration(days) * 24 * time.Hour
	rest := s[i+1:]
	if rest == "" {
		return dayPart, true
	}
	// Anything after the day component must be a normal duration.
	tail, err := time.ParseDuration(rest)
	if err != nil {
		return 0, false
	}
	return dayPart + tail, true
}

func newAuditVerifyChainCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "verify-chain",
		Short:        "Walk the audit log and assert hash-chain integrity",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditVerifyChain(cmd, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runAuditVerifyChain(cmd *cobra.Command, repoURL string) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	res, err := store.VerifyChain(cmd.Context())
	if err != nil {
		return output.NewError("audit.verify_failed",
			fmt.Sprintf("audit verify-chain: %v", err)).Wrap(err)
	}
	body := auditVerifyBody{
		EventsChecked:  res.EventsChecked,
		HashMismatches: res.HashMismatches,
		ChainBreaks:    res.ChainBreaks,
		OK:             res.OK,
	}
	if !res.OK {
		// verify.* namespace → ExitVerifyFailed (9). Tampered audit
		// log is a real corruption finding.
		return output.NewError("verify.audit_chain_broken",
			fmt.Sprintf("audit verify-chain: %d hash mismatch(es), %d chain break(s) across %d event(s)",
				len(res.HashMismatches), len(res.ChainBreaks), res.EventsChecked)).
			WithSuggestion(&output.Suggestion{
				Human:   "the audit log has been tampered with or storage corruption has hit it. Investigate the audit/ prefix in the repo.",
				Command: "pg_hardstorage audit search --repo " + repoURL + " --limit 100",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// --- anchor / verify-anchor ----------------------------------------------

// newAuditAnchorCmd writes the chain-head hash to the repo's
// transparency-log namespace (audit/anchors/...). ships the
// self-hosted StorageBackedLog; the same shape backs a real Sigstore
// Rekor backend.
func newAuditAnchorCmd() *cobra.Command {
	var (
		repoURL     string
		publisherID string
	)
	c := &cobra.Command{
		Use:          "anchor",
		Short:        "Publish the chain head into the transparency log",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditAnchor(cmd, repoURL, publisherID)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&publisherID, "publisher", "",
		"opaque label for who anchored (e.g. control-plane node ID)")
	return c
}

func runAuditAnchor(cmd *cobra.Command, repoURL, publisherID string) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	log := audit.NewStorageBackedLogWithRetention(sp, repoMeta.WORM)
	// Anchor every shard so the whole sharded chain is witnessed.
	anchors, err := store.AnchorAll(cmd.Context(), log, publisherID)
	if err != nil {
		return output.NewError("audit.anchor_failed",
			fmt.Sprintf("audit anchor: %v", err)).Wrap(err)
	}
	if len(anchors) == 0 {
		return output.NewError("audit.anchor_failed",
			"audit anchor: chain is empty; nothing to anchor")
	}
	body := auditAnchorBody{Count: len(anchors)}
	for _, a := range anchors {
		body.Anchors = append(body.Anchors, auditAnchorEntry{
			Shard:         a.Shard,
			LogID:         a.LogID,
			ChainHeadHash: a.ChainHeadHash,
			HeadSequence:  a.HeadSequence,
		})
	}
	// Legacy top-level fields describe a representative anchor (the
	// global chain if it was anchored, else the first), so existing
	// single-anchor consumers keep working.
	rep := anchors[0]
	for _, a := range anchors {
		if a.Shard == "" {
			rep = a
			break
		}
	}
	body.LogID = rep.LogID
	body.ChainHeadHash = rep.ChainHeadHash
	body.HeadSequence = rep.HeadSequence
	body.AnchoredAt = rep.AnchoredAt.Format(time.RFC3339)
	body.PublisherID = rep.PublisherID
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// newAuditVerifyAnchorCmd reads a previously-published anchor and
// asserts the local chain still agrees. Mismatch → ExitVerifyFailed
// via verify.* namespace.
func newAuditVerifyAnchorCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "verify-anchor <log-id>",
		Short:        "Compare the named anchor against the local chain",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditVerifyAnchor(cmd, args[0], repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runAuditVerifyAnchor(cmd *cobra.Command, logID, repoURL string) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	log := audit.NewStorageBackedLogWithRetention(sp, repoMeta.WORM)
	res, err := store.VerifyAnchor(cmd.Context(), log, logID)
	if err != nil {
		return output.NewError("audit.verify_anchor_failed",
			fmt.Sprintf("audit verify-anchor: %v", err)).Wrap(err)
	}
	body := auditVerifyAnchorBody{
		LogID:             res.LogID,
		ChainHeadHash:     res.ChainHeadHash,
		HeadSequence:      res.HeadSequence,
		LocalHeadHash:     res.LocalHeadHash,
		LocalHeadSequence: res.LocalHeadSequence,
		OK:                res.OK,
		Mismatch:          res.Mismatch,
	}
	if !res.OK {
		return output.NewError("verify.audit_anchor_mismatch",
			fmt.Sprintf("audit verify-anchor: %s", res.Mismatch)).
			WithSuggestion(&output.Suggestion{
				Human: "the local chain disagrees with the published anchor — the chain has been tampered with or truncated since the anchor was written. Run audit verify-chain for the per-event picture.",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// Result body shapes — stable per the v1 schema commitment.

type auditAppendBody struct {
	ID        string `json:"id"`
	Sequence  int64  `json:"sequence"`
	Action    string `json:"action"`
	Hash      string `json:"hash"`
	PrevHash  string `json:"prev_hash"`
	Timestamp string `json:"timestamp"`
}

// WriteText renders the appended event's hash chain coordinates as
// human-readable text to w.
func (b auditAppendBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ audit event appended\n  ID:        %s\n  Sequence:  %d\n  Action:    %s\n  Timestamp: %s\n  Hash:      %s\n  PrevHash:  %s",
		b.ID, b.Sequence, b.Action, b.Timestamp, b.Hash, b.PrevHash)
	return err
}

type auditSearchRow struct {
	ID         string `json:"id"`
	Sequence   int64  `json:"sequence"`
	Timestamp  string `json:"timestamp"`
	Action     string `json:"action"`
	Actor      string `json:"actor,omitempty"`
	Tenant     string `json:"tenant,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	BackupID   string `json:"backup_id,omitempty"`
	Hash       string `json:"hash"`
	PrevHash   string `json:"prev_hash"`
}

type auditSearchBody struct {
	Count  int              `json:"count"`
	Events []auditSearchRow `json:"events"`
}

// WriteText renders the matching audit events as a tabular summary to w.
func (b auditSearchBody) WriteText(w io.Writer) error {
	if b.Count == 0 {
		_, err := fmt.Fprintln(w, "no audit events match")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d audit event(s)\n", b.Count)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SEQ\tTIME\tACTION\tACTOR\tDEPLOYMENT\tBACKUP\tID")
	for _, r := range b.Events {
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Sequence, r.Timestamp, r.Action,
			defaultIfEmpty(r.Actor, "-"),
			defaultIfEmpty(r.Deployment, "-"),
			defaultIfEmpty(r.BackupID, "-"),
			r.ID)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// auditSummaryRow / Body / Filters are the v1-stable result
// shape for `audit summary`. Filters echoes the inputs so a
// downstream consumer sees what produced the rollup without
// re-parsing flags. All filter fields are omitempty so an
// unfiltered summary keeps the body compact.
type auditSummaryRow struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

type auditSummaryBody struct {
	Total   int                 `json:"total"`
	Counts  []auditSummaryRow   `json:"counts"`
	Filters auditSummaryFilters `json:"filters"`
}

type auditSummaryFilters struct {
	ActionPrefix  string `json:"action_prefix,omitempty"`
	Actor         string `json:"actor,omitempty"`
	ActorContains string `json:"actor_contains,omitempty"`
	Tenant        string `json:"tenant,omitempty"`
	Deployment    string `json:"deployment,omitempty"`
	BackupID      string `json:"backup_id,omitempty"`
	Since         string `json:"since,omitempty"`
	Until         string `json:"until,omitempty"`
}

// WriteText renders the per-action event rollup as human-readable text to w.
func (b auditSummaryBody) WriteText(w io.Writer) error {
	if b.Total == 0 {
		_, err := fmt.Fprintln(w, "no audit events match")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d audit event(s) — grouped by action\n", b.Total)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ACTION\tCOUNT")
	for _, r := range b.Counts {
		fmt.Fprintf(tw, "  %s\t%d\n", r.Action, r.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type auditVerifyBody struct {
	EventsChecked  int      `json:"events_checked"`
	HashMismatches []string `json:"hash_mismatches,omitempty"`
	ChainBreaks    []string `json:"chain_breaks,omitempty"`
	OK             bool     `json:"ok"`
}

// WriteText renders the chain-verify outcome — including any hash mismatches
// or chain breaks — as human-readable text to w.
func (b auditVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "audit verify-chain\n")
	fmt.Fprintf(bw, "  events checked: %d\n", b.EventsChecked)
	if b.OK {
		fmt.Fprintln(bw, "  ✓ chain intact")
	} else {
		if len(b.HashMismatches) > 0 {
			fmt.Fprintf(bw, "  ✗ %d hash mismatch(es): %s\n",
				len(b.HashMismatches), strings.Join(b.HashMismatches, ", "))
		}
		if len(b.ChainBreaks) > 0 {
			fmt.Fprintf(bw, "  ✗ %d chain break(s): %s\n",
				len(b.ChainBreaks), strings.Join(b.ChainBreaks, ", "))
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// auditAnchorBody is the v1 result of `audit anchor`. LogID is the
// deterministic identifier the storage-backed log derived from the
// chain-head hash + sequence, so re-anchoring the same chain head
// returns the same ID.
type auditAnchorBody struct {
	// Count + Anchors describe every shard anchored this run.
	Count   int                `json:"count"`
	Anchors []auditAnchorEntry `json:"anchors,omitempty"`
	// Legacy single-anchor fields describe a representative anchor (the
	// global chain if present, else the first), for backward compat.
	LogID         string `json:"log_id"`
	ChainHeadHash string `json:"chain_head_hash"`
	HeadSequence  int64  `json:"head_sequence"`
	AnchoredAt    string `json:"anchored_at"`
	PublisherID   string `json:"publisher_id,omitempty"`
}

// auditAnchorEntry is one shard's anchor in a multi-shard anchor run.
type auditAnchorEntry struct {
	Shard         string `json:"shard,omitempty"`
	LogID         string `json:"log_id"`
	ChainHeadHash string `json:"chain_head_hash"`
	HeadSequence  int64  `json:"head_sequence"`
}

// WriteText renders the anchor publication result as human-readable text to w.
func (b auditAnchorBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ anchored %d chain(s)\n", b.Count)
	for _, a := range b.Anchors {
		shard := a.Shard
		if shard == "" {
			shard = "global"
		}
		fmt.Fprintf(bw, "  [%s] log=%s seq=%d head=%s\n", shard, a.LogID, a.HeadSequence, a.ChainHeadHash)
	}
	if b.PublisherID != "" {
		fmt.Fprintf(bw, "  Publisher:  %s\n", b.PublisherID)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// auditVerifyAnchorBody is the v1 result of `audit verify-anchor`.
// OK=true means the local chain at the anchored sequence has the
// same hash as the published anchor; mismatch text describes the
// finding when not OK.
type auditVerifyAnchorBody struct {
	LogID             string `json:"log_id"`
	ChainHeadHash     string `json:"chain_head_hash"`
	HeadSequence      int64  `json:"head_sequence"`
	LocalHeadHash     string `json:"local_head_hash,omitempty"`
	LocalHeadSequence int64  `json:"local_head_sequence,omitempty"`
	OK                bool   `json:"ok"`
	Mismatch          string `json:"mismatch,omitempty"`
}

// WriteText renders the anchor-verify outcome as human-readable text to w,
// noting any mismatch between the local chain head and the published anchor.
func (b auditVerifyAnchorBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "audit verify-anchor %s\n", b.LogID)
	fmt.Fprintf(bw, "  Sequence:    %d\n", b.HeadSequence)
	fmt.Fprintf(bw, "  Anchor hash: %s\n", b.ChainHeadHash)
	if b.LocalHeadHash != "" {
		fmt.Fprintf(bw, "  Local hash:  %s\n", b.LocalHeadHash)
	}
	if b.OK {
		fmt.Fprintln(bw, "  ✓ chain matches anchor")
	} else {
		fmt.Fprintf(bw, "  ✗ %s\n", b.Mismatch)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
