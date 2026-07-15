// list.go — 'list' CLI verb: enumerates a deployment's backups (with deleted/tombstone filters).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRealListCmd implements `list <deployment>`.
func newRealListCmd() *cobra.Command {
	var (
		repoURL        string
		includeDeleted bool
		onlyDeleted    bool
	)
	c := &cobra.Command{
		Use:   "list <deployment>",
		Short: "List backups for a deployment, newest first",
		Long: `list shows backups for a deployment, newest first by stop time.

By default only LIVE backups are surfaced — tombstoned (soft-
deleted) manifests are filtered out, matching the way ` + "`" + `restore` + "`" + `
treats them. Pass --include-deleted to show every manifest with
a tombstone marker, or --only-deleted to see ONLY the tombstoned
set (useful when looking for ` + "`" + `backup undelete` + "`" + ` candidates
before chunk-GC reclaims their data).

Tombstoned rows are tagged with [DELETED] in the text view and
have ` + "`" + `tombstoned: true` + "`" + ` in the JSON body, so machine-
readable consumers can distinguish them without parsing prefixes.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, args[0], repoURL, includeDeleted, onlyDeleted)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&includeDeleted, "include-deleted", false,
		"surface tombstoned (soft-deleted) backups alongside live ones (each row marked tombstoned=true)")
	c.Flags().BoolVar(&onlyDeleted, "only-deleted", false,
		"show ONLY tombstoned backups — undelete-candidates discovery (implies --include-deleted)")
	return c
}

func runList(cmd *cobra.Command, deployment, repoURL string, includeDeleted, onlyDeleted bool) error {
	d := DispatcherFrom(cmd)
	// --repo required-ness is declared via MarkFlagRequired; cobra
	// enforces it before RunE (translated to usage.missing_flag in Run).

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
	var summaries []backupSummary
	var skipped int
	var mismatched int // subset of skipped: failed because the signing key doesn't match
	// Pre-flight: --only-deleted is a stricter filter on top of
	// --include-deleted. We always walk the include-tombstoned
	// iterator when EITHER flag is set; the differentiation is
	// purely in what we yield to the body.
	if includeDeleted || onlyDeleted {
		for entry, err := range store.ListIncludingTombstoned(cmd.Context(), deployment, verifier) {
			if err != nil {
				skipped++
				// Mirror the live-only path: a key mismatch means the
				// backup EXISTS but was signed with a different key, so
				// the "signed with a DIFFERENT key" notice must fire
				// under --include-deleted / --only-deleted too — without
				// this the notice silently disappeared for these flags.
				if errors.Is(err, backup.ErrPublicKeyMismatch) {
					mismatched++
				}
				continue
			}
			if onlyDeleted && !entry.Tombstoned {
				continue
			}
			s := summarizeManifest(entry.Manifest)
			s.Tombstoned = entry.Tombstoned
			if entry.Tombstoned {
				// Best-effort tombstone-body read. A read
				// failure (corrupt marker, schema mismatch,
				// race) shouldn't break list — the row is
				// still useful with just Tombstoned=true.
				if t, terr := store.ReadTombstone(cmd.Context(), deployment, entry.Manifest.BackupID); terr == nil && t != nil {
					ts := t.TombstonedAt
					s.DeletedAt = &ts
					s.DeleteReason = t.Reason
					s.DeletePolicy = t.Policy
				}
			}
			summaries = append(summaries, s)
		}
	} else {
		for m, err := range store.List(cmd.Context(), deployment, verifier) {
			if err != nil {
				skipped++
				if errors.Is(err, backup.ErrPublicKeyMismatch) {
					mismatched++
				}
				continue
			}
			summaries = append(summaries, summarizeManifest(m))
		}
	}
	// Newest first by StoppedAt.
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].StoppedAt.After(summaries[j].StoppedAt)
	})
	// A nil slice marshals as JSON null, and `"backups": null` breaks
	// every consumer iterating `.result.backups[]` — precisely on the
	// empty-deployment case scripts probe first. Always emit [].
	if summaries == nil {
		summaries = []backupSummary{}
	}

	body := listBody{
		Deployment:          deployment,
		Count:               len(summaries),
		Skipped:             skipped,
		SignatureMismatches: mismatched,
		IncludeDeleted:      includeDeleted || onlyDeleted,
		OnlyDeleted:         onlyDeleted,
		Backups:             summaries,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// backupSummary is the per-row shape `list` and `status` share.
//
// Tombstoned + DeletedAt + DeleteReason + DeletePolicy are set
// only when the row was surfaced via --include-deleted /
// --only-deleted; all four are omitempty in JSON so the default
// `list` body shape stays exactly as before. Additive only —
// 24-month JSON compatibility holds.
type backupSummary struct {
	BackupID         string     `json:"backup_id"`
	Type             string     `json:"type"`
	PGVersion        int        `json:"pg_version"`
	StartedAt        time.Time  `json:"started_at"`
	StoppedAt        time.Time  `json:"stopped_at"`
	DurationMS       int64      `json:"duration_ms"`
	StopLSN          string     `json:"stop_lsn"`
	Timeline         uint32     `json:"timeline"`
	FileCount        int        `json:"file_count"`
	LogicalBytes     int64      `json:"logical_bytes"`
	UniqueChunkCount int        `json:"unique_chunk_count"`
	UniqueChunkBytes int64      `json:"unique_chunk_bytes"`
	Tombstoned       bool       `json:"tombstoned,omitempty"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	DeleteReason     string     `json:"delete_reason,omitempty"`
	DeletePolicy     string     `json:"delete_policy,omitempty"`
}

// summarizeManifest collapses a Manifest into the per-row summary shape.
// Logical bytes and unique-chunk metrics are computed by walking Files.
func summarizeManifest(m *backup.Manifest) backupSummary {
	s := backupSummary{
		BackupID:   m.BackupID,
		Type:       string(m.Type),
		PGVersion:  m.PGVersion,
		StartedAt:  m.StartedAt,
		StoppedAt:  m.StoppedAt,
		DurationMS: m.StoppedAt.Sub(m.StartedAt).Milliseconds(),
		StopLSN:    m.StopLSN,
		Timeline:   m.Timeline,
		FileCount:  len(m.Files),
	}
	uniqueBytes := map[repo.Hash]int64{}
	for _, f := range m.Files {
		s.LogicalBytes += f.Size
		for _, c := range f.Chunks {
			uniqueBytes[c.Hash] = c.Len
		}
	}
	s.UniqueChunkCount = len(uniqueBytes)
	for _, sz := range uniqueBytes {
		s.UniqueChunkBytes += sz
	}
	return s
}

type listBody struct {
	Deployment string `json:"deployment"`
	Count      int    `json:"count"`
	Skipped    int    `json:"skipped,omitempty"`
	// SignatureMismatches is the subset of Skipped whose manifests failed
	// with ErrPublicKeyMismatch — i.e. real backups orphaned because the
	// signing key was rotated or lost. Surfaced loudly so "No backups" is
	// never mistaken for "your data is gone" (#103/#104).
	SignatureMismatches int             `json:"signature_mismatches,omitempty"`
	IncludeDeleted      bool            `json:"include_deleted,omitempty"`
	OnlyDeleted         bool            `json:"only_deleted,omitempty"`
	Backups             []backupSummary `json:"backups"`
}

// writeSignatureMismatchNotice prints the actionable warning shown whenever
// some of a deployment's backups failed signature verification with a key
// mismatch — they exist but can't be listed/restored until the original
// signing key is back in the keyring.
func writeSignatureMismatchNotice(w io.Writer, n int) {
	fmt.Fprintf(w, "\n⚠  %d backup(s) exist but FAILED signature verification — they were signed with a\n", n)
	fmt.Fprintf(w, "   DIFFERENT key than the current keyring holds (the signing key was likely rotated\n")
	fmt.Fprintf(w, "   or lost — e.g. an ephemeral/non-persistent keyring). They are NOT gone: restore the\n")
	fmt.Fprintf(w, "   original key to the keyring and they reappear.\n")
	fmt.Fprintf(w, "   → diagnose: pg_hardstorage doctor   ·   pg_hardstorage repo check --repo <url>\n")
}

// WriteText renders a tabular view of the backups. Tombstoned rows
// are tagged [DELETED] in the TYPE column when --include-deleted
// is in effect.
func (b listBody) WriteText(w io.Writer) error {
	if len(b.Backups) == 0 {
		// A key mismatch means backups DO exist — don't let the bare
		// "No backups" line imply data loss. Lead with the real cause.
		if b.SignatureMismatches > 0 {
			fmt.Fprintf(w, "No LISTABLE backups for %q.\n", b.Deployment)
			writeSignatureMismatchNotice(w, b.SignatureMismatches)
			if other := b.Skipped - b.SignatureMismatches; other > 0 {
				fmt.Fprintf(w, "(%d further manifest(s) failed verification and were skipped)\n", other)
			}
			return nil
		}
		switch {
		case b.OnlyDeleted:
			fmt.Fprintf(w, "No deleted backups for %q (nothing to undelete).\n", b.Deployment)
		default:
			fmt.Fprintf(w, "No backups for %q.\n", b.Deployment)
		}
		if b.Skipped > 0 {
			fmt.Fprintf(w, "(%d manifest(s) failed verification and were skipped)\n", b.Skipped)
		}
		return nil
	}
	bw := &strings.Builder{}
	header := fmt.Sprintf("Backups for %s (%d)", b.Deployment, b.Count)
	switch {
	case b.OnlyDeleted:
		header += " — deleted only"
	case b.IncludeDeleted:
		header += " — including deleted"
	}
	fmt.Fprintf(bw, "%s:\n", header)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  BACKUP ID\tTYPE\tWHEN\tFILES\tSIZE\tDEDUP\tDURATION")
	for _, r := range b.Backups {
		dedup := "-"
		if r.UniqueChunkBytes > 0 && r.LogicalBytes > 0 {
			dedup = fmt.Sprintf("%.2fx", float64(r.LogicalBytes)/float64(r.UniqueChunkBytes))
		}
		typeCol := r.Type
		if r.Tombstoned {
			typeCol = r.Type + " [DELETED]"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\t%d ms\n",
			r.BackupID, typeCol, r.StoppedAt.Format("2006-01-02 15:04"),
			r.FileCount, humanBytes(r.LogicalBytes), dedup, r.DurationMS)
		// Tombstone metadata on a continuation line so the
		// table stays narrow but the operator sees who/why/when.
		if r.Tombstoned && r.DeletedAt != nil {
			tag := r.DeletePolicy
			if tag == "" {
				tag = "policy?"
			}
			line := fmt.Sprintf("    └─ deleted %s (%s)",
				r.DeletedAt.Format("2006-01-02 15:04 MST"), tag)
			if r.DeleteReason != "" {
				line += ": " + r.DeleteReason
			}
			fmt.Fprintln(tw, line+"\t\t\t\t\t\t")
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if b.SignatureMismatches > 0 {
		writeSignatureMismatchNotice(bw, b.SignatureMismatches)
		if other := b.Skipped - b.SignatureMismatches; other > 0 {
			fmt.Fprintf(bw, "(%d further manifest(s) failed verification; run `pg_hardstorage repo check`)\n", other)
		}
	} else if b.Skipped > 0 {
		fmt.Fprintf(bw, "(%d manifest(s) failed verification; run `pg_hardstorage repo check` to investigate)\n", b.Skipped)
	}
	if b.IncludeDeleted {
		fmt.Fprintf(bw, "Tip: pg_hardstorage backup undelete %s <backup-id> resurrects a deleted backup before chunk-GC reclaims its data.\n",
			b.Deployment)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// loadVerifier resolves the keyring path and returns the Verifier
// half of the keypair. Used by every read-side CLI command (list,
// show, status, restore).
func loadVerifier() (*backup.Verifier, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("internal", err.Error()).Wrap(err)
	}
	_, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return nil, output.NewError("internal",
			fmt.Sprintf("read commands: signing key: %v", err)).Wrap(err)
	}
	return verifier, nil
}

// openRepo opens a repository at url, returning its parsed metadata
// and the StoragePlugin. Caller must Close the StoragePlugin.
//
// Errors are mapped to the same structured codes other commands use:
// notfound.repo, usage.unknown_scheme, etc.
func openRepo(ctx context.Context, url string) (*repo.Metadata, storage.StoragePlugin, error) {
	meta, sp, err := repo.Open(ctx, url)
	if err != nil {
		if errors.Is(err, repo.ErrNotARepo) {
			return nil, nil, output.NewError("notfound.repo",
				fmt.Sprintf("no pg_hardstorage repository at %s", url)).Wrap(err)
		}
		if errors.Is(err, storage.ErrUnknownScheme) {
			return nil, nil, output.NewError("usage.unknown_scheme",
				err.Error()).Wrap(output.ErrUsage)
		}
		return nil, nil, fmt.Errorf("open repo: %w", err)
	}
	return meta, sp, nil
}

// assertRepoWritable returns a structured error if the repo at sp is
// in read-only mode. Mutating commands (backup, wal stream, wal push,
// repo gc, kms rotate/shred, rotate) call this immediately after
// openRepo so a `set-mode read-only` flip surfaces at the top of the
// log instead of mid-operation.
//
// op is a short label ("repo gc", "wal stream", ...) embedded in the
// error message so the operator sees which command refused.
func assertRepoWritable(ctx context.Context, sp storage.StoragePlugin, op string) error {
	if err := repo.AssertWritable(ctx, sp); err != nil {
		if errors.Is(err, repo.ErrReadOnly) {
			return output.NewError("conflict.repo_read_only",
				fmt.Sprintf("%s: repository is in read-only mode", op)).
				WithSuggestion(&output.Suggestion{
					Human:   "flip the repository back to read-write if you really want this operation",
					Command: "pg_hardstorage repo set-mode <url> read-write",
				}).Wrap(err)
		}
		return output.NewError("repo.mode_check_failed",
			fmt.Sprintf("%s: read repo mode: %v", op, err)).Wrap(err)
	}
	return nil
}
