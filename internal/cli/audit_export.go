// audit_export.go — CLI surface for exporting and verifying signed audit evidence bundles.
package cli

import (
	"crypto/ed25519"
	stdjson "encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newAuditExportBundleCmd implements `audit export-bundle`.
//
// Produces a signed evidence bundle (gzipped tar) of audit events
// over a time window.  Used for:
//
//   - Compliance forensics ("show me a tamper-evident record of
//     every backup operation in Q1").
//   - Post-incident review (LLM-helper transcripts include
//     evidence-bundle exports of the conversation's audit trail).
//   - Regulatory submission ("here is signed proof of every
//     destructive operation we approved").
//
// The bundle is signed with the operator's ed25519 signing key
// (the same key that signs manifests).  An auditor verifies via
// `audit verify-bundle`.
func newAuditExportBundleCmd() *cobra.Command {
	var (
		repoURL        string
		out            string
		operator       string
		since          string
		until          string
		actionPrefix   string
		action         string
		actor          string
		tenant         string
		deployment     string
		backupID       string
		includeAnchors bool
	)
	c := &cobra.Command{
		Use:   "export-bundle --repo <url> --out <path>",
		Short: "Export a signed evidence bundle of audit events",
		Long: `Walks the audit chain over the requested window, packages
the events + chain proof + operator's public key into a gzipped
tarball, and signs the bundle with the operator's ed25519 key.

The output is a forensics-grade artifact suitable for compliance
review.  An auditor verifies via ` + "`pg_hardstorage audit verify-bundle`" + `
or via the canonical Go API in ` + "`internal/audit.VerifyBundle`" + `.

Window:
  --since DURATION_OR_RFC3339   lower-bound on event timestamp
  --until DURATION_OR_RFC3339   upper-bound (exclusive)

Filters (all optional):
  --action ACTION              exact match (e.g. backup.create)
  --action-prefix PREFIX       dotted-namespace prefix (e.g. backup.)
  --actor ACTOR                exact match
  --tenant TENANT              exact match
  --deployment NAME            exact match
  --backup-id ID               exact match

Other:
  --include-anchors            also include anchor history
  --operator NAME              record operator identity in manifest

Output:
  --out PATH                   destination .tar.gz file (required)

Read-only against the source repo.  Safe at any cadence.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditExportBundle(cmd, auditExportBundleFlags{
				repoURL:        repoURL,
				out:            out,
				operator:       operator,
				since:          since,
				until:          until,
				actionPrefix:   actionPrefix,
				action:         action,
				actor:          actor,
				tenant:         tenant,
				deployment:     deployment,
				backupID:       backupID,
				includeAnchors: includeAnchors,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&out, "out", "", "destination .tar.gz file (required)")
	_ = c.MarkFlagRequired("out")
	c.Flags().StringVar(&operator, "operator", "",
		"record operator identity in the bundle manifest")
	c.Flags().StringVar(&since, "since", "",
		"lower-bound on event timestamp (e.g. 30d, RFC3339)")
	c.Flags().StringVar(&until, "until", "",
		"upper-bound on event timestamp (exclusive)")
	c.Flags().StringVar(&action, "action", "", "exact-match audit action filter")
	c.Flags().StringVar(&actionPrefix, "action-prefix", "", "dotted-namespace prefix filter")
	c.Flags().StringVar(&actor, "actor", "", "exact-match actor filter")
	c.Flags().StringVar(&tenant, "tenant", "", "exact-match tenant filter")
	c.Flags().StringVar(&deployment, "deployment", "", "exact-match deployment filter")
	c.Flags().StringVar(&backupID, "backup-id", "", "exact-match backup ID filter")
	c.Flags().BoolVar(&includeAnchors, "include-anchors", false,
		"also include the anchor history in the bundle")
	return c
}

type auditExportBundleFlags struct {
	repoURL        string
	out            string
	operator       string
	since          string
	until          string
	actionPrefix   string
	action         string
	actor          string
	tenant         string
	deployment     string
	backupID       string
	includeAnchors bool
}

func runAuditExportBundle(cmd *cobra.Command, f auditExportBundleFlags) error {
	d := DispatcherFrom(cmd)
	since, err := parseSinceUntil(f.since)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit export-bundle: --since: %v", err)).Wrap(output.ErrUsage)
	}
	until, err := parseSinceUntil(f.until)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit export-bundle: --until: %v", err)).Wrap(output.ErrUsage)
	}

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	bsigner, _, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("audit export-bundle: load signer: %v", err)).Wrap(err)
	}

	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	// Open the destination file with O_EXCL so we never silently
	// overwrite a previous evidence bundle (operators frequently
	// run with --out same/path and would lose the prior bundle
	// without a refusal).
	abs, err := filepath.Abs(f.out)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit export-bundle: --out: %v", err)).Wrap(output.ErrUsage)
	}
	outFile, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return output.NewError("conflict.file_exists",
				fmt.Sprintf("audit export-bundle: --out %q already exists; refusing to overwrite", abs)).
				WithSuggestion(&output.Suggestion{
					Human: "delete the existing file or pick a different --out path",
				})
		}
		return output.NewError("internal",
			fmt.Sprintf("audit export-bundle: open %q: %v", abs, err)).Wrap(err)
	}
	defer outFile.Close()

	// Wrap backup.Signer in the audit.EventSigner adapter so the
	// audit package doesn't need an import on backup (cycle).
	signer := signerAdapter{s: bsigner}

	res, err := audit.ExportBundle(cmd.Context(), sp, outFile, signer, audit.ExportOptions{
		Filters: audit.ListFilters{
			Action:       f.action,
			ActionPrefix: f.actionPrefix,
			Actor:        f.actor,
			Tenant:       f.tenant,
			Deployment:   f.deployment,
			BackupID:     f.backupID,
			Since:        since,
			Until:        until,
		},
		IncludeAnchors: f.includeAnchors,
		Operator:       f.operator,
		SourceURL:      f.repoURL,
	})
	if err != nil {
		// Clean up the partial file on error.
		_ = outFile.Close()
		_ = os.Remove(abs)
		return output.NewError("audit.export_bundle_failed",
			fmt.Sprintf("audit export-bundle: %v", err)).Wrap(err)
	}
	// Evidence bundles are compliance artefacts; an unsynced bundle
	// could vanish on a crash immediately after the command claims
	// success.  fsync the file content, close, then fsync the parent
	// dir so the directory entry is durable.
	if syncErr := fsutil.SyncFile(outFile); syncErr != nil {
		_ = outFile.Close()
		_ = os.Remove(abs)
		return output.NewError("audit.export_bundle_failed",
			fmt.Sprintf("audit export-bundle: fsync %q: %v", abs, syncErr)).Wrap(syncErr)
	}
	if closeErr := outFile.Close(); closeErr != nil {
		_ = os.Remove(abs)
		return output.NewError("audit.export_bundle_failed",
			fmt.Sprintf("audit export-bundle: close %q: %v", abs, closeErr)).Wrap(closeErr)
	}
	if dirErr := fsutil.SyncDir(filepath.Dir(abs)); dirErr != nil {
		// File is committed; surface the dir-fsync failure but
		// don't unlink — the bundle exists and is readable.
		return output.NewError("audit.export_bundle_failed",
			fmt.Sprintf("audit export-bundle: fsync parent dir: %v", dirErr)).Wrap(dirErr)
	}
	res.Path = abs

	body := auditExportBundleBody{Result: res}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// signerAdapter wraps backup.Signer to satisfy audit.EventSigner.
// Lives here (cli) to avoid a backup → audit import cycle.
type signerAdapter struct {
	s *backup.Signer
}

// Sign returns the ed25519 signature of payload using the wrapped backup signer.
func (a signerAdapter) Sign(payload []byte) []byte { return a.s.Sign(payload) }

// PublicKey returns the ed25519 public key of the wrapped backup signer.
func (a signerAdapter) PublicKey() ed25519.PublicKey { return a.s.PublicKey() }

// PublicKeyPEM returns the wrapped signer's public key encoded as PEM.
func (a signerAdapter) PublicKeyPEM() ([]byte, error) { return a.s.PublicKeyPEM() }

// auditExportBundleBody is the v1-stable Result body wrapping
// audit.BundleResult.
type auditExportBundleBody struct {
	Result *audit.BundleResult
}

// MarshalJSON emits the embedded audit.BundleResult so the JSON shape stays
// stable across body wrapping changes.
func (b auditExportBundleBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Result)
}

// WriteText renders the export-bundle result as human-readable text to w.
func (b auditExportBundleBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	r := b.Result
	fmt.Fprintf(bw, "audit export-bundle\n")
	fmt.Fprintf(bw, "  Path:        %s\n", r.Path)
	fmt.Fprintf(bw, "  Bundle size: %d bytes (%s)\n",
		r.BundleBytes, humanBytes(r.BundleBytes))
	fmt.Fprintf(bw, "  SHA256:      %s\n", r.SHA256)
	fmt.Fprintf(bw, "  Events:      %d\n", r.EventCount)
	if r.AnchorCount > 0 {
		fmt.Fprintf(bw, "  Anchors:     %d\n", r.AnchorCount)
	}
	if r.HeadHash != "" {
		fmt.Fprintf(bw, "  Head hash:   %s (seq %d)\n", r.HeadHash, r.HeadSequence)
	}
	if r.Manifest != nil && r.Manifest.PublicKeyFingerprint != "" {
		fmt.Fprintf(bw, "  Signed by:   sha256:%s (ed25519)\n",
			r.Manifest.PublicKeyFingerprint)
	}
	fmt.Fprintf(bw, "  Walk:        %d ms\n", r.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "✓ Bundle written. Verify with `pg_hardstorage audit verify-bundle <path>`.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// newAuditVerifyBundleCmd implements `audit verify-bundle <path>`.
// Operator runs this to assert a previously-exported bundle's
// signature is valid.  Returns the manifest body on success.
func newAuditVerifyBundleCmd() *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:          "verify-bundle <path>",
		Short:        "Verify a signed audit evidence bundle",
		Long:         `Asserts the bundle's ed25519 signature is valid + the chain segment is contiguous. Returns the bundle manifest on success.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditVerifyBundle(cmd, args[0], format)
		},
	}
	c.Flags().StringVar(&format, "format", "json", "output format: json | text")
	return c
}

