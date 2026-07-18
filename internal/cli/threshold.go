// threshold.go — CLI surface for threshold (n-of-m) attestation signing and roster management.
package cli

import (
	"crypto/ed25519"
	"encoding/base64"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// newThresholdCmd builds the `pg_hardstorage threshold` parent
// command.  Subcommands cover the lifecycle of multi-party signing:
//
//	threshold whoami                                   — print my key fingerprint + pubkey
//	threshold roster create <id> --threshold k ...     — admin-sign a roster
//	threshold roster list / show <id>                  — read rosters
//	threshold attest sign <kind> <id> --roster R ...   — add my signature
//	threshold attest verify <kind> <id>                — quorum check
//	threshold attest show   <kind> <id>                — full attestation listing
//
// Storage layout + canonical signing details live in
// internal/threshold/threshold.go.
func newThresholdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "threshold",
		Short: "k-of-n attestations: multi-party signing for highest-assurance manifests",
		Long: `Manage k-of-n attestations for highest-assurance manifests.

A roster names n trusted signers and a quorum threshold k.
An attestation records that ≥ k roster members have vouched
for some subject (a backup manifest, an audit anchor, a KEK
rotation, etc.).  Each member signs independently with their
local operator key; signatures aggregate in the repo.

Operationally:

    # 1. Each operator publishes their pub-key once
    pg_hardstorage threshold whoami

    # 2. An admin assembles the roster
    pg_hardstorage threshold roster create prod-admins \
        --threshold 2 \
        --member alice@acme.example:<base64-pubkey> \
        --member bob@acme.example:<base64-pubkey> \
        --member charlie@acme.example:<base64-pubkey> \
        --description "Production cluster admins" \
        --repo s3://acme

    # 3. To attest a backup, each member runs (any order):
    pg_hardstorage threshold attest sign \
        backup_manifest db1.full.20260501T120000Z \
        --hash <sha256-hex> \
        --roster prod-admins \
        --repo s3://acme

    # 4. Verify the quorum at any point:
    pg_hardstorage threshold attest verify \
        backup_manifest db1.full.20260501T120000Z \
        --repo s3://acme`,
	}
	c.AddCommand(
		newThresholdWhoamiCmd(),
		newThresholdRosterCmd(),
		newThresholdAttestCmd(),
	)
	return c
}

// ----- whoami -----

func newThresholdWhoamiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "whoami",
		Short:        "Print this operator's roster-member identity (signer line for admins)",
		SilenceUsage: true,
		RunE:         runThresholdWhoami,
	}
	return c
}

func runThresholdWhoami(cmd *cobra.Command, _ []string) error {
	d := DispatcherFrom(cmd)
	signer, _, err := loadSignerForThreshold()
	if err != nil {
		return err
	}
	pub := signer.PublicKey()
	body := thresholdWhoamiBody{
		Fingerprint:    threshold.PublicKeyFingerprint(pub),
		PublicKey:      base64.StdEncoding.EncodeToString(pub),
		MemberSpecHint: "<your-signer-id>:" + base64.StdEncoding.EncodeToString(pub),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type thresholdWhoamiBody struct {
	Fingerprint    string `json:"fingerprint"`
	PublicKey      string `json:"public_key"`
	MemberSpecHint string `json:"member_spec_hint"`
}

// WriteText renders the operator's roster identity — fingerprint, public key,
// and ready-to-paste --member spec — as human-readable text to w.
func (b thresholdWhoamiBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "Your roster-member identity:")
	fmt.Fprintf(bw, "  Public-key SHA-256:  %s\n", b.Fingerprint)
	fmt.Fprintf(bw, "  Public key (base64): %s\n", b.PublicKey)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Send this --member spec to whoever creates the roster:")
	fmt.Fprintf(bw, "  %s\n", b.MemberSpecHint)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// ----- roster -----

func newThresholdRosterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "roster",
		Short: "Create / list / show rosters",
	}
	c.AddCommand(
		newThresholdRosterCreateCmd(),
		newThresholdRosterListCmd(),
		newThresholdRosterShowCmd(),
	)
	return c
}

