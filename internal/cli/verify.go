// verify.go — 'verify' CLI verb: "is this backup actually restorable?" without sandbox+WAL replay.
package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// newVerifyCmd implements `pg_hardstorage verify <deployment>
// [latest|<backup-id>]`. It is the operator-facing surface of
// "is this backup actually restorable?" — without taking up a
// sandbox or replaying WAL.
//
// The check has two layers:
//
//  1. Manifest signature — we read the manifest through ManifestStore,
//     which verifies its Ed25519 signature against the local public
//     key. A failure here is the loud "this backup wasn't signed by
//     someone you trust" error.
//  2. Chunk round-trip — every chunk the manifest references is
//     fetched, decrypted (if encrypted), decompressed, and the
//     plaintext SHA-256 is asserted to match the chunk's key.
//     Duplicate chunk hashes (the same chunk referenced by multiple
//     files) are de-duplicated so the work is O(unique chunks).
//
// Sandbox restore + `pg_amcheck` is the "full verify" — see
// the verifier subsystem in the SPEC. v0.1's `verify` is the cheap
// fast verify the runner triggers automatically after every
// successful backup commit, but addressable on demand via this
// command for ad-hoc checks ("did this backup survive the move?").
func newVerifyCmd() *cobra.Command {
	var (
		repoURL       string
		sample        int
		existenceOnly bool
		dispatch      dispatchAuthFlags
	)
	c := &cobra.Command{
		Use:   "verify <deployment> [latest|<backup-id>]",
		Short: "Verify a backup's signature and chunks (no restore required)",
		Long: `verify reads the named backup's manifest, validates its
signature with the local public key, then SHA-256-round-trips every
referenced chunk via the CAS read path. Encrypted backups are
decrypted in-process using the local KEK; nothing is restored to
disk and no WAL is replayed.

For the full restore-and-pg_verifybackup verification path, pass
` + "`" + `--full` + "`" + ` (requires Docker locally) or ` + "`" + `--control-plane <url>` + "`" + ` to
dispatch a sandbox verify to an agent (Docker on the agent host,
not yours). ` + "`" + `--control-plane` + "`" + ` always implies ` + "`" + `--full` + "`" + ` semantics —
fast verify doesn't need network round-trips.

For a much faster pre-flight ("are the chunks still here?"
without the bytes-and-checksums round-trip), pass
` + "`" + `--existence-only` + "`" + `. This Stats every unique chunk instead of
fetching it; useful before ` + "`" + `backup undelete` + "`" + ` (to confirm
chunk-GC hasn't reclaimed the body) or as a 100x-cheaper
sanity check on cold archives. Mismatch on bytes is NOT
detected in this mode — only absence.

Backup ID:
  - omitted or "latest" → the most recent (by stop time) backup
  - "<backup-id>"        → that exact backup`,
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deployment := args[0]
			backupID := "latest"
			if len(args) == 2 {
				backupID = args[1]
			}
			full, _ := cmd.Flags().GetBool("full")
			pgMajor, _ := cmd.Flags().GetString("pg-major")
			if dispatch.controlPlane != "" {
				if existenceOnly {
					return output.NewError("usage.bad_flag",
						"verify: --existence-only is incompatible with --control-plane (the existence check is a local-storage shortcut)").Wrap(output.ErrUsage)
				}
				return runVerifyControlPlane(cmd, deployment, backupID, repoURL, pgMajor, &dispatch)
			}
			if full {
				if existenceOnly {
					return output.NewError("usage.bad_flag",
						"verify: --existence-only is incompatible with --full (full verify implies a real restore)").Wrap(output.ErrUsage)
				}
				return runVerifyFull(cmd, deployment, backupID, repoURL, pgMajor)
			}
			return runVerify(cmd, deployment, backupID, repoURL, sample, existenceOnly)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	c.Flags().IntVar(&sample, "sample", 0,
		"verify only the first N chunks in hash-sorted order (0 = every referenced chunk)")
	c.Flags().BoolVar(&existenceOnly, "existence-only", false,
		"only check that each chunk EXISTS (Stat) — skip fetch+decrypt+SHA verification (much faster; useful pre-flight before `backup undelete`)")
	c.Flags().Bool("full", false,
		"perform a full restore into a Docker sandbox and run pg_verifybackup against it")
	c.Flags().String("pg-major", "",
		"PG major version for the sandbox image (default: derived from the backup's pg_version)")
	c.Flags().StringToString("kms-config", nil,
		"cloud KMS provider config for verifying a cloud-KMS-encrypted backup (e.g. region=eu-central-1,endpoint=...); empty uses ambient credentials")
	registerDispatchFlags(c, &dispatch)
	return c
}

func runVerify(cmd *cobra.Command, deployment, backupID, repoURL string, sample int, existenceOnly bool) error {
	d := DispatcherFrom(cmd)
	// Resolve --repo from the named deployment in config when omitted (#12).
	_, repoURL = deploymentDefaults(deployment, "", repoURL)
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo")
	}
	if sample < 0 {
		return output.NewError("usage.bad_flag",
			"verify: --sample must be ≥ 0").Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)

	// Resolve "latest" → newest by StoppedAt. Any signature mismatch
	// during the listing walk is silently skipped (same posture as
	// `list`); we want to find a usable backup for the operator to
	// verify, not double-error on a signature failure.
	if backupID == "latest" {
		latest, err := pickLatestBackup(cmd.Context(), store, deployment, verifier)
		if err != nil {
			return err
		}
		backupID = latest
	}

	m, err := store.Read(cmd.Context(), deployment, backupID, verifier)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("verify: backup %q for deployment %q not found",
					backupID, deployment)).
				WithSuggestion(&output.Suggestion{
					Human:   "list available backups with `pg_hardstorage list " + deployment + "`",
					Command: "pg_hardstorage list " + deployment,
				}).Wrap(err)
		}
		// Signature failure here is a verify failure — preserve the
		// underlying message but tag with the verify.* namespace so
		// the exit code is ExitVerifyFailed.
		return output.NewError("verify.manifest_signature",
			fmt.Sprintf("verify: manifest signature: %v", err)).Wrap(err)
	}

	var cas *repo.CAS
	if !existenceOnly {
		// Existence-only mode skips the CAS build entirely —
		// we don't need encryption codecs or KEK resolution to
		// Stat. (Avoids surfacing a kek_resolve_failed for an
		// operator who just wants to know "are the chunks
		// there?" on a backup whose KEK has been rotated.)
		kmsCfg, _ := cmd.Flags().GetStringToString("kms-config")
		cas, err = buildVerifyCAS(cmd.Context(), sp, m, stringMapToAny(kmsCfg))
		if err != nil {
			return err
		}
	}

	started := time.Now()
	var stats verifyStats
	if existenceOnly {
		stats, err = verifyChunkExistence(cmd.Context(), sp, m, sample)
	} else {
		stats, err = verifyChunks(cmd.Context(), cas, m, sample)
	}
	stats.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		// Context-cancelled mid-walk: don't pretend partial unfetched
		// chunks are "mismatches" — that would falsely trip
		// ExitVerifyFailed for what's actually a clean abort. Surface
		// as aborted so monitoring sees the right signal.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return output.NewError("aborted.context_cancelled",
				fmt.Sprintf("verify: aborted after %d/%d chunks (no integrity verdict reached)",
					stats.ChunksVerified, stats.ChunksSampled)).Wrap(err)
		}
		return output.NewError("internal",
			fmt.Sprintf("verify: %v", err)).Wrap(err)
	}

	// Record the verification run in the audit chain as `verify.run` so
	// the compliance report's verification section (SOC 2 A1.2 / ISO
	// A.8.13) can roll it up — the command's success Result is not
	// persisted, so this audit event is the only verify-run signal in
	// the chain. Best-effort: a chain-write failure must never fail the
	// verification itself.
	verifyOutcome := "ok"
	if len(stats.Mismatches) > 0 {
		verifyOutcome = "failed"
	}
	verifyMode := "fast"
	if existenceOnly {
		verifyMode = "existence"
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action:  "verify.run",
		Subject: audit.Subject{Deployment: m.Deployment, BackupID: m.BackupID, Repo: repoURL},
		Body:    map[string]any{"outcome": verifyOutcome, "mode": verifyMode},
	})

	body := verifyBody{
		Deployment:        m.Deployment,
		BackupID:          m.BackupID,
		ManifestSignature: "valid",
		ChunksReferenced:  stats.ChunksReferenced,
		ChunksUnique:      stats.ChunksUnique,
		ChunksSampled:     stats.ChunksSampled,
		ChunksVerified:    stats.ChunksVerified,
		ChunksMismatched:  len(stats.Mismatches),
		BytesVerified:     stats.BytesVerified,
		DurationMS:        stats.DurationMS,
		ExistenceOnly:     existenceOnly,
	}
	for _, h := range stats.Mismatches {
		body.Mismatches = append(body.Mismatches, h.String())
	}
	if len(stats.Mismatches) > 0 {
		// Existence-only mismatches are MISSING chunks — point
		// at chunk-GC + repo scrub. Full-verify mismatches are
		// CORRUPTION — point at repair scrub. The summary
		// includes the failing hashes (truncated for very
		// large lists) so a JSON-mode caller's stderr carries
		// the forensic data without parsing the body.
		errCode := "verify.chunk_mismatch"
		humanHint := "review the failing hashes (run `pg_hardstorage repair scrub`); a real mismatch means the repo is corrupt for this backup."
		commandHint := "pg_hardstorage repair scrub --repo " + repoURL
		hashList := summarizeHashes(stats.Mismatches, 5)
		summary := fmt.Sprintf("verify: %d chunk(s) failed integrity check: %s",
			len(stats.Mismatches), hashList)
		if existenceOnly {
			errCode = "verify.chunks_missing"
			summary = fmt.Sprintf("verify --existence-only: %d chunk(s) missing from the repo: %s",
				len(stats.Mismatches), hashList)
			humanHint = "the named chunks are absent from the repo — chunk-GC may have already reclaimed them, or the manifest references chunks never written. Restoring from this backup will fail; a tombstoned manifest with missing chunks is no longer recoverable via `backup undelete`."
			commandHint = ""
		}
		errResult := output.NewError(errCode, summary)
		if commandHint != "" {
			errResult = errResult.WithSuggestion(&output.Suggestion{Human: humanHint, Command: commandHint})
		} else {
			errResult = errResult.WithSuggestion(&output.Suggestion{Human: humanHint})
		}
		return errResult
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// pickLatestBackup walks the deployment's manifests and returns the
// backup_id of the one with the latest StoppedAt. Manifests that fail
// signature verification are silently skipped — same posture as `list`.
func pickLatestBackup(ctx context.Context, store *backup.ManifestStore, deployment string, verifier *backup.Verifier) (string, error) {
	var latestID string
	var latestStop time.Time
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			continue
		}
		if m.StoppedAt.After(latestStop) {
			latestID = m.BackupID
			latestStop = m.StoppedAt
		}
	}
	if latestID == "" {
		return "", output.NewError("notfound.backup",
			fmt.Sprintf("verify: no usable backups for deployment %q", deployment)).
			WithSuggestion(&output.Suggestion{
				Human:   "take one with `pg_hardstorage backup " + deployment + "`",
				Command: "pg_hardstorage backup " + deployment,
			})
	}
	return latestID, nil
}

