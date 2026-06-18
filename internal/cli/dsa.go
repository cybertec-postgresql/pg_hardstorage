// dsa.go — CLI surface for GDPR Data Subject Access reports.
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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/dsa"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newDsaCmd builds `pg_hardstorage dsa`: GDPR Data Subject Access
// helper.  Operationally:
//
//	dsa locate --subject-id <id> --tenant <T>          — produce a signed report
//	dsa list                                           — newest-first
//	dsa show <report-id>                               — full body + actions
//	dsa verify <report-id>                             — re-check the signature
func newDsaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dsa",
		Short: "GDPR Data Subject Access helper: locate which backups contain a subject's data",
		Long: `Locate which backups contain a given subject's data so the
operator can fulfil a GDPR Article 15 (right of access) or
Article 17 (right to erasure) request.

The command walks the manifests filtered by tenant — the
GDPR-compliance boundary defined by the per-tenant KEK.  It
produces a signed report enumerating every affected backup,
with a tenant-scoped action plan that pairs with kms shred
for Article 17 erasure or with partial restore for Article 15
extraction.

Privacy: the raw subject_id is hashed (SHA-256) before being
recorded in the report.  An auditor verifies the report by
re-hashing the same raw ID; pg_hardstorage doesn't archive
real subject identifiers.

Exit codes:
  0  report generated + signed + persisted
  6  notfound (report id absent for show/verify)
  9  verify failure (signature does not validate)`,
	}
	c.AddCommand(
		newDsaLocateCmd(),
		newDsaListCmd(),
		newDsaShowCmd(),
		newDsaVerifyCmd(),
	)
	return c
}

// ----- locate -----

func newDsaLocateCmd() *cobra.Command {
	var (
		repoURL    string
		subjectID  string
		tenant     string
		article    string
		note       string
		deployment string
		windowFrom string
		windowTo   string
		skipSign   bool
	)
	c := &cobra.Command{
		Use:          "locate",
		Short:        "Generate a signed DSA report for a subject + tenant",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDsaLocate(cmd, dsaLocateFlags{
				repoURL: repoURL, subjectID: subjectID, tenant: tenant,
				article: article, note: note, deployment: deployment,
				windowFrom: windowFrom, windowTo: windowTo, skipSign: skipSign,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&subjectID, "subject-id", "",
		"opaque subject identifier (hashed before recording; required)")
	_ = c.MarkFlagRequired("subject-id")
	c.Flags().StringVar(&tenant, "tenant", "",
		"tenant containing the subject's data (required)")
	_ = c.MarkFlagRequired("tenant")
	c.Flags().StringVar(&article, "article", "art_17_erasure",
		"GDPR article: art_15_access | art_17_erasure | other")
	c.Flags().StringVar(&note, "note", "",
		"operator note recorded in the report (e.g. ticket ref)")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict scan to one deployment (default: all)")
	c.Flags().StringVar(&windowFrom, "window-from", "",
		"only backups stopped at/after this RFC3339 timestamp")
	c.Flags().StringVar(&windowTo, "window-to", "",
		"only backups stopped at/before this RFC3339 timestamp")
	c.Flags().BoolVar(&skipSign, "skip-sign", false,
		"compute the report but do not sign / persist it (testing)")
	return c
}

type dsaLocateFlags struct {
	repoURL    string
	subjectID  string
	tenant     string
	article    string
	note       string
	deployment string
	windowFrom string
	windowTo   string
	skipSign   bool
}

func runDsaLocate(cmd *cobra.Command, f dsaLocateFlags) error {
	d := DispatcherFrom(cmd)
	article := dsa.Article(f.article)
	switch article {
	case dsa.ArticleAccess, dsa.ArticleErasure, dsa.ArticleOther:
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("dsa locate: --article %q is not recognised (allowed: art_15_access | art_17_erasure | other)",
				f.article)).Wrap(output.ErrUsage)
	}
	opts := dsa.LocateOptions{
		SubjectID:  f.subjectID,
		Tenant:     f.tenant,
		Article:    article,
		Note:       f.note,
		Deployment: f.deployment,
	}
	if f.windowFrom != "" {
		t, err := time.Parse(time.RFC3339, f.windowFrom)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("dsa locate: --window-from: %v", err)).Wrap(output.ErrUsage)
		}
		opts.WindowStart = t
	}
	if f.windowTo != "" {
		t, err := time.Parse(time.RFC3339, f.windowTo)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("dsa locate: --window-to: %v", err)).Wrap(output.ErrUsage)
		}
		opts.WindowEnd = t
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	signer, verifier, err := loadSignerForJIT()
	if err != nil {
		return err
	}
	ms := backup.NewManifestStore(sp)
	loc := dsa.NewLocator(ms, verifier)
	report, err := loc.Locate(cmd.Context(), opts)
	if err != nil {
		return output.NewError("dsa.locate_failed",
			fmt.Sprintf("dsa locate: %v", err)).Wrap(err)
	}
	if !f.skipSign {
		if err := dsa.SignReport(report, dsaSignerAdapter{s: signer}); err != nil {
			return output.NewError("dsa.sign_failed",
				fmt.Sprintf("dsa locate: sign: %v", err)).Wrap(err)
		}
		if err := dsa.NewReportStore(sp).Put(cmd.Context(), report); err != nil {
			return output.NewError("dsa.put_failed",
				fmt.Sprintf("dsa locate: persist: %v", err)).Wrap(err)
		}
	}
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "dsa.locate",
		Tenant:    f.tenant,
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"report_id":            report.ID,
			"subject_id_hash":      report.SubjectIDHash,
			"article":              string(article),
			"manifests_affected":   report.ManifestsAffected,
			"deployments_affected": report.DeploymentsAffected,
		},
	})
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(dsaReportBody{Report: report}))
}

