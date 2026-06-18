// repair.go — CLI surface for repair operations (attestation re-sign, manifest recovery, index, chunks, scrub).
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// newRepairCmd implements the `repair` command tree. v0.1 ships:
//
//	repair chunks --orphans   list (or delete) chunks no manifest references
//	repair chunks --missing   list manifests that reference missing chunks
//	repair scrub              SHA-verify chunks (sampled or full)
//
// `repair manifest`, `repair slot`, `repair index`, `repair
// attestation` are deferred to; the SPEC keeps them in the
// command vocabulary so operator muscle memory translates.
func newRepairCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "repair",
		Short: "Targeted repair tools — explicit, idempotent, audited",
		Long: `Each repair subcommand is explicit about what it touches and
defaults to dry-run. --apply flips writes on. Failures are
structured (per-error code + suggestion) and the safety gates
match the rest of the system: nothing destructive happens without
the operator typing --apply (and, for the most destructive paths,
--yes).`,
	}
	c.AddCommand(newRepairChunksCmd())
	c.AddCommand(newRepairScrubCmd())
	c.AddCommand(newRepairManifestCmd())
	c.AddCommand(newRepairSlotCmd())
	c.AddCommand(newRepairAttestationCmd())
	c.AddCommand(newRepairIndexCmd())
	return c
}

// newRepairAttestationCmd implements `repair attestation` — re-sign
// a manifest whose attestation no longer validates with the current
// keypair, then write an audit-log event so the provenance trail
// records that this happened.
//
// The use case: the operator rotated the signing keypair and now
// historical manifests fail ParseAndVerify with ErrPublicKeyMismatch.
// `repair attestation` parses the manifest WITHOUT signature
// verification, re-signs with the local Signer, writes back, and
// emits a `repair.attestation.resigned` audit event capturing the
// original embedded-public-key fingerprint.
//
// Refuses to run on a manifest that already verifies — re-signing a
// working manifest silently changes its keypair binding without
// recording why, which is the exact attack vector the audit chain
// is supposed to detect. The operator MUST pass --force to override.
func newRepairAttestationCmd() *cobra.Command {
	var (
		repoURL string
		actor   string
		reason  string
		force   bool
	)
	c := &cobra.Command{
		Use:          "attestation <deployment> <backup-id>",
		Short:        "Re-sign a manifest with the current keypair (audited)",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairAttestation(cmd, args[0], args[1], repoURL, actor, reason, force)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&actor, "actor", "",
		"operator principal (lands in the audit event for traceability)")
	c.Flags().StringVar(&reason, "reason", "",
		"why the re-sign is happening (lands in the audit event)")
	c.Flags().BoolVar(&force, "force", false,
		"re-sign even when the current attestation already verifies")
	return c
}

func runRepairAttestation(cmd *cobra.Command, deployment, backupID, repoURL, actor, reason string, force bool) error {
	d := DispatcherFrom(cmd)
	signer, verifier, err := loadSignerAndVerifier()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	primaryKey := backup.PrimaryPath(deployment, backupID)
	body, err := readManifestKey(cmd.Context(), sp, primaryKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("repair attestation: backup %s/%s not found",
					deployment, backupID)).Wrap(err)
		}
		return output.NewError("repair.attestation.read",
			fmt.Sprintf("repair attestation: read manifest: %v", err)).Wrap(err)
	}

	// Determine current verification state. Refuses to re-sign a
	// valid manifest without --force.
	var origFingerprint string
	if currentlyValid(body, verifier) {
		if !force {
			return output.NewError("aborted.attestation_valid",
				fmt.Sprintf("repair attestation: %s/%s already verifies; pass --force to re-sign anyway",
					deployment, backupID)).
				WithSuggestion(&output.Suggestion{
					Human: "re-signing a valid manifest changes its keypair binding without an integrity reason. The audit chain catches this attack — operators rotating keys should still pass --force consciously.",
				})
		}
	}
	// Capture the original fingerprint before re-sign for the audit.
	origFingerprint = extractAttestationFingerprint(body)

	// Parse without verify so we can re-sign even when verify fails.
	m, err := backup.ParseAttestationless(body)
	if err != nil {
		return output.NewError("repair.attestation.parse",
			fmt.Sprintf("repair attestation: parse: %v", err)).Wrap(err)
	}
	if m.Deployment != deployment || m.BackupID != backupID {
		return output.NewError("verify.identity_mismatch",
			fmt.Sprintf("repair attestation: manifest body says %s/%s, operator asked for %s/%s",
				m.Deployment, m.BackupID, deployment, backupID))
	}

	// Refuse to re-sign TAMPERED content. A manifest that fails the current
	// verifier could be (a) legitimately key-rotated — authentic content
	// signed by an old key — or (b) tampered: bytes altered after signing.
	// ParseAndVerify can't tell them apart (it returns ErrPublicKeyMismatch
	// before ever checking the signature), and re-signing case (b) would
	// launder attacker-modified content under the operator's trusted key,
	// defeating the whole attestation. VerifyEmbedded checks the signature
	// against the manifest's OWN embedded key: a valid self-signature means
	// the content is authentic (the key-rotation case this command exists
	// for); an invalid one means the bytes don't match what was signed —
	// refuse, --force or not. (Mirrors repair manifest's "never manufacture
	// a bad copy" posture.)
	if _, scerr := backup.VerifyEmbedded(body); scerr != nil {
		return output.NewError("verify.attestation_tampered",
			fmt.Sprintf("repair attestation: refusing to re-sign %s/%s — its content does not match its own embedded signature, so it was altered after signing (not a key rotation): %v",
				deployment, backupID, scerr)).
			WithSuggestion(&output.Suggestion{
				Human: "this is not a re-signable key-rotation case — the manifest bytes were modified after they were signed. Recover an authentic copy with `pg_hardstorage repair manifest` (rebuilds from the verified primary/replica) or re-back-up from the source; do not re-sign tampered content.",
			})
	}

	if err := m.Sign(signer); err != nil {
		return output.NewError("repair.attestation.sign",
			fmt.Sprintf("repair attestation: sign: %v", err)).Wrap(err)
	}
	newBytes, err := m.MarshalToBytes()
	if err != nil {
		return output.NewError("repair.attestation.marshal",
			fmt.Sprintf("repair attestation: marshal: %v", err)).Wrap(err)
	}

	// The manifest is stored in TWO copies — primary and replica —
	// and Read falls back to the replica when the primary fails to
	// verify ("survivability against a single corrupted primary").
	// A key rotation breaks BOTH copies' signatures with the same
	// stale key, so re-signing only the primary would leave the
	// replica stale-signed: the happy path still verifies (primary is
	// checked first), but the redundancy is silently gone — a later
	// primary loss falls back to a replica that no longer verifies,
	// exactly the case the replica exists to defend. Re-sign and
	// install BOTH copies with the same re-signed bytes.
	//
	// Replica first: until the replica carries the new signature a
	// crash leaves the primary as the operator found it (broken), so a
	// plain re-run (no --force) cleanly resumes. Once the replica is
	// valid there is always a verifiable copy for Read to fall back
	// to, and the only no-valid-copy window is the replica's own
	// delete→rename gap — which is just the pre-repair state.
	// Re-apply the repo's WORM lock to both re-signed copies (a re-sign on a
	// compliance repo must not leave the manifest deletable).
	wormUntil, wormMode := wormPolicyFor(repoMeta)
	replicaKey := backup.ReplicaPath(backupID)
	if err := installManifestOverwrite(cmd.Context(), sp, replicaKey, newBytes, "attest", wormUntil, wormMode); err != nil {
		return output.NewError("repair.attestation.install_replica",
			fmt.Sprintf("repair attestation: install replica: %v", err)).Wrap(err)
	}
	if err := installManifestOverwrite(cmd.Context(), sp, primaryKey, newBytes, "attest", wormUntil, wormMode); err != nil {
		return output.NewError("repair.attestation.install",
			fmt.Sprintf("repair attestation: install primary: %v", err)).Wrap(err)
	}

	// Audit-log event. We DO NOT fail the command on audit-log
	// write failures — the manifest re-sign is durable; a
	// separate audit verify-chain run would surface the missing
	// event. Better than rolling back a successful re-sign.
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	newFingerprint := extractAttestationFingerprint(newBytes)
	auditEv := &audit.Event{
		Action: "repair.attestation.resigned",
		Actor:  actor,
		Subject: audit.Subject{
			Deployment: deployment,
			BackupID:   backupID,
			Repo:       repoURL,
		},
		Body: map[string]any{
			"reason":               reason,
			"forced":               force,
			"original_fingerprint": origFingerprint,
			"new_fingerprint":      newFingerprint,
		},
	}
	auditWriteErr := auditStore.Append(cmd.Context(), auditEv)

	body2 := repairAttestationBody{
		Deployment:          deployment,
		BackupID:            backupID,
		PrimaryKey:          primaryKey,
		ReplicaKey:          replicaKey,
		OriginalFingerprint: origFingerprint,
		NewFingerprint:      newFingerprint,
		Forced:              force,
		AuditEventID:        auditEv.ID,
		AuditWriteErr:       errString(auditWriteErr),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body2))
}

