// repo_wipe.go — CLI surface for destroying a repo after approval workflow gating.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// RepoWipeOp is the approval-namespace string the `repo wipe`
// destructive op binds to. Target is the repo URL — same posture
// as `repo set-mode` and `repo gc`.
const RepoWipeOp = approval.Op("repo.wipe")

// newRepoWipeCmd implements `pg_hardstorage repo wipe <url>
// --require-approval <id> --yes`. The fourth and final destructive
// op the plan named.
//
// "Wipe" here means: permanently delete every object in the repo —
// every backup, every audit event, every approval, every chunk, the
// HSREPO marker. Gone. By design, the operator-facing nuclear
// option for a repo that needs to be wholly retired (decommissioned
// tenant, contaminated data, etc.).
//
// Posture mirrors `kms shred`: mandatory n-of-m approval bound to
// op + target, plus a typed `--yes` confirmation, plus an
// always-on audit emission BEFORE the wipe runs (so the chain
// entry is captured in some form before we delete the chain that
// would have held it).
//
// Rate-limit by design: this command takes minutes to hours on a
// real repo (one Delete per chunk). We make no attempt at parallel
// deletion — the sequential walk is operator-friendly (you can
// SIGINT mid-wipe, audit chain stays intact for the keys we
// haven't touched yet).
func newRepoWipeCmd() *cobra.Command {
	var (
		repoURL         string
		reason          string
		requireApproval string
		yes             bool
	)
	var force bool
	c := &cobra.Command{
		Use:   "wipe <url>",
		Short: "Permanently delete every object in the repo (n-of-m approval, or --force for non-WORM repos)",
		Long: `Permanently delete every object in the named repository — every
backup, every audit event, every approval, every chunk, plus the
HSREPO marker. After this completes, the URL is back to whatever
state the storage backend has for an empty location.

Two ways to authorise the wipe:

  • n-of-m approval (--require-approval <id>): the approval's Op
    must be ` + "`" + `repo.wipe` + "`" + ` and its Target must be the repo URL.
    This is the strict, cross-org-coordinated path and the ONLY
    path accepted for a WORM/compliance repo.

  • --force: a single-operator escape hatch for ORDINARY (non-WORM)
    repos — no approval required. Refused on WORM/compliance repos,
    where the n-of-m gate is mandatory by design.

` + "`" + `--yes` + "`" + ` is required either way — typed confirmation that the op
is irreversible is the always-on second gate.

Audit emission lands BEFORE the wipe runs (a record of intent),
because the wipe also clears the audit chain that would have held
the post-wipe entry. The CLI's Result body is the operator-side
witness once the audit chain is gone.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Positional-or-flag, identical to repo gc/audit/scrub: the repo
			// URL may come as the <url> positional OR via --repo. The
			// destructive gates (approval / --yes / --force / WORM) are
			// unchanged — only how the URL is supplied.
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo wipe: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			if repoURL == "" {
				return missingFlagErr(cmd, "--repo (or the first positional <url>)")
			}
			return runRepoWipe(cmd, repoURL, reason, requireApproval, force, yes)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	c.Flags().StringVar(&reason, "reason", "",
		"free-form reason captured in the audit chain pre-wipe")
	c.Flags().StringVar(&requireApproval, "require-approval", "",
		"approval request ID for the strict n-of-m gate (mandatory for WORM/compliance repos)")
	c.Flags().BoolVar(&force, "force", false,
		"skip the n-of-m approval on an ordinary (non-WORM) repo; still requires --yes")
	c.Flags().BoolVar(&yes, "yes", false,
		"acknowledge that this op is irreversible (always required)")
	return c
}

func runRepoWipe(cmd *cobra.Command, url, reason, approvalID string, force, yes bool) error {
	d := DispatcherFrom(cmd)

	repoMeta, sp, err := openRepo(cmd.Context(), url)
	if err != nil {
		return err
	}
	defer sp.Close()

	// Authorisation policy (issue #57): a WORM/compliance repo keeps the
	// mandatory n-of-m posture — --force is refused, an approval is the
	// only way in. An ordinary (non-WORM) repo accepts EITHER an
	// approval OR --force, so a single operator can retire their own
	// repo without standing up a full cross-org approval first.
	isWORM := repoMeta.WORM != nil
	if force && isWORM {
		return output.NewError("usage.force_refused_worm",
			"repo wipe: --force is refused on a WORM/compliance repo — an n-of-m approval is mandatory for it").
			WithSuggestion(&output.Suggestion{
				Human:   "create and approve an n-of-m gate for this compliance repo",
				Command: "pg_hardstorage approval request --op repo.wipe --target " + url + " --threshold 2 --approver-key alice.pub --approver-key bob.pub --repo " + url,
			}).Wrap(output.ErrUsage)
	}
	if approvalID == "" && !force {
		msg := "repo wipe: pass --require-approval <id> (n-of-m gate) or --force (single-operator, non-WORM repos only)"
		if isWORM {
			msg = "repo wipe: this is a WORM/compliance repo — --require-approval is REQUIRED (--force is not accepted)"
		}
		return output.NewError("usage.missing_flag", msg).
			WithSuggestion(&output.Suggestion{
				Human:   "quick single-operator wipe of a non-WORM repo: add --force --yes; otherwise create an n-of-m approval",
				Command: "pg_hardstorage approval request --op repo.wipe --target " + url + " --threshold 2 --approver-key alice.pub --approver-key bob.pub --repo " + url,
			}).Wrap(output.ErrUsage)
	}

	// When an approval ID is supplied we gate on it (Op + Target
	// binding refuses cross-op / cross-repo redemption — the
	// trust-foundation property) even if --force was also passed: an
	// explicit approval always takes the strict path.
	var gateReq *approval.Request
	if approvalID != "" {
		gr, gerr := approval.NewStore(sp).Gate(cmd.Context(), approval.GateOptions{
			RequestID: approvalID,
			Op:        RepoWipeOp,
			Target:    url,
		})
		if gerr != nil {
			return mapApprovalGateError("repo wipe", approvalID, gerr)
		}
		gateReq = gr
	}

	if !yes {
		return output.NewError("usage.confirmation_required",
			"repo wipe: pass --yes to acknowledge that this operation is irreversible").
			Wrap(output.ErrUsage)
	}

	// Pre-wipe audit emission. The wipe also clears the audit chain
	// that would have held this entry; the goal is to leave a trail
	// in whatever Sinks the operator has wired (Slack, Jira, syslog,
	// CEF, …) before the in-repo chain is gone. Best-effort.
	body := map[string]any{
		"url":    url,
		"reason": reason,
		"phase":  "pre-wipe",
	}
	if gateReq != nil {
		body["approval_id"] = gateReq.ID
		body["approval_op"] = string(gateReq.Op)
		body["threshold"] = gateReq.Threshold
		body["approvers"] = len(gateReq.Approvals)
	} else {
		// --force path: no approval. Record the bypass explicitly so
		// the audit trail shows a single operator authorised this.
		body["forced"] = true
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action:    "repo.wipe",
		Subject:   audit.Subject{Repo: url},
		Timestamp: time.Now().UTC(),
		Body:      body,
	})

	// Forward per-key Delete events through the dispatcher when the
	// renderer is NDJSON. JSON mode suppresses (single-document
	// contract); text mode emits a quiet status without flooding.
	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"
	progressFn := func(key string) {
		if suppressEvents {
			return
		}
		_ = d.Event(cmd.Context(),
			output.NewEvent(output.SeverityInfo, "repo.wipe", "delete").
				WithBody(map[string]any{"repo": url, "key": key}))
	}

	res, werr := repo.Wipe(cmd.Context(), sp, progressFn)
	if werr != nil {
		// Partial wipe: HSREPO preserved, some keys couldn't be
		// deleted. Surface as a structured error with the count so
		// the operator's automation knows to retry / investigate.
		return output.NewError("repo.wipe.partial",
			fmt.Sprintf("repo wipe: %v", werr)).Wrap(werr)
	}

	approvalIDOut := ""
	if gateReq != nil {
		approvalIDOut = gateReq.ID
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(repoWipeBody{
		URL:           url,
		ApprovalID:    approvalIDOut,
		Forced:        gateReq == nil,
		Reason:        reason,
		Chunks:        res.Chunks,
		Manifests:     res.Manifests,
		Audit:         res.Audit,
		Approvals:     res.Approvals,
		WAL:           res.WAL,
		Other:         res.Other,
		Total:         res.Total,
		HSREPORemoved: res.HSREPORemoved,
	}))
}

type repoWipeBody struct {
	URL           string `json:"url"`
	ApprovalID    string `json:"approval_id,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Chunks        int    `json:"chunks"`
	Manifests     int    `json:"manifests"`
	Audit         int    `json:"audit"`
	Approvals     int    `json:"approvals"`
	WAL           int    `json:"wal,omitempty"`
	Other         int    `json:"other,omitempty"`
	Total         int    `json:"total"`
	HSREPORemoved bool   `json:"hsrepo_removed"`
}

// WriteText renders the wipe result — per-category counters and the HSREPO
// disposition — as human-readable text to w.
func (b repoWipeBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ repo wipe — %d object(s) deleted\n", b.Total)
	fmt.Fprintf(bw, "  URL:        %s\n", b.URL)
	fmt.Fprintf(bw, "  Chunks:     %d\n", b.Chunks)
	fmt.Fprintf(bw, "  Manifests:  %d\n", b.Manifests)
	fmt.Fprintf(bw, "  Audit:      %d\n", b.Audit)
	fmt.Fprintf(bw, "  Approvals:  %d\n", b.Approvals)
	if b.WAL > 0 {
		fmt.Fprintf(bw, "  WAL:        %d\n", b.WAL)
	}
	if b.Other > 0 {
		fmt.Fprintf(bw, "  Other:      %d\n", b.Other)
	}
	if b.HSREPORemoved {
		fmt.Fprintln(bw, "  HSREPO:     removed (URL is no longer a pg_hardstorage repo)")
	} else {
		fmt.Fprintln(bw, "  HSREPO:     preserved (some keys couldn't be deleted; investigate)")
	}
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason:     %s\n", b.Reason)
	}
	if b.Forced {
		fmt.Fprint(bw, "  Approval:   (forced — single-operator, non-WORM repo)")
	} else {
		fmt.Fprintf(bw, "  Approval:   %s", b.ApprovalID)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}
