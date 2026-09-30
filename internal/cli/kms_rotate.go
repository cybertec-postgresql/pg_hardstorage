// kms_rotate.go — CLI surface for KEK rotation across manifests.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newKMSRotateCmd implements `pg_hardstorage kms rotate` (the+
// real implementation, replacing the v0.1.1 deferred stub).
//
// The plan calls this out: "KEK rotation. `pg_hardstorage kms
// rotate` walks all manifests, decrypts wrapped_DEK with old KEK,
// rewraps with new KEK, atomically rewrites manifest. Chunks are
// not re-encrypted. Old KEK retired after grace."
//
// Operator-facing shape:
//
//	pg_hardstorage kms rotate \
//	    --repo s3://acme/backups \
//	    --old-kek-ref aws-kms://arn:.../v1 \
//	    --new-kek-ref aws-kms://arn:.../v2 \
//	    --old-kek-file /etc/pg_hardstorage/keyring-old/kek.bin \
//	    --new-kek-file /etc/pg_hardstorage/keyring-new/kek.bin \
//	    --apply
//
// Default mode is dry-run. `--apply` mutates. `--apply` plus
// success emits one `kms.rotate` audit event per rotated manifest
// (rare-enough event that the chain entries are signal, not noise
// — the same posture repo.scrub.mismatch and anomaly.detected use).
//
// What happens to chunks: nothing. Per-chunk keys are derived from
// the (unchanged) BDEK via HKDF; rewrapping the DEK leaves every
// chunk's bytes untouched. KEK rotation is O(manifest count), not
// O(chunk count) — that's the elegance of envelope encryption.
//
// What about the old KEK: still required for all rotation
// operations until the operator confirms every backup has been
// rotated. The plan's "retire after grace" pattern is operator
// policy; this command never deletes the old KEK file.
func newKMSRotateCmd() *cobra.Command {
	var (
		repoURL    string
		oldKEKRef  string
		newKEKRef  string
		oldKEKFile string
		newKEKFile string
		apply      bool
	)
	c := &cobra.Command{
		Use:   "rotate",
		Short: "Re-wrap every encrypted backup's DEK with a new KEK",
		Long: `Walk every committed backup manifest in the repo — soft-deleted
(tombstoned) ones included, since undelete can still resurrect them —
and every WAL segment manifest. For each one wrapped with --old-kek-ref:

  1. Decrypt the wrapped DEK using the bytes at --old-kek-file.
  2. Re-wrap the DEK using the bytes at --new-kek-file.
  3. Mutate the manifest's encryption block to record --new-kek-ref
     and the new wrapped_dek; everything else stays bit-identical.
  4. Re-sign the manifest (the operator's signing keypair is
     unchanged).
  5. Atomically rewrite the manifest at its repo key (and the
     replica copy if present).

WAL segment manifests (wal/<deployment>/<timeline>/<segment>.json)
carry their own wrapped DEK, which wal fetch unwraps during PITR, so
they are rewrapped the same way (they are unsigned; steps 4 and the
replica do not apply). A segment the backend refuses to overwrite
(WORM lock) is reported as a failure: the old KEK is then still
needed for it.

Chunks are NOT re-encrypted. Per-chunk keys are derived via HKDF
from the (unchanged) BDEK; rewrapping the DEK leaves every chunk's
bytes untouched. KEK rotation is O(manifest count), not O(chunk
count).

Multi-tenant safety: only manifests with --old-kek-ref are touched.
Manifests wrapped under different KEK refs (other tenants) are
skipped, not failed. Operators rotating per-tenant KEKs run this
command once per tenant.

Resumability: a rotation interrupted partway through is safely
re-runnable with the same args. Manifests that were already
rotated (their KEKRef == --new-kek-ref) are counted as
'already_rotated' and skipped.

Default mode is dry-run; pass --apply to actually rewrite. With
--apply, one kms.rotate audit event is emitted per run.

Exits non-zero whenever anything still depends on the old KEK — a
failed manifest, replica or WAL segment — in dry-run too, so a plan
that cannot complete never reads as clean.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKMSRotate(cmd, kmsRotateFlags{
				repoURL:    repoURL,
				oldKEKRef:  oldKEKRef,
				newKEKRef:  newKEKRef,
				oldKEKFile: oldKEKFile,
				newKEKFile: newKEKFile,
				apply:      apply,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&oldKEKRef, "old-kek-ref", "",
		"the kek_ref currently on the manifests to rotate (required)")
	_ = c.MarkFlagRequired("old-kek-ref")
	c.Flags().StringVar(&newKEKRef, "new-kek-ref", "",
		"the kek_ref to record on rotated manifests (required)")
	_ = c.MarkFlagRequired("new-kek-ref")
	c.Flags().StringVar(&oldKEKFile, "old-kek-file", "",
		"path to the OLD KEK bytes (32 bytes raw); required")
	_ = c.MarkFlagRequired("old-kek-file")
	c.Flags().StringVar(&newKEKFile, "new-kek-file", "",
		"path to the NEW KEK bytes (32 bytes raw); required")
	_ = c.MarkFlagRequired("new-kek-file")
	c.Flags().BoolVar(&apply, "apply", false,
		"actually rewrite the manifests (default: dry-run)")
	return c
}

type kmsRotateFlags struct {
	repoURL    string
	oldKEKRef  string
	newKEKRef  string
	oldKEKFile string
	newKEKFile string
	apply      bool
}

func runKMSRotate(cmd *cobra.Command, f kmsRotateFlags) error {
	d := DispatcherFrom(cmd)
	if f.oldKEKRef == f.newKEKRef {
		return output.NewError("usage.bad_flag",
			"kms rotate: --old-kek-ref == --new-kek-ref (nothing to rotate)").Wrap(output.ErrUsage)
	}

	oldKEK, err := readKEKFile(f.oldKEKFile)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("kms rotate: --old-kek-file: %v", err)).Wrap(output.ErrUsage)
	}
	newKEK, err := readKEKFile(f.newKEKFile)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("kms rotate: --new-kek-file: %v", err)).Wrap(output.ErrUsage)
	}

	// Load the operator's signing keypair from the canonical
	// keyring so we can re-sign rotated manifests. The signing key
	// is unchanged across KEK rotation — only the encryption layer
	// rotates.
	// Load-only: this verb must never mint a keypair (see
	// loadExistingKeypair).
	signer, verifier, err := loadExistingKeypair("kms rotate")
	if err != nil {
		return err
	}

	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	if f.apply {
		if err := assertRepoWritable(cmd.Context(), sp, "kms rotate --apply"); err != nil {
			return err
		}
	}

	// Carry the repo's WORM policy so a rotated manifest (and its re-synced
	// replica) is re-locked instead of left deletable on a compliance repo.
	rotUntil, rotMode := wormPolicyFor(repoMeta)
	res, err := backup.RotateKEK(cmd.Context(), sp, backup.RotateKEKOptions{
		OldKEKRef:     f.oldKEKRef,
		OldKEK:        oldKEK,
		NewKEKRef:     f.newKEKRef,
		NewKEK:        newKEK,
		Signer:        signer,
		Verifier:      verifier,
		DryRun:        !f.apply,
		RetainUntil:   rotUntil,
		RetentionMode: rotMode,
	})
	if err != nil {
		return kmsOpError(err, "kms rotate", "kms.rotate_failed", nil)
	}

	// Best-effort audit emission per rotated manifest. Failures
	// here don't change the verdict — the JSON result already
	// carries the per-key detail, and the audit chain's job is
	// the longitudinal "when did we rotate" record.
	if f.apply && (res.Rotated > 0 || res.WALRotated > 0) {
		emitKMSRotateAudits(cmd.Context(), sp, repoMeta, f, res)
	}

	// Render the body first (per-manifest detail + counters), then signal
	// incompleteness via a non-zero exit.
	body := kmsRotateBody{RotateKEKResult: *res}
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}

	// INCOMPLETE rotation MUST exit non-zero. A replica failure is just
	// as dangerous as a primary failure here: the replica still holds the
	// OLD wrapped DEK, so retiring (shredding) the old KEK makes that copy
	// undecryptable — and on a primary loss the backup is unrecoverable.
	// Previously a replica-only failure exited 0 with a "✓ old KEK can be
	// retired" verdict, inviting exactly that data loss. Re-running is now
	// idempotent (it heals stale replicas), so the operator re-runs until
	// this exits clean BEFORE retiring the old KEK.
	if f.apply && !res.Complete() {
		return output.NewError("kms.rotate_incomplete",
			fmt.Sprintf("kms rotate: rotation INCOMPLETE — failed=%d replica_failures=%d wal_failed=%d; the old KEK MUST be kept (some manifest copies or WAL segments still hold it). Re-run until this exits clean.",
				res.Failed, res.ReplicaFailures, res.WALFailed)).
			WithSuggestion(&output.Suggestion{
				Human: "re-run `kms rotate --apply` with the same args — it is idempotent and now re-syncs replica copies and WAL segments that a prior run left on the old KEK. Only after a clean (exit 0) run, with failed=replica_failures=wal_failed=0, is it safe to retire the old KEK. A WAL segment the backend refuses to overwrite (WORM lock) needs the old KEK until it ages out of retention.",
			})
	}
	// A dry-run whose plan already contains failures used to exit 0,
	// so a scripted "preview, then apply if clean" pipeline went ahead
	// with a rotation that could never finish.
	if !f.apply && !res.Complete() {
		return output.NewError("kms.rotate_plan_failed",
			fmt.Sprintf("kms rotate: dry-run plan has failures — failed=%d wal_failed=%d; --apply would leave the rotation incomplete",
				res.Failed, res.WALFailed)).
			WithSuggestion(&output.Suggestion{
				Human: "inspect `failures` in the JSON body: a wrong --old-kek-file, an unreadable/unverifiable manifest, or a corrupt WAL segment manifest. Fix those before --apply.",
			})
	}
	return nil
}

// readKEKFile reads exactly KeyLen bytes from path. Files larger
// or smaller are an error (KEKs are fixed-size 32-byte values).
func readKEKFile(path string) ([encryption.KeyLen]byte, error) {
	var k [encryption.KeyLen]byte
	body, err := os.ReadFile(path)
	if err != nil {
		return k, fmt.Errorf("read %s: %w", path, err)
	}
	if len(body) != encryption.KeyLen {
		return k, fmt.Errorf("%s: expected %d bytes, got %d",
			path, encryption.KeyLen, len(body))
	}
	copy(k[:], body)
	return k, nil
}

// emitKMSRotateAudits appends ONE kms.rotate audit event per run
// with the aggregate counts. We deliberately emit once-per-run
// (not once-per-manifest) — a multi-thousand-manifest rotation
// would otherwise flood the chain. The result body's per-manifest
// detail covers individual failures; the chain's role is the
// longitudinal "when did we rotate" record.
func emitKMSRotateAudits(ctx context.Context, sp storage.StoragePlugin, repoMeta *repo.Metadata, f kmsRotateFlags, res *backup.RotateKEKResult) {
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	store.AppendOrLog(ctx, &audit.Event{
		Action:    "kms.rotate",
		Subject:   audit.Subject{Repo: f.repoURL},
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"old_kek_ref":           f.oldKEKRef,
			"new_kek_ref":           f.newKEKRef,
			"considered":            res.Considered,
			"rotated":               res.Rotated,
			"already_rotated":       res.AlreadyRotated,
			"skipped_unencrypted":   res.SkippedUnencrypted,
			"skipped_different_kek": res.SkippedDifferentKEK,
			"failed":                res.Failed,
			"replica_failures":      res.ReplicaFailures,
			"tombstoned_rotated":    res.TombstonedRotated,
			"wal_considered":        res.WALConsidered,
			"wal_rotated":           res.WALRotated,
			"wal_already_rotated":   res.WALAlreadyRotated,
			"wal_failed":            res.WALFailed,
			"duration_ms":           res.DurationMS,
		},
	})
}

// kmsRotateBody is the v1-stable Result body. The embedded
// RotateKEKResult carries the per-key detail; we don't add fields
// here
type kmsRotateBody struct {
	backup.RotateKEKResult
}

// WriteText renders the KEK rotation result as human-readable text to w,
// distinguishing dry-run from a real rewrite.
func (b kmsRotateBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "would rotate"
	if !b.DryRun {
		verb = "rotated"
	}
	fmt.Fprintf(bw, "kms rotate — %s → %s\n", b.OldKEKRef, b.NewKEKRef)
	if b.DryRun {
		fmt.Fprintln(bw, "  (dry-run — nothing rewritten)")
	}
	fmt.Fprintf(bw, "  Considered:           %d\n", b.Considered)
	fmt.Fprintf(bw, "  %s:%s%d\n", verb,
		strings.Repeat(" ", 22-len(verb)), b.Rotated)
	if b.AlreadyRotated > 0 {
		fmt.Fprintf(bw, "  Already rotated:      %d (resumed run)\n", b.AlreadyRotated)
	}
	if b.SkippedUnencrypted > 0 {
		fmt.Fprintf(bw, "  Skipped unencrypted:  %d\n", b.SkippedUnencrypted)
	}
	if b.SkippedDifferentKEK > 0 {
		fmt.Fprintf(bw, "  Skipped different KEK: %d (other tenants)\n", b.SkippedDifferentKEK)
	}
	if b.Failed > 0 {
		fmt.Fprintf(bw, "  ✗ Failed:              %d\n", b.Failed)
	}
	if b.TombstonedRotated > 0 {
		fmt.Fprintf(bw, "  (of which soft-deleted: %d)\n", b.TombstonedRotated)
	}
	if b.ReplicaFailures > 0 {
		fmt.Fprintf(bw, "  ⚠ Replica failures:   %d (primary OK; replica copy needs `repair manifest`)\n", b.ReplicaFailures)
	}
	fmt.Fprintf(bw, "  WAL segments:         %d considered, %s %d, %d already rotated\n",
		b.WALConsidered, verb, b.WALRotated, b.WALAlreadyRotated)
	if b.WALFailed > 0 {
		fmt.Fprintf(bw, "  ✗ WAL segments failed: %d\n", b.WALFailed)
	}
	fmt.Fprintf(bw, "  Duration:             %d ms\n", b.DurationMS)
	if b.Complete() {
		if b.DryRun {
			fmt.Fprintln(bw, "  ✓ rotation plan is clean — re-run with --apply to commit")
		} else if b.Rotated > 0 || b.WALRotated > 0 {
			fmt.Fprintln(bw, "  ✓ rotation clean — old KEK can be retired after the operator's grace window")
		}
	} else if b.Failed == 0 && b.WALFailed == 0 && b.ReplicaFailures > 0 {
		// Primaries rotated, but some REPLICAS still hold the old KEK —
		// retiring it now would strand them. Do NOT green-light retirement.
		fmt.Fprintln(bw, "  ✗ DO NOT retire the old KEK — replica copies still hold it.")
		fmt.Fprintln(bw, "    Re-run `kms rotate --apply` (it now re-syncs replicas) until replica_failures = 0.")
	} else {
		fmt.Fprintln(bw, "  ✗ rotation had failures — DO NOT retire the old KEK; see JSON body for per-object detail")
		for _, f := range b.Failures {
			id := f.BackupID
			if f.Key != "" {
				id = f.Key
			}
			fmt.Fprintf(bw, "    %s/%s — %s\n", f.Deployment, id, f.Err)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
