// show.go — 'show' CLI verb: full manifest detail (files/sizes/dedup/signature/tablespaces).
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// newManifestCmd implements `pg_hardstorage manifest`, the documented
// home for manifest inspection.
//
// Issue #94: the repo-layout docs referenced `pg_hardstorage manifest
// show …` but only a top-level `show` existed, so the command in the
// docs failed with "unknown command \"manifest\"".  `manifest show` is
// the same operation as the top-level `show`; both are kept so existing
// scripts and the documented form work.  The subcommand is the exact
// command `show` returns, so its flags, args, and output stay in lock-
// step with `show` automatically.
func newManifestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "manifest",
		Short: "Inspect backup manifests",
		Long: `Inspect the per-backup manifest — the JSON record (files, sizes,
dedup, signature fingerprint, tablespaces, PG metadata) stored under
the repository's manifests/ prefix.

` + "`manifest show`" + ` is the same operation as the top-level
` + "`show`" + ` command.`,
	}
	show := newRealShowCmd()
	show.Short = "Show a single backup's full manifest"
	c.AddCommand(show)
	return c
}

// newRealShowCmd implements `show <deployment> <backup-id>`.
func newRealShowCmd() *cobra.Command {
	var (
		repoURL        string
		includeDeleted bool
	)
	c := &cobra.Command{
		Use:   "show <deployment> <backup-id>",
		Short: "Show details of a single backup",
		Long: `show prints the full manifest of a single backup —
file count, sizes, dedup ratio, signature fingerprint, tablespaces,
and PG metadata.

By default a tombstoned (soft-deleted) manifest is treated as
not-found, matching ` + "`" + `restore` + "`" + ` semantics. Pass --include-deleted
to inspect a tombstoned manifest's body alongside its tombstone
metadata (deleted_at, delete_reason, delete_policy) — useful
when triaging undelete candidates discovered via
` + "`" + `list --only-deleted` + "`" + `.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0], args[1], repoURL, includeDeleted)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&includeDeleted, "include-deleted", false,
		"surface tombstoned (soft-deleted) manifests too (body marked tombstoned=true with deleted_at/reason/policy)")
	return c
}

func runShow(cmd *cobra.Command, deployment, backupID, repoURL string, includeDeleted bool) error {
	d := DispatcherFrom(cmd)

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	var (
		m       *backup.Manifest
		dead    bool
		readErr error
	)
	if includeDeleted {
		m, dead, readErr = store.ReadIncludingTombstoned(cmd.Context(), deployment, backupID, verifier)
	} else {
		m, readErr = store.Read(cmd.Context(), deployment, backupID, verifier)
	}
	if readErr != nil {
		if errors.Is(readErr, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("show: backup %q for deployment %q not found",
					backupID, deployment)).
				WithSuggestion(&output.Suggestion{
					Human: "list available backups with `pg_hardstorage list " + deployment + "`",
				}).Wrap(readErr)
		}
		// Tombstoned-without-include-deleted maps to a hint that
		// points at the recovery path rather than a generic
		// not-found. Operators chasing "where did my backup go?"
		// land on this and immediately find the fix.
		if errors.Is(readErr, backup.ErrTombstoned) {
			return output.NewError("notfound.backup_tombstoned",
				fmt.Sprintf("show: backup %s/%s is tombstoned (soft-deleted)",
					deployment, backupID)).
				WithSuggestion(&output.Suggestion{
					Human:   "pass --include-deleted to inspect the tombstoned manifest body, or `backup undelete` to resurrect it (before chunk-GC reclaims its data).",
					Command: "pg_hardstorage backup undelete " + deployment + " " + backupID + " --repo " + repoURL,
				}).Wrap(readErr)
		}
		return fmt.Errorf("show: read manifest: %w", readErr)
	}

	body := buildShowBody(m)
	if dead {
		body.Tombstoned = true
		// Best-effort tombstone-body read; same posture as the
		// list path. A corrupt/missing marker doesn't break
		// show — operator still gets the manifest body.
		if t, terr := store.ReadTombstone(cmd.Context(), deployment, backupID); terr == nil && t != nil {
			ts := t.TombstonedAt
			body.DeletedAt = &ts
			body.DeleteReason = t.Reason
			body.DeletePolicy = t.Policy
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// showBody embeds the full Manifest plus computed aggregates the
// caller is likely to want at a glance. Tombstoned + DeletedAt +
// DeleteReason + DeletePolicy are all omitempty so the default
// `show` body shape stays byte-identical to+; only
// `--include-deleted` populates them. Schema-additive — 24-month
// JSON-compat commitment holds.
type showBody struct {
	*backup.Manifest

	LogicalBytes     int64      `json:"logical_bytes"`
	UniqueChunkCount int        `json:"unique_chunk_count"`
	UniqueChunkBytes int64      `json:"unique_chunk_bytes"`
	DurationMS       int64      `json:"duration_ms"`
	PublicKeyFP      string     `json:"attestation_public_key_fingerprint,omitempty"`
	Tombstoned       bool       `json:"tombstoned,omitempty"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	DeleteReason     string     `json:"delete_reason,omitempty"`
	DeletePolicy     string     `json:"delete_policy,omitempty"`
}

func buildShowBody(m *backup.Manifest) showBody {
	body := showBody{
		Manifest:   m,
		DurationMS: m.StoppedAt.Sub(m.StartedAt).Milliseconds(),
	}
	uniqueBytes := map[[32]byte]int64{}
	for _, f := range m.Files {
		body.LogicalBytes += f.Size
		for _, c := range f.Chunks {
			uniqueBytes[[32]byte(c.Hash)] = c.Len
		}
	}
	body.UniqueChunkCount = len(uniqueBytes)
	for _, sz := range uniqueBytes {
		body.UniqueChunkBytes += sz
	}
	if m.Attestation != nil {
		body.PublicKeyFP = fingerprintPubKeyPEM(m.Attestation.PublicKey)
	}
	return body
}

// fingerprintPubKeyPEM returns a short SHA-256 hex of the PEM bytes.
// Operators use this to quickly compare which keypair signed which
// backups across a fleet.
func fingerprintPubKeyPEM(pem string) string {
	if pem == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(pem))
	return hex.EncodeToString(sum[:8]) // first 8 bytes = 16 hex chars
}

