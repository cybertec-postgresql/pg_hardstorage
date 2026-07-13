// partial.go — CLI surface for partial (per-table) backup inspection and restore.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newRealPartialCmd is the v0.1 partial-restore command. It ships
// `partial inspect` (a read-only reporter that shows what a partial
// restore would touch) and a `partial restore` that surfaces a
// structured "not yet implemented" error with the actionable
// workaround.
//
// The honest reasoning: real partial restore needs:
//
//   - A `pg_class.relfilenode` lookup against the live source DB so
//     we know which heap files in PGDATA correspond to the named
//     tables.
//   - A sandbox PG to extract from (or direct heap-file decoding,
//     which requires reimplementing pg_filedump).
//   - `pg_dump --tables=...` invocation and SQL emission.
//
// All three land alongside the verifier subsystem (the
// sandbox is the same code). For v0.1 we ship the CLI shape so
// scripts written against `partial inspect` continue to work
// without flag-syntax churn when the actual extraction lands.
func newRealPartialCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "partial <inspect|restore>",
		Short: "Table-level inspection and restore",
		Long: `Partial / table-level operations.

partial inspect: given a backup and a list of tables, list the
manifest entries (heap files) that contain the table data. This
answers the operator's "would my partial restore work?" question
before the extraction path lands.

partial restore — the actual table extraction into a running DB —
ships alongside the sandbox-PG verifier. The fallback documented
in the structured error is: full restore into a staging directory
+ pg_dump --table=...`,
	}
	c.AddCommand(newPartialInspectCmd())
	c.AddCommand(newPartialRestoreCmd())
	c.AddCommand(newPartialDumpCmd())
	return c
}