// buildVerifyCAS constructs the CAS the chunk-walk uses. Encrypted
// backups need the KEK resolved + DEK unwrapped; unencrypted ones
// use the default codec set. Mirrors restore's encryption_glue
// without taking on its restore-specific options.
func buildVerifyCAS(ctx context.Context, sp storage.StoragePlugin, m *backup.Manifest, providerConfig map[string]any) (*repo.CAS, error) {
	if m.Encryption == nil {
		return casdefault.New(sp), nil
	}
	if m.Encryption.Scheme != "aes-256-gcm" {
		return nil, output.NewError("verify.unknown_scheme",
			fmt.Sprintf("verify: unsupported encryption scheme %q", m.Encryption.Scheme))
	}
	wrapped, err := decodeBase64(m.Encryption.WrappedDEK)
	if err != nil {
		return nil, output.NewError("verify.bad_wrapped_dek",
			fmt.Sprintf("verify: decode wrapped DEK: %v", err)).Wrap(err)
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("internal", err.Error()).Wrap(err)
	}

	var dek []byte
	// Cloud KMS branch: unwrap the DEK server-side (issue #102). The KEK
	// never leaves the HSM, so the local KEKResolver can't resolve it.
	if scheme := kms.SchemeOf(m.Encryption.KEKRef); scheme != "" && scheme != "local" {
		dek, err = keystore.UnwrapDEK(ctx, m.Encryption.KEKRef, wrapped, keystore.UnwrapOpts{
			KeyringDir:     p.Keyring.Value,
			ProviderConfig: providerConfig,
		})
		if err != nil {
			switch {
			case kms.IsUnreachable(err):
				return nil, output.NewError("kms.unreachable",
					fmt.Sprintf("verify: cloud KMS for %q unreachable: %v", m.Encryption.KEKRef, err)).
					WithSuggestion(&output.Suggestion{
						Human: "verify network reachability + credentials for the KMS provider, then retry — exit 8 signals a transient infrastructure failure.",
					}).Wrap(err)
			case errors.Is(err, kms.ErrUnwrap):
				return nil, output.NewError("verify.kek_mismatch",
					fmt.Sprintf("verify: cloud KMS could not unwrap the DEK for %q: %v", m.Encryption.KEKRef, err)).Wrap(err)
			default:
				return nil, output.NewError("verify.kek_resolve_failed",
					fmt.Sprintf("verify: resolve DEK via cloud KMS %q: %v", m.Encryption.KEKRef, err)).Wrap(err)
			}
		}
	} else {
		// Local-custody path (unchanged).
		kek, kerr := keystore.KEKResolver(p.Keyring.Value)(m.Encryption.KEKRef)
		if kerr != nil {
			return nil, output.NewError("verify.kek_resolve_failed",
				fmt.Sprintf("verify: resolve KEK %q: %v", m.Encryption.KEKRef, kerr)).Wrap(kerr)
		}
		dekArr, uerr := encryption.Unwrap(kek, wrapped)
		if uerr != nil {
			return nil, output.NewError("verify.kek_mismatch",
				fmt.Sprintf("verify: unwrap DEK with KEK %q: %v", m.Encryption.KEKRef, uerr)).
				WithSuggestion(&output.Suggestion{
					Human: "the supplied KEK doesn't match the one that wrapped this backup's DEK; check the keyring path and KEKRef.",
				}).Wrap(uerr)
		}
		dek = dekArr[:]
	}

	enc, err := aesgcm.New(dek)
	if err != nil {
		return nil, output.NewError("internal",
			fmt.Sprintf("verify: build aes-gcm encryptor: %v", err)).Wrap(err)
	}
	return casdefault.NewEncrypted(sp, enc), nil
}