func newThresholdRosterCreateCmd() *cobra.Command {
	var (
		repoURL     string
		threshold_  int
		members     []string
		membersFile string
		description string
		createdBy   string
	)
	c := &cobra.Command{
		Use:          "create <id>",
		Short:        "Admin-sign a new roster (refuses to overwrite an existing one)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThresholdRosterCreate(cmd, args[0],
				thresholdRosterCreateFlags{
					repoURL: repoURL, threshold: threshold_,
					members: members, membersFile: membersFile,
					description: description, createdBy: createdBy,
				})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().IntVar(&threshold_, "threshold", 0, "quorum size k (required, 1..n)")
	c.Flags().StringSliceVar(&members, "member", nil,
		"member spec 'signer:base64-pubkey' (repeatable; mutually exclusive with --members-file)")
	c.Flags().StringVar(&membersFile, "members-file", "",
		"path to a JSON file with [{signer,public_key},...] (mutually exclusive with --member)")
	c.Flags().StringVar(&description, "description", "", "human-readable description")
	c.Flags().StringVar(&createdBy, "created-by", "",
		"creator identity (defaults to one of the local-key's roster-member signer IDs)")
	return c
}

type thresholdRosterCreateFlags struct {
	repoURL     string
	threshold   int
	members     []string
	membersFile string
	description string
	createdBy   string
}