func newPartialInspectCmd() *cobra.Command {
	var (
		repoURL   string
		backupID  string
		tablesRaw string
		pgConn    string
	)
	c := &cobra.Command{
		Use:   "inspect <deployment>",
		Short: "Report which manifest entries a partial restore would touch",
		Long: `Walk the named backup's manifest and project file-level statistics.

Without --tables, the output is the manifest summary: file/chunk
counts + total logical bytes — answers "how big is this backup?"

With --tables (a comma-separated list of qualified names), inspect
also cross-references the manifest to find each table's heap file:

  - --tables alone: report each table's expected heap path
    (computed from pg_relation_filepath conventions: base/<db>/<rfn>)
    matched against manifest FileEntries when possible.

  - --tables AND --pg-connection: queries the live source DB's
    pg_class for the actual relfilenode, then matches against the
    manifest. This is the path — gives the operator the
    concrete "X bytes across Y chunks" estimate for each table
    before the partial restore runs.

A failed pg_class lookup surfaces as a structured notice rather
than failing the inspect; the manifest-summary fields stay valid.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPartialInspect(cmd, partialInspectOptions{
				deployment: args[0],
				repoURL:    repoURL,
				backupID:   backupID,
				tables:     splitCommaTrim(tablesRaw),
				pgConn:     pgConn,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&backupID, "backup", "latest",
		"backup ID, or `latest` for the most recent committed backup")
	c.Flags().StringVar(&tablesRaw, "tables", "",
		"comma-separated table list (qualified, e.g. public.users)")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string — when set, looks up each --tables relfilenode in pg_class and matches against the manifest")
	return c
}

type partialInspectOptions struct {
	deployment string
	repoURL    string
	backupID   string
	tables     []string
	pgConn     string
}

func runPartialInspect(cmd *cobra.Command, opts partialInspectOptions) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	ms := backup.NewManifestStore(sp)

	// Resolve `latest` by walking the deployment's committed manifests
	// and picking the lexicographically-greatest ID (backup IDs embed
	// a UTC timestamp, so this == chronologically latest).
	id := opts.backupID
	if id == "latest" {
		latestID, err := latestBackupID(cmd, ms, opts.deployment)
		if err != nil {
			return err
		}
		id = latestID
	}

	m, err := ms.ReadAttestationless(cmd.Context(), opts.deployment, id)
	if err != nil {
		if errors.Is(err, backup.ErrTombstoned) {
			return output.NewError("notfound.backup_tombstoned",
				fmt.Sprintf("partial inspect: %s/%s is soft-deleted", opts.deployment, id)).Wrap(err)
		}
		return output.NewError("partial.inspect_failed",
			fmt.Sprintf("partial inspect: read manifest: %v", err)).Wrap(err)
	}

	body := partialInspectBody{
		Deployment: opts.deployment,
		BackupID:   m.BackupID,
		PGVersion:  m.PGVersion,
		Timeline:   m.Timeline,
		FileCount:  len(m.Files),
		Tables:     opts.tables,
	}
	for _, f := range m.Files {
		body.ChunkCount += len(f.Chunks)
		body.LogicalBytes += f.Size
	}

	// Per-table mapping when --tables is set. With --pg-connection,
	// we resolve each table's relfilenode against pg_class and
	// match the resulting path against the manifest's FileEntries.
	// Without --pg-connection, we leave the lookup blank but still
	// surface what was requested so the operator sees the gap.
	if len(opts.tables) > 0 && opts.pgConn != "" {
		rfns, lerr := partial.LookupRelfilenodes(cmd.Context(), partial.LookupOptions{
			PGConnString: opts.pgConn,
			Tables:       opts.tables,
		})
		if lerr != nil {
			body.LookupError = lerr.Error()
		} else {
			body.TableMappings = matchManifestPaths(m, rfns)
		}
	} else if len(opts.tables) > 0 {
		body.Note = "pass --pg-connection <url> to map each table to its manifest heap file via pg_class"
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// matchManifestPaths cross-references each Relfilenode against the
// manifest's FileEntries: returns one TableMapping per requested
// table with the heap file (and TOAST file, when present) plus the
// total chunk count + logical bytes.
//
// The match is by exact path equality. PG paths are
// "base/<dbnode>/<relfilenode>" with optional ".N" segment suffixes
// for relations larger than 1 GiB; we collect every segment whose
// path starts with the relfilenode prefix.
func matchManifestPaths(m *backup.Manifest, rfns []partial.Relfilenode) []partialTableMapping {
	out := make([]partialTableMapping, 0, len(rfns))
	for _, rfn := range rfns {
		entry := partialTableMapping{
			Qualified:   rfn.Qualified,
			OID:         rfn.OID,
			Relfilenode: rfn.Relfilenode,
			HeapPath:    rfn.Path,
			ToastPath:   rfn.ToastPath,
			NotFound:    rfn.NotFound,
		}
		if !rfn.NotFound {
			matchPaths(&entry.HeapBytes, &entry.HeapChunks, &entry.HeapSegments, m, rfn.Path)
			if rfn.ToastPath != "" {
				matchPaths(&entry.ToastBytes, &entry.ToastChunks, &entry.ToastSegments, m, rfn.ToastPath)
			}
		}
		out = append(out, entry)
	}
	return out
}

// matchPaths walks every FileEntry whose path starts with prefix
// (so segments base/16384/24576, base/16384/24576.1, ... all match),
// and accumulates totals into the supplied counters.
func matchPaths(bytes *int64, chunks, segments *int, m *backup.Manifest, prefix string) {
	for _, f := range m.Files {
		if f.Path != prefix && !strings.HasPrefix(f.Path, prefix+".") {
			continue
		}
		*bytes += f.Size
		*chunks += len(f.Chunks)
		*segments++
	}
}

// latestBackupID returns the lex-greatest non-tombstoned manifest ID
// for the deployment. Returns a structured notfound error when there
// are no committed backups.
func latestBackupID(cmd *cobra.Command, ms *backup.ManifestStore, deployment string) (string, error) {
	// ManifestStore.List requires a verifier; passing nil rejects
	// every manifest as "manifest: nil verifier", so `latest`
	// stays empty and the caller misreports the deployment as
	// having no committed backups even when it has plenty.  Use
	// the no-verify variant — `partial inspect` is a read-only
	// preview, not a path that acts on the manifest's claims.
	// "latest" means newest by the manifest's StoppedAt time, NOT the
	// lexicographic max of BackupID. An ID is "<dep>.<type>.<ts>.<seq>"
	// and the type segment sorts "full" < "incremental_lsn" <
	// "snapshot" before the timestamp, so a plain max(BackupID) would
	// pick an older incremental/snapshot over a newer full — dumping
	// the wrong backup. Tie-break by BackupID for determinism (matches
	// restore.ResolveLatest and the retention policy).
	var latest string
	var latestAt time.Time
	for m, err := range ms.ListAttestationless(cmd.Context(), deployment) {
		if err != nil {
			continue
		}
		if m == nil {
			continue
		}
		if latest == "" || m.StoppedAt.After(latestAt) ||
			(m.StoppedAt.Equal(latestAt) && m.BackupID > latest) {
			latest = m.BackupID
			latestAt = m.StoppedAt
		}
	}
	if latest == "" {
		return "", output.NewError("notfound.backup",
			fmt.Sprintf("partial: no committed backups for deployment %q", deployment))
	}
	return latest, nil
}

// splitCommaTrim trims whitespace and skips empties.
func splitCommaTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

type partialInspectBody struct {
	Deployment    string                `json:"deployment"`
	BackupID      string                `json:"backup_id"`
	PGVersion     int                   `json:"pg_version"`
	Timeline      uint32                `json:"timeline"`
	FileCount     int                   `json:"file_count"`
	ChunkCount    int                   `json:"chunk_count"`
	LogicalBytes  int64                 `json:"logical_bytes"`
	Tables        []string              `json:"requested_tables,omitempty"`
	TableMappings []partialTableMapping `json:"table_mappings,omitempty"`
	LookupError   string                `json:"lookup_error,omitempty"`
	Note          string                `json:"note,omitempty"`
}

// partialTableMapping is the per-table view rendered when
// --pg-connection lets us cross-reference pg_class against the
// manifest. HeapBytes / HeapChunks / HeapSegments are the totals for
// the table's heap; ToastBytes etc. cover the TOAST table (when
// present). NotFound flips when the table didn't exist in pg_class
// — useful as a typo-detection signal during inspect.
type partialTableMapping struct {
	Qualified     string `json:"qualified"`
	OID           uint32 `json:"oid,omitempty"`
	Relfilenode   uint32 `json:"relfilenode,omitempty"`
	HeapPath      string `json:"heap_path,omitempty"`
	HeapBytes     int64  `json:"heap_bytes,omitempty"`
	HeapChunks    int    `json:"heap_chunks,omitempty"`
	HeapSegments  int    `json:"heap_segments,omitempty"`
	ToastPath     string `json:"toast_path,omitempty"`
	ToastBytes    int64  `json:"toast_bytes,omitempty"`
	ToastChunks   int    `json:"toast_chunks,omitempty"`
	ToastSegments int    `json:"toast_segments,omitempty"`
	NotFound      bool   `json:"not_found,omitempty"`
}

// WriteText renders the per-relation inspect result — heap and toast file
// rollups — as human-readable text to w.
func (b partialInspectBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "partial inspect — %s/%s\n", b.Deployment, b.BackupID)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  PG version:\t%d\n", b.PGVersion)
	fmt.Fprintf(tw, "  Timeline:\t%d\n", b.Timeline)
	fmt.Fprintf(tw, "  Files:\t%d\n", b.FileCount)
	fmt.Fprintf(tw, "  Chunks:\t%d\n", b.ChunkCount)
	fmt.Fprintf(tw, "  Logical bytes:\t%s\n", humanBytes(b.LogicalBytes))
	if len(b.Tables) > 0 {
		fmt.Fprintf(tw, "  Tables (requested):\t%s\n", strings.Join(b.Tables, ", "))
	}
	if b.LookupError != "" {
		fmt.Fprintf(tw, "  Lookup error:\t%s\n", b.LookupError)
	}
	if b.Note != "" {
		fmt.Fprintf(tw, "  Note:\t%s\n", b.Note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(b.TableMappings) > 0 {
		bw.WriteString("\n  Table mappings:\n")
		mw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(mw, "    TABLE\tHEAP PATH\tBYTES\tCHUNKS\tTOAST")
		for _, m := range b.TableMappings {
			if m.NotFound {
				fmt.Fprintf(mw, "    %s\t(not in pg_class)\t-\t-\t-\n", m.Qualified)
				continue
			}
			toastInfo := "-"
			if m.ToastPath != "" {
				toastInfo = fmt.Sprintf("%s (%s, %d chunk(s))", m.ToastPath, humanBytes(m.ToastBytes), m.ToastChunks)
			}
			fmt.Fprintf(mw, "    %s\t%s\t%s\t%d\t%s\n",
				m.Qualified, m.HeapPath, humanBytes(m.HeapBytes), m.HeapChunks, toastInfo)
		}
		_ = mw.Flush()
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// newPartialRestoreCmd is the+ selective-extraction path.
//
// What it does today: extract ONLY the heap files (and TOAST + _vm
// + _fsm + segment siblings) for the named tables from the backup
// at --repo into --target. The result is a partial PGDATA-shaped
// directory containing just the requested relations. The operator
// can then `pg_dump --table=...` against a PG instance running on
// that directory (or copy the files into a running cluster's
// matching OID locations — advanced).
//
// What it does NOT do today: spin up a sandbox PG to run pg_dump
// for you. That step lands when the verifier sandbox lifecycle
// extends to "borrow my data dir" mode; for now the operator's
// pg_dump is one shell command away.
//
// Two table-resolution paths:
//
//	--pg-connection <dsn>   query pg_class on a live source DB
//	--relfilenode-map <path> JSON file with {qualified-name: relfilenode-info}
//
// Mutually exclusive. The JSON map shape is the same one that
// `partial inspect --pg-connection ... -o json` emits, so the
// air-gapped flow is: run inspect once with PG access, save the
// output, run restore offline with the saved map.
func newPartialRestoreCmd() *cobra.Command {
	var (
		repoURL            string
		backupID           string
		tables             string
		target             string
		pgConn             string
		relfilenodeMapPath string
		allowOverwrite     bool
		kmsConfig          map[string]string
	)
	c := &cobra.Command{
		Use:   "restore <deployment>",
		Short: "Extract specific tables' files from a backup into --target",
		Long: `Selective restore: writes ONLY the heap files (plus TOAST,
visibility-map, and segment siblings) for the requested tables into
--target. Other relations, indexes, control files, and WAL are
skipped.

Resolution: pass --pg-connection to query pg_class on a live source
DB, OR --relfilenode-map <path> to a JSON file (the same shape
` + "`partial inspect -o json`" + ` emits).

The output is a partial PGDATA layout. Run pg_dump against a
PG instance pointed at --target (or copy files into a matching
running cluster) to get SQL out — that automation is a
follow-up.

Encryption: encrypted backups are not yet supported by partial
restore. Use full ` + "`pg_hardstorage restore`" + ` + pg_dump.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPartialRestore(cmd, args[0], partialRestoreFlags{
				repoURL:            repoURL,
				backupID:           backupID,
				tables:             tables,
				target:             target,
				pgConn:             pgConn,
				relfilenodeMapPath: relfilenodeMapPath,
				allowOverwrite:     allowOverwrite,
				kmsConfig:          kmsConfig,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&backupID, "backup", "latest",
		"backup ID, or `latest` for the most recent committed backup")
	c.Flags().StringVar(&tables, "tables", "",
		"comma-separated qualified tables (required, e.g. public.users,public.events)")
	c.Flags().StringVar(&target, "target", "",
		"target directory where the selected files materialise (required)")
	_ = c.MarkFlagRequired("target")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq DSN for the live source DB to look up relfilenodes")
	c.Flags().StringVar(&relfilenodeMapPath, "relfilenode-map", "",
		"path to a JSON map of qualified-name → relfilenode (alternative to --pg-connection)")
	c.Flags().StringToStringVar(&kmsConfig, "kms-config", nil,
		"cloud KMS provider config for a cloud-KMS-encrypted backup (e.g. region=eu-central-1,endpoint=...); empty uses ambient credentials")
	c.Flags().BoolVar(&allowOverwrite, "force", false,
		"allow extracting into a non-empty --target")
	return c
}

type partialRestoreFlags struct {
	repoURL            string
	backupID           string
	tables             string
	target             string
	pgConn             string
	relfilenodeMapPath string
	allowOverwrite     bool
	kmsConfig          map[string]string
}

func runPartialRestore(cmd *cobra.Command, deployment string, f partialRestoreFlags) error {
	d := DispatcherFrom(cmd)
	tlist := splitCommaTrim(f.tables)
	if len(tlist) == 0 {
		return output.NewError("usage.missing_flag",
			"partial restore: --tables is required (comma-separated, qualified)").Wrap(output.ErrUsage)
	}
	if f.pgConn == "" && f.relfilenodeMapPath == "" {
		return output.NewError("usage.missing_flag",
			"partial restore: pass --pg-connection OR --relfilenode-map for table resolution").Wrap(output.ErrUsage)
	}
	if f.pgConn != "" && f.relfilenodeMapPath != "" {
		return output.NewError("usage.bad_flag",
			"partial restore: --pg-connection and --relfilenode-map are mutually exclusive").Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}

	// Resolve the keyring path so we can wire the KEK resolver for
	// encrypted backups. It's a no-op for unencrypted backups
	// (partial.Restore only consults it when m.Encryption != nil)
	// and the right resolver for encrypted ones.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	// Resolve --backup latest if needed (mirrors what restore does).
	backupID := f.backupID
	if backupID == "" || backupID == "latest" {
		_, sp, err := openRepo(cmd.Context(), f.repoURL)
		if err != nil {
			return err
		}
		store := backup.NewManifestStore(sp)
		latest, err := resolveLatestBackupID(cmd.Context(), store, deployment, verifier)
		sp.Close()
		if err != nil {
			return output.NewError("notfound.backup",
				fmt.Sprintf("partial restore: resolve latest: %v", err)).Wrap(err)
		}
		backupID = latest
	}

	// Build the relfilenode map (from JSON file when given).
	var rfnMap map[string]partial.Relfilenode
	if f.relfilenodeMapPath != "" {
		m, err := loadRelfilenodeMap(f.relfilenodeMapPath)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("partial restore: --relfilenode-map: %v", err)).Wrap(output.ErrUsage)
		}
		rfnMap = m
	}

	res, err := partial.Restore(cmd.Context(), partial.RestoreOptions{
		RepoURL:        f.repoURL,
		Deployment:     deployment,
		BackupID:       backupID,
		Verifier:       verifier,
		Tables:         tlist,
		PGConnString:   f.pgConn,
		RelfilenodeMap: rfnMap,
		TargetDir:      f.target,
		AllowOverwrite: f.allowOverwrite,
		// Wire the KEK resolver unconditionally. partial.Restore
		// short-circuits to plain CAS when m.Encryption is nil; for
		// encrypted backups this resolves the manifest's KEKRef
		// against the local keyring (the same mechanism `restore`
		// uses).
		KEKForRef: keystore.KEKResolver(p.Keyring.Value),
		UnwrapDEK: keystore.DEKResolver(p.Keyring.Value, stringMapToAny(f.kmsConfig)),
	})
	if err != nil {
		return output.NewError("partial.restore_failed",
			fmt.Sprintf("partial restore: %v", err)).Wrap(err)
	}

	body := partialRestoreBody{RestoreResult: *res}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// resolveLatestBackupID walks the manifest store and returns the
// latest backup ID by StoppedAt for the deployment.
func resolveLatestBackupID(ctx context.Context, store *backup.ManifestStore, deployment string, verifier *backup.Verifier) (string, error) {
	var latestID string
	var latestTS time.Time
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			continue
		}
		if m.StoppedAt.After(latestTS) {
			latestTS = m.StoppedAt
			latestID = m.BackupID
		}
	}
	if latestID == "" {
		return "", fmt.Errorf("no backups for deployment %q", deployment)
	}
	return latestID, nil
}