// verifyStats collects the running totals for runVerify. Kept
// separate from verifyBody so we can guard the result-shape stability
// independently of the internal accounting.
type verifyStats struct {
	ChunksReferenced int         // total chunks across all files (with dupes)
	ChunksUnique     int         // distinct hashes
	ChunksSampled    int         // how many we attempted
	ChunksVerified   int         // how many round-tripped successfully
	BytesVerified    int64       // sum of plaintext bytes for verified chunks
	DurationMS       int64       // wall-clock elapsed for the chunk walk
	Mismatches       []repo.Hash // hashes whose stored bytes failed verification
}

// verifyChunks walks every unique chunk hash in the manifest, fetches
// it via the CAS read path (which already decompresses, decrypts if
// needed, and SHA-checks the plaintext), and records the outcome.
//
// We pre-compute the unique set so a chunk referenced N times costs
// one round-trip, not N. Order is sorted by hex hash so output and
// CPU work are deterministic for tests.
//
// Context cancellation is honoured between chunks (and surfaced as a
// returned error). Without that, a Ctrl-C mid-walk would have every
// remaining GetChunkBytes return ctx.Err and get accumulated into
// stats.Mismatches — falsely tripping ExitVerifyFailed for a clean
// abort.
func verifyChunks(ctx context.Context, cas *repo.CAS, m *backup.Manifest, sample int) (verifyStats, error) {
	stats := verifyStats{}
	unique := map[repo.Hash]struct{}{}
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			stats.ChunksReferenced++
			unique[c.Hash] = struct{}{}
		}
	}
	stats.ChunksUnique = len(unique)

	hashes := make([]repo.Hash, 0, len(unique))
	for h := range unique {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
	if sample > 0 && sample < len(hashes) {
		hashes = hashes[:sample]
	}
	stats.ChunksSampled = len(hashes)

	for _, h := range hashes {
		// Cooperative cancellation point. We check BEFORE each
		// GetChunkBytes — checking after would still let one
		// ctx-cancelled call leak into stats.Mismatches.
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		body, err := cas.GetChunkBytes(ctx, h)
		if err != nil {
			// Distinguish ctx errors from real fetch failures: a
			// cancelled fetch is not a verification finding.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stats, err
			}
			// GetChunkBytes content-address-verifies every chunk it
			// returns — decrypt, decompress, then SHA-256 the plaintext
			// against the key, erroring with ErrChecksumMismatch on any
			// divergence (the exact path a restore reads through). So a
			// non-ctx error here IS the corruption / missing-chunk
			// finding; record it.
			stats.Mismatches = append(stats.Mismatches, h)
			continue
		}
		// No second SHA pass. A body returned WITHOUT error already
		// hashed to h inside GetChunkBytes, so re-hashing it here was an
		// always-false check that merely doubled verify's hashing CPU on
		// large backups (CPU-pathology audit #5). Detection is unchanged:
		// a corrupt chunk surfaces as the GetChunkBytes error above.
		stats.ChunksVerified++
		stats.BytesVerified += int64(len(body))
	}
	return stats, nil
}

