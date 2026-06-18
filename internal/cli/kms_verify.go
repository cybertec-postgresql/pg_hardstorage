// kms_verify.go — CLI surface for verifying KEK-wrapped envelopes across manifests.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// newKmsVerifyCmd implements `pg_hardstorage kms verify` — the
// fleet-wide encryption-envelope health check.
//
// Operator-facing shape:
//
//	pg_hardstorage kms verify --repo s3://acme/backups
//	pg_hardstorage kms verify --repo s3://acme --deployment db1
//	pg_hardstorage kms verify --repo s3://acme --kek-ref tenant-a:v1 \
//	    --kek-file /etc/pg_hardstorage/keyring-tenant-a/kek.bin
//
// The check is read-only and orders-of-magnitude cheaper than a
// full `verify <deployment>` walk: it only unwraps each manifest's
// DEK with the resolved KEK, never fetches a chunk byte. That makes
// it the right tool for:
//
//   - Post-rotation audits ("did every manifest land on the new KEK?").
//   - Pre-compliance sweeps ("are all of last quarter's backups still
//     decryptable?").
//   - Routine health monitoring ("dashboard says my keyring is fine
//     but is it actually wrapping every manifest?").
//
// What it surfaces:
//
//   - signature_failed — manifest in the repo doesn't verify against
//     the operator's public key (LOUD; possible tampering).
//   - kek_unknown — the manifest's KEKRef isn't recognised by the
//     resolver (operator's keyring is missing the key, or a multi-
//     tenant migration was incomplete).
//   - wrapped_dek_corrupt — the manifest's wrapped_dek field can't
//     be base64-decoded (manifest-level corruption).
//   - unwrap_failed — the resolver returned a key but Unwrap rejects
//     the AEAD tag (the KEK at the resolved ref doesn't match the
//     one that wrapped this DEK).
//   - unknown_scheme — the manifest's encryption scheme is one this
//     binary doesn't know how to verify (a future scheme written by
//     a newer pg_hardstorage).
//   - unencrypted — the manifest has no encryption block (NOT a
//     break — operators who require encryption can drive their
//     own policy from this counter).
//
// Exit-code mapping:
//
//   - 0 — every encrypted manifest unwraps cleanly (or every
//     manifest is unencrypted). Unencrypted-by-design fleets pass
//     this check.
//   - 9 (verify failed) — at least one encrypted manifest can't be
//     decrypted with the keyring's KEK, or a manifest signature
//     failed verification.
func newKmsVerifyCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		kekRef     string
		kekFile    string
	)
	c := &cobra.Command{
		Use:   "verify",
		Short: "Verify the encryption envelope on every backup manifest in the repo",
		Long: `kms verify walks every committed (non-tombstoned) backup manifest
in the repo and asks the only question that matters for at-rest
encryption health: "would the operator's current keyring decrypt
this backup if we had to restore it right now?".

For each manifest, kms verify:

  1. Validates the manifest's Ed25519 signature against the local
     public key (manifests that don't verify get the loudest
     classification: signature_failed).
  2. Reads the encryption block. Manifests with no encryption block
     are counted as 'unencrypted' (NOT a failure — that's the
     operator's policy call).
  3. Resolves the KEK by KEKRef. By default the resolver is the
     local keystore, which knows the "local:default" ref. With
     --kek-ref + --kek-file the operator points at an explicit
     KEK file for one ref (post-rotation or per-tenant audits).
  4. Tries to unwrap the wrapped_dek with the resolved KEK. A
     successful unwrap is "ok"; a tag-failure is "unwrap_failed".

Chunks are NOT fetched, decrypted, or hashed. For per-chunk
integrity verification of one specific backup, use
` + "`" + `pg_hardstorage verify <deployment> [backup-id]` + "`" + `. The fleet-wide
envelope check this command runs is O(manifest count); per-chunk
verification is O(unique chunks per backup) and orders of magnitude
more expensive.

Read-only by construction — safe to run against a read-only repo,
a WORM-locked repo, or production at any cadence.

Exit code is 0 when every encrypted manifest unwraps cleanly (or
every manifest is unencrypted). Exit code 9 (verify failed) is
returned when at least one encrypted manifest can't be decrypted
with the resolved KEK, or any manifest signature failed.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKmsVerify(cmd, kmsVerifyFlags{
				repoURL:    repoURL,
				deployment: deployment,
				kekRef:     kekRef,
				kekFile:    kekFile,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict the walk to one deployment (default: all deployments)")
	c.Flags().StringVar(&kekRef, "kek-ref", "",
		"restrict to manifests whose KEKRef matches this string (default: all kek_refs)")
	c.Flags().StringVar(&kekFile, "kek-file", "",
		"path to KEK bytes for --kek-ref (32 bytes raw); required when --kek-ref is set, not the local-keystore ref, and the local keyring can't resolve the ref")
	return c
}

type kmsVerifyFlags struct {
	repoURL    string
	deployment string
	kekRef     string
	kekFile    string
}

func runKmsVerify(cmd *cobra.Command, f kmsVerifyFlags) error {
	d := DispatcherFrom(cmd)
	if f.kekFile != "" && f.kekRef == "" {
		return output.NewError("usage.bad_flag",
			"kms verify: --kek-file requires --kek-ref (which ref does the file's bytes correspond to?)").Wrap(output.ErrUsage)
	}

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	_, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("kms verify: signing key: %v", err)).Wrap(err)
	}

	resolver, err := buildKMSVerifyResolver(p.Keyring.Value, f)
	if err != nil {
		return err
	}

	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	res, err := backup.VerifyEnvelopes(cmd.Context(), sp, backup.VerifyEnvelopesOptions{
		Verifier:         verifier,
		KEKResolver:      resolver,
		DeploymentFilter: f.deployment,
		KEKRefFilter:     f.kekRef,
	})
	if err != nil {
		return kmsOpError(err, "kms verify", "kms.verify_failed", nil)
	}

	body := kmsVerifyBody{VerifyEnvelopesResult: *res}

	// We always emit the structured body on stdout — the operator
	// needs to see the per-manifest findings whether the run passed
	// or failed. The dispatcher routes stdout for success Results,
	// stderr for error Results; we want the body on stdout regardless,
	// so we send it as a successful Result and surface the
	// envelope-break verdict via the returned error (which the root
	// runner renders to stderr and uses to pick the exit code).
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}

	if res.AnyBroken() {
		// `verify.*` namespace → ExitVerifyFailed (9). The full
		// per-manifest detail is already on stdout via the body
		// above; the message summarises the failure-class counts so
		// a JSON-mode pipeline that only reads stderr still gets an
		// actionable summary.
		human := "review the failures slice in the JSON body — manifests with `unwrap_failed` need a `kms rotate` from the correct old KEK; manifests with `kek_unknown` need the missing key in the resolver; `signature_failed` means a manifest in the repo isn't signed by the trusted keypair (potential tampering)."
		return output.NewError("verify.envelope_break", summarizeKmsVerify(res)).
			WithSuggestion(&output.Suggestion{Human: human})
	}
	return nil
}

// buildKMSVerifyResolver picks the right KEKResolver for the run. The
// rules are:
//
//   - When --kek-ref + --kek-file are both supplied: a one-ref
//     resolver that returns the file's bytes for that ref and
//     "unknown" for everything else.
//
//   - When neither is supplied: the local keystore resolver
//     (handles "local:default" + the empty ref).
//
//   - When only --kek-ref is supplied: same as the keystore resolver
//     plus a "we only care about this ref" filter (the filter is
//     wired separately via VerifyEnvelopesOptions.KEKRefFilter).
func buildKMSVerifyResolver(keyringDir string, f kmsVerifyFlags) (func(string) ([encryption.KeyLen]byte, error), error) {
	if f.kekFile != "" {
		// One-ref resolver. The CLI guarantees kekRef != "" via the
		// usage check above.
		var k [encryption.KeyLen]byte
		body, err := readKEKFile(f.kekFile)
		if err != nil {
			return nil, output.NewError("usage.bad_flag",
				fmt.Sprintf("kms verify: --kek-file: %v", err)).Wrap(output.ErrUsage)
		}
		k = body
		ref := f.kekRef
		return func(r string) ([encryption.KeyLen]byte, error) {
			if r == ref {
				return k, nil
			}
			return [encryption.KeyLen]byte{}, fmt.Errorf("--kek-file/--kek-ref pair only resolves %q; this manifest has %q", ref, r)
		}, nil
	}
	// Default: the local keystore resolver. Recognises
	// "local:default" + the empty ref; anything else surfaces as
	// kek_unknown via the resolver's error path.
	return keystore.KEKResolver(keyringDir), nil
}

// summarizeKmsVerify produces a one-line summary of the failure
// classes for the error message. Stable for CLI tests.
func summarizeKmsVerify(res *backup.VerifyEnvelopesResult) string {
	parts := []string{}
	if res.SignatureFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d signature_failed", res.SignatureFailed))
	}
	if res.UnwrapFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d unwrap_failed", res.UnwrapFailed))
	}
	if res.KEKUnknown > 0 {
		parts = append(parts, fmt.Sprintf("%d kek_unknown", res.KEKUnknown))
	}
	if res.WrappedDEKCorrupt > 0 {
		parts = append(parts, fmt.Sprintf("%d wrapped_dek_corrupt", res.WrappedDEKCorrupt))
	}
	if res.UnknownScheme > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown_scheme", res.UnknownScheme))
	}
	return "kms verify: " + strings.Join(parts, ", ") + " (out of " + fmt.Sprintf("%d considered", res.Considered) + ")"
}

// kmsVerifyBody is the v1-stable Result body. The embedded
// VerifyEnvelopesResult carries the per-key detail; we don't add
// fields here in v1.
type kmsVerifyBody struct {
	backup.VerifyEnvelopesResult
}

// WriteText renders the envelope-verify rollup — counts per outcome bucket —
// as human-readable text to w.
func (b kmsVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	scope := "all deployments"
	if b.DeploymentFilter != "" {
		scope = "deployment " + b.DeploymentFilter
	}
	fmt.Fprintf(bw, "kms verify — %s\n", scope)
	if b.KEKRefFilter != "" {
		fmt.Fprintf(bw, "  Restricted to KEKRef:  %s\n", b.KEKRefFilter)
	}
	fmt.Fprintf(bw, "  Considered:            %d\n", b.Considered)
	fmt.Fprintf(bw, "  ✓ OK:                  %d\n", b.OK)
	if b.Unencrypted > 0 {
		fmt.Fprintf(bw, "  · Unencrypted:         %d (policy decision)\n", b.Unencrypted)
	}
	if b.Skipped > 0 {
		fmt.Fprintf(bw, "  · Skipped (filter):    %d\n", b.Skipped)
	}
	if b.SignatureFailed > 0 {
		fmt.Fprintf(bw, "  ✗ Signature failed:    %d (LOUD — manifest not signed by the trusted keypair)\n", b.SignatureFailed)
	}
	if b.KEKUnknown > 0 {
		fmt.Fprintf(bw, "  ✗ KEK unknown:         %d (resolver doesn't know the manifest's kek_ref)\n", b.KEKUnknown)
	}
	if b.WrappedDEKCorrupt > 0 {
		fmt.Fprintf(bw, "  ✗ wrapped_dek corrupt: %d\n", b.WrappedDEKCorrupt)
	}
	if b.UnwrapFailed > 0 {
		fmt.Fprintf(bw, "  ✗ Unwrap failed:       %d (KEK at the resolved ref doesn't match what wrapped the DEK)\n", b.UnwrapFailed)
	}
	if b.UnknownScheme > 0 {
		fmt.Fprintf(bw, "  ✗ Unknown scheme:      %d\n", b.UnknownScheme)
	}
	fmt.Fprintf(bw, "  Duration:              %d ms\n", b.DurationMS)
	if !b.AnyBroken() {
		fmt.Fprintln(bw, "  ✓ envelope health: clean")
	} else {
		fmt.Fprintf(bw, "  ✗ envelope health: %d manifest(s) need attention; see JSON body for per-key detail\n",
			b.SignatureFailed+b.KEKUnknown+b.WrappedDEKCorrupt+b.UnwrapFailed+b.UnknownScheme)
		max := 5
		if len(b.Failures) < max {
			max = len(b.Failures)
		}
		for i := 0; i < max; i++ {
			fail := b.Failures[i]
			fmt.Fprintf(bw, "      %s/%s — %s — %s\n",
				fail.Deployment, fail.BackupID, fail.Status, fail.Reason)
		}
		if len(b.Failures) > max {
			fmt.Fprintf(bw, "      … (+%d more)\n", len(b.Failures)-max)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
