// kms_shred.go — CLI surface for destroying the KEK after approval workflow gating.
package cli

import (
	"context"
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

// KMSShredOp is the approval-namespace string the `kms shred`
// destructive op binds to. The Target is the keyring directory's
// canonical path so an approval for keyring A cannot be redeemed
// against keyring B.
const KMSShredOp = approval.Op("kms.shred")

// newKmsShredCmd implements `pg_hardstorage kms shred --repo <url>
// --require-approval <id>`.
//
// "Shred" here means: irreversibly destroy the local KEK that wraps
// every encrypted backup's DEK. After shred, every backup encrypted
// with that KEK is permanently unrecoverable — by design. This is
// the operator-facing GDPR Article 17 (right to erasure) primitive.
//
// Why mandatory n-of-m: shred is the most consequential destructive
// op in the binary. A typo or compromised credential should not be
// able to walk a fleet's encrypted history. We refuse to run
// without an approved n-of-m request whose Op is `kms.shred` and
// whose Target is the canonical keyring-directory path.
//
// Future+: KMS-provider plugins (AWS KMS, GCP KMS, Vault Transit)
// have their own shred semantics — they typically schedule
// destruction with a cooldown window. The plugin abstraction will
// route `kms shred --provider aws-kms ...` to the right backend;
// for the local-keyring case is implemented and provider-
// specific shred is a follow-up that drops in behind the same CLI
// shape.
func newKmsShredCmd() *cobra.Command {
	var (
		repoURL         string
		reason          string
		requireApproval string
		yes             bool
		confirmKeyring  string
		dryRun          bool
	)
	c := &cobra.Command{
		Use:   "shred",
		Short: "Irreversibly destroy the local KEK (n-of-m approval REQUIRED)",
		Long: `Destroy the local key-encryption key at the resolved keyring
directory. After this completes, every backup whose DEK was wrapped
with this KEK is permanently unrecoverable — that's the point.

The op is gated by THREE independent safety mechanisms:

  1. A mandatory n-of-m approval workflow. The approval's Op must
     be ` + "`" + `kms.shred` + "`" + ` and its Target must be the canonical keyring
     directory path; shred is refused at the gate otherwise.
  2. A typed-confirmation flag (--confirm-keyring) where the
     operator must repeat the literal keyring directory path. A
     compromised credential alone cannot drive shred without
     knowing the exact path.
  3. An acknowledgement flag (--yes) for non-interactive use.

Before shredding, the command enumerates every backup manifest in
the repo whose DEK is wrapped by this KEK — operators see the
exact scope of what becomes unrecoverable, captured in the audit
chain alongside the shred event.

Use --dry-run to enumerate the affected backups WITHOUT destroying
the KEK or requiring the approval / typed-keyring / yes gates.
Dry-run is the operator-friendly preview path: "show me what
shredding would destroy, so I can decide whether to ask for an
approval at all."  Dry-run never writes to the audit chain (it
performs no state change worth recording) and never touches the
KEK file.

To destroy provider-managed keys (AWS KMS, GCP KMS, Vault Transit),
use the provider's own destruction primitives — those have their
own cooldown windows + audit trails that this binary doesn't
attempt to reproduce.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKmsShred(cmd, repoURL, reason, requireApproval, confirmKeyring, yes, dryRun)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL the approval lives in (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&reason, "reason", "",
		"free-form reason captured in the audit chain (e.g. \"GDPR Art. 17 request #4421\")")
	c.Flags().StringVar(&requireApproval, "require-approval", "",
		"approval request ID — REQUIRED (kms shred refuses without an approved n-of-m gate)")
	c.Flags().BoolVar(&yes, "yes", false,
		"acknowledge that this operation is irreversible (still requires --require-approval and --confirm-keyring)")
	c.Flags().StringVar(&confirmKeyring, "confirm-keyring", "",
		"the literal keyring directory path; must match the resolved keyring (defence against compromised credentials)")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"preview the affected-backup scope without destroying the KEK or requiring approval / typed-keyring / yes gates")
	return c
}

func runKmsShred(cmd *cobra.Command, repoURL, reason, approvalID, confirmKeyring string, yes, dryRun bool) error {
	d := DispatcherFrom(cmd)

	// Resolve the canonical keyring directory the same way every
	// other CLI command does. The approval's Target binds to this
	// path; the gate refuses if they don't match.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	keyringDir := p.Keyring.Value

	// --dry-run skips every gate: it neither destroys the KEK nor
	// requires an approval, since enumeration is read-only state.
	// Without this preview path operators have to set up an
	// approval just to find out whether shred would even affect
	// anything they care about — that's the wrong order of
	// operations.  We DO still require --repo (the dry-run scans
	// that repo's manifests).
	if dryRun {
		_, sp, err := openRepo(cmd.Context(), repoURL)
		if err != nil {
			return err
		}
		defer sp.Close()
		affected, scanErr := scanAffectedBackups(cmd.Context(), sp, keystore.KEKRefLocal)
		body := kmsShredBody{
			KeyringDir:    keyringDir,
			Reason:        reason,
			DryRun:        true,
			AffectedCount: len(affected),
			AffectedIDs:   affected,
		}
		if scanErr != nil {
			body.AffectedScanError = scanErr.Error()
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}

	if approvalID == "" {
		return output.NewError("usage.missing_flag",
			"kms shred: --require-approval is REQUIRED — this op cannot run without an approved n-of-m gate").
			WithSuggestion(&output.Suggestion{
				Human:   "create an approval first; the simplest workflow uses your existing operator keyring as the approver identity",
				Command: "pg_hardstorage approval request --op kms.shred --target <keyring-dir> --threshold 2 --approver-key alice.pub --approver-key bob.pub --repo " + repoURL,
			}).Wrap(output.ErrUsage)
	}

	// Typed-keyring gate is the third independent safety mechanism
	// (audit).  An attacker with compromised credentials who
	// can satisfy --require-approval and --yes still has to know
	// the literal keyring path on the operator's host.
	if confirmKeyring == "" {
		return output.NewError("usage.confirmation_required",
			"kms shred: --confirm-keyring is REQUIRED — type the literal keyring directory path to confirm").
			WithSuggestion(&output.Suggestion{
				Human:   fmt.Sprintf("re-run with --confirm-keyring %q", keyringDir),
				Command: fmt.Sprintf("pg_hardstorage kms shred --repo %s --require-approval %s --confirm-keyring %q --yes", repoURL, approvalID, keyringDir),
			}).Wrap(output.ErrUsage)
	}
	if confirmKeyring != keyringDir {
		return output.NewError("usage.confirmation_mismatch",
			fmt.Sprintf("kms shred: --confirm-keyring %q does not match resolved keyring %q", confirmKeyring, keyringDir)).
			Wrap(output.ErrUsage)
	}

	// Open the repo + run the gate.
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	gateReq, gerr := approval.NewStore(sp).Gate(cmd.Context(), approval.GateOptions{
		RequestID: approvalID,
		Op:        KMSShredOp,
		Target:    keyringDir,
	})
	if gerr != nil {
		return mapApprovalGateError("kms shred", approvalID, gerr)
	}

	// Acknowledgement of irreversibility (kept even with the
	// typed-keyring gate above so a transcribed command-line
	// can't accidentally proceed without a deliberate flag).
	if !yes {
		return output.NewError("usage.confirmation_required",
			"kms shred: pass --yes to acknowledge that this operation is irreversible (n-of-m approval and --confirm-keyring already passed)").
			Wrap(output.ErrUsage)
	}

	// Enumerate the affected backups BEFORE destroying the KEK.
	// This is best-effort: a List failure on a single deployment
	// shouldn't block a GDPR-driven shred, but the operator wants
	// to see the count — and the audit log NEEDS to record it.
	affected, scanErr := scanAffectedBackups(cmd.Context(), sp, keystore.KEKRefLocal)
	if scanErr != nil {
		// Surface but don't block: the operator already passed three
		// gates; the scan is informational.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"kms shred: warning: affected-backup scan failed: %v (proceeding; the audit log will record an unknown scope)\n", scanErr)
	}

	if err := keystore.ShredKEK(keyringDir); err != nil {
		if errors.Is(err, keystore.ErrKEKAlreadyShred) {
			// Idempotent: KEK already gone, no work to do.
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(kmsShredBody{
				KeyringDir:  keyringDir,
				ApprovalID:  gateReq.ID,
				AlreadyDone: true,
			}))
		}
		return kmsOpError(err, "kms shred", "kms.shred_failed", nil)
	}

	// Audit emission. Always-on for kms shred — the most signal-
	// dense action in the system. Body captures keyring path,
	// approval, reason, and the affected-backup scope so the
	// audit log records exactly what became unrecoverable.  Best-
	// effort; a failed audit doesn't undo the shred (the bytes
	// are already gone).
	body := map[string]any{
		"keyring_dir":           keyringDir,
		"approval_id":           gateReq.ID,
		"approval_op":           string(gateReq.Op),
		"threshold":             gateReq.Threshold,
		"approvers":             len(gateReq.Approvals),
		"reason":                reason,
		"affected_backup_count": len(affected),
		"affected_backup_ids":   affected,
	}
	if scanErr != nil {
		body["affected_scan_error"] = scanErr.Error()
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action:    "kms.shred",
		Subject:   audit.Subject{Repo: repoURL},
		Timestamp: time.Now().UTC(),
		Body:      body,
	})

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(kmsShredBody{
		KeyringDir:    keyringDir,
		ApprovalID:    gateReq.ID,
		Reason:        reason,
		AffectedCount: len(affected),
		AffectedIDs:   affected,
	}))
}

// scanAffectedBackups walks every manifest in every deployment in
// the repo and returns the backup IDs whose KEKRef matches the
// shred target.  Best-effort: per-deployment errors are aggregated
// rather than aborting the scan, because the audit log's
// affected-scope field is most useful when populated even
// partially.
//
// Cost: one List + one Get per manifest.  Acceptable for a
// destructive op the operator already passed three gates for.
func scanAffectedBackups(ctx context.Context, sp storage.StoragePlugin, kekRef string) ([]string, error) {
	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	// Listing for scope, not for trust.  ManifestStore.List rejects
	// a nil verifier ("manifest: nil verifier"), and provisioning a
	// verifier via loadVerifier() touches the keyring — but kms
	// shred --dry-run is documented as keyring-free.  Use
	// ListAttestationless which parses the manifest without
	// signature verification; a forged manifest still represents
	// state in the repo the operator cares about for preview.
	var affected []string
	var firstErr error
	seen := map[string]struct{}{}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		affected = append(affected, id)
	}
	for _, dep := range deployments {
		for m, mErr := range ms.ListAttestationless(ctx, dep) {
			if mErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("deployment %q: %w", dep, mErr)
				}
				continue
			}
			if m == nil {
				continue
			}
			// Unencrypted backups can't be in scope: their
			// Encryption block is nil.  Skip without panicking
			// on m.Encryption.KEKRef (the previous code crashed
			// on the first unencrypted manifest in a mixed-mode
			// repo — see the scrub fix's pattern).
			if m.Encryption == nil {
				continue
			}
			if m.Encryption.KEKRef == kekRef {
				add(m.BackupID)
			}
		}
	}
	// Replicas too. ListAttestationless walks PRIMARY manifests only, but
	// a backup's REPLICA copy can lag (a KEK rotation that rewrote the
	// primary but not the replica). A replica still wrapped under the
	// shred target is just as unrecoverable once the KEK is gone, so it
	// MUST be reported here — otherwise shred under-states its blast
	// radius and an operator strands the replica.
	if rerr := scanAffectedReplicas(ctx, sp, kekRef, add); rerr != nil && firstErr == nil {
		firstErr = rerr
	}
	return affected, firstErr
}

// scanAffectedReplicas walks manifests/_replicas/ and calls add(backupID)
// for every replica manifest wrapped under kekRef.
func scanAffectedReplicas(ctx context.Context, sp storage.StoragePlugin, kekRef string, add func(string)) error {
	const prefix = "manifests/_replicas/"
	const suffix = ".manifest.json"
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return fmt.Errorf("list replicas: %w", err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		key := info.Key
		if !strings.HasSuffix(key, suffix) || strings.Contains(key, ".tmp.") {
			continue
		}
		rc, gerr := sp.Get(ctx, key)
		if gerr != nil {
			continue // racing prune/delete; not in scope for this scan
		}
		raw, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			continue
		}
		m, perr := backup.ParseAttestationless(raw)
		if perr != nil || m.Encryption == nil {
			continue
		}
		if m.Encryption.KEKRef == kekRef {
			add(strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix))
		}
	}
	return nil
}

// kmsShredBody is the v1 result. AlreadyDone surfaces the idempotent
// re-shred path so an automation script can tell "I just shredded"
// from "it was already shredded".  AffectedCount + AffectedIDs
// surface the scope of what just became unrecoverable; the audit
// chain captures the same fields.  DryRun=true means the report is
// a preview only — the KEK was NOT touched and no audit event was
// written.
type kmsShredBody struct {
	KeyringDir        string   `json:"keyring_dir"`
	ApprovalID        string   `json:"approval_id,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	AlreadyDone       bool     `json:"already_done,omitempty"`
	DryRun            bool     `json:"dry_run,omitempty"`
	AffectedCount     int      `json:"affected_backup_count,omitempty"`
	AffectedIDs       []string `json:"affected_backup_ids,omitempty"`
	AffectedScanError string   `json:"affected_scan_error,omitempty"`
}

// WriteText renders the shred outcome — dry-run preview, idempotent no-op,
// or destructive result — as human-readable text to w.
func (b kmsShredBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	switch {
	case b.DryRun:
		fmt.Fprintf(bw, "✓ kms shred --dry-run — preview only, KEK NOT destroyed\n")
		fmt.Fprintf(bw, "  Keyring:  %s\n", b.KeyringDir)
		if b.Reason != "" {
			fmt.Fprintf(bw, "  Reason:   %s\n", b.Reason)
		}
		writeAffectedSummary(bw, b)
		if b.AffectedScanError != "" {
			fmt.Fprintf(bw, "  Scan warning: %s\n", b.AffectedScanError)
		}
		fmt.Fprintf(bw, "  Note:     re-run without --dry-run plus --require-approval / --confirm-keyring / --yes to actually destroy the KEK")
		_, err := io.WriteString(w, bw.String())
		return err
	case b.AlreadyDone:
		fmt.Fprintf(bw, "✓ kms shred — KEK already absent at %s (idempotent no-op)\n", b.KeyringDir)
	default:
		fmt.Fprintf(bw, "✓ kms shred — KEK irreversibly destroyed\n")
		fmt.Fprintf(bw, "  Keyring:  %s\n", b.KeyringDir)
		if b.Reason != "" {
			fmt.Fprintf(bw, "  Reason:   %s\n", b.Reason)
		}
		writeAffectedSummary(bw, b)
	}
	fmt.Fprintf(bw, "  Approval: %s\n", b.ApprovalID)
	fmt.Fprintf(bw, "  Note:     every backup wrapped with this KEK is now permanently unrecoverable")
	_, err := io.WriteString(w, bw.String())
	return err
}

// writeAffectedSummary renders the "N backup(s) now unrecoverable"
// (or "would become unrecoverable" for dry-run) block, consistent
// across the live shred and dry-run paths.
func writeAffectedSummary(bw *strings.Builder, b kmsShredBody) {
	verb := "now unrecoverable"
	if b.DryRun {
		verb = "would become unrecoverable"
	}
	fmt.Fprintf(bw, "  Affected: %d backup(s) %s", b.AffectedCount, verb)
	const sample = 5
	switch {
	case b.AffectedCount == 0:
		fmt.Fprintf(bw, " (none — no encrypted backups in this repo were wrapped with this KEK)\n")
	case b.AffectedCount <= sample:
		fmt.Fprintf(bw, ":\n")
		for _, id := range b.AffectedIDs {
			fmt.Fprintf(bw, "    - %s\n", id)
		}
	default:
		fmt.Fprintf(bw, " (first %d shown; full list in audit chain):\n", sample)
		for _, id := range b.AffectedIDs[:sample] {
			fmt.Fprintf(bw, "    - %s\n", id)
		}
		fmt.Fprintf(bw, "    ... +%d more\n", b.AffectedCount-sample)
	}
}
