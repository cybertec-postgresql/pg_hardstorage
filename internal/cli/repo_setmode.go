// repo_setmode.go — CLI surface for flipping a repo's mutability mode (live / paused / WORM).
package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// SetModeOp is the approval-namespace string the `repo set-mode`
// destructive op binds to. Approval requests created with this Op
// can be redeemed by `repo set-mode --require-approval <id>`; an
// approval for a different op is refused at the gate.
const SetModeOp = approval.Op("repo.set_mode")

// newRepoSetModeCmd wires `pg_hardstorage repo set-mode <url> <mode>`.
//
// The two-argument shape is deliberate: mode flips are rare, manual,
// and high-stakes (read-only blocks every mutating subcommand). A
// flag-based shape would invite scripted flips; making the operator
// type the URL and the mode every time is the friction we want.
//
// We refuse anything other than "read-only" or "read-write" so a typo
// can't accidentally leave the repo in an undefined state.
//
// `--require-approval <id>` gates the flip on an n-of-m
// approval request. The approval's Op must be `repo.set_mode` and
// its Target must be the URL being changed; the gate refuses
// otherwise so an approval can't be redeemed for a different op or
// against a different repo.
func newRepoSetModeCmd() *cobra.Command {
	var requireApproval string
	c := &cobra.Command{
		Use:   "set-mode <url> <read-only|read-write>",
		Short: "Toggle the repository's write-access posture",
		Long: `Set the repository's write-access posture.

read-only:   refuses every mutating operation (backup, wal push,
             wal stream commit, gc, rotation, kms rotate/shred). Reads
             — restore, verify, list, show, repo usage — still work.
             Use for forensics or while a separate incident is in
             flight.

read-write:  the default. All operations permitted.

The mode is recorded in HSREPO; flipping it takes effect for any
subsequent operation. In-flight operations are NOT cancelled.

--require-approval <id>: gate the flip on an existing n-of-m
approval request. The approval's Op must be ` + "`" + `repo.set_mode` + "`" + ` and
its Target must equal the URL being changed; otherwise the flip is
refused at the gate. See ` + "`" + `pg_hardstorage approval` + "`" + `.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoSetMode(cmd, args[0], args[1], requireApproval)
		},
	}
	c.Flags().StringVar(&requireApproval, "require-approval", "",
		"approval request ID that must be in approved state for repo.set_mode + this URL (n-of-m gate)")
	return c
}

func runRepoSetMode(cmd *cobra.Command, url, modeArg, approvalID string) error {
	d := DispatcherFrom(cmd)

	mode := repo.Mode(modeArg)
	if !mode.IsValid() || mode == "" {
		return output.NewError("usage.bad_mode",
			fmt.Sprintf("repo set-mode: %q is not a valid mode (want read-only or read-write)", modeArg)).
			Wrap(output.ErrUsage)
	}

	// n-of-m gate: when --require-approval <id> is set, refuse the
	// flip unless the named request is approved + bound to this op +
	// this URL. We open the repo once for both the gate read and
	// (later) the audit emission.
	var gateReq *approval.Request
	if approvalID != "" {
		_, sp, err := openRepo(cmd.Context(), url)
		if err != nil {
			return err
		}
		store := approval.NewStore(sp)
		req, gerr := store.Gate(cmd.Context(), approval.GateOptions{
			RequestID: approvalID,
			Op:        SetModeOp,
			Target:    url,
		})
		sp.Close()
		if gerr != nil {
			return mapApprovalGateError("repo set-mode", approvalID, gerr)
		}
		gateReq = req
	}

	res, err := repo.SetMode(cmd.Context(), repo.SetModeOptions{URL: url, Mode: mode})
	if err != nil {
		return mapRepoSetModeError(url, err)
	}

	// Audit emission: link the action back to the approval that
	// authorised it, so a forensic walk shows who initiated, who
	// approved, and what they ended up running. Best-effort.
	if gateReq != nil {
		repoMeta, sp, oerr := openRepo(cmd.Context(), url)
		if oerr == nil {
			body := map[string]any{
				"url":           res.URL,
				"previous_mode": string(res.PreviousMode),
				"new_mode":      string(res.Mode),
				"approval_id":   gateReq.ID,
				"approval_op":   string(gateReq.Op),
				"threshold":     gateReq.Threshold,
				"approvers":     len(gateReq.Approvals),
				"updated_at":    res.UpdatedAt,
			}
			audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
				Action: "repo.set_mode",
				Tenant: gateReq.Tenant,
				Subject: audit.Subject{
					Repo:   res.URL,
					Tenant: gateReq.Tenant,
				},
				Timestamp: time.Now().UTC(),
				Body:      body,
			})
			sp.Close()
		}
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(repoSetModeBody{
		URL:          res.URL,
		PreviousMode: string(res.PreviousMode),
		Mode:         string(res.Mode),
		UpdatedAt:    res.UpdatedAt,
		ApprovalID:   approvalID,
	}))
}

// mapApprovalGateError maps the approval-package error sentinels to
// structured CLI errors. Reused by every destructive op that grows
// a --require-approval gate.
func mapApprovalGateError(opName, requestID string, err error) error {
	switch {
	case errors.Is(err, approval.ErrNotFound):
		return output.NewError("notfound.approval",
			fmt.Sprintf("%s: approval request %q not found", opName, requestID)).Wrap(err)
	case errors.Is(err, approval.ErrThresholdNotMet):
		return output.NewError("conflict.approval_pending",
			fmt.Sprintf("%s: approval request %q is still pending — collect more approvals before proceeding", opName, requestID)).
			WithSuggestion(&output.Suggestion{
				Human:   "check status with `pg_hardstorage approval status " + requestID + "`",
				Command: "pg_hardstorage approval status " + requestID,
			}).Wrap(err)
	case errors.Is(err, approval.ErrExpired):
		return output.NewError("approval.expired",
			fmt.Sprintf("%s: approval request %q has expired", opName, requestID)).Wrap(err)
	case errors.Is(err, approval.ErrRevoked):
		return output.NewError("approval.revoked",
			fmt.Sprintf("%s: approval request %q was revoked", opName, requestID)).Wrap(err)
	case errors.Is(err, approval.ErrOpMismatch):
		return output.NewError("auth.approval_op_mismatch",
			fmt.Sprintf("%s: approval %q is for a different op (refusing to redeem)", opName, requestID)).
			WithSuggestion(&output.Suggestion{
				Human: "create an approval whose --op matches the destructive op being attempted",
			}).Wrap(err)
	case errors.Is(err, approval.ErrTargetMismatch):
		return output.NewError("auth.approval_target_mismatch",
			fmt.Sprintf("%s: approval %q is for a different target (refusing to redeem)", opName, requestID)).
			WithSuggestion(&output.Suggestion{
				Human: "create an approval whose --target matches the resource being modified",
			}).Wrap(err)
	}
	return output.NewError("approval.gate_failed",
		fmt.Sprintf("%s: approval gate: %v", opName, err)).Wrap(err)
}

func mapRepoSetModeError(url string, err error) error {
	if errors.Is(err, repo.ErrNotARepo) {
		return output.NewError("notfound.repo",
			fmt.Sprintf("repo set-mode: no pg_hardstorage repository at %s", url)).
			WithSuggestion(&output.Suggestion{
				Human:   "create the repository first",
				Command: "pg_hardstorage repo init " + url,
			}).Wrap(err)
	}
	return output.NewError("repo.set_mode_failed",
		fmt.Sprintf("repo set-mode: %v", err)).Wrap(err)
}

type repoSetModeBody struct {
	URL          string `json:"url"`
	PreviousMode string `json:"previous_mode"`
	Mode         string `json:"mode"`
	UpdatedAt    string `json:"updated_at"`
	ApprovalID   string `json:"approval_id,omitempty"`
}

// WriteText renders the mode-change result as human-readable text to w,
// noting when the mode was already the requested value.
func (b repoSetModeBody) WriteText(w io.Writer) error {
	verb := "✓"
	change := fmt.Sprintf("%s → %s", b.PreviousMode, b.Mode)
	if b.PreviousMode == b.Mode {
		change = fmt.Sprintf("%s (unchanged)", b.Mode)
	}
	out := fmt.Sprintf("%s repo set-mode\n  URL:        %s\n  Mode:       %s\n  Updated at: %s",
		verb, b.URL, change, b.UpdatedAt)
	if b.ApprovalID != "" {
		out += "\n  Approval:   " + b.ApprovalID
	}
	_, err := fmt.Fprint(w, out)
	return err
}
