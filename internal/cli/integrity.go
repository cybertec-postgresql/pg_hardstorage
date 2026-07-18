// integrity.go — CLI surface for chunk and manifest integrity scans.
package cli

import (
	"crypto/ed25519"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// newIntegrityCmd builds `pg_hardstorage integrity`: continuous-
// attestation runs that re-verify manifests + (optionally) re-fetch
// chunks, sign the resulting Run, and persist it for forensics.
//
//	integrity run    --repo R [--strategy ...]    — execute + sign + persist
//	integrity list   --repo R                     — newest-first
//	integrity show   <id> --repo R                — full run body
//	integrity verify <id> --repo R                — re-check the signature
func newIntegrityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "integrity",
		Short: "Continuous-attestation runs: re-verify manifests + chunks, sign the report",
		Long: `Run periodic integrity scans of the repository.  Each run
re-verifies every committed manifest's signature, confirms its
referenced chunks are still present, and (optionally) re-fetches
a sample of chunks for plaintext SHA-256 verification.  The
result is signed with the operator's key and stored in the repo
under integrity/runs/<id>.json so an auditor can prove the repo
was intact at any historical attest time.

Strategies (cost vs assurance):

  manifests-only          fastest; just re-verify ed25519 signatures
  presence (default)      manifests + Stat every referenced chunk
  content-sample N        manifests + Stat all + plaintext-SHA-256 N% sample
  content-full            manifests + Stat all + plaintext-SHA-256 every chunk

Exit codes:

  0  no issues
  9  found_issues (signature break, missing or mismatched chunks) — exit
     code matches the "verify-failed" namespace; cron will alert.
  6  notfound (run id or strategy target absent)`,
	}
	c.AddCommand(
		newIntegrityRunCmd(),
		newIntegrityListCmd(),
		newIntegrityShowCmd(),
		newIntegrityVerifyCmd(),
	)
	return c
}

// ----- run -----