// installManifestOverwrite atomically replaces an EXISTING manifest
// key with body, via stage-tmp → delete → rename-if-not-exists.
// RenameIfNotExists is the only atomic rename the storage interface
// exposes, so an overwrite needs the prior Delete; the crash window
// between the two is bounded — the manifest's sibling copy (primary
// or replica) is left intact, so Read still resolves and a re-run
// finishes the job. fs and s3 Delete are both idempotent on a missing
// key, so a half-finished prior attempt re-runs cleanly. infix names
// the caller (e.g. "attest") so a leaked tmp is attributable.
func installManifestOverwrite(ctx context.Context, sp storage.StoragePlugin, key string, body []byte, infix string, retainUntil time.Time, mode storage.WORMMode) error {
	tmp := key + ".tmp." + infix + "." + randHex8()
	if _, err := sp.Put(ctx, tmp, strings.NewReader(string(body)),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		return fmt.Errorf("stage tmp: %w", err)
	}
	_ = sp.Delete(ctx, key)
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = sp.Delete(ctx, tmp)
		return fmt.Errorf("install %q: %w", key, err)
	}
	// Re-apply the repo's WORM lock to the overwritten manifest so a repair
	// on a compliance repo doesn't leave the copy it just rewrote freely
	// deletable. Empty mode → Compliance; non-WORM backends ignore it.
	if !retainUntil.IsZero() {
		m := mode
		if m == "" {
			m = storage.WORMCompliance
		}
		if err := sp.SetRetention(ctx, key, retainUntil, m); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return fmt.Errorf("lock %q: %w", key, err)
		}
	}
	return nil
}

// wormPolicyFor extracts the (deadline, mode) that a repair-time manifest
// write should stamp, from the destination repo's metadata. Zero deadline
// when WORM is off — the write is then a plain copy.
func wormPolicyFor(repoMeta *repo.Metadata) (time.Time, storage.WORMMode) {
	if repoMeta == nil || repoMeta.WORM == nil {
		return time.Time{}, ""
	}
	return repoMeta.WORM.RetainUntil(time.Now().UTC()), storage.WORMMode(repoMeta.WORM.Mode)
}

// loadSignerAndVerifier loads the local keypair (same path
// loadVerifier uses, but returning both halves).
func loadSignerAndVerifier() (*backup.Signer, *backup.Verifier, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, nil, output.NewError("internal", err.Error()).Wrap(err)
	}
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return nil, nil, output.NewError("internal",
			fmt.Sprintf("repair: keystore: %v", err)).Wrap(err)
	}
	return signer, verifier, nil
}

// currentlyValid reports whether the on-disk manifest verifies
// against verifier — used to decide whether --force is required.
func currentlyValid(body []byte, verifier *backup.Verifier) bool {
	_, err := backup.ParseAndVerify(body, verifier)
	return err == nil
}

// extractAttestationFingerprint returns a SHA-256 fingerprint of the
// manifest's embedded public key, or "" if no attestation is present
// or the body can't be parsed. Used to record what the manifest WAS
// signed by before we re-signed it.
func extractAttestationFingerprint(body []byte) string {
	m, err := backup.ParseAttestationless(body)
	if err != nil || m.Attestation == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(m.Attestation.PublicKey))
	return hex.EncodeToString(sum[:8])
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type repairAttestationBody struct {
	Deployment          string `json:"deployment"`
	BackupID            string `json:"backup_id"`
	PrimaryKey          string `json:"primary_key"`
	ReplicaKey          string `json:"replica_key,omitempty"`
	OriginalFingerprint string `json:"original_fingerprint,omitempty"`
	NewFingerprint      string `json:"new_fingerprint"`
	Forced              bool   `json:"forced,omitempty"`
	AuditEventID        string `json:"audit_event_id"`
	AuditWriteErr       string `json:"audit_write_error,omitempty"`
}

