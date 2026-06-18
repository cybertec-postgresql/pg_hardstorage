// approval.go — CLI surface for the n-of-m approval workflow gating destructive ops.
package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newApprovalCmd is the root of the approval workflow surface. The
// destructive ops the plan calls out — `kms shred`, `repo gc --delete`,
// `backup delete --force`, `repo wipe` — gate on requests in this
// chain. ships the workflow primitives; per-op `--require-approval`
// wiring lands as each destructive op grows the gate.
func newApprovalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "approval",
		Short: "n-of-m approval workflow for destructive operations",
		Long: `Multi-operator gate for the kind of action that can't be undone.
An initiator creates a Request specifying the op (e.g. backup.delete),
the target, a reason, a TTL, an approval threshold N, and the public
keys of the M operators allowed to approve. Each approver fetches
the request, decides yes/no, and signs an approval that lands inside
the Request's Approvals slice. When N distinct allowlisted approvers
have signed, the request flips to "approved" and the destructive op
can proceed.

Approvals are signed with each approver's ed25519 keypair (the same
shape as the manifest-signing keys we already use). Tampering with
the request invalidates every existing signature.`,
	}
	c.AddCommand(newApprovalRequestCmd())
	c.AddCommand(newApprovalApproveCmd())
	c.AddCommand(newApprovalStatusCmd())
	c.AddCommand(newApprovalListCmd())
	c.AddCommand(newApprovalRevokeCmd())
	c.AddCommand(newApprovalPurgeExpiredCmd())
	return c
}

// newApprovalPurgeExpiredCmd records expiry for requests past their TTL.
func newApprovalPurgeExpiredCmd() *cobra.Command {
	var (
		repoURL string
		yes     bool
		dryRun  bool
	)
	c := &cobra.Command{
		Use:   "purge-expired",
		Short: "Record expiry for approval requests past their TTL (audited)",
		Long: `Sweep every approval request that has passed its TTL without
reaching its approval threshold (and was not revoked), and record the
expiry in the audit chain as an ` + "`approval.expire`" + ` event — one per
request.

A request's status is otherwise a DERIVED state (computed from its TTL),
so an expired request leaves no audit-chain record and the compliance
report's expired-request count stays zero. This sweep gives expiry a
trail, mirroring ` + "`hold purge-expired`" + ` for legal holds.

The sweep is idempotent: a request whose expiry has already been recorded
is skipped, so re-running emits nothing new.

  pg_hardstorage approval purge-expired --dry-run   # preview only
  pg_hardstorage approval purge-expired --yes       # record expiry`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalPurgeExpired(cmd, repoURL, dryRun, yes)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"preview the requests that would be recorded as expired (no mutations, no audit emits)")
	c.Flags().BoolVar(&yes, "yes", false,
		"confirm the sweep — required unless --dry-run is set")
	return c
}

// approvalPurgeExpiredBody is the structured result of the sweep.
type approvalPurgeExpiredBody struct {
	Recorded   int      `json:"recorded"`
	DryRun     bool     `json:"dry_run"`
	RequestIDs []string `json:"request_ids,omitempty"`
}