// summarizeHashes formats up to max hashes from h for inclusion
// in an error message; if more exist, appends "... (+N more)" so
// the summary stays bounded but the operator sees enough to
// start triaging without parsing the JSON body.
func summarizeHashes(h []repo.Hash, max int) string {
	if len(h) == 0 {
		return ""
	}
	n := len(h)
	if n > max {
		n = max
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, h[i].String())
	}
	out := strings.Join(parts, ", ")
	if len(h) > max {
		out += fmt.Sprintf(" (+%d more)", len(h)-max)
	}
	return out
}

// verifyChunkExistence is the existence-only variant of
// verifyChunks. For each unique chunk hash, Stat the
// corresponding storage key (chunks/sha256/...). Missing chunks
// land in stats.Mismatches; present chunks count toward
// ChunksVerified. BytesVerified stays at zero (we never read
// the bodies) so the operator sees the existence vs full
// distinction at a glance.
//
// Trade-off: a chunk whose body has been silently corrupted
// (bit-rot, partial write that managed to register an Object)
// counts as "verified" here. That's by design — operators who
// want bit-integrity checks run the default verify; this mode
// is the 100x-faster "are the chunks even there?" pre-flight.
//
// Same context-cancellation discipline as verifyChunks: a
// Ctrl-C mid-walk surfaces context.Canceled rather than
// inflating Mismatches.
func verifyChunkExistence(ctx context.Context, sp storage.StoragePlugin, m *backup.Manifest, sample int) (verifyStats, error) {
	stats := verifyStats{}
	unique := map[repo.Hash]struct{}{}
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			stats.ChunksReferenced++
			unique[c.Hash] = struct{}{}
		}
	}
	stats.ChunksUnique = len(unique)

	hashes := make([]repo.Hash, 0, len(unique))
	for h := range unique {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
	if sample > 0 && sample < len(hashes) {
		hashes = hashes[:sample]
	}
	stats.ChunksSampled = len(hashes)

	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		_, err := sp.Stat(ctx, repo.ChunkKey(h))
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stats, err
			}
			// Treat ErrNotFound (the common case — chunk-GC
			// reclaimed the body) the same as any other
			// fetch failure: it's a "missing" finding.
			stats.Mismatches = append(stats.Mismatches, h)
			continue
		}
		stats.ChunksVerified++
	}
	return stats, nil
}

