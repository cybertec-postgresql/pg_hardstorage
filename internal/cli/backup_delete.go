// backup_delete.go — CLI surface for tombstoning backups (with optional cascade).
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// BackupDeleteOp is the approval-namespace string the `backup delete`
// destructive op binds to. Approvals created with this Op can be
// redeemed by `backup delete --require-approval <id>`; an approval
// for a different op (e.g. `repo.gc`) is refused at the gate.
const BackupDeleteOp = approval.Op("backup.delete")

// newBackupDeleteCmd implements `pg_hardstorage backup delete
// <deployment> <backup-id> [--require-approval <id>]`.
//
// Soft delete only. The manifest is moved to a tombstone marker so
// chunk-GC reclaims its space on the next `repo gc --apply` cycle.
// Until GC runs, an operator can still recover the backup by
// removing the tombstone — that's deliberate. "Manual deletion"
// outside of retention happens because someone classified data as
// over-retained (PII, accidentally-leaked secrets, etc.); the
// soft-delete + GC split gives a window to undo a wrong call.
//
// The `--require-approval <id>` gate is recommended for any
// operator-initiated deletion. Approval Op must be `backup.delete`
// and Target must be the backup ID being deleted; cross-op or
// cross-target redemption is refused at the gate (same trust
// posture as `repo set-mode` and `repo gc`).
func newBackupDeleteCmd() *cobra.Command {
	var (
		repoURL         string
		reason          string
		requireApproval string
		cascade         bool
		yes             bool
	)
	c := &cobra.Command{
		Use:   "delete <deployment> <backup-id>",
		Short: "Soft-delete a specific backup (chunks reclaimed by next repo gc)",
		Long: `delete tombstones the named backup. The manifest body and
chunks remain on disk until the next ` + "`" + `repo gc --apply` + "`" + ` cycle
clears unreferenced chunks; this split is deliberate so an operator
who deletes the wrong backup has time to recover.

If the backup has live incremental descendants the delete refuses
(chain-protection: tombstoning an anchor would leave its children
un-restorable). Pass --cascade to drain the entire chain leaf-first
in one operation; every tombstoned manifest is recorded in the
audit chain, and the same approval (--require-approval) gates the
whole cascade rather than each step.

The destructive op gate (--require-approval) is strongly recommended
for operator-initiated deletes outside of retention. Approval Op
must be ` + "`" + `backup.delete` + "`" + ` and Target must be the backup ID.

For automated deletion driven by retention policy, use
` + "`" + `pg_hardstorage rotate` + "`" + ` instead — it derives a structured
soft-delete plan from the configured policy.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Confirmation gate: the most destructive verb in the CLI
			// must not be WEAKER-guarded than the config-only
			// `deployment remove` (which refuses without --yes). An
			// approved n-of-m gate also counts as confirmation.
			if !yes && requireApproval == "" {
				return output.NewError("aborted.confirmation_required",
					fmt.Sprintf("backup delete: refusing to tombstone %s/%s without --yes (or an approval via --require-approval)", args[0], args[1])).
					WithSuggestion(&output.Suggestion{
						Human: "re-run with --yes to confirm; the backup stays recoverable via `backup undelete` until the next `repo gc --apply`",
					})
			}
			return runBackupDelete(cmd, args[0], args[1], repoURL, reason, requireApproval, cascade)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&reason, "reason", "",
		"free-form reason captured in the tombstone + audit chain")
	c.Flags().StringVar(&requireApproval, "require-approval", "",
		"approval request ID that must be in approved state for backup.delete + this backup ID (n-of-m gate; strongly recommended)")
	c.Flags().BoolVar(&cascade, "cascade", false,
		"also delete every incremental descendant of this backup, leaf-first")
	c.Flags().BoolVar(&yes, "yes", false,
		"confirm the tombstone (not needed when --require-approval is given)")
	return c
}

func runBackupDelete(cmd *cobra.Command, deployment, backupID, repoURL, reason, approvalID string, cascade bool) error {
	d := DispatcherFrom(cmd)
	if deployment == "" || backupID == "" {
		return output.NewError("usage.missing_arg",
			"backup delete: deployment and backup-id are required").Wrap(output.ErrUsage)
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	if err := assertRepoWritable(cmd.Context(), sp, "backup delete"); err != nil {
		return err
	}

	// Confirm the backup exists before consuming the approval. If we
	// gated first and then the manifest read failed, the approval
	// would be "burned" against a typo'd backup ID. Read first,
	// gate second.
	//
	// Verifier comes from the operator's keyring — same posture as
	// `backup` and `restore`. A signature-broken manifest can still
	// be deleted, but only via an explicit `repair` workflow; the
	// happy path requires verification matches the local trust root.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	_, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("backup delete: load keyring: %v", err)).Wrap(err)
	}
	store := backup.NewManifestStore(sp)
	m, err := store.Read(cmd.Context(), deployment, backupID, verifier)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("backup delete: %s/%s not found", deployment, backupID)).Wrap(err)
		}
		if errors.Is(err, backup.ErrTombstoned) {
			// Already deleted — surface as a no-op success rather
			// than an error. Idempotent re-deletion matches every
			// other backend op's posture.
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(backupDeleteBody{
				Deployment:     deployment,
				BackupID:       backupID,
				AlreadyDeleted: true,
			}))
		}
		return output.NewError("backup.delete.read_failed",
			fmt.Sprintf("backup delete: read manifest: %v", err)).Wrap(err)
	}

	// n-of-m gate: when --require-approval <id> is set, the approval
	// must exist, be approved, bound to op `backup.delete`, and
	// bound to this backup ID. Refusal returns a structured CLI
	// error via the shared mapApprovalGateError helper.
	var gateReq *approval.Request
	if approvalID != "" {
		req, gerr := approval.NewStore(sp).Gate(cmd.Context(), approval.GateOptions{
			RequestID: approvalID,
			Op:        BackupDeleteOp,
			Target:    backupID,
		})
		if gerr != nil {
			return mapApprovalGateError("backup delete", approvalID, gerr)
		}
		gateReq = req
	}

	// Tombstone reason format mirrors retention-driven soft deletes:
	// policy="manual", reason= operator-supplied.
	tombstoneReason := reason
	if tombstoneReason == "" {
		tombstoneReason = "operator-initiated"
	}
	var cascadeDeleted []string
	if cascade {
		// Cascade path: tombstone the entire chain leaf-first.
		// The same approval covers the whole cascade — we don't
		// re-gate at each step. Failure mid-cascade leaves
		// partial state visible (descendants tombstoned before
		// the failure stay tombstoned); the operator can re-run
		// after fixing the underlying issue and the cascade is
		// naturally idempotent (already-tombstoned descendants
		// are skipped by findLiveDescendants on the next pass).
		deleted, cerr := store.SoftDeleteCascade(cmd.Context(), deployment, backupID, "manual", tombstoneReason)
		cascadeDeleted = deleted
		if cerr != nil {
			// Hold-protection refusal: cascade aborted up-front
			// with the full list of held links so the operator
			// fixes them all at once. The Suggestion's Command
			// is a copy-pasteable `hold remove` for the FIRST
			// held link; Human prose covers the rest.
			var heldErr *backup.ChainHasHeldLinksError
			if errors.As(cerr, &heldErr) {
				ids := make([]string, 0, len(heldErr.Held))
				for _, l := range heldErr.Held {
					ids = append(ids, l.BackupID)
				}
				suggestionCommand := ""
				if len(heldErr.Held) > 0 {
					suggestionCommand = "pg_hardstorage backup hold remove " +
						deployment + " " + heldErr.Held[0].BackupID + " --repo " + repoURL
				}
				return output.NewError("conflict.chain_has_held_links",
					fmt.Sprintf("backup delete --cascade: %s/%s chain has %d held link(s): %s",
						deployment, backupID, len(heldErr.Held), strings.Join(ids, ", "))).
					WithSuggestion(&output.Suggestion{
						Human:   "every held link must be released before the cascade can run; the cascade refuses up-front so the chain isn't torn by a partial run. Use `backup hold remove <deployment> <backup-id>` for each.",
						Command: suggestionCommand,
					}).
					Wrap(cerr)
			}
			return output.NewError("backup.delete.tombstone_failed",
				fmt.Sprintf("backup delete --cascade: %v (deleted %d before failure)", cerr, len(deleted))).
				WithSuggestion(&output.Suggestion{
					Human: "the cascade tombstoned some descendants before failing. The operation is idempotent — re-run with the same arguments after fixing the underlying error to drain the rest of the chain.",
				}).
				Wrap(cerr)
		}
	} else if err := store.SoftDelete(cmd.Context(), deployment, backupID, "manual", tombstoneReason); err != nil {
		// Hold-protection refusal: a held manifest cannot be
		// deleted by ANY caller. Surface holder/reason from
		// the marker so the operator knows who to talk to.
		var heldErr *backup.ManifestHeldError
		if errors.As(err, &heldErr) {
			detail := fmt.Sprintf("backup delete: %s/%s is on legal hold", deployment, backupID)
			if heldErr.Holder != "" {
				detail += " (holder=" + heldErr.Holder + ")"
			}
			if heldErr.Reason != "" {
				detail += " — reason: " + heldErr.Reason
			}
			return output.NewError("conflict.manifest_held",
				detail).
				WithSuggestion(&output.Suggestion{
					Human:   "the hold protects this backup from any deletion, including retention cascades. Release it via `backup hold remove` if the regulatory/operational reason no longer applies.",
					Command: "pg_hardstorage backup hold remove " + deployment + " " + backupID + " --repo " + repoURL,
				}).
				Wrap(err)
		}
		// Chain-protection refusal: surface a structured error
		// with the descendants listed and a suggestion that
		// matches the supported workflow (delete leaf-first, or
		// run rotate to let chain-aware retention drain it).
		var chErr *backup.ChainHasLiveDescendantsError
		if errors.As(err, &chErr) {
			return output.NewError("conflict.chain_has_live_descendants",
				fmt.Sprintf("backup delete: %s/%s has %d live incremental descendant(s): %s",
					chErr.Deployment, chErr.BackupID, len(chErr.Descendants),
					strings.Join(chErr.Descendants, ", "))).
				WithSuggestion(&output.Suggestion{
					Human:   "soft-delete the leaf incrementals first (deepest in the chain), or pass --cascade to drain the entire chain in one operation, or run `pg_hardstorage rotate` so chain-aware retention drains the chain in the correct order.",
					Command: "pg_hardstorage backup delete " + deployment + " " + backupID + " --repo " + repoURL + " --cascade",
				}).
				Wrap(err)
		}
		return output.NewError("backup.delete.tombstone_failed",
			fmt.Sprintf("backup delete: %v", err)).Wrap(err)
	}

	// Audit emission. Always-on for backup delete (unlike repo gc,
	// which only audits gated applies) — manual deletion is rare
	// enough that the chain entry is signal, not noise. Best-effort.
	body := map[string]any{
		"deployment": deployment,
		"backup_id":  backupID,
		"reason":     tombstoneReason,
		"pg_version": m.PGVersion,
		"timeline":   m.Timeline,
		"started_at": m.StartedAt.Format(time.RFC3339),
	}
	if gateReq != nil {
		body["approval_id"] = gateReq.ID
		body["approval_op"] = string(gateReq.Op)
		body["threshold"] = gateReq.Threshold
		body["approvers"] = len(gateReq.Approvals)
	}
	if cascade {
		// Cascade-mode audit body: include the full deletion
		// order so post-incident review can reconstruct exactly
		// which manifests were tombstoned and in which order.
		body["cascade"] = true
		body["cascade_deleted"] = cascadeDeleted
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action: "backup.delete",
		Tenant: m.Tenant,
		Subject: audit.Subject{
			Deployment: deployment,
			BackupID:   backupID,
			Tenant:     m.Tenant,
			Repo:       repoURL,
		},
		Timestamp: time.Now().UTC(),
		Body:      body,
	})

	out := backupDeleteBody{
		Deployment: deployment,
		BackupID:   backupID,
		Reason:     tombstoneReason,
	}
	if gateReq != nil {
		out.ApprovalID = gateReq.ID
	}
	if cascade {
		out.Cascade = true
		out.CascadeDeleted = cascadeDeleted
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(out))
}

// backupDeleteBody is the v1-stable result. ApprovalID is populated
// only when --require-approval was used; AlreadyDeleted is true when
// re-deleting an already-tombstoned backup (idempotent path).
//
// Cascade fields surface ONLY when --cascade was used. The
// CascadeDeleted slice is the full deletion order (leaf-first),
// matching the audit-chain entry's `cascade_deleted` body field.
// omitempty keeps the JSON shape compact for the common single-
// backup-delete case.
type backupDeleteBody struct {
	Deployment     string   `json:"deployment"`
	BackupID       string   `json:"backup_id"`
	Reason         string   `json:"reason,omitempty"`
	ApprovalID     string   `json:"approval_id,omitempty"`
	AlreadyDeleted bool     `json:"already_deleted,omitempty"`
	Cascade        bool     `json:"cascade,omitempty"`
	CascadeDeleted []string `json:"cascade_deleted,omitempty"`
}

// WriteText renders the delete result as human-readable text to w, listing
// cascade-deleted backup IDs when applicable.
func (b backupDeleteBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.AlreadyDeleted {
		fmt.Fprintf(bw, "✓ backup %s/%s already tombstoned (no-op)\n", b.Deployment, b.BackupID)
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	if b.Cascade {
		fmt.Fprintf(bw, "✓ %s tombstoned (cascade: %d backup(s) total)\n",
			b.Deployment, len(b.CascadeDeleted))
		for i, id := range b.CascadeDeleted {
			marker := "├─"
			if i == len(b.CascadeDeleted)-1 {
				marker = "└─"
			}
			fmt.Fprintf(bw, "  %s %s\n", marker, id)
		}
		if b.Reason != "" {
			fmt.Fprintf(bw, "  Reason:   %s\n", b.Reason)
		}
		if b.ApprovalID != "" {
			fmt.Fprintf(bw, "  Approval: %s\n", b.ApprovalID)
		}
		fmt.Fprintf(bw, "  Note:     chunks reclaimed by next `repo gc --apply` cycle")
		_, err := io.WriteString(w, bw.String())
		return err
	}
	fmt.Fprintf(bw, "✓ backup %s/%s tombstoned\n", b.Deployment, b.BackupID)
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason:   %s\n", b.Reason)
	}
	if b.ApprovalID != "" {
		fmt.Fprintf(bw, "  Approval: %s\n", b.ApprovalID)
	}
	fmt.Fprintf(bw, "  Note:     chunks reclaimed by next `repo gc --apply` cycle")
	_, err := io.WriteString(w, bw.String())
	return err
}
