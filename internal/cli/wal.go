// wal.go — CLI surface for WAL segment push/fetch and auxiliary file handling.
package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// newWalCmd builds the `wal` command tree. v0.1 ships `wal stream`
// as a real working command; the other verbs (push / fetch / list /
// repair) remain stubs and follow in later slices.
//
// `wal` and its children are not implemented as a single big command
// because each sub-verb has materially different inputs and output
// shapes — splitting them keeps the Go code in step with the user-
// facing CLI tree.
func newWalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "wal",
		Short: "WAL transport: continuous streaming, push/fetch, list, repair",
		Long: `WAL transport plumbing.

The streaming subcommand connects via the PostgreSQL replication
protocol over a libpq connection — no host-level access to the database
is needed. It targets PostgreSQL you run yourself; fully-managed DBaaS
(RDS, Cloud SQL, ...) do not expose BASE_BACKUP / physical replication
and are not supported.
`,
	}
	c.AddCommand(newWalStreamCmd())
	c.AddCommand(newWalPreflightCmd())
	c.AddCommand(newWalFetchCmd())
	c.AddCommand(newWalListCmd())
	c.AddCommand(newWalAuditCmd())
	c.AddCommand(newWalPruneCmd())
	c.AddCommand(newWalRepairCmd())
	c.AddCommand(newWalPushCmd())
	c.AddCommand(newWalGapsCmd())
	c.AddCommand(newWalGapPurgeCmd())
	return c
}