// verifyBody is the v1-stable result body. Field names match the
// SPEC's "fast verify" vocabulary. ExistenceOnly is omitempty so the
// default (full) verify body shape stays byte-identical to+ —
// schema-additive only.
type verifyBody struct {
	Deployment        string   `json:"deployment"`
	BackupID          string   `json:"backup_id"`
	ManifestSignature string   `json:"manifest_signature"`
	ChunksReferenced  int      `json:"chunks_referenced"`
	ChunksUnique      int      `json:"chunks_unique"`
	ChunksSampled     int      `json:"chunks_sampled"`
	ChunksVerified    int      `json:"chunks_verified"`
	ChunksMismatched  int      `json:"chunks_mismatched"`
	BytesVerified     int64    `json:"bytes_verified"`
	DurationMS        int64    `json:"duration_ms"`
	Mismatches        []string `json:"mismatches,omitempty"`
	ExistenceOnly     bool     `json:"existence_only,omitempty"`
}

// WriteText renders the body as the operator-facing text form.
func (b verifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	mode := "full"
	if b.ExistenceOnly {
		mode = "existence-only"
	}
	fmt.Fprintf(bw, "verify %s/%s (%s)\n", b.Deployment, b.BackupID, mode)
	fmt.Fprintf(bw, "  manifest signature: %s\n", b.ManifestSignature)
	fmt.Fprintf(bw, "  chunks: %d referenced, %d unique, %d sampled\n",
		b.ChunksReferenced, b.ChunksUnique, b.ChunksSampled)
	if b.ChunksMismatched == 0 {
		if b.ExistenceOnly {
			fmt.Fprintf(bw, "  ✓ %d chunk(s) present (no integrity check) in %dms\n",
				b.ChunksVerified, b.DurationMS)
		} else {
			fmt.Fprintf(bw, "  ✓ %d chunk(s) verified — %s in %dms\n",
				b.ChunksVerified, humanBytes(b.BytesVerified), b.DurationMS)
		}
	} else {
		label := "FAILED verification"
		if b.ExistenceOnly {
			label = "MISSING from the repo"
		}
		fmt.Fprintf(bw, "  ✗ %d chunk(s) %s:\n", b.ChunksMismatched, label)
		for _, h := range b.Mismatches {
			fmt.Fprintf(bw, "      %s\n", h)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// decodeBase64 keeps the verify command independent of the restore
// package. Same logic as restore/encryption_glue.buildEncryptedCAS,
// inlined to avoid pulling in restore's option machinery.
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