func newIntegrityRunCmd() *cobra.Command {
	var (
		repoURL    string
		strategy   string
		percent    int
		count      int
		seed       int64
		deployment string
		note       string
		skipSign   bool
	)
	c := &cobra.Command{
		Use:          "run",
		Short:        "Execute one continuous-attestation run; sign + persist; exit 9 on issues",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIntegrityRun(cmd, integrityRunFlags{
				repoURL: repoURL, strategy: strategy,
				percent: percent, count: count, seed: seed,
				deployment: deployment, note: note,
				skipSign: skipSign,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&strategy, "strategy", "presence",
		"manifests-only | presence | content-sample | content-full")
	c.Flags().IntVar(&percent, "percent", 0,
		"sample percent (0..100; only valid with --strategy content-sample)")
	c.Flags().IntVar(&count, "count", 0,
		"sample chunk count (only valid with --strategy content-sample)")
	c.Flags().Int64Var(&seed, "seed", 0,
		"deterministic sampling seed (0 → derived from chunk count)")
	c.Flags().StringVar(&deployment, "deployment", "",
		"scope to one deployment (default: every deployment)")
	c.Flags().StringVar(&note, "note", "",
		"operator note recorded with the run (e.g. 'weekly cron')")
	c.Flags().BoolVar(&skipSign, "skip-sign", false,
		"compute the run but do not sign / persist it (testing)")
	return c
}

type integrityRunFlags struct {
	repoURL    string
	strategy   string
	percent    int
	count      int
	seed       int64
	deployment string
	note       string
	skipSign   bool
}

func runIntegrityRun(cmd *cobra.Command, f integrityRunFlags) error {
	d := DispatcherFrom(cmd)
	strategy := integrity.Strategy{
		Mode:    f.strategy,
		Percent: f.percent,
		Count:   f.count,
		Seed:    f.seed,
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if !f.skipSign {
		if err := assertRepoWritable(cmd.Context(), sp, "integrity run"); err != nil {
			return err
		}
	}
	signer, verifier, err := loadSignerForJIT() // shared keystore loader
	if err != nil {
		return err
	}
	cas := casdefault.New(sp)
	ms := backup.NewManifestStore(sp)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage:   sp,
		Manifests: ms,
		Verifier:  verifier,
		CAS:       cas,
	})
	run, err := eng.Execute(cmd.Context(), f.deployment, strategy, f.note)
	if err != nil {
		if errors.Is(err, integrity.ErrInvalidStrategy) {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("integrity run: %v", err)).Wrap(output.ErrUsage)
		}
		return output.NewError("integrity.run_failed",
			fmt.Sprintf("integrity run: %v", err)).Wrap(err)
	}
	if !f.skipSign {
		if err := integrity.SignRun(run, integritySignerAdapter{s: signer}); err != nil {
			return output.NewError("integrity.sign_failed",
				fmt.Sprintf("integrity run: sign: %v", err)).Wrap(err)
		}
		if err := integrity.NewRunStore(sp).Put(cmd.Context(), run); err != nil {
			return output.NewError("integrity.put_failed",
				fmt.Sprintf("integrity run: persist: %v", err)).Wrap(err)
		}
	}
	// Best-effort audit append (failure is not fatal). --skip-sign is a
	// genuinely read-only testing mode, so it must not append either.
	if !f.skipSign {
		auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
		_ = auditStore.Append(cmd.Context(), &audit.Event{
			Action:    "integrity.run",
			Timestamp: time.Now().UTC(),
			Body: map[string]any{
				"run_id":            run.ID,
				"status":            string(run.Status),
				"strategy":          strategy.Mode,
				"deployment":        f.deployment,
				"manifests_total":   run.Manifests.Total,
				"signatures_ok":     run.Manifests.SignaturesOK,
				"signatures_fail":   run.Manifests.SignaturesFail,
				"chunks_referenced": run.Chunks.DistinctReferenced,
				"chunks_missing":    run.Chunks.Missing,
				"chunks_mismatched": run.Chunks.Mismatched,
				"chunks_verified":   run.Chunks.Verified,
			},
		})
	}
	body := integrityRunBody{Run: run}
	// Dual-stream: emit body regardless; on issues, return a verify.*
	// error to flip the exit code.
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}
	if run.Status == integrity.StatusFoundIssues {
		return output.NewError("verify.integrity_issues",
			fmt.Sprintf("integrity run: %d signature failure(s), %d missing chunk(s), %d mismatched chunk(s)",
				run.Manifests.SignaturesFail,
				run.Chunks.Missing,
				run.Chunks.Mismatched)).
			WithSuggestion(&output.Suggestion{
				Human: "review run.body for the per-failure detail; consider repo replicate / repo heal to recover",
			})
	}
	return nil
}

// ----- list -----

func newIntegrityListCmd() *cobra.Command {
	var (
		repoURL    string
		since      string
		statusF    string
		deployment string
	)
	c := &cobra.Command{
		Use:          "list",
		Short:        "List integrity runs newest-first",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIntegrityList(cmd, repoURL, since, statusF, deployment)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&since, "since", "",
		"only runs that started at/after this RFC3339 timestamp (e.g. 2026-04-01T00:00:00Z)")
	c.Flags().StringVar(&statusF, "status", "",
		"filter by run status: ok | found_issues | error")
	c.Flags().StringVar(&deployment, "deployment", "",
		"filter by deployment scope")
	return c
}

