// repo_bundle.go — CLI surface for exporting and importing offline repo bundles.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// newRepoBundleCmd implements `pg_hardstorage repo bundle <export|import>`.
//
// The bundle is the air-gap transport for a repo — a tar file
// containing a manifest, every chunk it references, optionally
// the WAL it requires, and metadata.  It's the
// `air-gapped repo-bundle export/import` SPEC item.
//
// We deliberately keep CLI surface minimal:
//
//   - export: read-only on source repo, writes one tar.
//   - import: read+write on dst repo (idempotent, conditional puts),
//     reads one tar.
//
// Tar file goes to/from a path via --out / --in.  Operators who
// want to pipe through gzip do it themselves; we don't bake
// compression into the bundle (chunks already encode their
// storage-layer compression, and tar+stream-gzip is the
// orthodox Unix posture).
func newRepoBundleCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "bundle <export|import>",
		Short: "Air-gap export/import of repo state (manifests + chunks + WAL)",
	}
	c.AddCommand(newRepoBundleExportCmd(), newRepoBundleImportCmd())
	return c
}

func newRepoBundleExportCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		backupID   string
		outPath    string
		includeWAL bool
	)
	c := &cobra.Command{
		Use:          "export",
		Short:        "Export deployment state into an air-gap-transportable tar",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)

			_, sp, err := openRepo(cmd.Context(), repoURL)
			if err != nil {
				return err
			}
			defer sp.Close()

			f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return output.NewError("repo.bundle_export_failed",
					fmt.Sprintf("repo bundle export: open %s: %v", outPath, err)).Wrap(err)
			}
			defer f.Close()

			bm, err := bundle.Export(cmd.Context(), sp, f, bundle.ExportOptions{
				Deployment:    deployment,
				BackupID:      backupID,
				IncludeWAL:    includeWAL,
				SourceRepoURL: repoURL,
			})
			if err != nil {
				// best-effort cleanup on error so a half-tarred
				// file doesn't accumulate at the operator's path
				f.Close()
				os.Remove(outPath)
				return output.NewError("repo.bundle_export_failed",
					fmt.Sprintf("repo bundle export: %v", err)).Wrap(err)
			}
			// Flush + close BEFORE reporting success. A deferred Close
			// whose error is ignored can leave a truncated tar on disk
			// while the command still exits 0 — an operator would
			// discover the corruption only at import time. fsync the
			// bytes and surface a close-time error as a failure (and
			// remove the torn file).
			if syncErr := f.Sync(); syncErr != nil {
				f.Close()
				os.Remove(outPath)
				return output.NewError("repo.bundle_export_failed",
					fmt.Sprintf("repo bundle export: fsync %s: %v", outPath, syncErr)).Wrap(syncErr)
			}
			if closeErr := f.Close(); closeErr != nil {
				os.Remove(outPath)
				return output.NewError("repo.bundle_export_failed",
					fmt.Sprintf("repo bundle export: close %s: %v", outPath, closeErr)).Wrap(closeErr)
			}
			body := repoBundleExportBody{
				OutPath:    outPath,
				Backups:    len(bm.Backups),
				ChunkCount: bm.ChunkCount,
				ChunkBytes: bm.ChunkBytes,
				WALCount:   len(bm.WAL),
				IncludeWAL: includeWAL,
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "source repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&deployment, "deployment", "", "deployment to export (required)")
	_ = c.MarkFlagRequired("deployment")
	c.Flags().StringVar(&backupID, "backup-id", "", "single backup to export (default: every live backup for the deployment)")
	c.Flags().StringVar(&outPath, "out", "", "output path for the .tar bundle (required)")
	_ = c.MarkFlagRequired("out")
	c.Flags().BoolVar(&includeWAL, "include-wal", false, "also include WAL segments listed in each manifest's wal_required")
	return c
}