// WriteText renders a friendly per-backup overview.
func (b showBody) WriteText(w io.Writer) error {
	if b.Manifest == nil {
		return nil
	}
	bw := &strings.Builder{}
	header := fmt.Sprintf("Backup %s", b.BackupID)
	if b.Tombstoned {
		header += " [DELETED]"
	}
	fmt.Fprintf(bw, "%s\n", header)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "  %s\t%s\n", k, v) }

	row("Deployment", b.Deployment)
	if b.Tenant != "" && b.Tenant != "default" {
		row("Tenant", b.Tenant)
	}
	row("Type", string(b.Type))
	if b.ParentBackupID != "" {
		row("Parent", b.ParentBackupID)
	}
	row("PostgreSQL", fmt.Sprintf("%d", b.PGVersion))
	row("Cluster ID", b.SystemIdentifier)
	row("Start LSN", b.StartLSN)
	row("Stop LSN", b.StopLSN)
	row("Timeline", fmt.Sprintf("%d", b.Timeline))
	row("Started", b.StartedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	row("Stopped", b.StoppedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	row("Duration", fmt.Sprintf("%d ms", b.DurationMS))

	if b.Compression != "" {
		row("Compression", b.Compression)
	}
	row("Tablespaces", fmt.Sprintf("%d", len(b.Tablespaces)))
	for _, t := range b.Tablespaces {
		row("  oid="+fmt.Sprintf("%d", t.OID), t.Location)
	}
	row("Files", fmt.Sprintf("%d", len(b.Files)))
	row("Logical bytes", humanBytes(b.LogicalBytes))
	row("Unique chunks", fmt.Sprintf("%d (%s)", b.UniqueChunkCount, humanBytes(b.UniqueChunkBytes)))
	if b.LogicalBytes > 0 && b.UniqueChunkBytes > 0 {
		row("Dedup ratio", fmt.Sprintf("%.2fx", float64(b.LogicalBytes)/float64(b.UniqueChunkBytes)))
	}
	if b.BackupLabel != "" {
		row("backup_label", fmt.Sprintf("%d bytes", len(b.BackupLabel)))
	}
	if b.TablespaceMap != "" {
		row("tablespace_map", fmt.Sprintf("%d bytes", len(b.TablespaceMap)))
	}
	if b.Attestation != nil {
		row("Signature", b.Attestation.Scheme+" / fingerprint "+b.PublicKeyFP)
	}
	// Tombstone metadata block only appears when --include-deleted
	// surfaces a tombstoned manifest.
	if b.Tombstoned {
		row("", "") // visual gap
		row("Status", "DELETED (tombstoned)")
		if b.DeletedAt != nil {
			row("Deleted at", b.DeletedAt.UTC().Format("2006-01-02 15:04:05 MST"))
		}
		if b.DeletePolicy != "" {
			row("Delete policy", b.DeletePolicy)
		}
		if b.DeleteReason != "" {
			row("Delete reason", b.DeleteReason)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if b.Tombstoned {
		fmt.Fprintf(bw, "\nResurrect this backup before chunk-GC reclaims its data:\n  pg_hardstorage backup undelete %s %s\n",
			b.Deployment, b.BackupID)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}