func runApprovalPurgeExpired(cmd *cobra.Command, repoURL string, dryRun, yes bool) error {
	d := DispatcherFrom(cmd)
	if !dryRun && !yes {
		return output.NewError("aborted.confirmation_required",
			"approval purge-expired: refusing the sweep without --yes").
			WithSuggestion(&output.Suggestion{
				Human: "preview first with --dry-run to see which requests would be recorded as expired; re-run with --yes once you're sure. The record is audit-emitted, not destructive.",
			})
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if !dryRun {
		if err := assertRepoWritable(cmd.Context(), sp, "approval purge-expired"); err != nil {
			return err
		}
	}

	store := approval.NewStore(sp)
	expired, serr := store.SweepExpired(cmd.Context(), dryRun)
	if serr != nil {
		return output.NewError("approval.expire_failed",
			fmt.Sprintf("approval purge-expired: %v (%d recorded before failure)", serr, len(expired))).
			WithSuggestion(&output.Suggestion{
				Human: "the sweep is idempotent — re-run with the same arguments after fixing the underlying error to record the rest.",
			}).Wrap(serr)
	}

	// One audit event per newly-expired request (not a single bulk event)
	// so a forensic walk sees per-request expiry. Skipped on dry-run (no
	// state change). Best-effort: a chain failure must not undo the
	// ExpiredAt stamp already written.
	ids := make([]string, 0, len(expired))
	if !dryRun && len(expired) > 0 {
		auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
		for _, r := range expired {
			_ = auditStore.Append(cmd.Context(), &audit.Event{
				Action: "approval.expire",
				Subject: audit.Subject{
					Tenant: r.Tenant,
					Repo:   repoURL,
				},
				Body: map[string]any{
					"request_id": r.ID,
					"op":         string(r.Op),
					"target":     r.Target,
					"initiator":  r.Initiator,
					"expires_at": r.ExpiresAt.Format(time.RFC3339),
				},
			})
		}
	}
	for _, r := range expired {
		ids = append(ids, r.ID)
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(approvalPurgeExpiredBody{
		Recorded:   len(expired),
		DryRun:     dryRun,
		RequestIDs: ids,
	}))
}

func newApprovalRequestCmd() *cobra.Command {
	var (
		repoURL      string
		op           string
		target       string
		reason       string
		tenant       string
		threshold    int
		ttl          time.Duration
		approverKeys []string
	)
	c := &cobra.Command{
		Use:          "request",
		Short:        "Create a new n-of-m approval request",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runApprovalRequest(cmd, repoURL, op, target, reason, tenant, threshold, ttl, approverKeys)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&op, "op", "", "namespaced op being approved (e.g. backup.delete, kms.shred) (required)")
	_ = c.MarkFlagRequired("op")
	c.Flags().StringVar(&target, "target", "", "target identifier (e.g. a backup ID)")
	c.Flags().StringVar(&reason, "reason", "", "free-form reason for the destructive op")
	c.Flags().StringVar(&tenant, "tenant", "", "tenant scope")
	c.Flags().IntVar(&threshold, "threshold", 2, "number of distinct approvals required (≥ 1)")
	c.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long this request stays approvable")
	c.Flags().StringArrayVar(&approverKeys, "approver-key", nil,
		"path to an approver's ed25519 public-key PEM (repeatable; need at least --threshold)")
	return c
}

func runApprovalRequest(cmd *cobra.Command, repoURL, op, target, reason, tenant string, threshold int, ttl time.Duration, approverKeyFiles []string) error {
	d := DispatcherFrom(cmd)
	if len(approverKeyFiles) == 0 {
		return output.NewError("usage.missing_flag",
			"approval request: --approver-key is required (repeatable, supply at least --threshold)").Wrap(output.ErrUsage)
	}
	keys := make([][]byte, 0, len(approverKeyFiles))
	for _, path := range approverKeyFiles {
		body, err := os.ReadFile(path)
		if err != nil {
			return output.NewError("usage.bad_approver_key",
				fmt.Sprintf("approval request: read %s: %v", path, err)).Wrap(output.ErrUsage)
		}
		keys = append(keys, body)
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := approval.NewStore(sp)
	req, err := store.Create(cmd.Context(), approval.CreateOptions{
		Op:           approval.Op(op),
		Initiator:    initiatorFromEnv(),
		Target:       target,
		Reason:       reason,
		Tenant:       tenant,
		Threshold:    threshold,
		ApproverKeys: keys,
		TTL:          ttl,
	})
	if err != nil {
		return output.NewError("approval.create_failed",
			fmt.Sprintf("approval request: %v", err)).Wrap(err)
	}

	// Audit-chain emission — best-effort; failure surfaces as a
	// warning event but the request is committed.
	auditEv := &audit.Event{
		Action: "approval.request",
		Actor:  req.Initiator,
		Tenant: req.Tenant,
		Subject: audit.Subject{
			Tenant: req.Tenant,
			Repo:   repoURL,
		},
		Body: map[string]any{
			"approval_id":    req.ID,
			"op":             string(req.Op),
			"target":         req.Target,
			"reason":         req.Reason,
			"threshold":      req.Threshold,
			"approver_count": len(req.ApproverKeys),
			"expires_at":     req.ExpiresAt.Format(time.RFC3339),
		},
	}
	if err := audit.NewStoreWithRetention(sp, repoMeta.WORM).Append(cmd.Context(), auditEv); err != nil {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "approval", "audit_append_failed").
			WithBody(map[string]any{"error": err.Error()}))
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(approvalRequestBody{
		ID:           req.ID,
		Op:           string(req.Op),
		Target:       req.Target,
		Reason:       req.Reason,
		Threshold:    req.Threshold,
		ApproverKeys: len(req.ApproverKeys),
		Initiator:    req.Initiator,
		CreatedAt:    req.CreatedAt,
		ExpiresAt:    req.ExpiresAt,
	}))
}