// loadRelfilenodeMap reads a JSON file and returns it as the
// expected map. The on-disk shape mirrors `partial inspect`'s
// table_mappings array — we accept either:
//   - A flat object: { "public.users": {schema, table, path, ...} }
//   - An array of Relfilenode objects (with Qualified set).
func loadRelfilenodeMap(path string) (map[string]partial.Relfilenode, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("file %s is empty", path)
	}
	if body[0] == '[' {
		var arr []partial.Relfilenode
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("decode %s as array: %w", path, err)
		}
		out := make(map[string]partial.Relfilenode, len(arr))
		for _, r := range arr {
			if r.Qualified == "" {
				return nil, fmt.Errorf("entry missing 'qualified' field: %+v", r)
			}
			out[r.Qualified] = r
		}
		return out, nil
	}
	var m map[string]partial.Relfilenode
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode %s as object: %w", path, err)
	}
	// Make sure each entry's Qualified matches its key (helps
	// downstream code that doesn't re-set it).
	for k, v := range m {
		if v.Qualified == "" {
			v.Qualified = k
			m[k] = v
		}
	}
	return m, nil
}

// partialRestoreBody renders the structured result.
type partialRestoreBody struct {
	partial.RestoreResult
}

// WriteText renders the partial-restore outcome — tables, file and byte
// counts — as human-readable text to w.
func (b partialRestoreBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "partial restore — %s/%s → %s\n",
		b.Deployment, b.BackupID, b.TargetDir)
	fmt.Fprintf(bw, "  Tables:        %s\n", strings.Join(b.Tables, ", "))
	fmt.Fprintf(bw, "  Files written: %d\n", b.FilesWritten)
	fmt.Fprintf(bw, "  Bytes written: %s\n", humanBytes(b.BytesWritten))
	fmt.Fprintf(bw, "  Duration:      %d ms\n", b.DurationMS)
	if len(b.NotFound) > 0 {
		fmt.Fprintf(bw, "  Not found:     %s\n", strings.Join(b.NotFound, ", "))
	}
	if len(b.Mappings) > 0 {
		fmt.Fprintln(bw, "  Mappings:")
		mw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(mw, "    TABLE\tHEAP PATH\tBYTES\tFILES\tTOAST")
		for _, m := range b.Mappings {
			heapPath := m.HeapPath
			if heapPath == "" {
				heapPath = "(not found)"
			}
			toast := "-"
			if m.ToastPath != "" {
				toast = fmt.Sprintf("%s (%s, %d files)",
					m.ToastPath, humanBytes(m.ToastBytes), len(m.ToastFiles))
			}
			fmt.Fprintf(mw, "    %s\t%s\t%s\t%d\t%s\n",
				m.Qualified, heapPath, humanBytes(m.HeapBytes),
				len(m.HeapFiles), toast)
		}
		_ = mw.Flush()
	}
	fmt.Fprintln(bw, "\n  Next step: run pg_dump against a PG instance pointed at --target,")
	fmt.Fprintln(bw, "  e.g.  pg_dump --data-only --table=public.users -d postgres")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