// WriteText renders the attestation re-sign result as human-readable text to w.
func (b repairAttestationBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ repair attestation: re-signed %s/%s\n", b.Deployment, b.BackupID)
	if b.OriginalFingerprint != "" {
		fmt.Fprintf(bw, "  was signed by: sha256:%s\n", b.OriginalFingerprint)
	}
	fmt.Fprintf(bw, "  now signed by: sha256:%s\n", b.NewFingerprint)
	if b.ReplicaKey != "" {
		fmt.Fprintln(bw, "  re-signed both primary + replica copies")
	}
	if b.Forced {
		fmt.Fprintln(bw, "  (forced — original attestation was valid)")
	}
	fmt.Fprintf(bw, "  audit event:  %s", b.AuditEventID)
	if b.AuditWriteErr != "" {
		fmt.Fprintf(bw, "\n  ✗ audit write error: %s (manifest re-sign IS durable; investigate audit log)",
			b.AuditWriteErr)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// newRepairManifestCmd implements `repair manifest <deployment> <backup-id>`.
//
// Recovery flow:
//
//  1. Read manifests/_replicas/<id>.manifest.json (the redundant
//     copy every commit writes alongside the primary).
//  2. Verify signature with the local public key — refuses if the
//     replica is itself unsigned-by-us. Better to surface "we have
//     no usable copy" loudly than restore a foreign-signed manifest.
//  3. Cross-check deployment + backup_id match the operator's
//     arguments (replica file naming uses backup_id only, so a
//     mismatched deployment in the manifest body is a corruption
//     signal we refuse to ignore).
//  4. Write to the primary path via the atomic .tmp + rename
//     pattern. Refuses to overwrite a present-and-valid primary
//     unless --force is set (the operator is sometimes running
//     this defensively, sometimes recovering — explicit > clever).
//
// Idempotent: repeated invocations on a healthy primary are a no-op
// without --force.
func newRepairManifestCmd() *cobra.Command {
	var (
		repoURL string
		force   bool
	)
	c := &cobra.Command{
		Use:   "manifest <deployment> <backup-id>",
		Short: "Reconcile a manifest's primary + replica copies (recover whichever side is missing/corrupt)",
		Long: `Reconcile the two on-repo copies of a manifest. Whichever copy is
good repairs the other:

  - corrupt/missing PRIMARY, good replica  → primary re-fetched from
    the replica (the classic recovery direction).
  - missing/corrupt REPLICA, good primary  → replica rebuilt from the
    primary, restoring cross-prefix corruption survivability when the
    commit-time replica write failed or the replica was deleted
    out-of-band (rebuilt_replica=true in the result).

If neither copy verifies, the manifest is unrecoverable and the backup
it describes can no longer be restored.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairManifest(cmd, args[0], args[1], repoURL, force)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&force, "force", false,
		"overwrite the primary even when it's already valid (use sparingly)")
	return c
}

func runRepairManifest(cmd *cobra.Command, deployment, backupID, repoURL string, force bool) error {
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

	// Carry the repo's WORM policy so a rebuilt replica / overwritten primary
	// is locked like the commit-time copy (a compliance repo must not get an
	// unlocked manifest out of a repair). Zero when WORM is off.
	wormUntil, wormMode := wormPolicyFor(repoMeta)

	primaryKey := backup.PrimaryPath(deployment, backupID)
	replicaKey := backup.ReplicaPath(backupID)

	// Read the replica first — it's what we're recovering FROM.
	replicaBytes, err := readManifestKey(cmd.Context(), sp, replicaKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// The replica is the MISSING side (e.g. the commit-time
			// replica write failed, or it was deleted out-of-band).
			// Rebuild it FROM the primary to restore the lost
			// redundancy — data-loss audit path #3. EnsureReplica
			// refuses if the primary itself doesn't verify, so we
			// never manufacture a bad replica.
			rebuilt, herr := backup.NewManifestStore(sp).EnsureReplica(cmd.Context(), deployment, backupID, verifier, wormUntil, wormMode)
			if herr != nil {
				return output.NewError("notfound.replica_manifest",
					fmt.Sprintf("repair manifest: replica %s missing and could not be rebuilt from the primary: %v",
						replicaKey, herr)).
					WithSuggestion(&output.Suggestion{
						Human: "neither copy is usable; the manifest is unrecoverable and the backup it describes can no longer be restored.",
					}).Wrap(herr)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(repairManifestBody{
				Deployment:      deployment,
				BackupID:        backupID,
				PrimaryKey:      primaryKey,
				ReplicaKey:      replicaKey,
				PrimaryWasValid: true,
				RebuiltReplica:  rebuilt,
			}))
		}
		return output.NewError("repair.manifest.read_replica",
			fmt.Sprintf("repair manifest: read replica: %v", err)).Wrap(err)
	}
	repM, err := backup.ParseAndVerify(replicaBytes, verifier)
	if err != nil {
		// Replica is PRESENT but corrupt / foreign-signed. Don't
		// declare the manifest unrecoverable yet: the primary may be
		// perfectly valid, in which case the right repair is to rebuild
		// the replica FROM the primary — exactly what the missing-replica
		// branch above does. EnsureReplica clears the corrupt replica and
		// rebuilds it from the verified primary, refusing if the primary
		// doesn't verify either, so we never manufacture a bad copy.
		// (Without this, a corrupt replica + valid primary wrongly
		// reported "no recoverable copy" while a good primary sat right
		// there — the missing-replica path recovered but the corrupt one
		// didn't.)
		rebuilt, herr := backup.NewManifestStore(sp).EnsureReplica(cmd.Context(), deployment, backupID, verifier, wormUntil, wormMode)
		if herr != nil {
			return output.NewError("verify.replica_signature",
				fmt.Sprintf("repair manifest: replica is corrupt and could not be rebuilt from the primary: %v", herr)).
				WithSuggestion(&output.Suggestion{
					Human: "the replica copy is foreign-signed or corrupt AND the primary is not a verifiable rebuild source; this manifest has no recoverable copy in the repo. Investigate the audit log + the keyring's signing-pub fingerprint.",
				}).Wrap(herr)
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(repairManifestBody{
			Deployment:      deployment,
			BackupID:        backupID,
			PrimaryKey:      primaryKey,
			ReplicaKey:      replicaKey,
			PrimaryWasValid: true,
			RebuiltReplica:  rebuilt,
		}))
	}
	// Cross-check: the body's identity must match the operator's args.
	// Replica filename uses backup_id only; a wrong-deployment body
	// would be silent corruption otherwise.
	if repM.Deployment != deployment || repM.BackupID != backupID {
		return output.NewError("verify.replica_identity_mismatch",
			fmt.Sprintf("repair manifest: replica body says %s/%s, operator asked for %s/%s",
				repM.Deployment, repM.BackupID, deployment, backupID))
	}

	// If the primary IS already valid, refuse without --force.
	primaryValid := false
	if pBytes, err := readManifestKey(cmd.Context(), sp, primaryKey); err == nil {
		if _, perr := backup.ParseAndVerify(pBytes, verifier); perr == nil {
			primaryValid = true
		}
	}
	if primaryValid && !force {
		return output.NewError("aborted.primary_intact",
			fmt.Sprintf("repair manifest: primary %s is already valid; pass --force to overwrite anyway",
				primaryKey)).
			WithSuggestion(&output.Suggestion{
				Human: "this command is for recovery from corruption. If the primary verifies, the replica's content should be identical and overwriting risks zero data integrity wins.",
			})
	}

	// Atomic-replace via .tmp + rename (and re-apply the repo's WORM lock so
	// the repaired primary isn't left deletable on a compliance repo). We're
	// past the "primary intact + no force" abort gate, so we know we want to
	// write — either primary was invalid (corrupt content present) or
	// operator passed --force.
	if err := installManifestOverwrite(cmd.Context(), sp, primaryKey, replicaBytes, "repair", wormUntil, wormMode); err != nil {
		return output.NewError("repair.manifest.rename",
			fmt.Sprintf("repair manifest: install primary: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(repairManifestBody{
		Deployment:      deployment,
		BackupID:        backupID,
		PrimaryKey:      primaryKey,
		ReplicaKey:      replicaKey,
		PrimaryWasValid: primaryValid,
		Forced:          force,
	}))
}

// readManifestKey is a tiny helper that reads + closes. Returns
// storage.ErrNotFound on absent key.
func readManifestKey(ctx context.Context, sp storage.StoragePlugin, key string) ([]byte, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// randHex8 returns 8 random hex chars for tmp-file disambiguation.
// Same shape backup.randSuffix uses; duplicated here to keep cli's
// dep on backup limited to the public surface.
func randHex8() string {
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	now := time.Now().UnixNano()
	for i := 7; i >= 0; i-- {
		out[i] = hex[now&0xF]
		now >>= 4
	}
	return string(out)
}

type repairManifestBody struct {
	Deployment      string `json:"deployment"`
	BackupID        string `json:"backup_id"`
	PrimaryKey      string `json:"primary_key"`
	ReplicaKey      string `json:"replica_key"`
	PrimaryWasValid bool   `json:"primary_was_valid"`
	Forced          bool   `json:"forced,omitempty"`
	// RebuiltReplica is true when the repair rebuilt a missing/corrupt
	// REPLICA from a valid primary (the reverse of the usual
	// primary-from-replica recovery). See data-loss audit path #3.
	RebuiltReplica bool `json:"rebuilt_replica,omitempty"`
}

// WriteText renders the manifest-recovery result as human-readable text to w,
// noting whether the primary was overwritten or recovered from the replica.
func (b repairManifestBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.PrimaryWasValid {
		fmt.Fprintf(bw, "✓ repair manifest (force-overwrite): %s/%s\n", b.Deployment, b.BackupID)
	} else {
		fmt.Fprintf(bw, "✓ repair manifest: recovered %s/%s from replica\n", b.Deployment, b.BackupID)
	}
	fmt.Fprintf(bw, "  primary: %s\n", b.PrimaryKey)
	fmt.Fprintf(bw, "  replica: %s", b.ReplicaKey)
	_, err := io.WriteString(w, bw.String())
	return err
}

// newRepairIndexCmd implements `pg_hardstorage repair index` — walks
// chunks/sha256/ and reports the inventory.
//
// What this is NOT, despite the SPEC's "rebuild the local bloom
// filter" framing: v0.1 doesn't ship a persistent on-disk chunk
// index; the CAS keeps an in-memory `seen` cache that's naturally
// rebuilt on first access. There's no bloom filter file to recreate.
//
// What this IS: a diagnostic that walks chunks/sha256/, counts
// chunks per 2-char top-level bucket, totals bytes, and flags any
// non-parseable filenames as a corruption signal. Operationally
// this catches "your storage is enumerating but a sub-prefix is
// pathologically large" (early warning for hot keys) and "garbage
// files have appeared in chunks/sha256/" (corruption / misdirected
// `aws s3 cp`). The persistent-index implementation lands when the
// runner can usefully pre-load it.
func newRepairIndexCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "index",
		Short:        "Walk chunks/sha256/ and report the inventory",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepairIndex(cmd, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runRepairIndex(cmd *cobra.Command, repoURL string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	body := repairIndexBody{}
	bucketCounts := map[string]int64{}
	bucketBytes := map[string]int64{}

	for info, err := range sp.List(cmd.Context(), "chunks/sha256/") {
		if err != nil {
			return output.NewError("repair.index.list_failed",
				fmt.Sprintf("repair index: list chunks: %v", err)).Wrap(err)
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		// Parse the canonical chunk-key form: chunks/sha256/aa/bb/aabb<...>.chk
		// → top-level bucket "aa". Any path that doesn't fit becomes
		// an Unparseable finding rather than a sort-key.
		parts := strings.Split(info.Key, "/")
		if len(parts) < 4 || parts[0] != "chunks" || parts[1] != "sha256" {
			body.Unparseable++
			continue
		}
		bucket := parts[2]
		if len(bucket) != 2 || !isHexLower(bucket) {
			body.Unparseable++
			continue
		}
		if _, err := repo.ParseChunkKey(info.Key); err != nil {
			body.Unparseable++
			continue
		}
		bucketCounts[bucket]++
		bucketBytes[bucket] += info.Size
		body.TotalChunks++
		body.TotalBytes += info.Size
	}

	// Build top-N largest buckets list. 65,536 possible buckets makes
	// the full table impractical for text rendering; cap to top 10
	// for the at-a-glance view, expose all in JSON via Buckets.
	body.Buckets = make([]repairIndexBucket, 0, len(bucketCounts))
	for k, n := range bucketCounts {
		body.Buckets = append(body.Buckets, repairIndexBucket{
			Prefix: k,
			Chunks: n,
			Bytes:  bucketBytes[k],
		})
	}
	sort.Slice(body.Buckets, func(i, j int) bool {
		if body.Buckets[i].Chunks != body.Buckets[j].Chunks {
			return body.Buckets[i].Chunks > body.Buckets[j].Chunks
		}
		return body.Buckets[i].Prefix < body.Buckets[j].Prefix
	})
	body.UniqueBuckets = len(body.Buckets)

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// isHexLower reports whether every byte of s is a lowercase hex
// digit (0-9 or a-f). Bucket names use lowercase hex by convention;
// uppercase hex is a corruption signal we catch via Unparseable.
func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type repairIndexBucket struct {
	Prefix string `json:"prefix"`
	Chunks int64  `json:"chunks"`
	Bytes  int64  `json:"bytes"`
}

type repairIndexBody struct {
	TotalChunks   int64               `json:"total_chunks"`
	TotalBytes    int64               `json:"total_bytes"`
	UniqueBuckets int                 `json:"unique_buckets"`
	Unparseable   int64               `json:"unparseable,omitempty"`
	Buckets       []repairIndexBucket `json:"buckets,omitempty"`
}

// WriteText renders the chunk-tree inventory — totals, bucket coverage, and
// any unparseable findings — as human-readable text to w.
func (b repairIndexBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repair index — chunks/sha256/ inventory\n")
	fmt.Fprintf(bw, "  total chunks:    %d\n", b.TotalChunks)
	fmt.Fprintf(bw, "  total bytes:     %s\n", humanBytes(b.TotalBytes))
	fmt.Fprintf(bw, "  unique buckets:  %d / 65536\n", b.UniqueBuckets)
	if b.Unparseable > 0 {
		fmt.Fprintf(bw, "  ✗ unparseable:   %d (likely corruption — investigate the chunks/sha256/ tree)\n",
			b.Unparseable)
	}
	if len(b.Buckets) > 0 {
		fmt.Fprintln(bw, "  top buckets:")
		max := 10
		if len(b.Buckets) < max {
			max = len(b.Buckets)
		}
		for i := 0; i < max; i++ {
			bk := b.Buckets[i]
			fmt.Fprintf(bw, "    %s: %d chunks (%s)\n",
				bk.Prefix, bk.Chunks, humanBytes(bk.Bytes))
		}
		if len(b.Buckets) > max {
			fmt.Fprintf(bw, "    ... +%d more buckets\n", len(b.Buckets)-max)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// newRepairSlotCmd is a discoverability alias for `wal repair`. The
// SPEC's repair vocabulary is "every form of corruption gets a
// `repair <thing>` command" — having only `wal repair` violates
// muscle memory at 3am ("I'm in repair mode; what's the slot
// command?"). Same flags, same body, same exit codes.
func newRepairSlotCmd() *cobra.Command {
	var (
		pgConn   string
		repoURL  string
		slotName string
	)
	c := &cobra.Command{
		Use:   "slot <deployment>",
		Short: "Recreate a missing replication slot (alias for `wal repair`)",
		Long: `Recreates the named replication slot on the source PG, computes
the LSN gap (if any) between the slot's new restart_lsn and the
last archived WAL segment, and reports the result. Functionally
identical to ` + "`wal repair`" + `.

A non-zero positive gap means WAL was lost across the slot
recreation — the failover runbook documents the recovery options.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalRepair(cmd, args[0], pgConn, repoURL, slotName)
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the source PostgreSQL (required)")
	_ = c.MarkFlagRequired("pg-connection")
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — used to compute the gap from the highest archived segment (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&slotName, "slot", "",
		"replication slot name (default: pg_hardstorage_<deployment>)")
	return c
}

func newRepairChunksCmd() *cobra.Command {
	var (
		repoURL     string
		orphans     bool
		missing     bool
		apply       bool
		minChunkAge time.Duration
	)
	c := &cobra.Command{
		Use:          "chunks --orphans|--missing",
		Short:        "Find chunks not referenced by any manifest, or manifests with missing chunks",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orphans == missing {
				return output.NewError("usage.bad_flags",
					"repair chunks: pass exactly one of --orphans or --missing").Wrap(output.ErrUsage)
			}
			return runRepairChunks(cmd, repoURL, orphans, apply, minChunkAge)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&orphans, "orphans", false,
		"list chunks not referenced by any visible manifest")
	c.Flags().BoolVar(&missing, "missing", false,
		"list manifests that reference chunks the storage doesn't have")
	c.Flags().BoolVar(&apply, "apply", false,
		"with --orphans: actually delete the orphans (default: dry-run)")
	c.Flags().DurationVar(&minChunkAge, "min-chunk-age", repo.DefaultOrphanMinAge,
		"minimum age an unreferenced chunk must reach before --apply reaps it; defends an in-flight backup whose manifest hasn't committed yet (pass 0 to disable)")
	return c
}

func runRepairChunks(cmd *cobra.Command, repoURL string, orphans, apply bool, minChunkAge time.Duration) error {
	d := DispatcherFrom(cmd)
	_, sp, err := repo.Open(cmd.Context(), repoURL)
	if err != nil {
		return mapRepoOpenErr(repoURL, err)
	}
	defer sp.Close()

	refs, err := repo.CollectReferences(cmd.Context(), sp)
	if err != nil {
		return output.NewError("repair.collect_refs_failed",
			fmt.Sprintf("repair chunks: collect references: %v", err)).Wrap(err)
	}

	if orphans {
		// Chunk-age floor (default DefaultOrphanMinAge): never reap a
		// chunk an in-flight backup has written but not yet referenced
		// in a committed manifest.  repair is the everything's-on-fire
		// surface, but it should still not corrupt a concurrent backup.
		// --min-chunk-age 0 disables the floor on a quiesced repo.
		minAgeForCall := minChunkAge
		if minAgeForCall == 0 {
			minAgeForCall = -1
		}
		// Disarming the age floor on --apply removes the only guard
		// against reaping an in-flight backup's durable-but-uncommitted
		// chunks. `repo gc --apply` warns loudly when its floor is
		// disabled; the break-glass surface must too — an operator who
		// passed `--min-chunk-age 0` thinking it meant "use the default"
		// needs to see they disarmed it, not silently corrupt a
		// concurrent backup. Dry-runs delete nothing, so --apply-only.
		if apply && minAgeForCall <= 0 {
			_ = d.Event(cmd.Context(),
				output.NewEvent(output.SeverityWarning, "repair.chunks", "safety_floor_disabled").
					WithBody(map[string]any{
						"disabled_floors": []string{"min-chunk-age"},
						"impact":          "with the chunk-age floor disabled, --apply can delete an in-flight backup's chunks (durable but not yet manifest-committed) — unrecoverable",
						"hint":            "omit the flag (or pass a positive duration) to keep the default floor; here 0 means DISABLE, not 'use default'",
					}))
		}
		hashes, err := repo.FindOrphansWithOptions(cmd.Context(), sp, refs,
			repo.FindOrphansOptions{MinAge: minAgeForCall})
		if err != nil {
			return output.NewError("repair.find_orphans_failed",
				fmt.Sprintf("repair chunks: find orphans: %v", err)).Wrap(err)
		}
		body := repairChunksBody{
			DryRun:   !apply,
			Mode:     "orphans",
			Total:    len(hashes),
			RefCount: refs.Len(),
		}
		for _, h := range hashes {
			body.Chunks = append(body.Chunks, h.String())
		}
		if apply {
			cas := casdefault.New(sp)
			deleted := 0
			for _, h := range hashes {
				if err := cas.DeleteChunk(cmd.Context(), h); err != nil {
					return output.NewError("repair.delete_failed",
						fmt.Sprintf("repair chunks: delete %s: %v", h, err)).Wrap(err)
				}
				deleted++
			}
			body.Applied = deleted
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}

	// --missing path
	hashes, err := repo.FindMissing(cmd.Context(), sp, refs)
	if err != nil {
		return output.NewError("repair.find_missing_failed",
			fmt.Sprintf("repair chunks: find missing: %v", err)).Wrap(err)
	}
	body := repairChunksBody{
		DryRun:   true, // --missing is read-only
		Mode:     "missing",
		Total:    len(hashes),
		RefCount: refs.Len(),
	}
	for _, h := range hashes {
		body.Chunks = append(body.Chunks, h.String())
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newRepairScrubCmd() *cobra.Command {
	var (
		repoURL    string
		limit      int
		heal       bool
		replicaURL string
	)
	c := &cobra.Command{
		Use:          "scrub",
		Short:        "Verify chunk integrity (SHA-256 round-trip) across the repo",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `repair scrub samples N chunks (default 1000; 0 means every
referenced chunk), re-hashes each one, and reports mismatches.
Mismatches are the "your storage backend has corrupted bytes"
signal — exit code flips to ExitVerifyFailed so monitoring routes
correctly.

--heal --replica <url> turns the diagnostic into a remediation:
mismatched chunks get their bytes re-fetched from a replica repo
(the one populated by ` + "`pg_hardstorage repo replicate`" + `) and
re-written locally. The replica's chunk-envelope bytes are byte-
identical to the original (replicate copies verbatim), so a
successful heal restores the local copy to a state where the CAS's
plaintext-SHA round-trip passes again.

Heal is best-effort: a chunk missing at the replica is reported as
NotAtReplica and the run continues with the next mismatch. Use
--dry-run-heal to see what *would* heal without writing.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepairScrub(cmd, repoURL, limit, heal, replicaURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().IntVar(&limit, "limit", 1000,
		"sample size; 0 = full scrub (every chunk)")
	c.Flags().BoolVar(&heal, "heal", false,
		"refetch mismatched chunks from --replica and rewrite them locally")
	c.Flags().StringVar(&replicaURL, "replica", "",
		"replica repository URL to heal from (required when --heal is set)")
	return c
}

func runRepairScrub(cmd *cobra.Command, repoURL string, limit int, heal bool, replicaURL string) error {
	d := DispatcherFrom(cmd)
	if heal && replicaURL == "" {
		return output.NewError("usage.missing_flag",
			"repair scrub: --heal requires --replica <url>").Wrap(output.ErrUsage)
	}
	if heal && replicaURL == repoURL {
		return output.NewError("usage.bad_flag",
			"repair scrub: --replica must differ from --repo").Wrap(output.ErrUsage)
	}
	scrubMeta, sp, err := repo.Open(cmd.Context(), repoURL)
	if err != nil {
		return mapRepoOpenErr(repoURL, err)
	}
	defer sp.Close()

	// Per-manifest scrub: every backup manifest carries its own
	// encryption block (KEK ref + wrapped DEK), so the CAS that can
	// read its chunks differs per manifest.  Walk manifests, build
	// the right CAS per manifest, and verify the chunks that manifest
	// references.  Using a single default CAS across all chunks
	// silently treated every encrypted chunk as a mismatch (the
	// decryptor lookup failed and the loop counted the failure as
	// a "mismatch") — that's the bug this rewrite fixes.
	res, refsTotal, err := scrubManifestAware(cmd.Context(), sp, limit)
	if err != nil {
		return output.NewError("repair.scrub_failed",
			fmt.Sprintf("repair scrub: %v", err)).Wrap(err)
	}
	body := repairScrubBody{
		Sampled:         res.Sampled,
		OK:              res.OK,
		MismatchCount:   len(res.Mismatches),
		BytesVerified:   res.Bytes,
		ReferencedTotal: refsTotal,
	}
	for _, h := range res.Mismatches {
		body.Mismatches = append(body.Mismatches, h.String())
	}

	// No mismatches → done, nothing to heal.
	if len(res.Mismatches) == 0 {
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}

	// Mismatches present. If the operator didn't ask for --heal, this
	// is the diagnostic exit-code path: ExitVerifyFailed + a
	// suggestion that points at the actual heal command.
	if !heal {
		return output.NewError("verify.scrub_mismatch",
			fmt.Sprintf("repair scrub: %d chunk(s) failed verification", len(res.Mismatches))).
			WithSuggestion(&output.Suggestion{
				Human:   "auto-heal from a replica region: pass --heal --replica <replica-url>. The replica is the one populated by `pg_hardstorage repo replicate`.",
				Command: fmt.Sprintf("pg_hardstorage repair scrub --repo %s --heal --replica <replica-url>", repoURL),
			})
	}

	// --heal: open the replica and run the heal primitive against the
	// list of mismatch hashes from above.
	_, replicaSP, err := repo.Open(cmd.Context(), replicaURL)
	if err != nil {
		return mapRepoOpenErr(replicaURL, err)
	}
	defer replicaSP.Close()

	// Carry the destination repo's WORM policy so healed chunks are re-locked
	// on a compliance repo (the heal's IfNotExists Put carries no retention).
	healUntil, healMode := wormPolicyFor(scrubMeta)
	healRes, herr := repo.Heal(cmd.Context(), sp, replicaSP, res.Mismatches, repo.HealOptions{
		RetainUntil:   healUntil,
		RetentionMode: healMode,
	})
	if herr != nil {
		return output.NewError("repair.heal_failed",
			fmt.Sprintf("repair scrub --heal: %v", herr)).Wrap(herr)
	}
	body.HealResult = healRes
	body.ReplicaURL = replicaURL

	// Heal-time exit-code semantics: if EVERY mismatch was healed,
	// exit OK (the operator asked us to fix things, we fixed them).
	// If any couldn't be healed, surface the verify failure so cron
	// jobs alarm — but with a different code so the operator can
	// distinguish "scrub found findings" from "heal had to give up".
	if healRes.Failed > 0 || healRes.NotAtReplica > 0 {
		return output.NewError("verify.heal_incomplete",
			fmt.Sprintf("repair scrub --heal: %d healed, %d not at replica, %d failed (out of %d mismatches)",
				healRes.Healed, healRes.NotAtReplica, healRes.Failed, len(res.Mismatches))).
			WithSuggestion(&output.Suggestion{
				Human: "review the per-chunk failures in the result body; chunks not at the replica may need to be re-backed-up at the source",
			})
	}

	// Confirm the heal actually RESTORED integrity. repo.Heal runs at
	// the storage layer without encryption keys, so it only verifies
	// the replica BYTES were copied faithfully — NOT that they decrypt
	// to the expected plaintext hash (the chunk hash is over plaintext;
	// the on-disk bytes are a compressed+encrypted envelope). If the
	// replica's own copy of a chunk is corrupt, heal would install it
	// and report "healed" while the chunk is still broken. Re-verify
	// the once-mismatched chunks through the per-manifest CAS (which
	// HAS the keys) so that silent failure surfaces loudly instead.
	stillBad, verr := reverifyChunksPlaintext(cmd.Context(), sp, res.Mismatches)
	if verr != nil {
		return output.NewError("repair.heal_reverify_failed",
			fmt.Sprintf("repair scrub --heal: re-verify after heal: %v", verr)).Wrap(verr)
	}
	if len(stillBad) > 0 {
		return output.NewError("verify.heal_unverified",
			fmt.Sprintf("repair scrub --heal: %d of %d chunk(s) still fail verification after heal — the replica's copy is itself corrupt: %s",
				len(stillBad), len(res.Mismatches), hashListForMsg(stillBad))).
			WithSuggestion(&output.Suggestion{
				Human: "heal replaced the local chunks with the replica's bytes, but those bytes do not decrypt to the expected content — the replica is corrupt for these chunks too. They must be re-backed-up from a live source; no good copy exists in either repo.",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// Result body shapes — stable per the v1 schema commitment.

type repairChunksBody struct {
	DryRun   bool     `json:"dry_run"`
	Mode     string   `json:"mode"` // "orphans" | "missing"
	Total    int      `json:"total"`
	Applied  int      `json:"applied,omitempty"`
	RefCount int      `json:"ref_count"`
	Chunks   []string `json:"chunks,omitempty"`
}

// WriteText renders the orphan / missing chunk findings as human-readable
// text to w.
func (b repairChunksBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repair chunks --%s\n", b.Mode)
	fmt.Fprintf(bw, "  manifests reference %d distinct chunks\n", b.RefCount)
	switch b.Mode {
	case "orphans":
		if b.DryRun {
			fmt.Fprintf(bw, "  %d orphan chunk(s) (dry-run; pass --apply to delete)\n", b.Total)
		} else {
			fmt.Fprintf(bw, "  %d orphan chunk(s) — %d deleted\n", b.Total, b.Applied)
		}
	case "missing":
		fmt.Fprintf(bw, "  %d referenced chunk(s) NOT present in storage\n", b.Total)
		if b.Total > 0 {
			fmt.Fprintln(bw, "  ✗ this is a real corruption — restores referencing these chunks will fail")
		}
	}
	if len(b.Chunks) > 0 && len(b.Chunks) <= 50 {
		fmt.Fprintln(bw, "  hashes:")
		for _, c := range b.Chunks {
			fmt.Fprintf(bw, "    %s\n", c)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type repairScrubBody struct {
	Sampled         int      `json:"sampled"`
	OK              int      `json:"ok"`
	MismatchCount   int      `json:"mismatch_count"`
	BytesVerified   int64    `json:"bytes_verified"`
	ReferencedTotal int      `json:"referenced_total"`
	Mismatches      []string `json:"mismatches,omitempty"`

	// Heal-mode fields. Populated only when --heal was passed and
	// at least one mismatch was found. ReplicaURL records which
	// replica was consulted so audits show the source-of-truth.
	HealResult *repo.HealResult `json:"heal,omitempty"`
	ReplicaURL string           `json:"replica_url,omitempty"`
}

// WriteText renders the scrub result — sample size, mismatch list, and any
// heal-mode outcome — as human-readable text to w.
func (b repairScrubBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repair scrub\n")
	fmt.Fprintf(bw, "  sampled %d / %d referenced chunks (%s verified)\n",
		b.Sampled, b.ReferencedTotal, humanBytes(b.BytesVerified))
	if b.MismatchCount == 0 {
		fmt.Fprintln(bw, "  ✓ no integrity failures")
	} else {
		fmt.Fprintf(bw, "  ✗ %d chunk(s) failed integrity check:\n", b.MismatchCount)
		for _, h := range b.Mismatches {
			fmt.Fprintf(bw, "    %s\n", h)
		}
		if b.HealResult != nil {
			fmt.Fprintf(bw, "  heal — replica %s\n", b.ReplicaURL)
			fmt.Fprintf(bw, "    Healed:         %d\n", b.HealResult.Healed)
			fmt.Fprintf(bw, "    Already OK:     %d\n", b.HealResult.AlreadyOK)
			fmt.Fprintf(bw, "    Not at replica: %d\n", b.HealResult.NotAtReplica)
			fmt.Fprintf(bw, "    Failed:         %d\n", b.HealResult.Failed)
			fmt.Fprintf(bw, "    Bytes copied:   %s\n", humanBytes(b.HealResult.BytesCopied))
			if b.HealResult.NotAtReplica == 0 && b.HealResult.Failed == 0 {
				fmt.Fprintln(bw, "    ✓ all mismatches healed")
			}
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// scrubResultAgg accumulates ScrubResult across multiple manifests.
type scrubResultAgg struct {
	Sampled    int
	OK         int
	Bytes      int64
	Mismatches []repo.Hash
}

// hashListForMsg renders a hash slice for an error message, capping the
// count so a large mismatch set doesn't produce an unreadable line.
func hashListForMsg(hs []repo.Hash) string {
	const cap = 8
	parts := make([]string, 0, cap+1)
	for i, h := range hs {
		if i == cap {
			parts = append(parts, fmt.Sprintf("… (+%d more)", len(hs)-cap))
			break
		}
		parts = append(parts, h.String())
	}
	return strings.Join(parts, ", ")
}

// reverifyChunksPlaintext re-checks that each chunk in targets passes
// the plaintext SHA-256 round-trip, using the per-manifest CAS that
// holds the right decryption key (the same machinery scrubManifestAware
// uses for the initial scan). It returns the subset of targets that
// STILL fail.
//
// This is the integrity check repo.Heal cannot perform: heal runs at
// the storage layer without keys, so it confirms only that the replica
// bytes were copied — not that they decrypt to the expected plaintext
// hash. A chunk that is corrupt at the replica too would otherwise be
// installed and reported "healed". Re-verifying here turns that silent
// corruption into a loud heal_unverified failure.
//
// The empty Hash (scrubManifestAware's synthetic KEK-missing marker) is
// not a real chunk and is skipped.
func reverifyChunksPlaintext(ctx context.Context, sp storage.StoragePlugin, targets []repo.Hash) ([]repo.Hash, error) {
	pending := make(map[repo.Hash]struct{}, len(targets))
	for _, h := range targets {
		if h == (repo.Hash{}) {
			continue
		}
		pending[h] = struct{}{}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	verifier, verr := loadVerifier()
	if verr != nil {
		return nil, verr
	}
	stillBad := map[repo.Hash]struct{}{}

	verify := func(cas *repo.CAS, h repo.Hash) {
		if _, want := pending[h]; !want {
			return
		}
		delete(pending, h)
		body, gerr := cas.GetChunkBytes(ctx, h)
		if gerr != nil || repo.Hash(sha256.Sum256(body)) != h {
			stillBad[h] = struct{}{}
		}
	}

	// Walk backup manifests, building the per-manifest CAS so encrypted
	// chunks decrypt with the right key.
	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return nil, err
	}
	for _, dep := range deployments {
		for m, merr := range ms.List(ctx, dep, verifier) {
			if merr != nil {
				continue
			}
			cas, cerr := buildVerifyCAS(ctx, sp, m, nil)
			if cerr != nil {
				continue
			}
			for _, f := range m.Files {
				for _, c := range f.Chunks {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					verify(cas, c.Hash)
				}
			}
			if len(pending) == 0 {
				break
			}
		}
		if len(pending) == 0 {
			break
		}
	}

	// Anything still pending is a WAL chunk (unencrypted, default CAS)
	// or a chunk we couldn't locate in a readable manifest. Verify via
	// the default CAS; if it can't be read/round-tripped, treat it as
	// still-bad — heal cannot claim a success it can't confirm.
	if len(pending) > 0 {
		defaultCAS := casdefault.New(sp)
		for h := range pending {
			verify(defaultCAS, h)
		}
	}

	out := make([]repo.Hash, 0, len(stillBad))
	for h := range stillBad {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// scrubManifestAware walks every backup manifest in the repo, builds
// a per-manifest CAS (encrypted when the manifest carries an
// Encryption block), and verifies that manifest's chunks through
// THAT CAS.  Also walks WAL segment manifests with the default CAS
// (the WAL stream format has no manifest-level encryption block —
// streamed chunks share the deployment-wide DEK only when paired
// with a backup, which scrub today can't disambiguate).
//
// limit caps the global number of chunks sampled.  A chunk referenced
// by multiple manifests is verified once: subsequent encounters are
// skipped via the `seen` set so the cap reflects actual work done.
//
// Returns the aggregate result, the count of distinct hashes the
// scrub would have walked at limit=0 (the "referenced total" header
// the operator sees), and any storage / iteration error.
func scrubManifestAware(ctx context.Context, sp storage.StoragePlugin, limit int) (scrubResultAgg, int, error) {
	var agg scrubResultAgg
	seen := make(map[repo.Hash]struct{})

	// distinctRefs counts how many unique hashes the repo would have
	// scrubbed at limit=0 — surfaces as ReferencedTotal in the body.
	refs, err := repo.CollectReferences(ctx, sp)
	if err != nil {
		return agg, 0, err
	}
	distinctRefs := refs.Len()

	verifyChunk := func(cas *repo.CAS, h repo.Hash) {
		if _, dup := seen[h]; dup {
			return
		}
		seen[h] = struct{}{}
		if limit > 0 && agg.Sampled >= limit {
			return
		}
		body, gerr := cas.GetChunkBytes(ctx, h)
		if gerr != nil {
			agg.Sampled++
			agg.Mismatches = append(agg.Mismatches, h)
			return
		}
		// Defensive re-hash: CAS.GetChunkBytes already verifies, but
		// a future wrapper that bypasses the check would slip through
		// without this second guard.
		if repo.Hash(sha256.Sum256(body)) != h {
			agg.Sampled++
			agg.Mismatches = append(agg.Mismatches, h)
			return
		}
		agg.Sampled++
		agg.OK++
		agg.Bytes += int64(len(body))
	}

	// Walk backup manifests, deployment by deployment.  ManifestStore
	// requires a verifier (nil rejects every manifest as unsigned)
	// even though we don't strictly need signature verification here;
	// load the local-keyring verifier.  A signature-mismatch manifest
	// will be skipped — that's a different kind of error, surfaced
	// by the `verify` command, not scrub.
	verifier, verr := loadVerifier()
	if verr != nil {
		return agg, distinctRefs, verr
	}
	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return agg, distinctRefs, err
	}
	for _, dep := range deployments {
		for m, merr := range ms.List(ctx, dep, verifier) {
			if merr != nil {
				// Skip un-readable manifests — they'll surface in
				// other tools (verify, list).  Don't fail the scrub.
				continue
			}
			if limit > 0 && agg.Sampled >= limit {
				break
			}
			cas, cerr := buildVerifyCAS(ctx, sp, m, nil)
			if cerr != nil {
				// Encrypted manifest whose KEK isn't on this host:
				// can't verify its chunks.  Surface as one synthetic
				// "mismatch" so the operator knows it's not a clean
				// scrub; the suggestion in the structured error
				// points at --kek-file if/when that flag lands.
				agg.Mismatches = append(agg.Mismatches, repo.Hash{})
				continue
			}
			for _, f := range m.Files {
				for _, c := range f.Chunks {
					if err := ctx.Err(); err != nil {
						return agg, distinctRefs, err
					}
					verifyChunk(cas, c.Hash)
					if limit > 0 && agg.Sampled >= limit {
						break
					}
				}
				if limit > 0 && agg.Sampled >= limit {
					break
				}
			}
		}
		if limit > 0 && agg.Sampled >= limit {
			break
		}
	}

	// Walk WAL segment manifests with the default CAS.  WAL chunks
	// today are not encrypted at the manifest level (walsink writes
	// through casdefault.New, not NewEncrypted), so the plain CAS
	// reads them back correctly.  If a future walsink revision
	// encrypts WAL chunks the manifest schema will grow an Encryption
	// block — this loop will need a buildVerifyCAS-style switch then.
	defaultCAS := casdefault.New(sp)
	for info, lerr := range sp.List(ctx, "wal/") {
		if lerr != nil {
			break
		}
		if !strings.HasSuffix(info.Key, ".json") || strings.Contains(info.Key, ".json.tmp.") {
			continue
		}
		if limit > 0 && agg.Sampled >= limit {
			break
		}
		hashes, herr := scrubWALManifestHashes(ctx, sp, info.Key)
		if herr != nil {
			// Skip un-readable WAL manifests; verify command will
			// surface them.  Don't fail the scrub on one bad file.
			continue
		}
		for _, h := range hashes {
			if err := ctx.Err(); err != nil {
				return agg, distinctRefs, err
			}
			verifyChunk(defaultCAS, h)
			if limit > 0 && agg.Sampled >= limit {
				break
			}
		}
	}

	return agg, distinctRefs, nil
}

// scrubWALManifestHashes reads a WAL segment manifest at key and
// returns the list of chunk hashes it references.  Used by
// scrubManifestAware to verify WAL chunks alongside backup chunks.
// Errors from the underlying storage are propagated; a malformed
// manifest returns a parse error.
func scrubWALManifestHashes(ctx context.Context, sp storage.StoragePlugin, key string) ([]repo.Hash, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	// Partial decode — we only need the chunk hashes.  Tracking the
	// full SegmentManifest type from internal/pg/walsink would
	// pull an import cycle, so we re-decode locally.  Stable as
	// long as the manifest's `chunks[].hash` key stays.
	var m struct {
		Chunks []struct {
			Hash string `json:"hash"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	out := make([]repo.Hash, 0, len(m.Chunks))
	for _, c := range m.Chunks {
		var h repo.Hash
		raw, derr := hex.DecodeString(c.Hash)
		if derr != nil || len(raw) != len(h) {
			continue
		}
		copy(h[:], raw)
		out = append(out, h)
	}
	return out, nil
}