// newWalPushCmd implements `pg_hardstorage wal push <deployment>
// <segment-path>`. PG invokes this from archive_command, substituting
// %p for the segment file's path:
//
//	archive_command = 'pg_hardstorage wal push db1 %p --repo s3://...'
//
// Idempotent (re-pushes a no-op via RenameIfNotExists). Refuses to
// push if the repo is in read-only mode. Refuses .history files in
// v0.1 — PG's archive_command swallows non-zero exit and retries, so
// our refusal becomes a structured-event log line and PG eventually
// gives up via archive_timeout / archive_failures.
func newWalPushCmd() *cobra.Command {
	var (
		repoURL      string
		pgConn       string
		tde          bool
		kekRef       string
		kmsConfig    map[string]string
		walSegSizeMB int
	)
	c := &cobra.Command{
		Use:   "push <deployment> <segment-path>",
		Short: "Archive one segment via archive_command (PG-invoked)",
		Long: `Push one PostgreSQL WAL segment file into the repository.

PG invokes this command from archive_command during normal operation:

  archive_command = 'pg_hardstorage wal push db1 %p --repo <url>'

The %p substitution is a path on the PG host; we read the file, chunk
it through the CAS, and commit a segment manifest atomically. Re-pushes
of an already-committed segment are no-ops.

system_identifier — stamped on every segment manifest so cross-cluster
repo contamination is detectable — is derived in this order:

  1. --system-identifier <decimal> if supplied (zero-cost path).
     This is the unsigned-decimal value pg_control_system() reports
     (SELECT system_identifier FROM pg_control_system()) — the SAME
     form rule 2 derives from the segment header. Passing it in any
     other base would mismatch header-derived pushes and trip a
     spurious splitbrain.system_identifier_mismatch.
  2. Otherwise read directly from the segment file's first-page
     XLogLongPageHeader.xlp_sysid — every WAL segment PG writes
     carries it.  No libpq round-trip per call.
  3. Otherwise --pg-connection <dsn> if supplied (legacy path).

The default canonical archive_command shape — repo only, no extra
flags — works because of (2).

Transparent Data Encryption (TDE):

  Pass --tde when the source PostgreSQL has TDE enabled (CYBERTEC
  PGEE, pg_tde, EDB TDE).  Under TDE the segment file on disk is
  ciphertext, so the on-segment header read (precedence rule 2
  above) is meaningless — it would either fail outright OR return
  bogus bytes that happen to look header-shaped.  --tde skips
  rule 2 unconditionally; you MUST supply --system-identifier or
  --pg-connection.

Exit codes:
  0  - segment archived (or already present)
  >0 - error; PG will retry per archive_timeout`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalPush(cmd, walPushOptions{
				deployment:  args[0],
				segmentPath: args[1],
				repoURL:     repoURL,
				pgConn:      pgConn,
				systemID:    cmd.Flag("system-identifier").Value.String(),
				tde:         tde,
				kekRef:      kekRef,
				kmsConfig:   kmsConfig,
				segSize:     int64(walSegSizeMB) << 20,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string — used once to fetch the system_identifier")
	c.Flags().String("system-identifier", "",
		"explicit pg_control system_identifier (skip libpq round-trip)")
	c.Flags().BoolVar(&tde, "tde", false,
		"the source PG has Transparent Data Encryption enabled; skip on-segment header parsing (requires --system-identifier or --pg-connection). See docs/explanation/tde-awareness.md.")
	c.Flags().StringVar(&kekRef, "kek", "",
		"KEK reference for WAL encryption under a cloud KMS (e.g. aws-kms://...); MUST match the deployment's base-backup KEKRef. Local-KEK encryption is automatic when a keyring kek.bin is present.")
	c.Flags().StringToStringVar(&kmsConfig, "kms-config", nil,
		"cloud KMS provider config (region/endpoint/credentials); only consulted when --kek is a cloud scheme — the same values base backups use.")
	c.Flags().IntVar(&walSegSizeMB, "wal-segsize", 16,
		"cluster wal_segment_size in megabytes (matches initdb --wal-segsize); a segment file whose length differs from this is refused as truncated/corrupt")
	return c
}

type walPushOptions struct {
	deployment  string
	segmentPath string
	repoURL     string
	pgConn      string
	systemID    string
	tde         bool
	kekRef      string
	kmsConfig   map[string]string
	segSize     int64 // declared wal_segment_size in bytes; 0 → 16 MiB
}

func runWalPush(cmd *cobra.Command, opts walPushOptions) error {
	d := DispatcherFrom(cmd)
	// Validate an explicit --system-identifier up front (before the
	// repo round-trip): it is stamped verbatim onto the segment
	// manifest and compared for equality against header-derived pushes
	// (FormatUint, unsigned decimal). A hex value or a typo would
	// otherwise be accepted silently and only surface later as a
	// confusing splitbrain.system_identifier_mismatch when a sibling
	// segment was stamped from the header.
	if opts.systemID != "" && !validSystemIdentifier(opts.systemID) {
		return output.NewError("usage.bad_system_identifier",
			fmt.Sprintf("wal push: --system-identifier %q must be a positive unsigned-decimal integer "+
				"(the value `SELECT system_identifier FROM pg_control_system()` reports)", opts.systemID)).
			Wrap(output.ErrUsage)
	}
	repoMeta, sp, err := repo.Open(cmd.Context(), opts.repoURL)
	if err != nil {
		return mapRepoOpenErr(opts.repoURL, err)
	}
	defer sp.Close()
	if err := assertRepoWritable(cmd.Context(), sp, "wal push"); err != nil {
		return err
	}

	// archive_command receives both real WAL segments AND the
	// small companion files PG emits — `.backup` (backup-history)
	// and `.history` (timeline-history).  The companion files have
	// no segment header to derive system_identifier from, so the
	// canonical-shape archive_command had no path forward for them
	// before issue #10 — every `.backup`-archive call failed and PG
	// retried until `archive_failures` tripped.  Detect them by
	// suffix and route through the dedicated verbatim-copy path.
	if kind := walsink.ClassifyArchiveInput(filepath.Base(opts.segmentPath)); kind != walsink.AuxiliaryNone {
		key, _, perr := walsink.PushAuxiliaryFile(cmd.Context(), sp, opts.segmentPath, walsink.PushOptions{
			Deployment: opts.deployment,
			WORM:       repoMeta.WORM,
		})
		if perr != nil {
			return mapWalPushError(opts.segmentPath, perr)
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(walPushAuxResultBody{
			Deployment: opts.deployment,
			FileName:   filepath.Base(opts.segmentPath),
			Kind:       auxiliaryKindString(kind),
			RepoKey:    key,
		}))
	}

	// Resolve system_identifier.  Precedence:
	//  1. Explicit --system-identifier flag
	//  2. The segment file's own XLogLongPageHeader.xlp_sysid
	//     (issue #8: archive_command should not need libpq) —
	//     SKIPPED UNDER --tde because the file is ciphertext.
	//  3. --pg-connection libpq round-trip
	//
	// The segment-file fallback makes the canonical archive_command
	// shape work without any extra flags:
	//
	//   archive_command = 'pg_hardstorage wal push db1 %p --repo <url>'
	//
	// every WAL segment PG hands the archiver carries the
	// cluster's system identifier in its first page header, so
	// the agent can stamp the manifest without a side-channel
	// connection.
	//
	// Under TDE (--tde) the segment file is ciphertext on disk;
	// reading 32 bytes at offset 0 returns random-looking bytes
	// that would either fail the XLP_LONG_HEADER check or, worse,
	// happen to satisfy it and stamp a bogus xlp_sysid on the
	// manifest.  We skip rule 2 unconditionally and require the
	// operator to use rule 1 or rule 3.
	sysID := opts.systemID
	if sysID == "" && !opts.tde {
		if id, ferr := walsink.ReadSystemIdentifierFromSegment(opts.segmentPath); ferr == nil {
			sysID = id
		} else if opts.pgConn == "" {
			return output.NewError("usage.missing_flag",
				"wal push: cannot derive system_identifier — segment header read failed and no --pg-connection / --system-identifier given").
				WithSuggestion(&output.Suggestion{
					Human:   "the canonical archive_command shape derives the system_identifier from the segment file itself; this fell back because: " + ferr.Error(),
					Command: "pg_hardstorage doctor",
				}).Wrap(output.ErrUsage)
		}
	}
	if sysID == "" && opts.tde && opts.pgConn == "" {
		// TDE skipped rule 2 and the operator gave neither rule 1
		// nor rule 3 — refuse loudly so the cron archive_command
		// fails fast rather than stamping ciphertext-derived junk.
		return output.NewError("usage.missing_flag",
			"wal push --tde: cannot derive system_identifier — segment header is ciphertext under TDE and no --pg-connection / --system-identifier given").
			WithSuggestion(&output.Suggestion{
				Human:   "set --system-identifier <hex> (recommended for archive_command; obtain once via `SELECT system_identifier FROM pg_control_system()`) or pass --pg-connection so we can fetch it per push",
				DocURL:  "docs/explanation/tde-awareness.md",
				Command: "pg_hardstorage doctor " + opts.deployment,
			}).Wrap(output.ErrUsage)
	}
	if sysID == "" {
		id, err := identifySystem(cmd.Context(), opts.pgConn)
		if err != nil {
			return err
		}
		sysID = id.SystemID
	}

	// WORM thread: when the repo carries a retention policy, both
	// chunks (via casdefault.NewWithRetention) and the per-segment
	// manifest (via walsink.PushOptions.WORM) get the policy
	// propagated. now is captured here once per push so chunks +
	// manifest of THIS segment share the same retention deadline,
	// matching the pattern the backup runner uses.
	wormNow := time.Now().UTC()
	// Encrypt the segment under the deployment's shared DEK when a local KEK
	// exists, so WAL is no longer plaintext at rest (issue #106). The CAS and
	// the manifest envelope MUST use the same DEK; buildWALEncryption returns
	// both. nil enc → plaintext push (no KEK), preserving prior behaviour.
	enc, encInfo, err := resolveWALEncryption(cmd.Context(), sp, opts.deployment, opts.kekRef, opts.kmsConfig)
	if err != nil {
		return output.NewError("wal.encryption_setup_failed",
			fmt.Sprintf("wal push: %v", err)).Wrap(err)
	}
	var cas *repo.CAS
	if enc != nil {
		cas = casdefault.NewEncryptedWithRetention(sp, enc, repoMeta.WORM, wormNow,
			casdefault.WithCompressionLevel(repoMeta.Compression))
	} else {
		cas = casdefault.NewWithRetention(sp, repoMeta.WORM, wormNow,
			casdefault.WithCompressionLevel(repoMeta.Compression))
	}
	m, err := walsink.PushSegmentFile(cmd.Context(), cas, sp, opts.segmentPath, walsink.PushOptions{
		Deployment:       opts.deployment,
		SystemIdentifier: sysID,
		WORM:             repoMeta.WORM,
		Encryption:       encInfo,
		SegmentSize:      opts.segSize,
	})
	if err != nil {
		return mapWalPushError(opts.segmentPath, err)
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(walPushResultBody{
		Deployment:  opts.deployment,
		SegmentName: m.SegmentName,
		Timeline:    m.Timeline,
		StartLSN:    m.StartLSN,
		EndLSN:      m.EndLSN,
		ChunkCount:  len(m.Chunks),
		BytesIn:     m.SegmentSize,
	}))
}

func mapWalPushError(path string, err error) error {
	if errors.Is(err, walsink.ErrNotASegmentFile) {
		return output.NewError("notfound.wal_segment_name",
			fmt.Sprintf("wal push: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "this archive_command input is neither a canonical 16 MiB WAL segment nor a recognised companion file (.backup / .history / .partial) — check the %f PG passed",
			}).Wrap(err)
	}
	return output.NewError("wal.push_failed",
		fmt.Sprintf("wal push: %v", err)).Wrap(err)
}

type walPushResultBody struct {
	Deployment  string `json:"deployment"`
	SegmentName string `json:"segment_name"`
	Timeline    uint32 `json:"timeline"`
	StartLSN    string `json:"start_lsn"`
	EndLSN      string `json:"end_lsn"`
	ChunkCount  int    `json:"chunk_count"`
	BytesIn     int64  `json:"bytes_in"`
}

// WriteText renders the WAL push result as a single-line confirmation to w.
func (b walPushResultBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"✓ wal push %s (TLI %d, %d chunks, %s)",
		b.SegmentName, b.Timeline, b.ChunkCount, humanBytes(b.BytesIn))
	return err
}

// walPushAuxResultBody is the success body for an auxiliary
// (`.backup` / `.history`) archive_command invocation.  Distinct
// schema from walPushResultBody so JSON consumers can tell the two
// apart at a glance — no chunk count, no LSN, no segment name.
type walPushAuxResultBody struct {
	Deployment string `json:"deployment"`
	FileName   string `json:"file_name"`
	Kind       string `json:"kind"`
	RepoKey    string `json:"repo_key"`
}

// WriteText renders the auxiliary-file push result as a single-line
// confirmation to w.
func (b walPushAuxResultBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"✓ wal push %s (%s, archived to %s)",
		b.FileName, b.Kind, b.RepoKey)
	return err
}

func auxiliaryKindString(k walsink.AuxiliaryFileKind) string {
	switch k {
	case walsink.AuxiliaryBackup:
		return "backup-history"
	case walsink.AuxiliaryHistory:
		return "timeline-history"
	case walsink.AuxiliaryPartial:
		return "partial-segment"
	default:
		return "unknown"
	}
}

// newWalFetchCmd implements `pg_hardstorage wal fetch <deployment>
// <segment-name> <target-path>`. PostgreSQL invokes this from
// restore_command during recovery, substituting %f for the segment
// name and %p for the destination path.
//
// PG-contract semantics:
//
//   - Exit 0 on successful retrieval (segment exists in repo and was
//     written byte-for-byte to <target-path>).
//   - Exit non-zero when the segment isn't in the repo. PG treats
//     this as "no more WAL available" and stops recovery. We return
//     a "notfound" structured error with code "wal.fetch.not_found".
//   - Companion files PG archives — `.history` (timeline navigation),
//     `.backup` (backup-history), `.partial` (a timeline's last partial
//     segment on promotion) — are stored verbatim by `wal push` (issue #10)
//     and served here via the auxiliary path. A request for one not in the
//     repo still hits the not-found path, which PG handles gracefully (a
//     missing `.history` just means "stay on the requested timeline").
//
// Atomicity: the segment is reassembled into <target-path>.tmp and
// atomically renamed. PG's recovery treats partial files as
// truncation, so atomic rename matters.
func newWalFetchCmd() *cobra.Command {
	var (
		repoURL string
	)
	c := &cobra.Command{
		Use:   "fetch <deployment> <segment-name> <target-path>",
		Short: "Fetch one WAL segment from the repository (restore_command shim)",
		Long: `Reassemble one PostgreSQL WAL segment from the repository's
content-addressed chunk store into <target-path>.

PostgreSQL invokes this command from restore_command during recovery;
the canonical configuration line is:

  restore_command = 'pg_hardstorage wal fetch <deployment> %f %p --repo <url>'

The deployment + repo identify the source; %f and %p are PG's
substitutions for the segment file name and the target path
respectively.

Exit codes:
  0  - segment retrieved and written
  6  - segment not in repo (ExitNotFound)
  >0 - other error (network, repo missing, etc.)

IMPORTANT: at end-of-archive this command exits 6, NOT 1. PostgreSQL's
postmaster treats any restore_command exit other than 0/1 as a crash
and restarts recovery, which loops forever once the cluster is caught
up. The canonical restore_command above must therefore map 6 -> 1 (the
"no more WAL, stop and promote" signal PG expects). The line that
` + "`pg_hardstorage restore`" + ` writes does this automatically via the
shim '...; ec=$?; [ $ec = 6 ] && exit 1 || exit $ec'. If you hand-write
restore_command, you MUST include that 6->1 mapping yourself.`,
		Args:         cobra.ExactArgs(3),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalFetch(cmd, walFetchOptions{
				deployment:  args[0],
				segmentName: args[1],
				targetPath:  args[2],
				repoURL:     repoURL,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

type walFetchOptions struct {
	deployment  string
	segmentName string
	targetPath  string
	repoURL     string
}

// runWalFetch is the body of `wal fetch`. PG invokes this once per
// segment during recovery; it must be quiet on success (PG ignores
// stdout). Errors flow through the normal Result/Error path so
// `--output json` consumers (e.g. test harnesses) get structured
// information.
func runWalFetch(cmd *cobra.Command, opts walFetchOptions) error {
	d := DispatcherFrom(cmd)

	// Open repo. The same error mapping as `wal stream`.
	_, sp, err := repo.Open(cmd.Context(), opts.repoURL)
	if err != nil {
		return mapRepoOpenErr(opts.repoURL, err)
	}
	defer sp.Close()

	// Auxiliary files (`.backup` / `.history`) are stored verbatim
	// alongside segment manifests since issue #10 — when PG asks
	// for one, retrieve the raw bytes and write them straight to
	// %p. Recovery uses `.history` to navigate timelines; `.backup`
	// requests are rare but operators sometimes hit them when
	// resurrecting an external base backup, and "fetch what we
	// archived" is the obvious symmetric behaviour.
	if kind := walsink.ClassifyArchiveInput(opts.segmentName); kind != walsink.AuxiliaryNone {
		return runWalFetchAuxiliary(cmd, sp, opts, kind)
	}

	// Validate the segment name shape. We accept the canonical 24-
	// character hex form. Anything else is reported as not-found so
	// PG's recovery behaves naturally. Code prefix "notfound." maps
	// to ExitNotFound (6) per the v1 exit-code contract.
	tli, _, ok := parseSegmentNameForFetch(opts.segmentName)
	if !ok {
		return output.NewError("notfound.wal_segment_name",
			fmt.Sprintf("wal fetch: %q is not a canonical WAL segment name", opts.segmentName))
	}

	// Locate the manifest. SegmentPath embeds the timeline+name
	// canonical layout; we read the manifest body verbatim, parse,
	// then walk its chunks.
	key := walsink.SegmentPath(opts.deployment, tli, opts.segmentName)
	rc, err := sp.Get(cmd.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.wal_segment",
				fmt.Sprintf("wal fetch: segment %s not in repo", opts.segmentName))
		}
		return output.NewError("wal.fetch.read_failed",
			fmt.Sprintf("wal fetch: read manifest %q: %v", key, err)).Wrap(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return output.NewError("wal.fetch.read_failed",
			fmt.Sprintf("wal fetch: read manifest body: %v", err)).Wrap(err)
	}

	m, err := walsink.ParseSegmentManifest(body)
	if err != nil {
		return output.NewError("wal.fetch.bad_manifest",
			fmt.Sprintf("wal fetch: parse manifest: %v", err)).Wrap(err)
	}
	// Recompute the expected contiguous segment number from the
	// requested name using the MANIFEST'S recorded segment size — PG
	// packs 4 GiB / size segments per log-id, so segment_number depends
	// on the cluster's wal_segment_size. Using the segNum from
	// parseSegmentNameForFetch (which assumes the 16 MiB default) would
	// falsely reject every non-default-size cluster here.
	_, expectedSegNum, _ := walsink.ParseSegmentName(opts.segmentName, m.SegmentSize)
	if m.SegmentNumber != expectedSegNum {
		// No .Wrap here: err is nil at this point (ParseSegmentManifest
		// succeeded). Wrapping nil would give the structured error an
		// empty Unwrap chain — this mismatch is self-contained.
		return output.NewError("wal.fetch.manifest_mismatch",
			fmt.Sprintf("wal fetch: manifest segment_number=%d but caller asked for %d (segment %q, size %d)",
				m.SegmentNumber, expectedSegNum, opts.segmentName, m.SegmentSize))
	}

	// Reassemble bytes through the CAS, write atomically.
	//
	// Pick the CAS from the segment's own posture:
	//   - Encrypted segment (issue #106): the manifest carries its envelope,
	//     so resolve the shared DEK from THAT and decrypt — this works even
	//     when no base backup exists yet (WAL-first). A missing keyring /
	//     unresolvable DEK is a hard error: the chunks are ciphertext and
	//     can't be reassembled without the key.
	//   - Plaintext segment: a plain CAS. But the chunk store is shared with
	//     base backups (content-addressed by PLAINTEXT hash), so a
	//     plaintext-era WAL segment can dedup against an ENCRYPTED backup
	//     chunk; the plain CAS then fails with encryption.ErrUnknownAlgorithm.
	//     Only on that specific error do we fall back to the deployment's
	//     shared DEK resolved from a base-backup manifest. The all-plaintext
	//     common case pays nothing.
	if m.Encryption != nil {
		encCAS, ok := decryptingCASFromEnvelope(cmd.Context(), sp,
			m.Encryption.Scheme, m.Encryption.KEKRef, m.Encryption.WrappedDEK)
		if !ok {
			return output.NewError("wal.fetch.decrypt_unavailable",
				fmt.Sprintf("wal fetch: segment %q is encrypted but its DEK could not be resolved (keyring/KMS unavailable or wrong KEK) — recovery needs key access, same as an encrypted base-backup restore", opts.segmentName))
		}
		if err := writeSegmentAtomically(cmd.Context(), encCAS, m, opts.targetPath); err != nil {
			return err
		}
	} else {
		cas := casdefault.New(sp)
		err = writeSegmentAtomically(cmd.Context(), cas, m, opts.targetPath)
		if err != nil && errors.Is(err, encryption.ErrUnknownAlgorithm) {
			if encCAS, ok := buildWALDecryptingCAS(cmd.Context(), sp, opts.deployment); ok {
				err = writeSegmentAtomically(cmd.Context(), encCAS, m, opts.targetPath)
			}
		}
		if err != nil {
			return err
		}
	}

	// Success. Emit a Result so JSON consumers get a structured
	// document; PG's stdout is ignored so this is operator-tooling
	// only.
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(walFetchResultBody{
		Deployment:  opts.deployment,
		SegmentName: m.SegmentName,
		Timeline:    m.Timeline,
		StartLSN:    m.StartLSN,
		EndLSN:      m.EndLSN,
		BytesOut:    m.SegmentSize,
		ChunkCount:  len(m.Chunks),
		TargetPath:  opts.targetPath,
	}))
}

// fetchAuxBody reads an auxiliary file's bytes from the repo. The primary
// location is the archive_command aux path (AuxiliaryFilePath). For a
// `.history` request that misses there, it falls back to the streaming-HA
// follower's timeline store (wal/<dep>/timelines/<decimal-tli>.history) — the
// follower captures `.history` files on a failover into THAT store, separate
// from the archive aux path, so without this fallback a `wal stream`-only
// (no archive_command) HA deployment can't fetch the timeline-history file PG
// needs to navigate past the failover during a PITR. Either location is
// authoritative; we serve whichever has it.
func fetchAuxBody(ctx context.Context, sp storage.StoragePlugin, key string, kind walsink.AuxiliaryFileKind, deployment, segmentName string) ([]byte, error) {
	rc, err := sp.Get(ctx, key)
	if err == nil {
		defer rc.Close()
		return io.ReadAll(rc)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if kind == walsink.AuxiliaryHistory {
		if tli, ok := historyRequestTLI(segmentName); ok {
			if b, terr := timeline.New(sp).Get(ctx, deployment, tli); terr == nil {
				return b, nil // found in the follower's timeline store
			}
		}
	}
	return nil, err // original NotFound from the aux path
}

// historyRequestTLI parses the timeline from a PG history-file request name
// (`00000002.history`, 8 hex chars) into the uint32 the follower's timeline
// store keys on. Returns false when name isn't a `<hex>.history`.
func historyRequestTLI(name string) (uint32, bool) {
	s := strings.TrimSuffix(name, ".history")
	if s == name || s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// runWalFetchAuxiliary streams an archived `.backup` or `.history`
// file from the repo to the target path PG passed.  Symmetric to
// PushAuxiliaryFile (issue #10): no chunk reassembly, no manifest
// parsing — just a verbatim copy with the same atomic-rename
// guarantees as the segment path so a crash during recovery never
// leaves PG looking at a truncated history file.
func runWalFetchAuxiliary(cmd *cobra.Command, sp storage.StoragePlugin, opts walFetchOptions, kind walsink.AuxiliaryFileKind) error {
	d := DispatcherFrom(cmd)
	// The aux basename flows straight into the repo key
	// (AuxiliaryFilePath interpolates it), so — like the 24-hex
	// segment-name gate — reject a name carrying path separators or a
	// ".." traversal component. Real PG aux names ("00000003.history",
	// "<24hex>.<offset>.backup") never contain these. The storage layer
	// already refuses escaping keys, but matching the segment path's
	// posture keeps the failure a clean NotFound (exit 6, "no such
	// file" — the recovery-natural signal) instead of a read_failed
	// (exit 1) and avoids leaning on every backend's key sanitiser.
	if !safeAuxBasename(opts.segmentName) {
		return output.NewError("notfound.wal_segment_name",
			fmt.Sprintf("wal fetch: %q is not a valid auxiliary file name", opts.segmentName))
	}
	key := walsink.AuxiliaryFilePath(opts.deployment, opts.segmentName, kind)
	body, err := fetchAuxBody(cmd.Context(), sp, key, kind, opts.deployment, opts.segmentName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Recovery treats not-found here as "no such
			// timeline / no backup-history archived" — for
			// `.history` PG continues without it, which is
			// what we want when the cluster is on the original
			// timeline and never promoted.
			return output.NewError("notfound.wal_segment",
				fmt.Sprintf("wal fetch: %s not in repo", opts.segmentName))
		}
		return output.NewError("wal.fetch.read_failed",
			fmt.Sprintf("wal fetch: read aux file %q: %v", key, err)).Wrap(err)
	}

	tmp := opts.targetPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return output.NewError("wal.fetch.target_open",
			fmt.Sprintf("wal fetch: open %q: %v", tmp, err)).Wrap(err)
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return output.NewError("wal.fetch.write_failed",
			fmt.Sprintf("wal fetch: write to %q: %v", tmp, err)).Wrap(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return output.NewError("wal.fetch.fsync_failed",
			fmt.Sprintf("wal fetch: fsync: %v", err)).Wrap(err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return output.NewError("wal.fetch.close_failed",
			fmt.Sprintf("wal fetch: close: %v", err)).Wrap(err)
	}
	if err := os.Rename(tmp, opts.targetPath); err != nil {
		cleanup()
		return output.NewError("wal.fetch.rename_failed",
			fmt.Sprintf("wal fetch: rename %q -> %q: %v", tmp, opts.targetPath, err)).Wrap(err)
	}
	if err := fsutil.SyncDir(filepath.Dir(opts.targetPath)); err != nil {
		return output.NewError("wal.fetch.fsync_failed",
			fmt.Sprintf("wal fetch: fsync parent dir: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(walFetchAuxResultBody{
		Deployment: opts.deployment,
		FileName:   opts.segmentName,
		Kind:       auxiliaryKindString(kind),
		BytesOut:   int64(len(body)),
		TargetPath: opts.targetPath,
	}))
}

// walFetchAuxResultBody is the success body for an auxiliary
// fetch.  Distinct from walFetchResultBody so JSON consumers can
// tell segment vs aux apart without sniffing field shapes.
type walFetchAuxResultBody struct {
	Deployment string `json:"deployment"`
	FileName   string `json:"file_name"`
	Kind       string `json:"kind"`
	BytesOut   int64  `json:"bytes_out"`
	TargetPath string `json:"target_path"`
}

// WriteText renders the auxiliary-file fetch result as a single-line
// confirmation to w.
func (b walFetchAuxResultBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"✓ wal fetch %s -> %s (%s, %s)",
		b.FileName, b.TargetPath, b.Kind, humanBytes(b.BytesOut))
	return err
}

// parseSegmentNameForFetch validates the 24-char canonical name and
// extracts (timeline, segment_number). Returns ok=false for anything
// that doesn't match — including ".history" file requests, which
// fall through to a not-found response.
// slotNameCharsSafe reports whether s is composed solely of
// [A-Za-z0-9_] and is 1–63 chars — the character class PG permits in a
// replication slot name. Deliberately does NOT impose ValidIdentifier's
// "must start with a letter/underscore" rule: PG allows digit-start
// slot names, so enforcing that here would refuse a pre-existing valid
// slot. The sole purpose is to reject the injection surface
// (whitespace, quotes, ';', '/', backslash, control chars) before the
// name reaches an unquoted replication-protocol command.
func slotNameCharsSafe(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// validSystemIdentifier reports whether s is a PG system_identifier in
// the canonical unsigned-decimal form: a positive uint64. Rejects
// empty, zero (no real cluster reports 0 — the segment-header path
// refuses it too), hex, signs, and any non-digit garbage.
func validSystemIdentifier(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n != 0
}

// safeAuxBasename reports whether name is a plain filename safe to
// interpolate into a repo key: non-empty, no path separators, and no
// ".." traversal component. Legitimate ".history"/".backup" names
// contain only hex, single dots, and the suffix — never "/", "\", or
// "..".
func safeAuxBasename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	return true
}

func parseSegmentNameForFetch(name string) (uint32, uint64, bool) {
	if len(name) != 24 {
		return 0, 0, false
	}
	tli, err1 := parseHex32(name[:8])
	if err1 != nil {
		return 0, 0, false
	}
	segNum, ok := parseSegmentNumber(name)
	if !ok {
		return 0, 0, false
	}
	return tli, segNum, true
}

// buildWALDecryptingCAS returns a CAS that can decrypt chunks written
// under the deployment's shared DEK — needed when a plaintext WAL segment
// deduped against an encrypted base-backup chunk in the shared
// chunks/sha256/ namespace. The DEK is the single key every encrypted
// backup under one KEK shares, so any backup manifest's envelope yields
// it; we resolve the KEK from the keyring (local custody) or the cloud
// KMS, mirroring a base-backup restore.
//
// Best-effort: any failure to find an encrypted manifest or resolve the
// DEK returns ok=false, and the caller keeps the original plaintext-CAS
// error. That's the right posture — an environment that can't reach the
// keyring here also can't restore the encrypted base backup, so the
// failure is consistent and the error already names the unreadable chunk.
func buildWALDecryptingCAS(ctx context.Context, sp storage.StoragePlugin, deployment string) (*repo.CAS, bool) {
	var info *backup.EncryptionInfo
	for m, lerr := range backup.NewManifestStore(sp).ListAttestationless(ctx, deployment) {
		if lerr != nil || m == nil || m.Encryption == nil {
			continue
		}
		info = m.Encryption
		break
	}
	if info == nil {
		return nil, false
	}
	return decryptingCASFromEnvelope(ctx, sp, info.Scheme, info.KEKRef, info.WrappedDEK)
}

// decryptingCASFromEnvelope resolves the plaintext DEK described by a single
// envelope (scheme / kekRef / base64 wrapped DEK) — via cloud KMS or the local
// keyring, mirroring a base-backup restore — and returns a CAS that decrypts
// chunks with it. ok=false on any scheme mismatch or DEK-resolution failure,
// so the caller keeps its original error. Shared by the segment-manifest
// envelope path (issue #106) and the backup-manifest fallback.
func decryptingCASFromEnvelope(ctx context.Context, sp storage.StoragePlugin, scheme, kekRef, wrappedB64 string) (*repo.CAS, bool) {
	if scheme != "aes-256-gcm" {
		return nil, false
	}
	wrapped, err := base64.StdEncoding.DecodeString(wrappedB64)
	if err != nil {
		return nil, false
	}

	var dek []byte
	if s := kms.SchemeOf(kekRef); s != "" && s != "local" {
		d, uerr := keystore.UnwrapDEK(ctx, kekRef, wrapped, keystore.UnwrapOpts{})
		if uerr != nil {
			return nil, false
		}
		dek = d
	} else {
		pth, perr := paths.Resolve(paths.DefaultOptions())
		if perr != nil || pth.Keyring.Value == "" || !keystore.KEKExists(pth.Keyring.Value) {
			return nil, false
		}
		kek, kerr := keystore.KEKResolver(pth.Keyring.Value)(kekRef)
		if kerr != nil {
			return nil, false
		}
		d, uerr := encryption.Unwrap(kek, wrapped)
		if uerr != nil {
			return nil, false
		}
		dek = d[:]
	}

	enc, err := aesgcm.New(dek)
	if err != nil {
		return nil, false
	}
	return casdefault.NewEncrypted(sp, enc), true
}

// deploymentBackupKEKRef returns the KEKRef of the deployment's first
// encrypted base-backup manifest, if any. Used to detect the deployment's
// established encryption posture so WAL doesn't encrypt under a KEK that
// diverges from the backups. Manifests are read without verification (same
// posture as buildWALDecryptingCAS — the KEKRef only steers posture, the
// unwrap step is the real gate).
func deploymentBackupKEKRef(ctx context.Context, sp storage.StoragePlugin, deployment string) (string, bool) {
	for m, lerr := range backup.NewManifestStore(sp).ListAttestationless(ctx, deployment) {
		if lerr != nil || m == nil || m.Encryption == nil {
			continue
		}
		return m.Encryption.KEKRef, true
	}
	return "", false
}

// buildWALEncryption resolves the deployment's shared DEK from cfg and returns
// an encryptor plus the envelope to stamp on segment manifests, so WAL segments
// are encrypted at rest under the SAME key base backups use (issues #106/#108).
//
// cfg comes from the same resolveBackupEncryption builder backups use, so the
// key-custody model matches exactly:
//   - local KEK (cfg.KEK set, KEKRef "local:default") — wrap/unwrap in-process.
//   - cloud KMS (cfg.Provider set) — wrap/unwrap via the provider.
//   - cfg == nil — no encryption configured (no keyring, no --kek): plaintext.
//
// The shared DEK is minted via sharedkey.ResolveOrMint, which serialises
// the mint through an atomic single-winner PUT on a well-known shared-DEK
// object (seeded from existing base-backup / WAL manifests for legacy
// repos). This makes concurrent WAL streaming and base backups converge on
// ONE DEK even when neither has committed a manifest yet — the earlier
// scan-then-mint could have each writer mint a DIFFERENT DEK, leaving a
// full-page image that deduped against a base chunk unrestorable (issue
// #31). A prior DEK that cfg can't unwrap FAILS the write rather than
// forking a fresh DEK that would leave deduped chunks unrestorable (issue #28).
//
// Divergence guard: if the deployment's established backups use a DIFFERENT
// KEKRef than cfg, WAL stays plaintext rather than diverge (e.g. a leftover
// local kek.bin while backups are cloud-KMS, or a mismatched --kek). The
// read-side mitigation still covers any cross-posture chunk collision.
func buildWALEncryption(ctx context.Context, sp storage.StoragePlugin, deployment string, cfg *runner.EncryptionConfig) (encryption.Encryptor, *walsink.EncryptionInfo, error) {
	if cfg == nil {
		return nil, nil, nil // no encryption configured → plaintext WAL
	}
	if established, ok := deploymentBackupKEKRef(ctx, sp, deployment); ok && established != cfg.KEKRef {
		return nil, nil, nil // mismatched posture → stay plaintext (avoid divergence)
	}

	// Custody-specific wrap/unwrap — the only thing that differs between the
	// local-KEK and cloud-KMS paths.
	var unwrap sharedkey.Unwrapper
	var wrap func(dek [encryption.KeyLen]byte) ([]byte, error)
	if cfg.Provider != nil {
		unwrap = func(w []byte) ([]byte, error) { return cfg.Provider.UnwrapDEK(ctx, w) }
		wrap = func(dek [encryption.KeyLen]byte) ([]byte, error) { return cfg.Provider.WrapDEK(ctx, dek[:]) }
	} else {
		kek := cfg.KEK
		unwrap = func(w []byte) ([]byte, error) {
			d, uerr := encryption.Unwrap(kek, w)
			if uerr != nil {
				return nil, uerr
			}
			return d[:], nil
		}
		wrap = func(dek [encryption.KeyLen]byte) ([]byte, error) { return encryption.Wrap(kek, dek) }
	}

	// ResolveOrMint mints the shared DEK atomically (single-winner PUT on
	// the shared-DEK object), so WAL streaming starting concurrently with a
	// base backup converges on ONE DEK. The old Resolve-then-mint path let
	// each writer mint its own DEK when neither had committed a manifest
	// yet, so a WAL full-page image that dedups against a base chunk was
	// stored under a different DEK than the backup manifest referenced it
	// by — leaving the backup unrestorable (issue #31).
	res, rerr := sharedkey.ResolveOrMint(ctx, sp, cfg.KEKRef, unwrap, wrap)
	if rerr != nil {
		return nil, nil, fmt.Errorf("wal: cannot determine or mint the shared DEK; refusing to write WAL that the CAS's plaintext-hash dedup would leave unrestorable: %w", rerr)
	}
	if res.UnusableCandidate {
		return nil, nil, fmt.Errorf("wal: a prior DEK for KEK %q exists but could not be unwrapped; refusing to write WAL under a fresh DEK that would leave deduped chunks unrestorable (verify the KEK material matches this repo)", cfg.KEKRef)
	}
	if !res.Have {
		return nil, nil, fmt.Errorf("wal: shared-DEK resolution returned no key for KEK %q", cfg.KEKRef)
	}
	dek := res.DEK

	wrapped, werr := wrap(dek)
	if werr != nil {
		return nil, nil, fmt.Errorf("wal: wrap DEK: %w", werr)
	}
	enc, eerr := aesgcm.New(dek[:])
	if eerr != nil {
		return nil, nil, fmt.Errorf("wal: build encryptor: %w", eerr)
	}
	env := &walsink.EncryptionInfo{
		Scheme:          enc.Name(),
		KEKRef:          cfg.KEKRef,
		WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
		EnvelopeVersion: 2,
	}
	return enc, env, nil
}

// resolveWALEncryption builds the WAL encryption config the same way backups
// do (resolveBackupEncryption), then resolves the shared DEK into an encryptor
// + envelope. It owns the cloud-KMS provider's lifecycle: the provider is only
// needed to wrap/unwrap the DEK here, so it is closed before returning — the
// returned encryptor is DEK-based and outlives it (important for `wal stream`,
// which then runs for a long time). Empty kekRef + no keyring → plaintext.
func resolveWALEncryption(ctx context.Context, sp storage.StoragePlugin, deployment, kekRef string, kmsConfig map[string]string) (encryption.Encryptor, *walsink.EncryptionInfo, error) {
	keyringDir := ""
	if pth, perr := paths.Resolve(paths.DefaultOptions()); perr == nil {
		keyringDir = pth.Keyring.Value
	}
	cfg, err := resolveBackupEncryption(ctx, keyringDir, false, false, kekRef, kmsConfig)
	if err != nil {
		return nil, nil, err
	}
	if cfg != nil && cfg.Provider != nil {
		defer cfg.Provider.Close()
	}
	return buildWALEncryption(ctx, sp, deployment, cfg)
}

// writeSegmentAtomically reassembles m's chunks from cas into
// <targetPath>.tmp and renames to targetPath. fsync before rename so
// a crash during recovery doesn't leave a half-written segment file
// behind that PG might pick up on next start.
func writeSegmentAtomically(ctx context.Context, cas *repo.CAS, m *walsink.SegmentManifest, targetPath string) error {
	tmp := targetPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return output.NewError("wal.fetch.target_open",
			fmt.Sprintf("wal fetch: open %q: %v", tmp, err)).Wrap(err)
	}
	cleanup := func() { _ = os.Remove(tmp) }

	var written int64
	for _, c := range m.Chunks {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			cleanup()
			return err
		}
		// The chunk's recorded offset MUST equal where we're about to
		// write it: chunks are a contiguous, ascending, gap-free cover of
		// the segment. WAL segment manifests aren't signed, so a corrupt,
		// tampered, or out-of-order chunk list would otherwise assemble
		// bytes (each hash-verified, so individually correct) into the
		// WRONG positions and still pass the total-size check below —
		// silently-corrupt WAL that PG would replay. Refuse instead.
		if c.Offset != written {
			_ = f.Close()
			cleanup()
			return output.NewError("verify.chunk_offset_mismatch",
				fmt.Sprintf("wal fetch: chunk %s offset=%d but assembly is at %d (non-contiguous/out-of-order chunk list — corrupt manifest)",
					c.Hash, c.Offset, written))
		}
		bs, err := cas.GetChunkBytes(ctx, c.Hash)
		if err != nil {
			_ = f.Close()
			cleanup()
			return output.NewError("wal.fetch.chunk_missing",
				fmt.Sprintf("wal fetch: get chunk %s: %v", c.Hash, err)).Wrap(err)
		}
		if int64(len(bs)) != c.Len {
			_ = f.Close()
			cleanup()
			return output.NewError("verify.chunk_size_mismatch",
				fmt.Sprintf("wal fetch: chunk %s len=%d, manifest says %d",
					c.Hash, len(bs), c.Len))
		}
		if _, err := f.Write(bs); err != nil {
			_ = f.Close()
			cleanup()
			return output.NewError("wal.fetch.write_failed",
				fmt.Sprintf("wal fetch: write to %q: %v", tmp, err)).Wrap(err)
		}
		written += int64(len(bs))
	}

	if written != m.SegmentSize {
		_ = f.Close()
		cleanup()
		return output.NewError("verify.short_assembly",
			fmt.Sprintf("wal fetch: assembled %d bytes, segment_size says %d",
				written, m.SegmentSize))
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return output.NewError("wal.fetch.fsync_failed",
			fmt.Sprintf("wal fetch: fsync: %v", err)).Wrap(err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return output.NewError("wal.fetch.close_failed",
			fmt.Sprintf("wal fetch: close: %v", err)).Wrap(err)
	}
	if err := os.Rename(tmp, targetPath); err != nil {
		cleanup()
		return output.NewError("wal.fetch.rename_failed",
			fmt.Sprintf("wal fetch: rename %q -> %q: %v", tmp, targetPath, err)).Wrap(err)
	}
	// Sync the parent directory so the rename(2) survives a power
	// loss.  POSIX file fsync flushes the file's data + inode but
	// NOT the parent dentry list — without this, PG might find no
	// segment file even though we believe the rename succeeded.
	if err := fsutil.SyncDir(filepath.Dir(targetPath)); err != nil {
		return output.NewError("wal.fetch.fsync_failed",
			fmt.Sprintf("wal fetch: fsync parent dir: %v", err)).Wrap(err)
	}
	return nil
}

// walFetchResultBody is the typed Result body for `wal fetch` success.
type walFetchResultBody struct {
	Deployment  string `json:"deployment"`
	SegmentName string `json:"segment_name"`
	Timeline    uint32 `json:"timeline"`
	StartLSN    string `json:"start_lsn"`
	EndLSN      string `json:"end_lsn"`
	BytesOut    int64  `json:"bytes_out"`
	ChunkCount  int    `json:"chunk_count"`
	TargetPath  string `json:"target_path"`
}

// WriteText for the fetch result. Quiet — PG ignores stdout, but
// `pg_hardstorage wal fetch ... --output text` invoked manually at a
// terminal benefits from a 1-line summary.
func (b walFetchResultBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"✓ wal fetch %s -> %s (%d chunks, %s)",
		b.SegmentName, b.TargetPath, b.ChunkCount, humanBytes(b.BytesOut))
	return err
}

// newWalStreamCmd implements `pg_hardstorage wal stream <deployment>`.
func newWalStreamCmd() *cobra.Command {
	var opts walStreamOptions
	c := &cobra.Command{
		Use:   "stream <deployment>",
		Short: "Continuously stream WAL into the repository",
		Long: `Stream PostgreSQL's WAL into the configured repository.

The agent connects via the replication protocol on a libpq
connection, ensures the persistent physical replication slot exists
(creating it if absent), and runs an indefinite receive loop.

Each completed 16 MiB segment is content-addressed through the CAS
and committed atomically. The slot's confirmed_flush_lsn is advanced
only after a segment commits — a crash between commits causes
PostgreSQL to resend the segment's bytes on the next start, so the
pipeline is gap-free and idempotent across restarts.

Send SIGINT or SIGTERM (Ctrl-C in an interactive shell) to stop
streaming cleanly. Any partially-buffered segment is discarded; the
slot retains the bytes for the next run.

Resume LSN is determined automatically:
  * If --start-lsn is given, it is used verbatim (must be segment-aligned).
  * Otherwise the highest already-committed segment in the repository
    is used (resume-where-we-left-off).
  * On a fresh deployment with no committed segments, LSN 0/0 is
    passed and PostgreSQL starts from the slot's restart_lsn.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.deployment = args[0]
			return runWalStream(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.pgConn, "pg-connection", "",
		"libpq connection string for the source PostgreSQL (required)")
	_ = c.MarkFlagRequired("pg-connection")
	c.Flags().StringVar(&opts.repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&opts.slotName, "slot", "",
		"replication slot name (default: pg_hardstorage_<deployment>)")
	c.Flags().StringVar(&opts.startLSN, "start-lsn", "",
		"explicit start LSN (e.g. 0/3000000); overrides resume-from-repo logic")
	c.Flags().DurationVar(&opts.statusInterval, "status-interval", 10*time.Second,
		"how often to send Standby Status Updates to PostgreSQL")
	c.Flags().DurationVar(&opts.inactivityTimeout, "inactivity-timeout", 0,
		"abort the stream if no message arrives in this duration (0 = streaming default of 5 minutes; default tolerates the default PG wal_sender_timeout=60s with a 10× margin)")
	c.Flags().BoolVar(&opts.noInactivityTimeout, "no-inactivity-timeout", false,
		"disable the client-side inactivity watchdog entirely; required when the source PG has wal_sender_timeout=0 (PG never sends keepalives, so the client's read deadline is the wrong place to detect hangs — issue #12)")
	c.Flags().BoolVar(&opts.once, "once", false,
		"exit after the first segment commits (useful for testing)")
	c.Flags().BoolVar(&opts.skipPreflight, "skip-preflight", false,
		"skip the PG-configuration preflight (catches wal_level / max_wal_senders / role.replication issues before opening the stream)")
	c.Flags().BoolVar(&opts.noSlot, "no-slot", false,
		"stream without a replication slot (UNSAFE — PG can recycle WAL out from under the streamer; use only for archive-only setups that retain WAL another way)")
	c.Flags().BoolVar(&opts.noReconnect, "no-reconnect", false,
		"exit on stream connection break instead of reconnecting; default behaviour reconnects with exponential backoff (1s → 30s) using EnsureSlot resume — survives Patroni failovers and PG bounces transparently")
	c.Flags().DurationVar(&opts.maxReconnectBackoff, "max-reconnect-backoff", 30*time.Second,
		"upper bound on the auto-reconnect backoff (initial 1s, doubles on each failure)")
	c.Flags().StringVar(&opts.durability, "durability", "per-segment",
		"WAL durability: per-segment (one syncfs per 16 MiB segment, the fast default) "+
			"| per-chunk (fsync every chunk — slower, the 'fsync every object' opt-in)")
	c.Flags().StringVar(&opts.kekRef, "kek", "",
		"KEK reference for WAL encryption under a cloud KMS (e.g. aws-kms://...); MUST match the deployment's base-backup KEKRef. Local-KEK encryption is automatic when a keyring kek.bin is present.")
	c.Flags().StringToStringVar(&opts.kmsConfig, "kms-config", nil,
		"cloud KMS provider config (region/endpoint/credentials); only consulted when --kek is a cloud scheme — the same values base backups use.")
	c.Flags().BoolVar(&opts.allowSysIDChange, "allow-system-identifier-change", false,
		"proceed even when the cluster's pg_control system identifier differs from the "+
			"deployment's already-archived WAL (the pg_upgrade / clone / restore signature). "+
			"By default this is refused to stop two clusters' WAL being interleaved under one "+
			"lineage; set it only when deliberately continuing the deployment onto a new cluster")
	c.Flags().BoolVarP(&opts.verbose, "verbose", "v", false,
		"emit one wal.stream.progress event per --status-interval covering "+
			"the segment just committed, the partial PG is currently writing, "+
			"and the interval throughput.  Off by default (the streamer's "+
			"existing output is a single starting event + the eventual "+
			"stopped event); set this for an interactive view of a long-"+
			"running stream.  Suppressed under --output json — JSON consumers "+
			"should read the structured stream's other events instead.")
	return c
}

// resolveDurability maps the --durability flag to a walsink mode,
// rejecting unknown values. Empty defaults to per-segment.
func resolveDurability(s string) (walsink.DurabilityMode, error) {
	switch s {
	case "", string(walsink.DurabilityPerSegment):
		return walsink.DurabilityPerSegment, nil
	case string(walsink.DurabilityPerChunk):
		return walsink.DurabilityPerChunk, nil
	default:
		return "", fmt.Errorf("wal stream: unknown --durability %q (want per-segment | per-chunk)", s)
	}
}

type walStreamOptions struct {
	deployment          string
	pgConn              string
	repoURL             string
	slotName            string
	startLSN            string
	statusInterval      time.Duration
	inactivityTimeout   time.Duration
	once                bool
	skipPreflight       bool
	noSlot              bool
	noReconnect         bool
	maxReconnectBackoff time.Duration
	noInactivityTimeout bool
	durability          string
	// kekRef + kmsConfig select WAL encryption under a cloud KMS (issue
	// #108). Empty kekRef keeps the automatic local-KEK posture (a keyring
	// kek.bin encrypts WAL transparently). A cloud kekRef MUST match the
	// deployment's base-backup KEKRef so the shared DEK converges.
	kekRef    string
	kmsConfig map[string]string
	// segmentSize is the cluster's probed wal_segment_size in bytes,
	// resolved once at stream start and threaded into the walsink + the
	// resume/alignment math. 0 means "not yet probed / use the 16 MiB
	// default" (NormSegmentSize handles the fallback).
	segmentSize int64
	// allowSysIDChange overrides the system-identifier preflight: by
	// default `wal stream` refuses to archive into a deployment whose
	// existing WAL was stamped with a DIFFERENT pg_control system
	// identifier (the pg_upgrade / clone / restore signature). Setting
	// this proceeds anyway — only for an operator who deliberately
	// wants to continue the deployment onto a new cluster.
	allowSysIDChange bool
	// verbose enables periodic `wal.stream.progress` events
	// (one per --status-interval) covering the segment just
	// committed, the partial PG is currently writing, and the
	// recent throughput.  Issue #53 — the streamer's default
	// output is the one-shot starting event plus the eventual
	// stopped event, which looks idle for hours of healthy
	// streaming; verbose mode gives the operator something to
	// watch.
	verbose bool
}

// runWalStream is the main entry-point for `wal stream`. Pulled out
// of the cobra closure so unit tests can call it with a synthesized
// command + context without setting up the full CLI binary.
// probeSegmentSize probes the cluster's wal_segment_size and returns the
// value the streamer should chop and name segments with. A probe that
// can't run (old PG, transient connect failure) falls back to the 16 MiB
// default so a flaky preflight never blocks a valid stream — the connect
// error, if any, resurfaces in streamAttempt with proper retry/backoff.
// A probed value that is not a valid WAL segment size (a power of two in
// [1 MiB, 1 GiB]) is refused: PG cannot produce such a size, so it
// signals a broken probe or a non-PG endpoint rather than a real
// cluster, and streaming would mis-name segments.
func probeSegmentSize(ctx context.Context, dsn string) (int64, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return walsink.DefaultSegmentSize, nil
	}
	defer c.Close(ctx)
	got, err := pg.QueryWALSegmentSize(ctx, c)
	if err != nil {
		return walsink.DefaultSegmentSize, nil
	}
	if !walsink.ValidSegmentSize(got) {
		return 0, output.NewError("preflight.wal_segment_size",
			fmt.Sprintf("wal stream: cluster reports wal_segment_size=%s (%d bytes), which is not a valid WAL "+
				"segment size — PG uses a power of two in [1 MiB, 1 GiB]. Refusing to stream rather than "+
				"mis-name segments.", walSegSizeHuman(got), got))
	}
	return got, nil
}

// guardSystemIdentifier refuses to stream when the live cluster's
// system_identifier differs from the one the deployment's existing WAL
// was archived under. liveSysID == "" or allowChange == true skips the
// check; a probe/read failure or a deployment with no WAL yet also
// skips (conservative — never block a valid stream on a flaky
// preflight, and the first-ever stream establishes the baseline).
func guardSystemIdentifier(ctx context.Context, sp storage.StoragePlugin, deployment, liveSysID string, allowChange bool) error {
	if liveSysID == "" || allowChange {
		return nil
	}
	recorded, found, err := deploymentRecordedSysID(ctx, sp, deployment)
	if err != nil || !found || recorded == liveSysID {
		return nil
	}
	return output.NewError("preflight.system_identifier_changed",
		fmt.Sprintf("wal stream: this cluster's system_identifier is %s, but deployment %q previously "+
			"archived WAL under %s. A changed system identifier means a DIFFERENT cluster — typically a "+
			"pg_upgrade, a restore onto new storage, or a cloned datadir. Streaming the new cluster's WAL "+
			"into the existing lineage would interleave two incompatible clusters under one timeline and "+
			"corrupt point-in-time recovery. Refusing.", liveSysID, deployment, recorded)).
		WithSuggestion(&output.Suggestion{
			Human: "back up the new (e.g. pg_upgrade'd) cluster under a FRESH deployment name: the old lineage stays intact for PITR of the pre-upgrade cluster, and a new full backup establishes the new one. Only if you deliberately want to continue THIS deployment onto the new cluster, re-run with --allow-system-identifier-change.",
		})
}

// deploymentRecordedSysID returns the pg_control system_identifier the
// deployment's already-archived WAL was stamped with, read from the
// first committed segment manifest under wal/<deployment>/. found=false
// when no segment has been archived yet. The identifier is cluster-wide
// and stable across timelines, so any one segment manifest is
// representative; we stop at the first readable one.
func deploymentRecordedSysID(ctx context.Context, sp storage.StoragePlugin, deployment string) (string, bool, error) {
	prefix := fmt.Sprintf("wal/%s/", deployment)
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return "", false, lerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", false, cerr
		}
		key := info.Key
		// Per-segment manifests only: skip in-flight tmp files and the
		// history/ aux tree (history files carry no system_identifier).
		if !strings.HasSuffix(key, ".json") || strings.Contains(key, ".json.tmp.") {
			continue
		}
		if strings.Contains(key, "/history/") {
			continue
		}
		rc, err := sp.Get(ctx, key)
		if err != nil {
			// Racing prune/delete between List and Get — try the next key.
			continue
		}
		raw, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			continue
		}
		m, perr := walsink.ParseSegmentManifest(raw)
		if perr != nil || m.SystemIdentifier == "" {
			continue
		}
		return m.SystemIdentifier, true, nil
	}
	return "", false, nil
}

// walSegSizeHuman renders a power-of-two byte count the way PG does (16MB,
// 64MB, 1GB, 1MB), falling back to a raw count for anything unaligned.
func walSegSizeHuman(b int64) string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGB", b/(1<<30))
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMB", b/(1<<20))
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dkB", b/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

func runWalStream(cmd *cobra.Command, opts walStreamOptions) error {
	d := DispatcherFrom(cmd)

	if _, err := resolveDurability(opts.durability); err != nil {
		return output.NewError("usage.bad_flag", err.Error()).Wrap(output.ErrUsage)
	}
	if opts.slotName == "" {
		// PG replication slot names are [a-z0-9_]+ — hyphens
		// in the deployment name (e.g. "l3-pitr-forward") would
		// produce slot names PG rejects with SQLSTATE 42601
		// (syntax error).  Translate hyphens to underscores so
		// the deployment naming convention can stay free of
		// the slot-syntax constraint.
		safe := strings.ReplaceAll(opts.deployment, "-", "_")
		opts.slotName = "pg_hardstorage_" + safe
	}
	// Defence in depth: the slot name is interpolated UNQUOTED into the
	// replication-protocol commands (pglogrepl's CREATE_REPLICATION_SLOT
	// / START_REPLICATION — and EnsureSlot's "slot already exists" path
	// reaches START without running the create-side ValidIdentifier
	// gate). The downstream create path is already safe, but validate
	// here at the boundary too so a `--slot` (or a deployment-derived
	// default) carrying whitespace, quotes, ';', or control characters
	// can never steer the protocol tokenizer. We reject ONLY characters
	// outside [A-Za-z0-9_] — that blocks the entire injection surface
	// while still accepting anything PG itself could accept as a slot
	// name (it permits digit-start and is at most as strict on case),
	// so no pre-existing valid slot is newly refused.
	if !slotNameCharsSafe(opts.slotName) {
		return output.NewError("usage.bad_slot_name",
			fmt.Sprintf("wal stream: slot name %q must be 1–63 characters of [A-Za-z0-9_] "+
				"(PG replication slot grammar); derived from the deployment when --slot is omitted",
				opts.slotName)).Wrap(output.ErrUsage)
	}

	// Validate --start-lsn shape early.  Without this, an operator
	// typing `--start-lsn hm` would burn through preflight + connect
	// + ensureSlot first and surface a connect-time error with
	// exit 1 — masking the real problem (a typo) behind a
	// transport-class failure.  Mirrors the issue #78 fix for
	// --to-lsn.  The full segment-alignment check + assertion that
	// start >= slot.restart_lsn still lives in resolveStartLSN —
	// this guard only catches the shape-level garbage that should
	// never reach the streamer at all.
	if opts.startLSN != "" {
		// pglogrepl.ParseLSN silently truncates trailing garbage —
		// "0/3000028x" would parse to 0x3000028.  The strict
		// "<hex>/<hex>" shape check (shared with --to-lsn, see
		// internal/restore/LooksLikeLSN) goes ahead of the parser.
		if !restore.LooksLikeLSN(opts.startLSN) {
			return output.NewError("usage.bad_lsn",
				fmt.Sprintf("wal stream: --start-lsn %q: expected hex form like 0/3000028",
					opts.startLSN)).Wrap(output.ErrUsage)
		}
		if _, err := pglogrepl.ParseLSN(opts.startLSN); err != nil {
			return output.NewError("usage.bad_lsn",
				fmt.Sprintf("wal stream: --start-lsn %q: %v", opts.startLSN, err)).
				Wrap(output.ErrUsage)
		}
	}

	// 1. Open repo first — fail fast on misconfiguration before we
	//    touch PostgreSQL.
	//
	// DETACHED from the root signal context (values preserved): the
	// root command installs signal.NotifyContext, whose FIRST SIGINT
	// would cancel this ctx — and with it streamCtx — before the wal
	// handler below could run pg_switch_wal, killing the documented
	// graceful drain on every real Ctrl-C and letting the sink take
	// the hard-abort branch with repoCtx already cancelled
	// (concurrency audit). installSignalCancel is the SOLE
	// signal-to-cancel authority for the stream: first signal =
	// graceful (switch + flush, self-bounded ~10s), second = hard.
	repoCtx := context.WithoutCancel(cmd.Context())
	repoMeta, sp, err := repo.Open(repoCtx, opts.repoURL)
	if err != nil {
		return mapRepoOpenErr(opts.repoURL, err)
	}
	defer sp.Close()
	if err := assertRepoWritable(repoCtx, sp, "wal stream"); err != nil {
		return err
	}

	// Probe the cluster's wal_segment_size and stream + name segments
	// using it. PG packs 4 GiB / size segments per log-id, so the size
	// shapes both the chop boundary and the segment names. A probe that
	// can't run (old PG, transient failure) falls back to the 16 MiB
	// default so a flaky preflight never blocks a valid stream; a probed
	// value that is not a valid WAL segment size (power of two in
	// [1 MiB, 1 GiB]) is refused — it cannot be a real PG cluster.
	segSize, err := probeSegmentSize(repoCtx, opts.pgConn)
	if err != nil {
		return err
	}
	opts.segmentSize = segSize

	// Preflight: refuse to stream into a deployment whose existing WAL
	// belongs to a DIFFERENT cluster. A changed pg_control system
	// identifier is the signature of a pg_upgrade, a restore onto fresh
	// storage, or a cloned datadir — the new cluster's WAL is NOT a
	// continuation of the old lineage and interleaving them under one
	// timeline would corrupt PITR (a recovery could fetch segments from
	// the wrong cluster). A promotion/failover keeps the SAME sysid, so
	// this never trips on normal HA events. Probe failures degrade to
	// "allow" so a flaky preflight never blocks a valid stream.
	if liveID, idErr := identifySystem(repoCtx, opts.pgConn); idErr == nil {
		if err := guardSystemIdentifier(repoCtx, sp, opts.deployment, liveID.SystemID, opts.allowSysIDChange); err != nil {
			return err
		}
	}

	// Encrypt streamed WAL under the deployment's shared DEK when a local KEK
	// exists, closing the plaintext-at-rest gap that the startup warning used
	// to flag (issue #106). buildWALEncryption resolves or mints the shared
	// DEK (converging with base backups in either order) and returns the
	// encryptor plus the envelope stamped on every segment manifest. A nil
	// encryptor means no local KEK — the stream stays plaintext, exactly as
	// before.
	walEnc, walEncInfo, err := resolveWALEncryption(repoCtx, sp, opts.deployment, opts.kekRef, opts.kmsConfig)
	if err != nil {
		return output.NewError("wal.encryption_setup_failed",
			fmt.Sprintf("wal stream: %v", err)).Wrap(err)
	}

	// Durability mode (validated up-front by runWalStream) decides
	// how the streamer's CAS is built:
	//   per-segment — chunks DurabilityDeferred; finalizeSegment
	//                 flushes one cas.Barrier per 16 MiB segment
	//                 before the manifest commit (~1 syncfs/segment).
	//   per-chunk   — chunks DurabilityInline; every chunk fsync'd.
	durabilityMode, _ := resolveDurability(opts.durability)
	casOpts := []casdefault.Option{
		casdefault.WithCompressionLevel(repoMeta.Compression),
	}
	if durabilityMode != walsink.DurabilityPerChunk {
		casOpts = append(casOpts, casdefault.WithChunkDurability(storage.DurabilityDeferred))
	}
	var cas *repo.CAS
	if walEnc != nil {
		cas = casdefault.NewEncrypted(sp, walEnc, casOpts...)
	} else {
		cas = casdefault.New(sp, casOpts...)
	}

	// 2. Cancellable streaming context + signal handler.  Ctrl-C
	//    cancels the context, which terminates the retry loop and
	//    in-flight Stream call cleanly.  The shutdown struct
	//    carries the live sink and DSN so the signal handler can
	//    run pg_switch_wal + wait for the in-flight segment to
	//    flush before cancelling — the property that lets PITR
	//    target the most recent operator-visible LSN.
	streamCtx, cancel := context.WithCancel(repoCtx)
	defer cancel()
	shutdown := &walShutdown{gracefulDone: make(chan struct{})}
	dsnCopy := opts.pgConn
	shutdown.dsn.Store(&dsnCopy)
	installSignalCancel(streamCtx, cancel, shutdown)

	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"
	emit := func(e *output.Event) {
		if suppressEvents {
			return
		}
		_ = d.Event(streamCtx, e)
	}

	// 4. Per-attempt resume loop.  Each iteration re-runs
	//    identifySystem (timeline may have changed), ensureSlot
	//    (Strategy A/B/C), resolveStartLSN, then opens a fresh
	//    streaming connection and calls replication.Stream.
	//
	//    On clean ctx cancellation (signal received) the loop
	//    returns the success result body.  On stream connection
	//    break with --no-reconnect, the loop returns the wrapped
	//    error.  Otherwise the loop sleeps with exponential
	//    backoff and retries — this is the supervisor-free
	//    "keep streaming" property: a Patroni failover or PG
	//    bounce surfaces as a Stream error, and the next retry
	//    iteration finds the new leader (leader-aware DSN),
	//    runs EnsureSlot (Strategy A finds the propagated slot,
	//    Strategy C recreates with RESERVE_WAL), validates the
	//    resume LSN against the new slot's restart_lsn, and
	//    resumes streaming.
	startedAt := time.Now().UTC()
	const initialBackoff = time.Second
	backoff := initialBackoff
	var (
		firstStartLSN  pglogrepl.LSN
		firstStartSet  bool
		latestSynced   pglogrepl.LSN
		latestBuffered pglogrepl.LSN
		latestTimeline uint32
		attempt        int
		lastStreamErr  error // streamErr of the final attempt, for the honest clean_stop
		streamSysID    string
	)
	for {
		if streamCtx.Err() != nil {
			break
		}
		attempt++
		attemptWallStart := time.Now()
		streamErr, syncedAtExit, bufferedAtExit, attemptStart, attemptTLI, attemptInfo, attemptErr := streamAttempt(streamCtx, repoCtx, sp, cas, repoMeta, opts, d, emit, attempt, shutdown, walEncInfo)
		// System-identifier continuity across reconnects: the startup
		// guardSystemIdentifier check runs ONCE, but the retry loop
		// exists to survive failovers — and a failover/VIP repoint can
		// land the DSN on a DIFFERENT cluster (restored clone, wrongly
		// re-initialized standby). Without this recheck the next attempt
		// would archive the foreign cluster's WAL into the same
		// deployment lineage — exactly the corruption the startup
		// refusal prevents (concurrency audit). A sysid change is a
		// permanent, operator-actionable condition: stop, don't retry.
		if attemptInfo != "" {
			if perr := checkSysIDContinuity(&streamSysID, attemptInfo, opts.deployment, opts.allowSysIDChange); perr != nil {
				return perr
			}
		}
		if attemptErr != nil {
			// Pre-Stream setup error (preflight, ensureSlot,
			// resolveStartLSN, connect).  Treat as a connection-
			// class failure for retry purposes — most setup
			// failures clear once the new leader is up.
			if streamCtx.Err() != nil {
				break
			}
			// Permanent setup errors (operator must intervene)
			// bypass the retry loop.  Without this, a recycled-WAL
			// gap surfaces as a tight wal.stream.reconnecting loop
			// that masks the real, actionable structured error
			// (issue #79).  --no-reconnect already takes the same
			// exit; both arrive at the same return so the operator
			// sees one clean failure mode.
			if opts.noReconnect || isPermanentStreamSetupError(attemptErr) {
				return attemptErr
			}
			emit(output.NewEvent(output.SeverityWarning, "wal.stream", "reconnecting").
				WithBody(map[string]any{
					"attempt": attempt,
					"error":   attemptErr.Error(),
					"backoff": backoff.String(),
					"reason":  "setup_failure",
				}))
			if !sleepBackoff(streamCtx, backoff) {
				break
			}
			backoff = nextBackoff(backoff, opts.maxReconnectBackoff)
			continue
		}
		_ = attemptInfo
		lastStreamErr = streamErr
		if !firstStartSet {
			firstStartLSN = attemptStart
			firstStartSet = true
		}
		if syncedAtExit > latestSynced {
			latestSynced = syncedAtExit
		}
		if bufferedAtExit > latestBuffered {
			latestBuffered = bufferedAtExit
		}
		latestTimeline = attemptTLI

		if errors.Is(streamErr, context.Canceled) {
			// Clean exit via ctx cancellation (signal or --once).
			break
		}
		if opts.noReconnect {
			return output.NewError("wal.stream_error",
				fmt.Sprintf("wal stream: %v", streamErr)).Wrap(streamErr)
		}
		// Per-attempt connection that ended in an error — the most
		// common shape after a Patroni failover. A connection that
		// stayed up long enough to actually stream resets the backoff
		// (a clean reconnect starts at the floor); one that broke
		// almost immediately keeps escalating, so a flapping/instantly-
		// failing stream can't spin us in a tight full-setup reconnect
		// loop (CPU-pathology audit #1).
		reason := "stream_break"
		if errors.Is(streamErr, replication.ErrServerClosedStream) {
			// The SERVER ended the COPY (CopyDone) — the walsender is
			// going away, which during a Patroni switchover means the
			// primary is shutting down to demote. Reconnecting on the
			// usual 1s floor lands right back on the still-up, still-
			// read-write demoting node (target_session_attrs=primary
			// matches it until pg_ctl stop finishes), re-arming a
			// walsender that BLOCKS the very fast-shutdown in progress —
			// the demote then hangs until this process is restarted
			// (issue #34). Back off with an escalating grace floor
			// instead, so the node gets windows with zero walsenders to
			// complete its shutdown; the next reconnect then routes to
			// the new primary.
			backoff = serverClosedBackoff(backoff, opts.maxReconnectBackoff)
			reason = "server_closed_stream"
		} else {
			backoff = nextStreamBreakBackoff(time.Since(attemptWallStart), backoff, initialBackoff, opts.maxReconnectBackoff)
		}
		emit(output.NewEvent(output.SeverityWarning, "wal.stream", "reconnecting").
			WithBody(map[string]any{
				"attempt":    attempt,
				"error":      streamErr.Error(),
				"backoff":    backoff.String(),
				"synced_lsn": syncedAtExit.String(),
				"reason":     reason,
			}))
		if !sleepBackoff(streamCtx, backoff) {
			break
		}
	}

	stoppedAt := time.Now().UTC()
	// If a graceful stop is in flight, WAIT for it before judging the
	// stop: gracefulStopAndCancel stores switchLSN up to ~10s after the
	// signal (connect + pg_switch_wal + flush-wait), and reading it
	// mid-flight frequently saw 0 → a falsely optimistic clean_stop
	// with no stopped_with_unarchived_wal warning (concurrency audit).
	// Self-bounded: the goroutine's own timeouts cap this wait; the
	// extra timer is a belt-and-suspenders backstop only.
	if shutdown.gracefulStarted.Load() {
		select {
		case <-shutdown.gracefulDone:
		case <-time.After(15 * time.Second):
		}
	}
	// clean_stop must be honest: the streamer stopped cleanly only if
	// it exited without a hard error AND archived all WAL up to the
	// point a graceful stop's pg_switch_wal fixed. A streamer that
	// stopped with WAL still un-archived must NOT claim a clean stop —
	// an operator would otherwise believe that gap is safely in the
	// repo when a restore past synced_lsn would in fact fail.
	cleanStop := true
	var lagBytes uint64
	if lastStreamErr != nil && !errors.Is(lastStreamErr, context.Canceled) {
		cleanStop = false
	}
	if sw := shutdown.switchLSN.Load(); sw != 0 && uint64(latestSynced) < sw {
		cleanStop = false
		lagBytes = sw - uint64(latestSynced)
	}
	// Clamp both byte counters to >= 0.  latestSynced or latestBuffered
	// can legitimately be below firstStartLSN — most often when the
	// stream stopped before any 16 MiB segment had a chance to commit
	// (latestSynced stays at 0).  Reporting a negative count is a
	// reporting bug (issue #76), not a real LSN inversion.
	bytesAdvanced := int64(0)
	if uint64(latestSynced) > uint64(firstStartLSN) {
		bytesAdvanced = int64(uint64(latestSynced) - uint64(firstStartLSN))
	}
	bytesReceived := int64(0)
	if uint64(latestBuffered) > uint64(firstStartLSN) {
		bytesReceived = int64(uint64(latestBuffered) - uint64(firstStartLSN))
	}
	body := walStreamResultBody{
		Deployment:    opts.deployment,
		Slot:          opts.slotName,
		Timeline:      latestTimeline,
		StartLSN:      firstStartLSN.String(),
		SyncedLSN:     latestSynced.String(),
		ReceivedLSN:   latestBuffered.String(),
		BytesAdvanced: bytesAdvanced,
		BytesReceived: bytesReceived,
		StartedAt:     startedAt,
		StoppedAt:     stoppedAt,
		DurationMS:    stoppedAt.Sub(startedAt).Milliseconds(),
		CleanStop:     cleanStop,
	}
	if lagBytes > 0 {
		emit(output.NewEvent(output.SeverityWarning, "wal.stream", "stopped_with_unarchived_wal").
			WithSubject(output.Subject{Deployment: opts.deployment, Timeline: latestTimeline}).
			WithBody(map[string]any{
				"synced_lsn": latestSynced.String(),
				"lag_bytes":  lagBytes,
				"message": "the stream stopped before archiving all WAL up to the switch point; " +
					"the un-archived WAL is NOT in the repo — a restore past synced_lsn will fail " +
					"until the streamer is resumed and allowed to catch up",
			}))
	}
	// Operator-friendly note when we received bytes but stopped mid-
	// segment — `bytes_advanced` of 0 with a non-zero `bytes_received`
	// otherwise reads as "nothing happened" when actually the slot
	// observed activity, just not enough to cross a 16 MiB boundary.
	// PG resends the buffered bytes from `start_lsn` on the next
	// stream, so durability is preserved; this event makes the state
	// visible in the audit trail.
	if bytesAdvanced == 0 && bytesReceived > 0 {
		emit(output.NewEvent(output.SeverityInfo, "wal.stream", "stopped_mid_segment").
			WithSubject(output.Subject{Deployment: opts.deployment, Timeline: latestTimeline}).
			WithBody(map[string]any{
				"start_lsn":      firstStartLSN.String(),
				"received_lsn":   latestBuffered.String(),
				"bytes_received": bytesReceived,
				"message": "received bytes are buffered into a partial 16 MiB segment and were not committed; " +
					"PG will resend them from start_lsn on the next stream — slot durability is preserved",
			}))
	}
	emit(output.NewEvent(output.SeverityNotice, "wal.stream", "stopped").
		WithSubject(output.Subject{
			Deployment: opts.deployment,
			Timeline:   latestTimeline,
			LSN:        latestSynced.String(),
		}).
		WithBody(map[string]any{
			"duration_ms":    body.DurationMS,
			"bytes_advanced": body.BytesAdvanced,
			"bytes_received": body.BytesReceived,
			"received_lsn":   body.ReceivedLSN,
			"attempts":       attempt,
		}))
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// streamAttempt runs one connect-and-stream cycle: re-runs
// IDENTIFY_SYSTEM, ensureSlot, resolveStartLSN, opens a fresh
// replication connection, and calls replication.Stream.  Returns
// the Stream call's error (or context.Canceled on clean stop), the
// sink's SyncedLSN at exit, and metadata about the attempt for
// the caller's accounting.
//
// The setupErr return is non-nil when the attempt failed BEFORE
// reaching replication.Stream — preflight, ensureSlot,
// resolveStartLSN, or connect failure.  These are retryable in
// the same shape as Stream errors (a Patroni failover often
// surfaces as a connect-error first), so the caller treats them
// the same way.
func streamAttempt(
	streamCtx, repoCtx context.Context,
	sp storage.StoragePlugin,
	cas *repo.CAS,
	repoMeta *repo.Metadata,
	opts walStreamOptions,
	d *output.Dispatcher,
	emit func(*output.Event),
	attempt int,
	shutdown *walShutdown,
	encInfo *walsink.EncryptionInfo,
) (streamErr error, synced, buffered pglogrepl.LSN, startLSN pglogrepl.LSN, timeline uint32, identityID string, setupErr error) {
	// Preflight runs on every attempt by default — the new
	// leader after a Patroni failover may have different
	// configuration than the old, and "preflight passed at
	// startup" doesn't imply "preflight passes now".  Skipped
	// when --skip-preflight is set.
	if !opts.skipPreflight {
		role := inferRoleFromDSN(opts.pgConn)
		pf, perr := runPreflight(repoCtx, opts.pgConn, role, opts.slotName)
		if perr != nil {
			return nil, 0, 0, 0, 0, "", output.NewError("wal.preflight_failed",
				fmt.Sprintf("wal stream attempt %d: preflight: %v", attempt, perr)).Wrap(perr)
		}
		// Only emit findings on the first attempt and on any
		// fatal-finding attempts — repeating the info-level
		// findings on every reconnect would be noise.
		if attempt == 1 || pf.HasFatal() {
			for _, f := range pf.Findings {
				sev := output.SeverityNotice
				switch f.Severity {
				case replication.PreflightFatal:
					sev = output.SeverityError
				case replication.PreflightWarning:
					sev = output.SeverityWarning
				case replication.PreflightInfo:
					sev = output.SeverityInfo
				}
				_ = d.Event(repoCtx, output.NewEvent(sev, "wal.preflight", f.Code).
					WithBody(map[string]any{
						"message":    f.Message,
						"observed":   f.Observed,
						"required":   f.Required,
						"suggestion": f.Suggestion,
					}))
			}
		}
		if pf.HasFatal() {
			return nil, 0, 0, 0, 0, "", output.NewError("wal.preflight_fatal",
				"wal stream: preflight detected at least one fatal misconfiguration; refusing to start").
				WithSuggestion(&output.Suggestion{
					Human: "address each fatal finding emitted above, OR pass --skip-preflight to override (NOT recommended)",
				})
		}
	}

	identity, err := identifySystem(repoCtx, opts.pgConn)
	if err != nil {
		return nil, 0, 0, 0, 0, "", err
	}
	timeline = uint32(identity.Timeline)
	identityID = identity.SystemID

	var slotInfo *replication.SlotInfo
	if opts.noSlot {
		if attempt == 1 {
			_ = d.Event(repoCtx, output.NewEvent(output.SeverityWarning, "wal.slot", "disabled").
				WithBody(map[string]any{
					"message": "streaming without a replication slot — PG can recycle WAL out from under the streamer; use only when WAL retention is guaranteed another way (e.g. wal_keep_size)",
				}))
		}
	} else {
		slotInfo, err = ensureSlot(repoCtx, opts.pgConn, opts.slotName)
		if err != nil {
			return nil, 0, 0, 0, 0, "", err
		}
	}

	startLSN, resumeNote, err := resolveStartLSN(repoCtx, sp, opts, timeline, slotInfo)
	if err != nil {
		return nil, 0, 0, 0, timeline, identityID, err
	}

	// opts.durability was validated by runWalStream, so the error
	// here cannot fire — resolve it again to avoid threading the
	// mode through streamAttempt's already-long signature.
	durabilityMode, _ := resolveDurability(opts.durability)
	sink, err := walsink.New(cas, sp, walsink.Options{
		Deployment:       opts.deployment,
		Timeline:         timeline,
		SystemIdentifier: identityID,
		SegmentSize:      opts.segmentSize,
		Durability:       durabilityMode,
		// WORM thread: the streaming CAS already locks chunks
		// (casdefault.NewWithRetention above), but the segment MANIFEST
		// must be locked too — otherwise it can be deleted/overwritten
		// before its retention deadline, stranding the WORM-locked chunks
		// and making the segment unrecoverable (WORM bypassed for streamed
		// WAL). Mirrors the `wal push` path (PushOptions.WORM).
		WORM: repoMeta.WORM,
		// Stamp the shared-DEK envelope on each segment manifest so restore
		// resolves the DEK from the segment alone (issue #106). nil when the
		// stream is plaintext. The CAS above is built to match.
		Encryption: encInfo,
	})
	if err != nil {
		return nil, 0, 0, startLSN, timeline, identityID, output.NewError("internal", err.Error()).Wrap(err)
	}
	// Safety net: the sink runs a background processor goroutine that
	// must be stopped on every exit path (a connect failure below
	// returns before replication.Stream). Close is idempotent, so the
	// explicit drain on the streaming path is unaffected.
	defer func() { _ = sink.Close(repoCtx) }()

	// Publish the live sink so the signal handler's graceful-stop
	// goroutine can poll its SyncedLSN.  Cleared at the end of the
	// attempt so a between-attempts signal doesn't poll a stale
	// sink whose SyncedLSN can't advance.
	if shutdown != nil {
		shutdown.sink.Store(sink)
		defer shutdown.sink.Store(nil)
	}

	// --once watcher: derive a child context with its own cancel so
	// the once-watcher can actually stop the stream when the first
	// segment commits.  The previous shape passed `func() {}` as the
	// cancel — observing the segment did nothing, the streaming loop
	// kept running until the package-level test timeout fired and
	// killed every goroutine.  Surfaced in CI as
	// `TestIntegration_WalStream_OneSegment timed out after 10m`.
	onceCtx := streamCtx
	if opts.once {
		var onceCancel context.CancelFunc
		onceCtx, onceCancel = context.WithCancel(streamCtx)
		defer onceCancel()
		go watchOnceCancel(onceCtx, onceCancel, sink, startLSN)
	}

	// Pin application_name to the slot name so PG records this
	// agent under a stable identity in pg_stat_replication — what an
	// operator would name in synchronous_standby_names, and what the
	// preflight's sync-standby check looks for.
	streamConn, err := pg.Connect(repoCtx, opts.pgConn, pg.ModeReplication,
		pg.WithApplicationName(opts.slotName))
	if err != nil {
		return nil, 0, 0, startLSN, timeline, identityID, output.NewError("connect.replication",
			fmt.Sprintf("wal stream: open replication conn: %v", err)).Wrap(err)
	}
	defer streamConn.Close(repoCtx)

	// Enrich the starting event with the static-config snapshot
	// operators ask for when debugging a stream — what's the repo
	// already holding, how is it compressed/encrypted, where do we
	// pick up.  Runtime metrics (throughput, latency) need
	// accumulation and arrive in the wal.stream.progress event.
	// See GH issue #42.
	body := map[string]any{
		"attempt":         attempt,
		"slot":            opts.slotName,
		"start_lsn":       startLSN.String(),
		"resume_strategy": resumeNote,
		"system_id":       identityID,
		"status_interval": opts.statusInterval.String(),
		"compression":     string(repoMeta.Compression),
	}
	// last_lsn_in_repo: the highest LSN already archived for this
	// (deployment, timeline) before this attempt opened.  Cheap
	// one-shot lookup at startup; failures degrade to omitting the
	// field rather than blocking the start (issue #42).
	if lastLSN, found, err := inventory.HighestArchivedLSN(repoCtx, sp, opts.deployment, timeline); err == nil && found {
		body["last_lsn_in_repo"] = lastLSN.String()
	}
	// Encryption posture: true when a local KEK engaged shared-DEK
	// envelope encryption for this stream (issue #106), false when the
	// stream is plaintext (no KEK). Reported honestly so the operator
	// sees the actual at-rest posture.
	body["encryption"] = encInfo != nil
	if encInfo != nil {
		body["encryption_type"] = encInfo.Scheme
	}
	emit(output.NewEvent(output.SeverityInfo, "wal.stream", "starting").
		WithSubject(output.Subject{
			Deployment: opts.deployment,
			Timeline:   timeline,
		}).
		WithBody(body))

	// Interactive progress ticker — issue #53.  Wired AFTER the
	// starting event so the operator's terminal output reads
	// "starting → progress → progress → ... → stopped" in order.
	// Stop on every exit path (Stream may return cleanly, with
	// an error, or after onceCtx is cancelled) so the goroutine
	// can't outlive the streamAttempt.
	if opts.verbose && opts.statusInterval > 0 {
		ticker := newWalStreamProgressTicker(
			opts.statusInterval, sink, emit,
			opts.deployment, timeline, startLSN)
		ticker.Start(onceCtx)
		defer ticker.Stop()
	}

	streamErr = replication.Stream(onceCtx, streamConn, replication.StreamOptions{
		Slot:                 opts.slotName,
		StartLSN:             startLSN,
		Timeline:             timeline,
		StatusUpdateInterval: opts.statusInterval,
		InactivityTimeout:    resolveInactivityTimeout(opts),
	}, sink)

	// Drain the async sink: the receive side has stopped, but the
	// processor may still be chunking/committing handed-off segments.
	// Close commits every one of them so SyncedLSN reflects all WAL
	// that was received before the stream stopped. repoCtx is alive on
	// a graceful stop (full drain) and cancelled only on a hard
	// shutdown (prompt abort). A processing error must surface even on
	// a clean ctx-cancel stop, so it overrides a bare context.Canceled.
	if cerr := sink.Close(repoCtx); cerr != nil {
		if streamErr == nil || errors.Is(streamErr, context.Canceled) {
			streamErr = cerr
		}
	}
	synced = sink.SyncedLSN()
	buffered = sink.BufferedLSN()
	return streamErr, synced, buffered, startLSN, timeline, identityID, nil
}

// resolveInactivityTimeout translates the streamer's CLI flags into
// the streaming.Reader's tri-state convention:
//
//	> 0  — exact value (operator-supplied --inactivity-timeout)
//	= 0  — use streaming.DefaultInactivityTimeout
//	< 0  — disable the watchdog entirely (--no-inactivity-timeout)
//
// An explicit --inactivity-timeout duration wins over the
// --no-inactivity-timeout bool: operators who pass both presumably
// want the duration.  Issue #12 documents the failure mode the
// bool flag is here to fix.
func resolveInactivityTimeout(opts walStreamOptions) time.Duration {
	if opts.inactivityTimeout != 0 {
		return opts.inactivityTimeout
	}
	if opts.noInactivityTimeout {
		return -1
	}
	return 0
}

// sleepBackoff sleeps for d, returning false if streamCtx is
// cancelled during the sleep (signalling a clean stop the caller
// should propagate by breaking out of the retry loop).
func sleepBackoff(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextBackoff doubles cur up to max.  Caller-provided max so
// tests can pin a small value.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if max > 0 && next > max {
		next = max
	}
	return next
}

// minHealthyStreamDuration is how long a stream attempt must stay
// connected before a break is treated as a genuine reconnect (backoff
// resets to the floor) rather than a flap. Setup + START_REPLICATION
// take well under a second, so a connection that lasts at least this
// long actually streamed; one that breaks faster is flapping.
const minHealthyStreamDuration = 5 * time.Second

// serverClosedGrace is the minimum delay before reconnecting after the
// SERVER ended the COPY (CopyDone). It must be long enough that a
// demoting primary gets a walsender-free window to finish its
// fast-shutdown before we reconnect and re-arm one (issue #34).
const serverClosedGrace = 10 * time.Second

// serverClosedBackoff computes the reconnect delay after a server-
// initiated CopyDone: an escalating backoff that NEVER resets to the
// 1s floor and never drops below serverClosedGrace, so repeated
// re-arms during a slow demote back off increasingly instead of
// hammering the shutting-down node once per second.
func serverClosedBackoff(prev, max time.Duration) time.Duration {
	next := nextBackoff(prev, max)
	if next < serverClosedGrace {
		next = serverClosedGrace
	}
	if max > 0 && next > max {
		next = max
	}
	return next
}

// nextStreamBreakBackoff decides the reconnect backoff after a stream
// attempt that CONNECTED and then broke. A connection that stayed up
// for at least minHealthyStreamDuration is a real reconnect — reset to
// the floor. One that broke almost immediately (a flapping slot, an
// instantly-failing connection, a PG that accepts then drops) keeps
// ESCALATING, so we don't spin in a tight full-setup reconnect loop
// burning CPU + connection churn every initialBackoff. (CPU-pathology
// audit #1.)
func nextStreamBreakBackoff(streamDuration, prevBackoff, initial, max time.Duration) time.Duration {
	if streamDuration >= minHealthyStreamDuration {
		return initial
	}
	return nextBackoff(prevBackoff, max)
}

// isPermanentStreamSetupError reports whether err describes a setup
// failure no amount of reconnecting can heal — the operator must
// intervene (drop the slot, pass --start-lsn, fix grants).  The retry
// loop short-circuits on these so the operator sees the structured
// error once instead of an unbounded "wal.stream.reconnecting" spam
// (issue #79).
//
// The set is deliberately small and conservative: only error codes
// whose remediation requires operator action are listed.  Transient
// classes (connect.replication, pg.identify_failed) stay on the retry
// path because a Patroni failover legitimately surfaces as those.
// checkSysIDContinuity pins the stream's system identifier to the first
// attempt's value and refuses any later attempt that reports a
// different one (unless the operator passed --allow-sysid-change). See
// the retry-loop comment: without this, a reconnect after a DSN
// repoint to a different cluster interleaves foreign WAL into the
// deployment's lineage.
func checkSysIDContinuity(pinned *string, observed, deployment string, allowChange bool) error {
	if *pinned == "" {
		*pinned = observed
		return nil
	}
	if observed == *pinned || allowChange {
		return nil
	}
	return output.NewError("wal.system_identifier_changed",
		fmt.Sprintf("wal stream: system identifier changed across reconnect for %q: streaming began against cluster %s but the connection now reaches cluster %s — refusing to interleave a different cluster's WAL into this deployment's lineage", deployment, *pinned, observed)).
		WithSuggestion(&output.Suggestion{
			Human: "the DSN now resolves to a different PostgreSQL cluster (restored clone, re-initialized standby, or a repointed VIP/DNS entry). Point the DSN back at the original cluster, or — if the change is intentional — archive the new cluster under a NEW deployment name (or pass --allow-system-identifier-change after reading the pg_upgrade runbook).",
		})
}

func isPermanentStreamSetupError(err error) bool {
	oe, ok := output.AsOutputError(err)
	if !ok {
		return false
	}
	// Every usage.* code is an operator-input error (bad DSN, bad LSN,
	// bad flag, ...) — retrying can never fix it. Before this blanket
	// rule, a literal `--pg-connection ...` placeholder pasted from a
	// hint retried `usage.bad_pg_dsn` forever with backoff instead of
	// failing fast.
	if strings.HasPrefix(oe.Code, "usage.") {
		return true
	}
	switch oe.Code {
	case "wal.start_before_slot_restart_lsn",
		"wal.slot_no_restart_lsn":
		return true
	}
	return false
}

// identifySystem opens a short-lived replication-mode conn just to
// run IDENTIFY_SYSTEM. We don't reuse the streaming conn for this
// because the streaming conn is hijacked; running IDENTIFY_SYSTEM
// against it would fight the streaming.Reader's lifecycle.
func identifySystem(ctx context.Context, dsn string) (pg.SystemIdentity, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return pg.SystemIdentity{}, output.NewError("connect.replication",
			fmt.Sprintf("wal stream: open replication conn for IDENTIFY_SYSTEM: %v", err)).Wrap(err)
	}
	defer c.Close(ctx)
	id, err := pg.IdentifySystem(ctx, c)
	if err != nil {
		return pg.SystemIdentity{}, output.NewError("pg.identify_failed",
			fmt.Sprintf("wal stream: IDENTIFY_SYSTEM: %v", err)).Wrap(err)
	}
	return id, nil
}

// ensureSlot finds-or-creates the persistent physical slot using
// RESERVE_WAL so that the slot's restart_lsn is populated immediately
// at creation time.  This is the property that makes the streamer
// safe against PG recycling WAL out from under it during the gap
// between slot create and START_REPLICATION — without RESERVE_WAL
// the slot doesn't pin any LSN until first START_REPLICATION, and a
// busy primary can recycle past the segment we'd ask for.
//
// Implementation reuses replication.EnsureSlot (the Patroni-aware
// continuity primitive); on a fresh single-node deployment that
// reduces to "create with RESERVE_WAL".  On resume it returns the
// existing slot's restart_lsn for the caller's safety check.
//
// Returns the slot's *current* state (restart_lsn populated) so
// runWalStream can validate the resume LSN against it.
func ensureSlot(ctx context.Context, dsn, slotName string) (*replication.SlotInfo, error) {
	regConn, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return nil, output.NewError("connect.regular",
			fmt.Sprintf("wal stream: open regular conn for slot probe: %v", err)).Wrap(err)
	}
	defer regConn.Close(ctx)

	replConn, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return nil, output.NewError("connect.replication",
			fmt.Sprintf("wal stream: open replication conn for slot create: %v", err)).Wrap(err)
	}
	defer replConn.Close(ctx)

	res, err := replication.EnsureSlot(ctx, regConn, replConn, slotName, 0)
	if err != nil {
		return nil, output.NewError("wal.slot_ensure_failed",
			fmt.Sprintf("wal stream: ensure slot %q: %v", slotName, err)).
			WithSuggestion(&output.Suggestion{
				Human: "the replication user needs the REPLICATION attribute and pg_hba.conf must permit `replication` from this host",
			}).Wrap(err)
	}
	if res == nil || res.Slot == nil {
		return nil, output.NewError("wal.slot_ensure_failed",
			fmt.Sprintf("wal stream: ensure slot %q returned no slot info", slotName))
	}
	if res.Slot.RestartLSN == "" {
		// RESERVE_WAL guarantees a populated restart_lsn on PG 15+.
		// An empty value here means either an ancient PG or a
		// concurrent DROP_REPLICATION_SLOT race; either way we
		// refuse to start streaming because the slot is not
		// pinning any WAL.
		return nil, output.NewError("wal.slot_no_restart_lsn",
			fmt.Sprintf("wal stream: slot %q has empty restart_lsn after ensure (slot is not pinning WAL — refusing to stream)",
				slotName)).
			WithSuggestion(&output.Suggestion{
				Human: "drop the slot manually (`SELECT pg_drop_replication_slot('" + slotName + "')`) and retry; the next start will recreate it with RESERVE_WAL",
			})
	}
	return res.Slot, nil
}

// resolveStartLSN computes the LSN to pass as START_REPLICATION's
// start position.  Precedence:
//
//  1. opts.startLSN (CLI flag) — explicit, parsed, must be segment-aligned.
//  2. Highest committed segment in the repo for this deployment+timeline.
//  3. Slot's restart_lsn (RESERVE_WAL) aligned UP to the next segment
//     boundary — fresh-deployment path.
//
// The slotInfo argument carries the slot's current restart_lsn so
// this function can both (a) ground the fresh-deployment case in a
// position PG provably retains and (b) refuse to return a value
// older than restart_lsn (which would mean PG already recycled what
// we'd ask for — the bug the proper-slot work fixes).
//
// Note string explains which path was taken so it lands in the
// start event's body for operator-visible diagnostics.
func resolveStartLSN(ctx context.Context, sp storage.StoragePlugin, opts walStreamOptions, timeline uint32, slotInfo *replication.SlotInfo) (pglogrepl.LSN, string, error) {
	// Slot restart_lsn is the floor — any computed start LSN below
	// it would ask PG for WAL it may have already recycled (or, if
	// max_slot_wal_keep_size kicked in, definitely has).  Parse it
	// up front so the three precedence branches can validate
	// against the same value.
	var restartLSN pglogrepl.LSN
	if slotInfo != nil && slotInfo.RestartLSN != "" {
		parsed, perr := pglogrepl.ParseLSN(slotInfo.RestartLSN)
		if perr != nil {
			return 0, "", output.NewError("internal",
				fmt.Sprintf("wal stream: parse slot restart_lsn %q: %v", slotInfo.RestartLSN, perr)).Wrap(perr)
		}
		restartLSN = parsed
	}

	segSize := walsink.NormSegmentSize(opts.segmentSize)

	if opts.startLSN != "" {
		lsn, err := pglogrepl.ParseLSN(opts.startLSN)
		if err != nil {
			return 0, "", output.NewError("usage.bad_lsn",
				fmt.Sprintf("wal stream: --start-lsn %q: %v", opts.startLSN, err)).Wrap(output.ErrUsage)
		}
		if uint64(lsn)%uint64(segSize) != 0 {
			return 0, "", output.NewError("usage.unaligned_lsn",
				fmt.Sprintf("wal stream: --start-lsn %s is not segment-aligned (must be a multiple of %d)",
					opts.startLSN, segSize)).Wrap(output.ErrUsage)
		}
		if err := assertStartGEQRestart(lsn, restartLSN, "explicit-flag", segSize); err != nil {
			return 0, "", err
		}
		return lsn, "explicit-flag", nil
	}

	// Resume point: the end LSN of the highest committed segment for
	// this deployment+timeline. inventory.HighestArchivedLSN reads it
	// from that segment's manifest, so it is exact and segment-size-
	// agnostic (no 16 MiB assumption, correct across a >4 GiB log-id
	// roll).
	endLSN, found, err := inventory.HighestArchivedLSN(ctx, sp, opts.deployment, timeline)
	if err != nil {
		return 0, "", output.NewError("repo.list_failed",
			fmt.Sprintf("wal stream: list committed segments: %v", err)).Wrap(err)
	}
	if found {
		if err := assertStartGEQRestart(endLSN, restartLSN, "resume-from-repo", segSize); err != nil {
			return 0, "", err
		}
		return endLSN, "resume-from-repo", nil
	}

	// Fresh deployment: no committed segments yet.  Use the slot's
	// restart_lsn as the anchor — PG provably retains WAL from
	// restart_lsn onwards (RESERVE_WAL on slot create), and the
	// segment CONTAINING restart_lsn is retained too (slot pinned
	// it).  Align DOWN to that segment's start boundary so:
	//
	//   - walsink's segment-aligned commit invariant holds;
	//   - the start LSN never outruns PG's current WAL flush
	//     position (which would surface as "requested starting
	//     point X is ahead of the WAL flush position Y");
	//   - we get the bytes from segment-start through restart_lsn
	//     "for free" — they're already retained inside the
	//     retained segment.
	//
	// Aligning UP would push the start past PG's current flush
	// position on a quiet primary and fail with the
	// ahead-of-flush error above.  Aligning DOWN is what makes
	// the slot's retention guarantee operational.
	if restartLSN > 0 {
		alignedDown := pglogrepl.LSN(uint64(restartLSN) &^ uint64(segSize-1))
		return alignedDown, "fresh-slot-restart-lsn", nil
	}
	// Slot info absent (legacy --no-slot path or unit-test stub
	// without a pgConn) — fall back to the historical behaviour.
	// This branch is the only place LSN(0) can leak through, and
	// only when the operator opted out of the slot.
	if opts.pgConn == "" {
		return pglogrepl.LSN(0), "fresh-no-slot-no-conn", nil
	}
	curLSN, qerr := queryCurrentWalInsertLSN(ctx, opts.pgConn)
	if qerr != nil {
		return pglogrepl.LSN(0), "fresh-no-slot-query-failed", nil
	}
	alignedLSN := pglogrepl.LSN(uint64(curLSN) &^ uint64(segSize-1))
	return alignedLSN, "fresh-no-slot-current", nil
}

// assertStartGEQRestart refuses to return a start LSN whose
// containing WAL segment is older than the segment containing the
// slot's restart_lsn.  PG retains WAL by segment, not by byte: the
// segment containing restart_lsn is pinned, segments before it are
// recyclable.  The check therefore compares segment-floors, not
// raw LSNs — start_lsn aligned DOWN to its segment must be ≥
// restart_lsn aligned DOWN to its segment.
//
// Why segment-floor comparison: a fresh-slot start LSN of
// alignedDown(restart_lsn) is below restart_lsn in raw bytes (e.g.
// restart_lsn = 0/3000800 → aligned-down = 0/3000000), but
// references the same segment PG provably retains.  A raw-LSN
// "start >= restart" check would refuse this safe case and mask
// the real bug it's meant to catch (asking for a segment older
// than the slot's pinned one).
//
// restartLSN of 0 means "no slot info" (legacy --no-slot path) — in
// that case we accept any start position because we have nothing
// to compare against.  All callers that pass real slot info will
// have a non-zero restartLSN.
func assertStartGEQRestart(start, restartLSN pglogrepl.LSN, branch string, segSize int64) error {
	if restartLSN == 0 {
		return nil
	}
	startSeg := uint64(start) &^ uint64(segSize-1)
	restartSeg := uint64(restartLSN) &^ uint64(segSize-1)
	if startSeg >= restartSeg {
		return nil
	}
	return output.NewError("wal.start_before_slot_restart_lsn",
		fmt.Sprintf("wal stream: computed start LSN %s (%s) sits in a WAL segment older than the slot's restart_lsn %s — PG has already recycled past this point and would refuse the stream",
			start.String(), branch, restartLSN.String())).
		WithSuggestion(&output.Suggestion{
			Human: "the WAL between the highest archived segment and the slot's restart_lsn is unrecoverable — taking a fresh full backup ALONE does not heal this (the wal-stream resume looks at archived segments, not at backups). To restart cleanly: (1) take a fresh full backup (`pg_hardstorage backup <deployment>`), (2) drop the replication slot (`SELECT pg_drop_replication_slot('" + "<slot>" + "')` on the primary — the next `wal stream` recreates it with RESERVE_WAL anchored at PG's current position), (3) restart `wal stream`. Alternatively pass `--start-lsn=<segment-aligned LSN at or after restart_lsn>` to acknowledge the gap and resume from there.",
		})
}

// queryCurrentWalInsertLSN runs `SELECT pg_current_wal_insert_lsn()`
// against pgConn and returns the result.  Used by the fresh-slot
// path of resolveStartLSN to pick a start LSN that PG actually
// has on disk, instead of the literal LSN 0/0 which PG interprets
// as "segment 0" (long since recycled on a running cluster).
func queryCurrentWalInsertLSN(ctx context.Context, pgConn string) (pglogrepl.LSN, error) {
	conn, err := pg.Connect(ctx, pgConn, pg.ModeRegular)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	var lsnText string
	rows, err := conn.PgConn().Exec(ctx, "SELECT pg_current_wal_insert_lsn()::text").ReadAll()
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	if len(rows) == 0 || len(rows[0].Rows) == 0 || len(rows[0].Rows[0]) == 0 {
		return 0, fmt.Errorf("empty result from pg_current_wal_insert_lsn()")
	}
	lsnText = string(rows[0].Rows[0][0])
	parsed, err := pglogrepl.ParseLSN(lsnText)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", lsnText, err)
	}
	return parsed, nil
}

// highestCommittedSegment walks wal/<deployment>/<timeline-hex>/ and
// returns the highest segment number whose `<name>.json` manifest
// exists. found=false when there are no segments at all.
//
// This is intentionally cheap: we don't read manifest bodies, we
// only need the segment-number encoded in the file name.
func highestCommittedSegment(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (uint64, bool, error) {
	prefix := fmt.Sprintf("wal/%s/%08X/", deployment, timeline)
	var maxSeg uint64
	any := false
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return 0, false, err
		}
		// Cooperative cancellation: same shape as the gc walks.
		// Long WAL retention windows can produce hundreds of
		// thousands of segments per timeline; the operator's
		// Ctrl-C should reach this loop without waiting for the
		// underlying List to complete.
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		// Extract the bare 24-char segment name. info.Key looks like
		// "wal/<dep>/<TLI>/<24chars>.json".
		key := info.Key
		const wantSuffix = ".json"
		if !strings.HasSuffix(key, wantSuffix) {
			continue
		}
		// Skip in-flight tmp files (foo.json.tmp.*).
		if strings.Contains(key, ".json.tmp.") {
			continue
		}
		base := key[len(prefix) : len(key)-len(wantSuffix)]
		if len(base) != 24 {
			// Defensive: ignore anything that doesn't look like a
			// canonical segment name. Lets the repo grow auxiliary
			// metadata files alongside without breaking us.
			continue
		}
		segNum, ok := parseSegmentNumber(base)
		if !ok {
			continue
		}
		if !any || segNum > maxSeg {
			maxSeg = segNum
			any = true
		}
	}
	return maxSeg, any, nil
}

// parseSegmentNumber extracts the segment number from a 24-char
// canonical PG WAL segment name. Layout:
//
//	TTTTTTTT LLLLLLLL SSSSSSSS
//	timeline log_id   seg_in_log
//
// segment_number = log_id * 256 + seg_in_log (256 = segments-per-log
// for the canonical 16 MiB segment size).
func parseSegmentNumber(name string) (uint64, bool) {
	if len(name) != 24 {
		return 0, false
	}
	logID, err1 := parseHex32(name[8:16])
	segLo, err2 := parseHex32(name[16:24])
	if err1 != nil || err2 != nil {
		return 0, false
	}
	const segmentsPerLog = 256
	return uint64(logID)*segmentsPerLog + uint64(segLo), true
}

func parseHex32(s string) (uint32, error) {
	if len(s) != 8 {
		return 0, fmt.Errorf("hex32: want 8 chars, got %d", len(s))
	}
	var v uint32
	for i := 0; i < 8; i++ {
		c := s[i]
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, fmt.Errorf("hex32: non-hex character %q at index %d", c, i)
		}
		v = (v << 4) | d
	}
	return v, nil
}

// installSignalCancel cancels ctx on SIGINT or SIGTERM, after first
// running a graceful-stop sequence that lets the streamer finalize
// the segment containing the operator's stop point.
//
// Without this, a signal-driven cancel discards whatever WAL the
// streamer was assembling in memory (per-segment commits are
// atomic; partial segments are not), leaving the most recent few
// MB of WAL outside the repo.  PITR targets that fell inside the
// last segment then fail recovery with "recovery ended before
// configured recovery target was reached".
//
// The graceful sequence:
//
//  1. Open a fresh regular-mode conn (the replication conn is
//     hijacked by the streamer's receive loop).
//  2. Run `SELECT pg_switch_wal()` — PG closes the current
//     segment at its current insert position, returns that LSN
//     (the "switch LSN").
//  3. Poll the live sink's SyncedLSN until it advances past the
//     switch LSN, or a 5-second deadline expires.
//  4. Cancel the streaming context.
//
// On signal #2 (operator impatient), cancel immediately without
// waiting for the flush.
//
// shutdown.sink is read atomically because with auto-reconnect
// the sink object changes between attempts.  An empty sink ptr
// (set when the streamer is between attempts) skips the wait —
// there's no in-flight segment to finalize.
func installSignalCancel(ctx context.Context, cancel context.CancelFunc, shutdown *walShutdown) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sig)
		select {
		case <-sig:
		case <-ctx.Done():
			// The stream ended on its own (error, --once). Mark the
			// graceful path as never-started so nobody waits on it.
			close(shutdown.gracefulDone)
			return
		}
		// First signal: graceful. gracefulStarted lets the result
		// path know it must WAIT for gracefulDone before reading
		// switchLSN — reading it mid-flight produced a racy, falsely
		// optimistic clean_stop (concurrency audit).
		shutdown.gracefulStarted.Store(true)
		go func() {
			defer close(shutdown.gracefulDone)
			gracefulStopAndCancel(shutdown, cancel)
		}()
		// Second signal: cancel immediately without waiting for
		// the flush.  The graceful goroutine above is already
		// racing to finish; whichever wins, cancel is idempotent.
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()
}

// walShutdown is the shared state the signal handler reads when
// orchestrating a graceful stop.  Updated by the per-attempt loop
// as the live sink and DSN change.
type walShutdown struct {
	sink atomic.Pointer[walsink.Sink]
	dsn  atomic.Pointer[string]
	// switchLSN is the LSN pg_switch_wal returned during a graceful
	// stop. 0 means no graceful stop ran (a hard cancel, --once, or a
	// stream error). When non-zero it is the bar the streamer's
	// SyncedLSN must have reached for the stop to count as clean.
	switchLSN atomic.Uint64
	// gracefulStarted flips when the first signal launches the
	// graceful-stop goroutine; gracefulDone closes when that goroutine
	// (or the no-signal exit path) finishes. The result path waits on
	// it so clean_stop/switchLSN are read only after pg_switch_wal had
	// its chance to record the bar — not mid-flight.
	gracefulStarted atomic.Bool
	gracefulDone    chan struct{}
}

// gracefulStopAndCancel implements the signal-driven graceful stop
// sequence (see installSignalCancel).  Runs until either:
//   - the live sink's SyncedLSN advances past pg_switch_wal's
//     switch LSN, OR
//   - a 5-second wall-clock deadline expires.
//
// In all cases it calls cancel() at the end so the streamer's
// retry loop terminates.  Errors from PG (switch_wal not
// permitted on a replica, conn refused, etc.) cause a fast cancel
// without flush — best-effort.
func gracefulStopAndCancel(shutdown *walShutdown, cancel context.CancelFunc) {
	defer cancel()
	if shutdown == nil {
		return
	}
	dsnPtr := shutdown.dsn.Load()
	if dsnPtr == nil || *dsnPtr == "" {
		return
	}
	ctx, cleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanup()
	c, err := pg.Connect(ctx, *dsnPtr, pg.ModeRegular)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	res := c.PgConn().ExecParams(ctx, "SELECT pg_switch_wal()::text",
		nil, nil, nil, nil).Read()
	if res.Err != nil || len(res.Rows) == 0 {
		return
	}
	switchLSN, perr := pglogrepl.ParseLSN(string(res.Rows[0][0]))
	if perr != nil {
		return
	}
	// Record the switch point so the stopped-stream result can report
	// honestly whether the streamer archived everything up to it.
	shutdown.switchLSN.Store(uint64(switchLSN))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := shutdown.sink.Load()
		if s != nil && uint64(s.SyncedLSN()) >= uint64(switchLSN) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// watchOnceCancel polls SyncedLSN and cancels after the first
// segment commit (SyncedLSN > startLSN). The poll cadence is short
// so the test-and-development feedback loop stays tight; the cost is
// trivially small (one atomic load every 50 ms).
func watchOnceCancel(ctx context.Context, cancel context.CancelFunc, sink *walsink.Sink, start pglogrepl.LSN) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if uint64(sink.SyncedLSN()) > uint64(start) {
				cancel()
				return
			}
		}
	}
}

// mapRepoOpenErr translates repo.Open failures into the structured
// errors the CLI contract uses; mirrors the runner's mapRepoErr.
func mapRepoOpenErr(repoURL string, err error) error {
	if errors.Is(err, repo.ErrNotARepo) {
		return output.NewError("notfound.repo",
			fmt.Sprintf("wal stream: no pg_hardstorage repository at %s", repoURL)).
			WithSuggestion(&output.Suggestion{
				Human:   "create the repository first",
				Command: "pg_hardstorage repo init " + repoURL,
			}).Wrap(err)
	}
	if errors.Is(err, storage.ErrUnknownScheme) {
		return output.NewError("usage.unknown_scheme", err.Error()).Wrap(output.ErrUsage)
	}
	return output.NewError("repo.open_failed",
		fmt.Sprintf("wal stream: open repo %s: %v", repoURL, err)).Wrap(err)
}

// walStreamResultBody is the typed Result body for `wal stream`. The
// shape is stable per the v1 schema commitment.
type walStreamResultBody struct {
	Deployment string `json:"deployment"`
	Slot       string `json:"slot"`
	Timeline   uint32 `json:"timeline"`
	StartLSN   string `json:"start_lsn"`
	// SyncedLSN is the end LSN of the most recently committed 16 MiB
	// segment.  When zero, no segment completed during this run;
	// ReceivedLSN may still be non-zero in that case.
	SyncedLSN string `json:"synced_lsn"`
	// ReceivedLSN is the highest end-of-record LSN the streamer
	// observed from the upstream — including bytes that are buffered
	// into a partial segment and were NOT committed.  Reported for
	// diagnostics so an operator who stops mid-segment can see that
	// the streamer was actively receiving bytes even though no
	// segment crossed the commit boundary.  PG retransmits buffered
	// bytes from StartLSN on the next stream, so durability is
	// preserved.
	ReceivedLSN string `json:"received_lsn"`
	// BytesAdvanced is the number of WAL bytes whose containing
	// segment landed in the repo this run.  Always >= 0.
	BytesAdvanced int64 `json:"bytes_advanced"`
	// BytesReceived is the number of WAL bytes the streamer received
	// from the upstream this run (committed + buffered).  Always >= 0.
	BytesReceived int64     `json:"bytes_received"`
	StartedAt     time.Time `json:"started_at"`
	StoppedAt     time.Time `json:"stopped_at"`
	DurationMS    int64     `json:"duration_ms"`
	CleanStop     bool      `json:"clean_stop"`
}

// WriteText is the text-renderer hook. Compact and operator-readable.
func (b walStreamResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.CleanStop {
		fmt.Fprintln(bw, "✓ WAL stream stopped cleanly")
	} else {
		fmt.Fprintln(bw, "✗ WAL stream terminated")
	}
	fmt.Fprintf(bw, "  Deployment:   %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Slot:         %s\n", b.Slot)
	fmt.Fprintf(bw, "  Timeline:     %d\n", b.Timeline)
	fmt.Fprintf(bw, "  Start LSN:    %s\n", b.StartLSN)
	fmt.Fprintf(bw, "  Synced LSN:   %s\n", b.SyncedLSN)
	// Only show ReceivedLSN / Received when it differs meaningfully
	// from the committed view — keeps the happy-path summary compact
	// while preserving the diagnostic value on a mid-segment stop.
	if b.BytesReceived != b.BytesAdvanced {
		fmt.Fprintf(bw, "  Received LSN: %s\n", b.ReceivedLSN)
		fmt.Fprintf(bw, "  Received:     %s (buffered, not committed; PG resends on next stream)\n",
			humanBytes(b.BytesReceived))
	}
	fmt.Fprintf(bw, "  Advanced:     %s\n", humanBytes(b.BytesAdvanced))
	fmt.Fprintf(bw, "  Duration:     %s", time.Duration(b.DurationMS)*time.Millisecond)
	_, err := io.WriteString(w, bw.String())
	return err
}