func runThresholdRosterCreate(cmd *cobra.Command, id string, f thresholdRosterCreateFlags) error {
	d := DispatcherFrom(cmd)
	if f.threshold <= 0 {
		return output.NewError("usage.missing_flag",
			"threshold roster create: --threshold is required (and must be ≥ 1)").Wrap(output.ErrUsage)
	}
	if len(f.members) == 0 && f.membersFile == "" {
		return output.NewError("usage.missing_flag",
			"threshold roster create: supply --member (repeatable) or --members-file").Wrap(output.ErrUsage)
	}
	if len(f.members) > 0 && f.membersFile != "" {
		return output.NewError("usage.bad_flag",
			"threshold roster create: --member and --members-file are mutually exclusive").Wrap(output.ErrUsage)
	}
	parsedMembers, err := loadMembers(f.members, f.membersFile)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("threshold roster create: %v", err)).Wrap(output.ErrUsage)
	}
	signer, _, err := loadSignerForThreshold()
	if err != nil {
		return err
	}
	// Default --created-by to whichever member matches our key.
	createdBy := f.createdBy
	if createdBy == "" {
		fp := threshold.PublicKeyFingerprint(signer.PublicKey())
		for _, m := range parsedMembers {
			if m.PublicKeyFingerprint == fp {
				createdBy = m.Signer
				break
			}
		}
		if createdBy == "" {
			return output.NewError("usage.bad_flag",
				"threshold roster create: --created-by required (local key's fingerprint is not in the member list)").
				Wrap(output.ErrUsage)
		}
	}

	r := threshold.NewRoster(id, f.description, f.threshold, parsedMembers, time.Now().UTC())
	if err := threshold.SignRoster(r, thresholdSignerAdapter{s: signer}, createdBy); err != nil {
		return output.NewError("threshold.sign_failed",
			fmt.Sprintf("threshold roster create: %v", err)).Wrap(err)
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if err := assertRepoWritable(cmd.Context(), sp, "threshold roster create"); err != nil {
		return err
	}
	// WORM-lock the roster on a compliance repo (it gates restore /
	// kms-shred quorum, so it must be as immutable as the audit chain),
	// and anchor its creator to the local key that just signed it.
	wormUntil, wormMode := wormPolicyFor(repoMeta)
	store := threshold.NewRosterStore(sp).
		WithTrustedKeys(signer.PublicKey()).
		WithRetention(wormUntil, wormMode)
	if err := store.Put(cmd.Context(), r); err != nil {
		switch {
		case errors.Is(err, threshold.ErrRosterAlreadyExists):
			return output.NewError("conflict.roster_exists",
				fmt.Sprintf("threshold roster create: roster %q already exists", id)).Wrap(err)
		case errors.Is(err, threshold.ErrInvalidThreshold),
			errors.Is(err, threshold.ErrInvalidMembers),
			errors.Is(err, threshold.ErrInvalidMember),
			errors.Is(err, threshold.ErrInvalidID),
			errors.Is(err, threshold.ErrDescriptionTooLong):
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("threshold roster create: %v", err)).Wrap(output.ErrUsage)
		}
		return output.NewError("threshold.store_failed",
			fmt.Sprintf("threshold roster create: %v", err)).Wrap(err)
	}
	// Best-effort audit append.  Failure does not roll back the
	// roster — auditors verify the chain periodically.
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "threshold.roster_create",
		Actor:     createdBy,
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"roster_id":    id,
			"threshold":    f.threshold,
			"member_count": len(parsedMembers),
			"roster_hash":  threshold.RosterHash(r),
		},
	})
	body := rosterBody(r)
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newThresholdRosterListCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "list",
		Short:        "List rosters in the repository",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runThresholdRosterList(cmd, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runThresholdRosterList(cmd *cobra.Command, repoURL string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	rs, err := threshold.NewRosterStore(sp).List(cmd.Context())
	if err != nil {
		return output.NewError("threshold.list_failed",
			fmt.Sprintf("threshold roster list: %v", err)).Wrap(err)
	}
	body := rosterListBody{Count: len(rs)}
	for _, r := range rs {
		body.Entries = append(body.Entries, *rosterBody(r))
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newThresholdRosterShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <id>",
		Short:        "Show one roster's full body + members",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThresholdRosterShow(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runThresholdRosterShow(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	r, err := threshold.NewRosterStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, threshold.ErrRosterNotFound) {
			return output.NewError("notfound.roster",
				fmt.Sprintf("threshold roster show: roster %q not found", id)).Wrap(err)
		}
		return output.NewError("threshold.get_failed",
			fmt.Sprintf("threshold roster show: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(rosterBody(r)))
}

// ----- attest -----

func newThresholdAttestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "attest",
		Short: "Sign / verify / show k-of-n attestations",
	}
	c.AddCommand(
		newThresholdAttestSignCmd(),
		newThresholdAttestVerifyCmd(),
		newThresholdAttestShowCmd(),
	)
	return c
}

func newThresholdAttestSignCmd() *cobra.Command {
	var (
		repoURL  string
		hash     string
		rosterID string
		as       string
	)
	c := &cobra.Command{
		Use:   "sign <kind> <id>",
		Short: "Add this operator's signature to an attestation (creates the header on first sign)",
		Long: `Sign an attestation as the local operator.  The first signer
implicitly creates the header (subject + roster reference);
subsequent signers attach their signatures alongside.  A
member's repeated sign with the same content is a no-op.

Required flags:
  --hash HEX        SHA-256 hex of the canonical bytes of <kind>/<id>
  --roster ID       roster under which the attestation is governed`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThresholdAttestSign(cmd, args[0], args[1],
				thresholdAttestSignFlags{
					repoURL: repoURL, hash: hash,
					rosterID: rosterID, as: as,
				})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&hash, "hash", "",
		"subject hash (SHA-256 hex of canonical bytes; required)")
	_ = c.MarkFlagRequired("hash")
	c.Flags().StringVar(&rosterID, "roster", "", "roster ID (required)")
	_ = c.MarkFlagRequired("roster")
	c.Flags().StringVar(&as, "as", "",
		"sign as a specific roster member (defaults to the unique member matching the local key)")
	return c
}

type thresholdAttestSignFlags struct {
	repoURL  string
	hash     string
	rosterID string
	as       string
}

func runThresholdAttestSign(cmd *cobra.Command, kind, id string, f thresholdAttestSignFlags) error {
	d := DispatcherFrom(cmd)
	signer, _, err := loadSignerForThreshold()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if err := assertRepoWritable(cmd.Context(), sp, "threshold attest sign"); err != nil {
		return err
	}
	// Anchor the roster to the local operator key before vouching for a
	// backup under it — don't lend a signature to a roster this operator
	// didn't create (a forged roster's attestation is inert at the restore
	// gate, but signing it at all is wasted trust). Same single-key trust
	// model the restore gate and roster-create use.
	rosterStore := threshold.NewRosterStore(sp).WithTrustedKeys(signer.PublicKey())
	r, err := rosterStore.Get(cmd.Context(), f.rosterID)
	if err != nil {
		switch {
		case errors.Is(err, threshold.ErrRosterNotFound):
			return output.NewError("notfound.roster",
				fmt.Sprintf("threshold attest sign: roster %q not found", f.rosterID)).Wrap(err)
		case errors.Is(err, threshold.ErrRosterUntrusted):
			return output.NewError("verify.roster_untrusted",
				fmt.Sprintf("threshold attest sign: roster %q was not created by this operator's key; refusing to sign against an untrusted roster", f.rosterID)).Wrap(err)
		}
		return output.NewError("threshold.get_failed",
			fmt.Sprintf("threshold attest sign: roster: %v", err)).Wrap(err)
	}
	subject := threshold.AttestationSubject{Kind: kind, ID: id, Hash: f.hash}
	now := time.Now().UTC()
	sig, err := threshold.SignAttestation(subject, r,
		thresholdSignerAdapter{s: signer}, f.as, now)
	if err != nil {
		switch {
		case errors.Is(err, threshold.ErrLocalKeyDoesNotMatchMember):
			return output.NewError("auth.key_mismatch",
				fmt.Sprintf("threshold attest sign: %v", err)).Wrap(err)
		case errors.Is(err, threshold.ErrSignerNotInRoster):
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("threshold attest sign: %v", err)).Wrap(output.ErrUsage)
		}
		return output.NewError("threshold.sign_failed",
			fmt.Sprintf("threshold attest sign: %v", err)).Wrap(err)
	}
	attStore := threshold.NewAttestationStore(sp)
	header := &threshold.AttestationHeader{
		Schema:     threshold.SchemaAttestationHeader,
		Subject:    subject,
		RosterID:   r.ID,
		RosterHash: threshold.RosterHash(r),
		Threshold:  r.Threshold,
		// CreatedAt is rounded to the second so concurrent first-
		// signers race-write the same byte-equal header.  Sub-second
		// jitter would otherwise produce different bodies.
		CreatedAt: now.Truncate(time.Second),
	}
	if err := attStore.PutHeader(cmd.Context(), header); err != nil {
		return output.NewError("threshold.put_header_failed",
			fmt.Sprintf("threshold attest sign: header: %v", err)).Wrap(err)
	}
	if err := attStore.PutSignature(cmd.Context(), sig); err != nil {
		if errors.Is(err, threshold.ErrSubjectAlreadySigned) {
			return output.NewError("conflict.already_signed",
				fmt.Sprintf("threshold attest sign: %v", err)).Wrap(err)
		}
		return output.NewError("threshold.put_sig_failed",
			fmt.Sprintf("threshold attest sign: signature: %v", err)).Wrap(err)
	}
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "threshold.attest_sign",
		Actor:     sig.Signer,
		Timestamp: now,
		Body: map[string]any{
			"subject_kind": kind,
			"subject_id":   id,
			"subject_hash": f.hash,
			"roster_id":    r.ID,
		},
	})
	body := thresholdSignBody{
		Signer:               sig.Signer,
		PublicKeyFingerprint: sig.PublicKeyFingerprint,
		Subject:              sig.Subject,
		RosterID:             r.ID,
		SignedAt:             sig.SignedAt,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newThresholdAttestVerifyCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:   "verify <kind> <id>",
		Short: "Verify the quorum on an attestation; exit 9 on quorum-not-met / tampering",
		Long: `Reads the attestation header + every per-member signature,
revalidates each, and asserts ≥ k distinct valid signatures
where k is the roster threshold.

Exit codes:
  0 — quorum met
  6 — attestation or roster not found
  9 — quorum not met (or tampering detected)`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThresholdAttestVerify(cmd, repoURL, args[0], args[1])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runThresholdAttestVerify(cmd *cobra.Command, repoURL, kind, id string) error {
	d := DispatcherFrom(cmd)
	signer, _, err := loadSignerForThreshold()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	attStore := threshold.NewAttestationStore(sp)
	att, err := attStore.LoadAttestation(cmd.Context(), kind, id)
	if err != nil {
		if errors.Is(err, threshold.ErrAttestationNotFound) {
			return output.NewError("notfound.attestation",
				fmt.Sprintf("threshold attest verify: attestation %s/%s not found", kind, id)).Wrap(err)
		}
		return output.NewError("threshold.load_failed",
			fmt.Sprintf("threshold attest verify: %v", err)).Wrap(err)
	}
	// Anchor the roster to the local operator key: a "quorum met" verdict
	// is only meaningful under a roster this operator created. A forged
	// roster (even one whose signatures all check out against its own
	// members) must not verify as satisfied.
	rosterStore := threshold.NewRosterStore(sp).WithTrustedKeys(signer.PublicKey())
	r, err := rosterStore.Get(cmd.Context(), att.Header.RosterID)
	if err != nil {
		switch {
		case errors.Is(err, threshold.ErrRosterNotFound):
			return output.NewError("notfound.roster",
				fmt.Sprintf("threshold attest verify: roster %q not found",
					att.Header.RosterID)).Wrap(err)
		case errors.Is(err, threshold.ErrRosterUntrusted):
			return output.NewError("verify.roster_untrusted",
				fmt.Sprintf("threshold attest verify: roster %q was not created by this operator's key; its attestation cannot be honoured",
					att.Header.RosterID)).Wrap(err)
		}
		return output.NewError("threshold.get_failed",
			fmt.Sprintf("threshold attest verify: %v", err)).Wrap(err)
	}
	res, err := threshold.VerifyAttestation(att.Header, att.Signatures, r)
	if err != nil {
		return output.NewError("verify.attestation_invalid",
			fmt.Sprintf("threshold attest verify: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the roster hash recorded in the header doesn't match the current roster — the roster may have been modified after the attestation was created",
			})
	}
	body := thresholdVerifyBody{
		Subject:           att.Header.Subject,
		RosterID:          r.ID,
		Threshold:         r.Threshold,
		Signatures:        len(att.Signatures),
		ValidSignatures:   res.Members,
		InvalidSignatures: res.InvalidSignatures,
		Met:               res.Met,
	}
	// Dual-stream: emit the body first (stdout) so operators see the
	// counts whether or not the quorum was met.  Then, on a not-met
	// verdict, return a verify.* error that flips the exit code to 9
	// (ExitVerifyFailed) — same posture as `kms verify` and
	// `replicate verify`.
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}
	if !res.Met {
		return output.NewError("verify.quorum_not_met",
			fmt.Sprintf("threshold attest verify: %d valid signatures, threshold %d",
				res.Members, r.Threshold)).
			WithSuggestion(&output.Suggestion{
				Human: fmt.Sprintf("collect %d more signature(s) before this attestation is valid",
					r.Threshold-res.Members),
			})
	}
	return nil
}

func newThresholdAttestShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <kind> <id>",
		Short:        "Show one attestation: header + every signature with its validity",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThresholdAttestShow(cmd, repoURL, args[0], args[1])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runThresholdAttestShow(cmd *cobra.Command, repoURL, kind, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	attStore := threshold.NewAttestationStore(sp)
	att, err := attStore.LoadAttestation(cmd.Context(), kind, id)
	if err != nil {
		if errors.Is(err, threshold.ErrAttestationNotFound) {
			return output.NewError("notfound.attestation",
				fmt.Sprintf("threshold attest show: %s/%s not found", kind, id)).Wrap(err)
		}
		return output.NewError("threshold.load_failed",
			fmt.Sprintf("threshold attest show: %v", err)).Wrap(err)
	}
	r, err := threshold.NewRosterStore(sp).Get(cmd.Context(), att.Header.RosterID)
	if err != nil {
		// We still want to render the attestation even if the roster
		// is missing — useful for forensics.  Mark every signature as
		// "roster-unavailable".
		body := thresholdShowBody{
			Header:          att.Header,
			Signatures:      renderSignaturesUnverified(att.Signatures),
			RosterAvailable: false,
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}
	body := thresholdShowBody{
		Header:          att.Header,
		Threshold:       r.Threshold,
		RosterAvailable: true,
	}
	met := 0
	seen := make(map[string]struct{}, len(att.Signatures))
	for _, sig := range att.Signatures {
		entry := thresholdShowSig{
			Signer:               sig.Signer,
			PublicKeyFingerprint: sig.PublicKeyFingerprint,
			SignedAt:             sig.SignedAt,
		}
		if err := threshold.VerifySignature(sig, r); err != nil {
			entry.Valid = false
			entry.Reason = err.Error()
		} else {
			entry.Valid = true
			if _, dup := seen[sig.PublicKeyFingerprint]; dup {
				entry.Reason = "duplicate (counted only once)"
			} else {
				seen[sig.PublicKeyFingerprint] = struct{}{}
				met++
			}
		}
		body.Signatures = append(body.Signatures, entry)
	}
	body.ValidDistinct = met
	body.QuorumMet = met >= r.Threshold
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- bodies + text rendering -----

type rosterMemberView struct {
	Signer               string `json:"signer"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
}

type rosterView struct {
	ID                          string             `json:"id"`
	Description                 string             `json:"description,omitempty"`
	Threshold                   int                `json:"threshold"`
	Members                     []rosterMemberView `json:"members"`
	CreatedAt                   time.Time          `json:"created_at"`
	CreatedBy                   string             `json:"created_by"`
	CreatorPublicKeyFingerprint string             `json:"creator_public_key_fingerprint"`
	Hash                        string             `json:"hash"`
}

func rosterBody(r *threshold.Roster) *rosterView {
	v := &rosterView{
		ID:                          r.ID,
		Description:                 r.Description,
		Threshold:                   r.Threshold,
		CreatedAt:                   r.CreatedAt,
		CreatedBy:                   r.CreatedBy,
		CreatorPublicKeyFingerprint: r.CreatorPublicKeyFingerprint,
		Hash:                        threshold.RosterHash(r),
	}
	for _, m := range r.Members {
		v.Members = append(v.Members, rosterMemberView{
			Signer:               m.Signer,
			PublicKeyFingerprint: m.PublicKeyFingerprint,
		})
	}
	sort.Slice(v.Members, func(i, j int) bool {
		return v.Members[i].Signer < v.Members[j].Signer
	})
	return v
}

// WriteText renders the roster view — threshold, members, and content hash —
// as human-readable text to w.
func (v rosterView) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Roster %s\n", v.ID)
	if v.Description != "" {
		fmt.Fprintf(bw, "  Description: %s\n", v.Description)
	}
	fmt.Fprintf(bw, "  Threshold:   %d of %d\n", v.Threshold, len(v.Members))
	fmt.Fprintf(bw, "  Created at:  %s\n", v.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Created by:  %s (%s)\n", v.CreatedBy, v.CreatorPublicKeyFingerprint)
	fmt.Fprintf(bw, "  Hash:        %s\n", v.Hash)
	fmt.Fprintln(bw, "  Members:")
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	for _, m := range v.Members {
		fmt.Fprintf(tw, "    %s\t%s\n", m.Signer, m.PublicKeyFingerprint)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type rosterListBody struct {
	Count   int          `json:"count"`
	Entries []rosterView `json:"entries"`
}

// WriteText renders the stored rosters as a tabular summary to w.
func (b rosterListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d roster(s)\n\n", b.Count)
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTHRESHOLD\tMEMBERS\tCREATED")
	for _, e := range b.Entries {
		fmt.Fprintf(tw, "%s\t%d / %d\t%d\t%s\n",
			e.ID, e.Threshold, len(e.Members), len(e.Members),
			e.CreatedAt.Format(time.RFC3339))
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type thresholdSignBody struct {
	Signer               string                       `json:"signer"`
	PublicKeyFingerprint string                       `json:"public_key_fingerprint"`
	Subject              threshold.AttestationSubject `json:"subject"`
	RosterID             string                       `json:"roster_id"`
	SignedAt             time.Time                    `json:"signed_at"`
}

// WriteText renders the added-signature confirmation as human-readable text to w.
func (b thresholdSignBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ Attestation signature added\n")
	fmt.Fprintf(bw, "  Signer:    %s (%s)\n", b.Signer, b.PublicKeyFingerprint)
	fmt.Fprintf(bw, "  Subject:   %s/%s\n", b.Subject.Kind, b.Subject.ID)
	fmt.Fprintf(bw, "  Roster:    %s\n", b.RosterID)
	fmt.Fprintf(bw, "  Signed at: %s\n", b.SignedAt.Format(time.RFC3339))
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type thresholdVerifyBody struct {
	Subject           threshold.AttestationSubject       `json:"subject"`
	RosterID          string                             `json:"roster_id"`
	Threshold         int                                `json:"threshold"`
	Signatures        int                                `json:"signatures"`
	ValidSignatures   int                                `json:"valid_signatures"`
	InvalidSignatures []threshold.InvalidSignatureRecord `json:"invalid_signatures,omitempty"`
	Met               bool                               `json:"met"`
}

// WriteText renders the quorum-verify outcome and any invalid-signature
// detail as human-readable text to w.
func (b thresholdVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verdict := "✗ quorum NOT met"
	if b.Met {
		verdict = "✓ quorum met"
	}
	fmt.Fprintf(bw, "%s for %s/%s\n", verdict, b.Subject.Kind, b.Subject.ID)
	fmt.Fprintf(bw, "  Roster:            %s\n", b.RosterID)
	fmt.Fprintf(bw, "  Threshold:         %d\n", b.Threshold)
	fmt.Fprintf(bw, "  Total signatures:  %d\n", b.Signatures)
	fmt.Fprintf(bw, "  Valid signatures:  %d\n", b.ValidSignatures)
	if len(b.InvalidSignatures) > 0 {
		fmt.Fprintln(bw, "  Invalid signatures:")
		for _, ir := range b.InvalidSignatures {
			fmt.Fprintf(bw, "    %s (%s) — %s\n", ir.Signer, ir.Fingerprint, ir.Reason)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type thresholdShowSig struct {
	Signer               string    `json:"signer"`
	PublicKeyFingerprint string    `json:"public_key_fingerprint"`
	SignedAt             time.Time `json:"signed_at"`
	Valid                bool      `json:"valid"`
	Reason               string    `json:"reason,omitempty"`
}

type thresholdShowBody struct {
	Header          *threshold.AttestationHeader `json:"header"`
	Signatures      []thresholdShowSig           `json:"signatures"`
	Threshold       int                          `json:"threshold,omitempty"`
	ValidDistinct   int                          `json:"valid_distinct,omitempty"`
	QuorumMet       bool                         `json:"quorum_met"`
	RosterAvailable bool                         `json:"roster_available"`
}

// WriteText renders the attestation header plus per-signature verification
// state as human-readable text to w.
func (b thresholdShowBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	h := b.Header
	fmt.Fprintf(bw, "Attestation %s/%s\n", h.Subject.Kind, h.Subject.ID)
	fmt.Fprintf(bw, "  Subject hash:  %s\n", h.Subject.Hash)
	fmt.Fprintf(bw, "  Roster:        %s (hash %s)\n", h.RosterID, h.RosterHash)
	fmt.Fprintf(bw, "  Created at:    %s\n", h.CreatedAt.Format(time.RFC3339))
	if b.RosterAvailable {
		fmt.Fprintf(bw, "  Threshold:     %d\n", b.Threshold)
		fmt.Fprintf(bw, "  Distinct OK:   %d\n", b.ValidDistinct)
		verdict := "✗ quorum NOT met"
		if b.QuorumMet {
			verdict = "✓ quorum met"
		}
		fmt.Fprintf(bw, "  Verdict:       %s\n", verdict)
	} else {
		fmt.Fprintln(bw, "  Roster:        unavailable (skipping per-signature verification)")
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "Signatures (%d):\n", len(b.Signatures))
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SIGNER\tFINGERPRINT\tSIGNED AT\tVALID\tNOTES")
	for _, s := range b.Signatures {
		valid := "✓"
		if !s.Valid {
			valid = "✗"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			s.Signer, s.PublicKeyFingerprint,
			s.SignedAt.Format(time.RFC3339), valid, s.Reason)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func renderSignaturesUnverified(sigs []*threshold.AttestationSignature) []thresholdShowSig {
	out := make([]thresholdShowSig, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, thresholdShowSig{
			Signer:               s.Signer,
			PublicKeyFingerprint: s.PublicKeyFingerprint,
			SignedAt:             s.SignedAt,
			Valid:                false,
			Reason:               "roster unavailable",
		})
	}
	return out
}

// ----- helpers -----

// loadMembers parses --member specs or a --members-file into the
// threshold.Member slice.
func loadMembers(specs []string, file string) ([]threshold.Member, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		var entries []struct {
			Signer    string `json:"signer"`
			PublicKey string `json:"public_key"`
		}
		if err := stdjson.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		out := make([]threshold.Member, 0, len(entries))
		for _, e := range entries {
			pub, err := base64.StdEncoding.DecodeString(e.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("member %q: pubkey not base64", e.Signer)
			}
			if len(pub) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("member %q: pubkey length %d != %d",
					e.Signer, len(pub), ed25519.PublicKeySize)
			}
			out = append(out, threshold.NewMember(e.Signer, pub))
		}
		return out, nil
	}
	out := make([]threshold.Member, 0, len(specs))
	for _, s := range specs {
		m, err := threshold.ParseMemberSpec(s)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// loadSignerForThreshold loads the operator's signing keypair via
// the canonical keystore path.  Same as loadSignerForJIT — the two
// features share the operator's identity key.
func loadSignerForThreshold() (*backup.Signer, *backup.Verifier, error) {
	return loadSignerForJIT()
}

// thresholdSignerAdapter wraps backup.Signer to satisfy
// threshold.Signer (avoids the threshold → backup import cycle).
type thresholdSignerAdapter struct {
	s *backup.Signer
}

// Sign returns the ed25519 signature of payload using the wrapped backup signer.
func (a thresholdSignerAdapter) Sign(payload []byte) []byte { return a.s.Sign(payload) }

// PublicKey returns the ed25519 public key of the wrapped backup signer.
func (a thresholdSignerAdapter) PublicKey() ed25519.PublicKey { return a.s.PublicKey() }