func newApprovalApproveCmd() *cobra.Command {
	var (
		repoURL    string
		approverID string
		reason     string
		keyFile    string
	)
	c := &cobra.Command{
		Use:          "approve <request-id>",
		Short:        "Approve an outstanding request",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalApprove(cmd, args[0], repoURL, approverID, reason, keyFile)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&approverID, "approver", "", "operator-readable approver ID (e.g. email; appears in the audit chain)")
	c.Flags().StringVar(&reason, "reason", "", "free-form reason; appears in the approval record")
	c.Flags().StringVar(&keyFile, "key", "",
		"path to the approver's ed25519 PRIVATE-key PEM (defaults to the local keyring's signing key)")
	return c
}

func runApprovalApprove(cmd *cobra.Command, id, repoURL, approverID, reason, keyFile string) error {
	d := DispatcherFrom(cmd)

	priv, err := loadPrivateKeyForApproval(keyFile)
	if err != nil {
		return err
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := approval.NewStore(sp)
	req, err := store.Approve(cmd.Context(), id, priv, approverID, reason)
	if err != nil {
		switch {
		case errors.Is(err, approval.ErrNotFound):
			return output.NewError("notfound.approval",
				fmt.Sprintf("approval approve: request %q not found", id)).Wrap(err)
		case errors.Is(err, approval.ErrExpired):
			return output.NewError("approval.expired",
				fmt.Sprintf("approval approve: request %q has expired", id)).Wrap(err)
		case errors.Is(err, approval.ErrRevoked):
			return output.NewError("approval.revoked",
				fmt.Sprintf("approval approve: request %q has been revoked", id)).Wrap(err)
		case errors.Is(err, approval.ErrApproverNotAllowed):
			return output.NewError("auth.approver_not_allowed",
				"approval approve: your public key is not in this request's approver allowlist").Wrap(err)
		}
		return output.NewError("approval.approve_failed",
			fmt.Sprintf("approval approve: %v", err)).Wrap(err)
	}

	st := approval.StatusPending
	if c, _ := approval.VerifyApprovals(req); c >= req.Threshold {
		st = approval.StatusApproved
	}

	auditEv := &audit.Event{
		Action: "approval.approve",
		Actor:  approverID,
		Tenant: req.Tenant,
		Subject: audit.Subject{
			Tenant: req.Tenant,
			Repo:   repoURL,
		},
		Body: map[string]any{
			"approval_id":    req.ID,
			"op":             string(req.Op),
			"target":         req.Target,
			"approval_count": len(req.Approvals),
			"threshold":      req.Threshold,
			"status_after":   string(st),
		},
	}
	if err := audit.NewStoreWithRetention(sp, repoMeta.WORM).Append(cmd.Context(), auditEv); err != nil {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "approval", "audit_append_failed").
			WithBody(map[string]any{"error": err.Error()}))
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(approvalApproveBody{
		ID:            req.ID,
		Op:            string(req.Op),
		Status:        string(st),
		ApprovalCount: len(req.Approvals),
		Threshold:     req.Threshold,
	}))
}

func newApprovalStatusCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "status <request-id>",
		Short:        "Show the current status of a request",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalStatus(cmd, args[0], repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runApprovalStatus(cmd *cobra.Command, id, repoURL string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := approval.NewStore(sp)
	req, err := store.Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, approval.ErrNotFound) {
			return output.NewError("notfound.approval",
				fmt.Sprintf("approval status: request %q not found", id)).Wrap(err)
		}
		return output.NewError("approval.read_failed",
			fmt.Sprintf("approval status: %v", err)).Wrap(err)
	}
	st, _ := store.StatusOf(cmd.Context(), id)
	count, _ := approval.VerifyApprovals(req)

	body := approvalStatusBody{
		ID:            req.ID,
		Op:            string(req.Op),
		Initiator:     req.Initiator,
		Target:        req.Target,
		Reason:        req.Reason,
		Threshold:     req.Threshold,
		ApprovalCount: count,
		Status:        string(st),
		ExpiresAt:     req.ExpiresAt,
	}
	for _, a := range req.Approvals {
		body.Approvals = append(body.Approvals, approvalEntry{
			Approver:       a.Approver,
			KeyFingerprint: a.KeyFingerprint,
			At:             a.At,
			Reason:         a.Reason,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newApprovalListCmd() *cobra.Command {
	var (
		repoURL string
		opF     string
		statusF string
		tenantF string
	)
	c := &cobra.Command{
		Use:          "list",
		Short:        "List approval requests in the repo",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runApprovalList(cmd, repoURL, opF, statusF, tenantF)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&opF, "op", "", "filter by op")
	c.Flags().StringVar(&statusF, "status", "", "filter by status: pending|approved|expired|revoked")
	c.Flags().StringVar(&tenantF, "tenant", "", "filter by tenant")
	return c
}

func runApprovalList(cmd *cobra.Command, repoURL, opF, statusF, tenantF string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := approval.NewStore(sp)
	requests, err := store.List(cmd.Context(), approval.ListFilters{
		Op:     approval.Op(opF),
		Status: approval.Status(statusF),
		Tenant: tenantF,
	})
	if err != nil {
		return output.NewError("approval.list_failed",
			fmt.Sprintf("approval list: %v", err)).Wrap(err)
	}
	body := approvalListBody{}
	for _, r := range requests {
		st, _ := store.StatusOf(cmd.Context(), r.ID)
		count, _ := approval.VerifyApprovals(r)
		body.Requests = append(body.Requests, approvalListEntry{
			ID:            r.ID,
			Op:            string(r.Op),
			Initiator:     r.Initiator,
			Target:        r.Target,
			Threshold:     r.Threshold,
			ApprovalCount: count,
			Status:        string(st),
			ExpiresAt:     r.ExpiresAt,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newApprovalRevokeCmd() *cobra.Command {
	var (
		repoURL string
		by      string
		reason  string
	)
	c := &cobra.Command{
		Use:          "revoke <request-id>",
		Short:        "Revoke an outstanding request (cannot be approved further)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalRevoke(cmd, args[0], repoURL, by, reason)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&by, "by", "", "operator-readable revoker ID (lands in audit chain)")
	c.Flags().StringVar(&reason, "reason", "", "free-form reason")
	return c
}

func runApprovalRevoke(cmd *cobra.Command, id, repoURL, by, reason string) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := approval.NewStore(sp)
	req, err := store.Revoke(cmd.Context(), id, by, reason)
	if err != nil {
		if errors.Is(err, approval.ErrNotFound) {
			return output.NewError("notfound.approval",
				fmt.Sprintf("approval revoke: request %q not found", id)).Wrap(err)
		}
		return output.NewError("approval.revoke_failed",
			fmt.Sprintf("approval revoke: %v", err)).Wrap(err)
	}
	auditEv := &audit.Event{
		Action: "approval.revoke",
		Actor:  by,
		Tenant: req.Tenant,
		Subject: audit.Subject{
			Tenant: req.Tenant,
			Repo:   repoURL,
		},
		Body: map[string]any{
			"approval_id": req.ID,
			"op":          string(req.Op),
			"target":      req.Target,
			"reason":      reason,
		},
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), auditEv)
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(approvalRevokeBody{
		ID:        req.ID,
		Op:        string(req.Op),
		RevokedAt: *req.RevokedAt,
		RevokedBy: req.RevokedBy,
	}))
}

// loadPrivateKeyForApproval reads a private-key PEM from disk; when
// --key is empty, it falls back to the local keystore's
// manifest-signing key (so the simplest workflow has the operator
// approving with their existing identity).
func loadPrivateKeyForApproval(keyFile string) (ed25519.PrivateKey, error) {
	var pemBody []byte
	if keyFile != "" {
		body, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, output.NewError("usage.bad_key_file",
				fmt.Sprintf("approval approve: read key file: %v", err)).Wrap(output.ErrUsage)
		}
		pemBody = body
	} else {
		// Fall back to the local keystore's manifest-signing key —
		// the same identity the operator's manifest signatures
		// already use. Path mirrors keystore.PrivateKeyFile.
		p, err := paths.Resolve(paths.DefaultOptions())
		if err != nil {
			return nil, output.NewError("internal", err.Error()).Wrap(err)
		}
		body, err := os.ReadFile(p.Keyring.Value + "/" + keystore.PrivateKeyFile)
		if err != nil {
			return nil, output.NewError("approval.no_signing_key",
				fmt.Sprintf("approval approve: pass --key to point at the approver's private key, or run any backup/restore command first to bootstrap a signing key (%v)", err)).
				WithSuggestion(&output.Suggestion{
					Human:   "the simplest path is to use your existing manifest-signing key as your approver identity",
					Command: "pg_hardstorage doctor",
				}).Wrap(err)
		}
		pemBody = body
	}
	signer, err := backup.LoadSigner(pemBody)
	if err != nil {
		return nil, output.NewError("approval.bad_key",
			fmt.Sprintf("approval approve: parse private key: %v", err)).Wrap(err)
	}
	return signer.PrivateKey(), nil
}

// initiatorFromEnv returns a sensible Initiator string for an audit
// event. For we read $USER as the cheapest meaningful actor;
// future SSO/OIDC integration replaces this.
func initiatorFromEnv() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}

// --- result bodies ---

type approvalRequestBody struct {
	ID           string    `json:"id"`
	Op           string    `json:"op"`
	Target       string    `json:"target,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Threshold    int       `json:"threshold"`
	ApproverKeys int       `json:"approver_keys"`
	Initiator    string    `json:"initiator,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// WriteText renders the created request summary as human-readable text to w.
func (b approvalRequestBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ approval request created\n")
	fmt.Fprintf(bw, "  ID:        %s\n", b.ID)
	fmt.Fprintf(bw, "  Op:        %s\n", b.Op)
	if b.Target != "" {
		fmt.Fprintf(bw, "  Target:    %s\n", b.Target)
	}
	fmt.Fprintf(bw, "  Threshold: %d of %d allowlisted approvers\n", b.Threshold, b.ApproverKeys)
	fmt.Fprintf(bw, "  Expires:   %s\n", b.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Approve:   pg_hardstorage approval approve %s --repo <url>\n", b.ID)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type approvalApproveBody struct {
	ID            string `json:"id"`
	Op            string `json:"op"`
	Status        string `json:"status"`
	ApprovalCount int    `json:"approval_count"`
	Threshold     int    `json:"threshold"`
}

// WriteText renders the approve result as human-readable text to w, flagging
// whether the request has now crossed the threshold.
func (b approvalApproveBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Status == string(approval.StatusApproved) {
		fmt.Fprintf(bw, "✓ approval recorded — request now APPROVED (%d/%d)\n", b.ApprovalCount, b.Threshold)
	} else {
		fmt.Fprintf(bw, "✓ approval recorded — request still %s (%d/%d)\n", b.Status, b.ApprovalCount, b.Threshold)
	}
	fmt.Fprintf(bw, "  ID: %s\n", b.ID)
	fmt.Fprintf(bw, "  Op: %s\n", b.Op)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type approvalEntry struct {
	Approver       string    `json:"approver,omitempty"`
	KeyFingerprint string    `json:"key_fingerprint"`
	At             time.Time `json:"at"`
	Reason         string    `json:"reason,omitempty"`
}

type approvalStatusBody struct {
	ID            string          `json:"id"`
	Op            string          `json:"op"`
	Initiator     string          `json:"initiator,omitempty"`
	Target        string          `json:"target,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Threshold     int             `json:"threshold"`
	ApprovalCount int             `json:"approval_count"`
	Status        string          `json:"status"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Approvals     []approvalEntry `json:"approvals,omitempty"`
}

// WriteText renders the request status, including each recorded approval, as
// human-readable text to w.
func (b approvalStatusBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "approval %s\n", b.ID)
	fmt.Fprintf(bw, "  Op:           %s\n", b.Op)
	fmt.Fprintf(bw, "  Initiator:    %s\n", b.Initiator)
	if b.Target != "" {
		fmt.Fprintf(bw, "  Target:       %s\n", b.Target)
	}
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason:       %s\n", b.Reason)
	}
	fmt.Fprintf(bw, "  Status:       %s (%d/%d approvals)\n", b.Status, b.ApprovalCount, b.Threshold)
	fmt.Fprintf(bw, "  Expires:      %s\n", b.ExpiresAt.Format(time.RFC3339))
	if len(b.Approvals) > 0 {
		fmt.Fprintf(bw, "  Approvals:\n")
		tw := tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
		for _, a := range b.Approvals {
			id := a.Approver
			if id == "" {
				id = a.KeyFingerprint[:12] + "…"
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", a.At.Format(time.RFC3339), id, a.Reason)
		}
		tw.Flush()
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type approvalListEntry struct {
	ID            string    `json:"id"`
	Op            string    `json:"op"`
	Initiator     string    `json:"initiator,omitempty"`
	Target        string    `json:"target,omitempty"`
	Threshold     int       `json:"threshold"`
	ApprovalCount int       `json:"approval_count"`
	Status        string    `json:"status"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type approvalListBody struct {
	Requests []approvalListEntry `json:"requests"`
}

// WriteText renders the list of approval requests as a tabular summary to w.
func (b approvalListBody) WriteText(w io.Writer) error {
	if len(b.Requests) == 0 {
		_, err := io.WriteString(w, "no approval requests")
		return err
	}
	bw := &strings.Builder{}
	tw := tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\tOP\tSTATUS\tAPPROVALS\tEXPIRES\tTARGET\n")
	for _, r := range b.Requests {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			r.ID, r.Op, r.Status, r.ApprovalCount, r.Threshold,
			r.ExpiresAt.Format(time.RFC3339), r.Target)
	}
	tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type approvalRevokeBody struct {
	ID        string    `json:"id"`
	Op        string    `json:"op"`
	RevokedAt time.Time `json:"revoked_at"`
	RevokedBy string    `json:"revoked_by,omitempty"`
}

// WriteText renders the revocation confirmation as human-readable text to w.
func (b approvalRevokeBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ approval %s revoked\n", b.ID)
	fmt.Fprintf(bw, "  Op:        %s\n", b.Op)
	fmt.Fprintf(bw, "  Revoked:   %s\n", b.RevokedAt.Format(time.RFC3339))
	if b.RevokedBy != "" {
		fmt.Fprintf(bw, "  By:        %s\n", b.RevokedBy)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// silence-unused: context imported indirectly via cobra.Command.
var _ = context.Background