func runIntegrityList(cmd *cobra.Command, repoURL, since, statusF, deployment string) error {
	d := DispatcherFrom(cmd)
	filter := integrity.ListFilter{Deployment: deployment}
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("integrity list: --since: %v", err)).Wrap(output.ErrUsage)
		}
		filter.Since = &t
	}
	switch strings.ToLower(statusF) {
	case "":
	case "ok":
		filter.Status = integrity.StatusOK
	case "found_issues":
		filter.Status = integrity.StatusFoundIssues
	case "error":
		filter.Status = integrity.StatusError
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("integrity list: unknown --status %q", statusF)).Wrap(output.ErrUsage)
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	runs, err := integrity.NewRunStore(sp).List(cmd.Context(), filter)
	if err != nil {
		return output.NewError("integrity.list_failed",
			fmt.Sprintf("integrity list: %v", err)).Wrap(err)
	}
	body := integrityListBody{Count: len(runs)}
	for _, r := range runs {
		body.Entries = append(body.Entries, integrityRunSummary{
			ID:               r.ID,
			Status:           string(r.Status),
			Strategy:         r.Strategy.Mode,
			StartedAt:        r.StartedAt,
			Deployment:       r.Deployment,
			ManifestsTotal:   r.Manifests.Total,
			SignaturesFail:   r.Manifests.SignaturesFail,
			ChunksReferenced: r.Chunks.DistinctReferenced,
			ChunksMissing:    r.Chunks.Missing,
			ChunksMismatched: r.Chunks.Mismatched,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- show -----

func newIntegrityShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <id>",
		Short:        "Show one integrity run's full body + per-failure detail",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIntegrityShow(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runIntegrityShow(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	r, err := integrity.NewRunStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, integrity.ErrRunNotFound) {
			return output.NewError("notfound.run",
				fmt.Sprintf("integrity show: run %q not found", id)).Wrap(err)
		}
		return output.NewError("integrity.get_failed",
			fmt.Sprintf("integrity show: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(integrityRunBody{Run: r}))
}

// ----- verify -----

func newIntegrityVerifyCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "verify <id>",
		Short:        "Re-validate the signature on a previously-stored run",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIntegrityVerify(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runIntegrityVerify(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	r, err := integrity.NewRunStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, integrity.ErrRunNotFound) {
			return output.NewError("notfound.run",
				fmt.Sprintf("integrity verify: run %q not found", id)).Wrap(err)
		}
		return output.NewError("integrity.get_failed",
			fmt.Sprintf("integrity verify: %v", err)).Wrap(err)
	}
	signer, _, err := loadSignerForJIT()
	if err != nil {
		return err
	}
	resolver := &integrity.SingleKeyResolver{Key: signer.PublicKey()}
	body := integrityVerifyBody{
		ID:                   r.ID,
		Status:               string(r.Status),
		PublicKeyFingerprint: r.PublicKeyFingerprint,
	}
	if err := integrity.VerifyRun(r, resolver); err != nil {
		body.SignatureValid = false
		body.Reason = err.Error()
		if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
			return rerr
		}
		return output.NewError("verify.integrity_signature",
			fmt.Sprintf("integrity verify: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the run's signature did not validate; either the run was tampered with or it was signed with a different operator key (compare public_key_fingerprint to the local key's)",
			})
	}
	body.SignatureValid = true
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- bodies + text rendering -----

type integrityRunBody struct {
	*integrity.Run
}

// WriteText renders the integrity run — verdict, manifest and chunk rollups,
// and any failure detail — as human-readable text to w.
func (b integrityRunBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	r := b.Run
	verdict := map[integrity.Status]string{
		integrity.StatusOK:          "✓ ok",
		integrity.StatusFoundIssues: "✗ found_issues",
		integrity.StatusError:       "✗ error",
	}[r.Status]
	if verdict == "" {
		verdict = string(r.Status)
	}
	fmt.Fprintf(bw, "Integrity run %s\n", r.ID)
	fmt.Fprintf(bw, "  Status:      %s\n", verdict)
	fmt.Fprintf(bw, "  Strategy:    %s", r.Strategy.Mode)
	if r.Strategy.Percent > 0 {
		fmt.Fprintf(bw, " (%d%% sample)", r.Strategy.Percent)
	}
	if r.Strategy.Count > 0 {
		fmt.Fprintf(bw, " (%d-chunk sample)", r.Strategy.Count)
	}
	fmt.Fprintln(bw)
	if r.Deployment != "" {
		fmt.Fprintf(bw, "  Deployment:  %s\n", r.Deployment)
	}
	if r.Note != "" {
		fmt.Fprintf(bw, "  Note:        %s\n", r.Note)
	}
	fmt.Fprintf(bw, "  Started at:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Finished at: %s\n", r.FinishedAt.Format(time.RFC3339))
	if r.PublicKeyFingerprint != "" {
		fmt.Fprintf(bw, "  Signed by:   %s\n", r.PublicKeyFingerprint)
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Manifests:")
	fmt.Fprintf(bw, "  total:           %d\n", r.Manifests.Total)
	fmt.Fprintf(bw, "  signatures ok:   %d\n", r.Manifests.SignaturesOK)
	fmt.Fprintf(bw, "  signatures fail: %d\n", r.Manifests.SignaturesFail)
	if r.Strategy.Mode != "manifests-only" {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Chunks:")
		fmt.Fprintf(bw, "  distinct referenced: %d\n", r.Chunks.DistinctReferenced)
		fmt.Fprintf(bw, "  presence checked:    %d\n", r.Chunks.PresenceChecked)
		fmt.Fprintf(bw, "  sampled:             %d\n", r.Chunks.Sampled)
		fmt.Fprintf(bw, "  verified:            %d\n", r.Chunks.Verified)
		fmt.Fprintf(bw, "  mismatched:          %d\n", r.Chunks.Mismatched)
		fmt.Fprintf(bw, "  missing:             %d\n", r.Chunks.Missing)
		if r.Chunks.Skipped > 0 {
			fmt.Fprintf(bw, "  skipped (encrypted): %d\n", r.Chunks.Skipped)
		}
	}
	if len(r.Manifests.Failures) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Manifest failures:")
		for _, f := range r.Manifests.Failures {
			id := f.Deployment
			if f.BackupID != "" {
				id += "/" + f.BackupID
			}
			fmt.Fprintf(bw, "  %s — %s\n", id, f.Reason)
		}
	}
	if len(r.Chunks.Failures) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Chunk failures:")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  HASH\tREASON\tREFERENCED BY")
		for _, f := range r.Chunks.Failures {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n",
				f.ChunkHash, f.Reason, strings.Join(f.ReferencedBy, ","))
		}
		_ = tw.Flush()
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type integrityRunSummary struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Strategy         string    `json:"strategy"`
	StartedAt        time.Time `json:"started_at"`
	Deployment       string    `json:"deployment,omitempty"`
	ManifestsTotal   int       `json:"manifests_total"`
	SignaturesFail   int       `json:"signatures_fail"`
	ChunksReferenced int       `json:"chunks_referenced"`
	ChunksMissing    int       `json:"chunks_missing"`
	ChunksMismatched int       `json:"chunks_mismatched"`
}

type integrityListBody struct {
	Count   int                   `json:"count"`
	Entries []integrityRunSummary `json:"entries"`
}

// WriteText renders the saved integrity-run list as a tabular summary to w.
func (b integrityListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d run(s)\n\n", b.Count)
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tSTRATEGY\tSTARTED\tMANIFESTS\tCHUNKS")
	for _, e := range b.Entries {
		issues := ""
		if e.SignaturesFail > 0 || e.ChunksMissing > 0 || e.ChunksMismatched > 0 {
			issues = fmt.Sprintf(" (%d sig-fail · %d missing · %d mismatched)",
				e.SignaturesFail, e.ChunksMissing, e.ChunksMismatched)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d%s\n",
			e.ID, e.Status, e.Strategy,
			e.StartedAt.Format(time.RFC3339),
			e.ManifestsTotal, e.ChunksReferenced, issues)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type integrityVerifyBody struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
	SignatureValid       bool   `json:"signature_valid"`
	Reason               string `json:"reason,omitempty"`
}

// WriteText renders the signature-verify outcome for a stored integrity run
// as human-readable text to w.
func (b integrityVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verdict := "✓ signature valid"
	if !b.SignatureValid {
		verdict = "✗ signature INVALID"
	}
	fmt.Fprintf(bw, "%s for run %s\n", verdict, b.ID)
	fmt.Fprintf(bw, "  Run status:     %s\n", b.Status)
	fmt.Fprintf(bw, "  Signed by:      %s\n", b.PublicKeyFingerprint)
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason:         %s\n", b.Reason)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// integritySignerAdapter wraps backup.Signer to satisfy the
// integrity.Signer interface (no import cycle).
type integritySignerAdapter struct {
	s *backup.Signer
}

// Sign returns the ed25519 signature of payload using the wrapped backup signer.
func (a integritySignerAdapter) Sign(payload []byte) []byte { return a.s.Sign(payload) }

// PublicKey returns the ed25519 public key of the wrapped backup signer.
func (a integritySignerAdapter) PublicKey() ed25519.PublicKey { return a.s.PublicKey() }

// _ anchors the encoding/json import.
var _ = stdjson.Marshal