func newRepoBundleImportCmd() *cobra.Command {
	var (
		repoURL string
		inPath  string
	)
	c := &cobra.Command{
		Use:          "import",
		Short:        "Import a previously-exported bundle into a destination repo (idempotent)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			f, err := os.Open(inPath)
			if err != nil {
				return output.NewError("repo.bundle_import_failed",
					fmt.Sprintf("repo bundle import: open %s: %v", inPath, err)).Wrap(err)
			}
			defer f.Close()

			_, sp, err := openRepo(cmd.Context(), repoURL)
			if err != nil {
				return err
			}
			defer sp.Close()
			if err := assertRepoWritable(cmd.Context(), sp, "repo bundle import"); err != nil {
				return err
			}

			bm, err := bundle.Import(cmd.Context(), f, sp, bundle.ImportOptions{})
			if err != nil {
				return output.NewError("repo.bundle_import_failed",
					fmt.Sprintf("repo bundle import: %v", err)).Wrap(err)
			}
			body := repoBundleImportBody{
				InPath:     inPath,
				Backups:    len(bm.Backups),
				ChunkCount: bm.ChunkCount,
				ChunkBytes: bm.ChunkBytes,
				WALCount:   len(bm.WAL),
				SourceRepo: bm.SourceRepo,
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
	c.Flags().StringVar(&repoURL, "to", "", "destination repository URL (required)")
	_ = c.MarkFlagRequired("to")
	c.Flags().StringVar(&inPath, "in", "", "input path to a .tar bundle (required)")
	_ = c.MarkFlagRequired("in")
	return c
}

// openRepo is shared with kms_shred + others; redeclared here only
// when the package doesn't already export it.  We defer to the
// existing helper.

// helper bodies ------------------------------------------------------

type repoBundleExportBody struct {
	OutPath    string `json:"out_path"`
	Backups    int    `json:"backups"`
	ChunkCount int    `json:"chunk_count"`
	ChunkBytes int64  `json:"chunk_bytes"`
	WALCount   int    `json:"wal_count,omitempty"`
	IncludeWAL bool   `json:"include_wal"`
}

// WriteText renders the bundle-export summary as human-readable text to w.
func (b repoBundleExportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ repo bundle export\n")
	fmt.Fprintf(bw, "  Output:       %s\n", b.OutPath)
	fmt.Fprintf(bw, "  Backups:      %d\n", b.Backups)
	fmt.Fprintf(bw, "  Chunks:       %d (%d bytes)\n", b.ChunkCount, b.ChunkBytes)
	if b.IncludeWAL {
		fmt.Fprintf(bw, "  WAL segments: %d\n", b.WALCount)
	}
	fmt.Fprintf(bw, "  Note:         the bundle is signed neither stronger nor weaker than its source repo; verify with `repo bundle import` against a destination repo")
	_, err := io.WriteString(w, bw.String())
	return err
}

type repoBundleImportBody struct {
	InPath     string `json:"in_path"`
	Backups    int    `json:"backups"`
	ChunkCount int    `json:"chunk_count"`
	ChunkBytes int64  `json:"chunk_bytes"`
	WALCount   int    `json:"wal_count,omitempty"`
	SourceRepo string `json:"source_repo,omitempty"`
}

// WriteText renders the bundle-import summary as human-readable text to w.
func (b repoBundleImportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ repo bundle import\n")
	fmt.Fprintf(bw, "  Input:        %s\n", b.InPath)
	if b.SourceRepo != "" {
		fmt.Fprintf(bw, "  Source repo:  %s\n", b.SourceRepo)
	}
	fmt.Fprintf(bw, "  Backups:      %d\n", b.Backups)
	fmt.Fprintf(bw, "  Chunks:       %d (%d bytes)\n", b.ChunkCount, b.ChunkBytes)
	if b.WALCount > 0 {
		fmt.Fprintf(bw, "  WAL segments: %d\n", b.WALCount)
	}
	fmt.Fprintf(bw, "  Note:         idempotent — re-running this command on the same bundle is a no-op (PutIfNotExists)")
	_, err := io.WriteString(w, bw.String())
	return err
}

// silence unused in case `context` is dropped by future refactor
var _ = context.Background