// ----- list -----

func newDsaListCmd() *cobra.Command {
	var (
		repoURL string
		since   string
		tenant  string
		article string
		subject string
	)
	c := &cobra.Command{
		Use:          "list",
		Short:        "List DSA reports newest-first",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDsaList(cmd, repoURL, since, tenant, article, subject)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&since, "since", "",
		"only reports generated at/after this RFC3339 timestamp")
	c.Flags().StringVar(&tenant, "tenant", "",
		"filter by tenant")
	c.Flags().StringVar(&article, "article", "",
		"filter by article (art_15_access | art_17_erasure | other)")
	c.Flags().StringVar(&subject, "subject-id", "",
		"filter by subject (the value is hashed before matching, so the same opaque ID can be used as on locate)")
	return c
}

func runDsaList(cmd *cobra.Command, repoURL, since, tenant, article, subject string) error {
	d := DispatcherFrom(cmd)
	filter := dsa.ListFilter{Tenant: tenant}
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("dsa list: --since: %v", err)).Wrap(output.ErrUsage)
		}
		filter.Since = &t
	}
	if article != "" {
		switch dsa.Article(article) {
		case dsa.ArticleAccess, dsa.ArticleErasure, dsa.ArticleOther:
			filter.Article = dsa.Article(article)
		default:
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("dsa list: --article %q is not recognised", article)).Wrap(output.ErrUsage)
		}
	}
	if subject != "" {
		filter.SubjectIDHash = dsa.HashSubjectIDForFilter(subject)
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	reports, err := dsa.NewReportStore(sp).List(cmd.Context(), filter)
	if err != nil {
		return output.NewError("dsa.list_failed",
			fmt.Sprintf("dsa list: %v", err)).Wrap(err)
	}
	body := dsaListBody{Count: len(reports)}
	for _, r := range reports {
		body.Entries = append(body.Entries, dsaReportSummary{
			ID:                  r.ID,
			GeneratedAt:         r.GeneratedAt,
			SubjectIDHash:       r.SubjectIDHash,
			Tenant:              r.Tenant,
			Article:             string(r.Article),
			ManifestsAffected:   r.ManifestsAffected,
			DeploymentsAffected: r.DeploymentsAffected,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- show -----

func newDsaShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <report-id>",
		Short:        "Show one DSA report's full body + suggested actions",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDsaShow(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runDsaShow(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	r, err := dsa.NewReportStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, dsa.ErrReportNotFound) {
			return output.NewError("notfound.report",
				fmt.Sprintf("dsa show: report %q not found", id)).Wrap(err)
		}
		return output.NewError("dsa.get_failed",
			fmt.Sprintf("dsa show: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(dsaReportBody{Report: r}))
}

// ----- verify -----

func newDsaVerifyCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "verify <report-id>",
		Short:        "Re-validate the signature on a previously-stored DSA report",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDsaVerify(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runDsaVerify(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	r, err := dsa.NewReportStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, dsa.ErrReportNotFound) {
			return output.NewError("notfound.report",
				fmt.Sprintf("dsa verify: report %q not found", id)).Wrap(err)
		}
		return output.NewError("dsa.get_failed",
			fmt.Sprintf("dsa verify: %v", err)).Wrap(err)
	}
	signer, _, err := loadSignerForJIT()
	if err != nil {
		return err
	}
	resolver := &dsa.SingleKeyResolver{Key: signer.PublicKey()}
	body := dsaVerifyBody{
		ID:                   r.ID,
		Tenant:               r.Tenant,
		Article:              string(r.Article),
		PublicKeyFingerprint: r.PublicKeyFingerprint,
	}
	if err := dsa.VerifyReport(r, resolver); err != nil {
		body.SignatureValid = false
		body.Reason = err.Error()
		if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
			return rerr
		}
		return output.NewError("verify.dsa_signature",
			fmt.Sprintf("dsa verify: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the report's signature did not validate; either the report was tampered with or it was signed with a different operator key",
			})
	}
	body.SignatureValid = true
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- bodies + text rendering -----

type dsaReportBody struct {
	*dsa.Report
}

// WriteText renders the DSA report — rollups, affected backups, suggested
// follow-up actions — as human-readable text to w.
func (b dsaReportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	r := b.Report
	fmt.Fprintf(bw, "DSA report %s\n", r.ID)
	fmt.Fprintf(bw, "  Generated at:        %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Subject ID (SHA-256):%s\n", r.SubjectIDHash)
	fmt.Fprintf(bw, "  Tenant:              %s\n", r.Tenant)
	fmt.Fprintf(bw, "  Article:             %s\n", r.Article)
	if r.Note != "" {
		fmt.Fprintf(bw, "  Note:                %s\n", r.Note)
	}
	if r.WindowStart != nil || r.WindowEnd != nil {
		from := "—"
		to := "—"
		if r.WindowStart != nil {
			from = r.WindowStart.Format(time.RFC3339)
		}
		if r.WindowEnd != nil {
			to = r.WindowEnd.Format(time.RFC3339)
		}
		fmt.Fprintf(bw, "  Window:              %s → %s\n", from, to)
	}
	if r.PublicKeyFingerprint != "" {
		fmt.Fprintf(bw, "  Signed by:           %s\n", r.PublicKeyFingerprint)
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "Scanned %d manifest(s); %d affected across %d deployment(s).\n",
		r.ManifestsScanned, r.ManifestsAffected, r.DeploymentsAffected)

	if len(r.Deployments) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Per-deployment rollup:")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  DEPLOYMENT\tBACKUPS")
		for _, d := range r.Deployments {
			fmt.Fprintf(tw, "  %s\t%d\n", d.Deployment, d.BackupCount)
		}
		_ = tw.Flush()
	}
	if len(r.AffectedBackups) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Affected backups (oldest first):")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  STARTED\tDEPLOYMENT\tBACKUP_ID\tTYPE\tENCRYPTED\tKEK_REF")
		for _, ab := range r.AffectedBackups {
			enc := "·"
			if ab.Encrypted {
				enc = "✓"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				ab.StartedAt.Format(time.RFC3339), ab.Deployment, ab.BackupID,
				ab.Type, enc, ab.KEKRef)
		}
		_ = tw.Flush()
	}
	if len(r.SuggestedActions) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Suggested actions:")
		for i, a := range r.SuggestedActions {
			fmt.Fprintf(bw, "  %d. [%s] %s\n", i+1, a.Article, a.Description)
			if a.Command != "" {
				fmt.Fprintf(bw, "       command: %s\n", a.Command)
			}
			if a.DocURL != "" {
				fmt.Fprintf(bw, "       doc:     %s\n", a.DocURL)
			}
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type dsaReportSummary struct {
	ID                  string    `json:"id"`
	GeneratedAt         time.Time `json:"generated_at"`
	SubjectIDHash       string    `json:"subject_id_hash"`
	Tenant              string    `json:"tenant"`
	Article             string    `json:"article"`
	ManifestsAffected   int       `json:"manifests_affected"`
	DeploymentsAffected int       `json:"deployments_affected"`
}

type dsaListBody struct {
	Count   int                `json:"count"`
	Entries []dsaReportSummary `json:"entries"`
}

// WriteText renders the list of stored DSA reports as a tabular summary to w.
func (b dsaListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d report(s)\n\n", b.Count)
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGENERATED\tTENANT\tARTICLE\tAFFECTED\tDEPLOYMENTS\tSUBJECT-HASH")
	for _, e := range b.Entries {
		// Truncate the subject hash to the leading 12 hex chars
		// for readability in the table; full hash stays in JSON.
		short := e.SubjectIDHash
		if len(short) > 12 {
			short = short[:12] + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			e.ID, e.GeneratedAt.Format(time.RFC3339),
			e.Tenant, e.Article,
			e.ManifestsAffected, e.DeploymentsAffected, short)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type dsaVerifyBody struct {
	ID                   string `json:"id"`
	Tenant               string `json:"tenant"`
	Article              string `json:"article"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
	SignatureValid       bool   `json:"signature_valid"`
	Reason               string `json:"reason,omitempty"`
}

// WriteText renders the signature-verify outcome for a stored DSA report as
// human-readable text to w.
func (b dsaVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verdict := "✓ signature valid"
	if !b.SignatureValid {
		verdict = "✗ signature INVALID"
	}
	fmt.Fprintf(bw, "%s for report %s\n", verdict, b.ID)
	fmt.Fprintf(bw, "  Tenant:    %s\n", b.Tenant)
	fmt.Fprintf(bw, "  Article:   %s\n", b.Article)
	fmt.Fprintf(bw, "  Signed by: %s\n", b.PublicKeyFingerprint)
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason:    %s\n", b.Reason)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// dsaSignerAdapter wraps backup.Signer to satisfy dsa.Signer.
type dsaSignerAdapter struct {
	s *backup.Signer
}

// Sign returns the ed25519 signature of payload using the wrapped backup signer.
func (a dsaSignerAdapter) Sign(payload []byte) []byte { return a.s.Sign(payload) }

// PublicKey returns the ed25519 public key of the wrapped backup signer.
func (a dsaSignerAdapter) PublicKey() ed25519.PublicKey { return a.s.PublicKey() }

var _ = stdjson.Marshal