func runAuditVerifyBundle(cmd *cobra.Command, path, format string) error {
	d := DispatcherFrom(cmd)
	abs, err := filepath.Abs(path)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("audit verify-bundle: %v", err)).Wrap(output.ErrUsage)
	}
	f, err := os.Open(abs)
	if err != nil {
		return output.NewError("notfound.bundle",
			fmt.Sprintf("audit verify-bundle: %v", err)).Wrap(err)
	}
	defer f.Close()
	manifest, err := audit.VerifyBundle(f)
	if err != nil {
		return output.NewError("verify.bundle_invalid",
			fmt.Sprintf("audit verify-bundle: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the bundle's signature didn't verify; check that the bundle wasn't truncated/modified post-export and that the operator's public key matches the one that signed it",
			}).Wrap(err)
	}
	body := auditVerifyBundleBody{
		Path:     abs,
		Manifest: manifest,
		format:   format,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type auditVerifyBundleBody struct {
	Path     string                `json:"path"`
	Manifest *audit.BundleManifest `json:"manifest"`
	format   string
}

// MarshalJSON emits only the externally-visible fields so the internal format
// hint does not leak into the JSON response.
func (b auditVerifyBundleBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(struct {
		Path     string                `json:"path"`
		Manifest *audit.BundleManifest `json:"manifest"`
	}{b.Path, b.Manifest})
}

// WriteText renders the verify-bundle outcome and bundle manifest as
// human-readable text to w.
func (b auditVerifyBundleBody) WriteText(w io.Writer) error {
	m := b.Manifest
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ Bundle %s verified.\n", b.Path)
	fmt.Fprintf(bw, "  Generated at: %s\n", m.GeneratedAt.Format(time.RFC3339))
	if m.Operator != "" {
		fmt.Fprintf(bw, "  Operator:     %s\n", m.Operator)
	}
	if m.SourceURL != "" {
		fmt.Fprintf(bw, "  Source:       %s\n", m.SourceURL)
	}
	if m.PublicKeyFingerprint != "" {
		fmt.Fprintf(bw, "  Signed by:    sha256:%s (ed25519)\n", m.PublicKeyFingerprint)
	}
	fmt.Fprintf(bw, "  Events:       %d\n", m.EventCount)
	if m.AnchorCount > 0 {
		fmt.Fprintf(bw, "  Anchors:      %d\n", m.AnchorCount)
	}
	fmt.Fprintf(bw, "  Window:       %s → %s\n",
		m.Since.Format(time.RFC3339), m.Until.Format(time.RFC3339))
	if m.HeadHash != "" {
		fmt.Fprintf(bw, "  Head hash:    %s (seq %d)\n", m.HeadHash, m.HeadSequence)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
