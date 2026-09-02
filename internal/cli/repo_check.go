// repo_check.go — 'repo check' CLI verb: HSREPO sanity + manifest signatures + chunk presence.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRepoCheckCmd implements `pg_hardstorage repo check <url>`. The
// "is this whole repository structurally healthy?" pass.
//
// What it checks (composed from existing primitives):
//
//  1. HSREPO sanity (repo.Open already does this).
//  2. Every primary manifest verifies under the local public key
//     (ManifestStore.List does this on the way through).
//  3. Every chunk referenced by a (non-tombstoned) manifest exists
//     under chunks/sha256/... (repo.FindMissing).
//  4. Tombstone hygiene — counts tombstones for the operator's
//     benefit ("how much is queued for GC?").
//
// What it deliberately does NOT do:
//
//   - Per-chunk integrity (round-trip + SHA verify) — that's
//     `repo gc`'s job at the chunk-level, and `repair scrub` /
//     `verify` for full reads. `check` stays in O(n manifests +
//     n unique chunks) Stat calls — fast enough to schedule.
//   - WAL gap detection — `wal list --gaps-only` covers that.
//
// Health verdict:
//
//   - missing chunks → ExitVerifyFailed (a manifest references
//     bytes the storage doesn't have; restores depending on those
//     bytes will fail). Surfaced as `verify.missing_chunks` —
//     the verify.* namespace is the one wired to ExitVerifyFailed
//     in exitcode.go, even though the command name is `repo check`.
//   - signature failures (manifests skipped during the walk) are
//     reported as a non-zero count but don't fail the command —
//     they're orphan manifests, not corruption of live ones.
//     The operator can drill in with `repair manifest`.
func newRepoCheckCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "check <url>",
		Short:        "Verify repository integrity (signatures + chunk references)",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo check: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runRepoCheck(cmd, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	return c
}

func runRepoCheck(cmd *cobra.Command, repoURL string) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the first positional <url>)")
	}
	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	meta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	commitMode := "stage_and_rename"
	if sp.Capabilities().ConditionalPut {
		commitMode = "conditional_put"
	}

	store := backup.NewManifestStore(sp)

	// 1. Walk manifests by deployment, capture per-deployment stats.
	deployments, err := store.Deployments(cmd.Context())
	if err != nil {
		return output.NewError("repo.check.deployments_failed",
			fmt.Sprintf("repo check: enumerate deployments: %v", err)).Wrap(err)
	}
	depReports := make([]repoCheckDeployment, 0, len(deployments))
	totalSigFailed := 0
	totalManifests := 0
	for _, dep := range deployments {
		dr := repoCheckDeployment{Name: dep}
		for m, err := range store.List(cmd.Context(), dep, verifier) {
			if err != nil {
				// Distinguish a genuine signature/parse failure (the
				// manifest was fetched but failed Ed25519 verification,
				// schema/parse, or the embedded key mismatched) from a
				// transient backend/List error. Counting a List/Get
				// failure as a signature failure would report
				// "potential tampering" (exit 9) and present a
				// truncated walk as complete — a storage hiccup must
				// not masquerade as corruption.
				if !isManifestSignatureFailure(err) {
					return output.NewError("repo.check.manifest_walk_failed",
						fmt.Sprintf("repo check: list manifests for %s: %v", dep, err)).Wrap(err)
				}
				dr.SignatureFailures++
				totalSigFailed++
				continue
			}
			dr.LiveManifests++
			totalManifests++
			_ = m // we only count here; FindMissing does the ref walk
		}
		// Tombstone count — list once with a hand-rolled walk since
		// the store filters them out of List by design.
		t, err := countTombstones(cmd.Context(), sp, dep)
		if err != nil {
			return output.NewError("repo.check.tombstone_walk_failed",
				fmt.Sprintf("repo check: tombstone walk %s: %v", dep, err)).Wrap(err)
		}
		dr.Tombstones = t
		depReports = append(depReports, dr)
	}

	// 1b. Orphaned redundancy copies: a manifests/_replicas/<id> entry
	// whose primary manifest is gone.
	//
	// Every commit writes the primary and then a redundancy copy under
	// manifests/_replicas/, precisely so that "if the primary is lost
	// (a single misdirected `aws s3 rm`, say), the replica still has
	// the bytes". `repair manifest <deployment> <backup-id>` restores
	// the primary from it.
	//
	// But that recovery needs a backup ID, and a backup whose primary
	// is gone appears in NO listing: List walks
	// manifests/<dep>/backups/, `list`/`status` show nothing, and this
	// command's LiveManifests count simply gets smaller. The redundancy
	// was unusable in the exact scenario it exists for, because nothing
	// told the operator which backups to recover — and `repo check`
	// reported healthy: true over a repository that had silently lost
	// backups, with the evidence sitting unexamined one prefix away.
	orphanedReplicas, err := findOrphanedReplicas(cmd.Context(), sp, store, verifier)
	if err != nil {
		return output.NewError("repo.check.replica_walk_failed",
			fmt.Sprintf("repo check: replica walk: %v", err)).Wrap(err)
	}

	// 2. Reference completeness across every (non-tombstoned) manifest.
	refs, err := repo.CollectReferences(cmd.Context(), sp)
	if err != nil {
		return output.NewError("repo.check.collect_refs_failed",
			fmt.Sprintf("repo check: collect references: %v", err)).Wrap(err)
	}
	missing, err := repo.FindMissing(cmd.Context(), sp, refs)
	if err != nil {
		return output.NewError("repo.check.find_missing_failed",
			fmt.Sprintf("repo check: find missing chunks: %v", err)).Wrap(err)
	}

	body := repoCheckBody{
		URL:               repoURL,
		RepoID:            meta.ID,
		Schema:            meta.Schema,
		Deployments:       depReports,
		LiveManifests:     totalManifests,
		SignatureFailures: totalSigFailed,
		ChunkRefs:         refs.Len(),
		MissingChunks:     len(missing),
		OrphanedReplicas:  orphanedReplicas,
		WORM:              meta.WORM,
		CommitMode:        commitMode,
	}
	const maxListedHashes = 64
	for i, h := range missing {
		if i >= maxListedHashes {
			body.MissingHashes = append(body.MissingHashes,
				fmt.Sprintf("... +%d more", len(missing)-maxListedHashes))
			break
		}
		body.MissingHashes = append(body.MissingHashes, h.String())
	}
	// Healthy is the operator-facing roll-up: true iff
	// EVERY integrity invariant repo check covers is intact.
	// Both signature failures AND missing chunks are
	// disqualifying — a corrupted manifest whose chunks happen
	// to all still be present is NOT a healthy repo.  Previous
	// code only checked MissingChunks, so a manifest that failed
	// Ed25519 verification still produced `healthy: true` and
	// exit 0; operators running `repo check` in cron would
	// silently miss the corruption.  Surfaced by
	// L8_repo_check_detects_manifest_corruption.
	body.Healthy = body.MissingChunks == 0 && body.SignatureFailures == 0 &&
		len(body.OrphanedReplicas) == 0

	if body.MissingChunks > 0 {
		// verify.* is the namespace operators wire ExitVerifyFailed
		// to (see internal/output/exitcode.go); a missing-chunks
		// finding IS a verification failure even though the command
		// is `repo check`.
		return output.NewError("verify.missing_chunks",
			fmt.Sprintf("repo check: %d chunk(s) referenced by manifests are NOT present in storage",
				body.MissingChunks)).
			WithSuggestion(&output.Suggestion{
				Human:   "this is real corruption — restores referencing these chunks will fail. Investigate with `pg_hardstorage repair chunks --missing` before taking new backups.",
				Command: "pg_hardstorage repair chunks --missing --repo " + repoURL,
			})
	}
	if len(body.OrphanedReplicas) > 0 {
		return output.NewError("verify.orphaned_replicas",
			fmt.Sprintf("repo check: %d backup(s) have a redundancy copy under manifests/_replicas/ but no primary manifest: %s",
				len(body.OrphanedReplicas), strings.Join(body.OrphanedReplicas, ", "))).
			WithSuggestion(&output.Suggestion{
				Human:   "the primary manifest was deleted while its redundancy copy survived — which is what that copy is for. Each is recoverable with `repair manifest <deployment> <backup-id>`, which re-verifies the replica's signature before restoring it. Until then these backups are invisible to list/status/restore.",
				Command: "pg_hardstorage repair manifest <deployment> " + body.OrphanedReplicas[0] + " --repo " + repoURL,
			})
	}
	if body.SignatureFailures > 0 {
		// Mirror the missing-chunks exit path.  A signature
		// failure is also a verify.* condition: the manifest
		// either failed Ed25519 verification or failed to parse
		// as JSON in the first place, both of which mean a
		// restore against it would fail (or worse, succeed with
		// silently-wrong bytes).  Operators must be told via
		// exit code, not just a field in the JSON body.
		return output.NewError("verify.signature_failures",
			fmt.Sprintf("repo check: %d manifest signature(s) failed verification",
				body.SignatureFailures)).
			WithSuggestion(&output.Suggestion{
				Human:   "a manifest either failed Ed25519 verification or failed to parse — investigate with `pg_hardstorage repair manifest` and check the audit chain for tampering.",
				Command: "pg_hardstorage audit verify-chain --repo " + repoURL,
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// isManifestSignatureFailure reports whether err from a manifest walk
// is a genuine signature/verification failure — the manifest bytes
// were fetched but failed to parse, verify, or matched a different
// signing key — rather than a transient backend/List error (storage
// unreachable, listing interrupted, context cancelled). Only the
// former is "potential tampering" (exit 9); the latter means the walk
// was TRUNCATED and must be surfaced as a backend error so a storage
// hiccup doesn't masquerade as corruption or report an incomplete
// walk as clean.
//
// Classification is by exclusion: a storage-layer sentinel or a
// cancellation is unambiguously a backend error; everything else that
// bubbles out of ParseAndVerify (key mismatch, unsigned, bad
// signature, JSON-parse, schema-mismatch) is a real verification
// failure over bytes we DID fetch.
func isManifestSignatureFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, storage.ErrNotFound) ||
		errors.Is(err, storage.ErrChecksumMismatch) ||
		errors.Is(err, storage.ErrUnsupported) ||
		errors.Is(err, storage.ErrUnknownScheme) ||
		errors.Is(err, storage.ErrAlreadyExists) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// countTombstones walks the deployment's manifest tree once and
// returns the count of *.tombstone marker files.
func countTombstones(ctx context.Context, sp storage.StoragePlugin, deployment string) (int, error) {
	const suffix = "/manifest.json.tombstone"
	prefix := "manifests/" + deployment + "/backups/"
	count := 0
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return 0, err
		}
		if strings.HasSuffix(info.Key, suffix) {
			count++
		}
	}
	return count, nil
}

// repoCheckDeployment is the per-deployment line in the report.
type repoCheckDeployment struct {
	Name              string `json:"name"`
	LiveManifests     int    `json:"live_manifests"`
	SignatureFailures int    `json:"signature_failures"`
	Tombstones        int    `json:"tombstones"`
}

// repoCheckBody is the v1-stable result body.
type repoCheckBody struct {
	URL               string                `json:"url"`
	RepoID            string                `json:"repo_id"`
	Schema            string                `json:"schema"`
	Deployments       []repoCheckDeployment `json:"deployments"`
	LiveManifests     int                   `json:"live_manifests"`
	SignatureFailures int                   `json:"signature_failures"`
	ChunkRefs         int                   `json:"chunk_refs"`
	MissingChunks     int                   `json:"missing_chunks"`
	// OrphanedReplicas lists backup IDs whose redundancy copy under
	// manifests/_replicas/ survives but whose primary manifest is gone.
	// They are invisible to every listing until `repair manifest`
	// restores the primary, so a repo holding them is not healthy.
	OrphanedReplicas []string `json:"orphaned_replicas,omitempty"`
	MissingHashes    []string `json:"missing_hashes,omitempty"`
	Healthy          bool     `json:"healthy"`
	// CommitMode reports how this repository publishes manifests:
	// "conditional_put" (one atomic PUT, nothing deleted) or
	// "stage_and_rename" (a temporary object, a conditional COPY and a
	// DELETE per commit).
	//
	// Surfaced because the difference is invisible until it bites, and
	// then bites in two ways an operator cannot easily attribute: a
	// bucket kept append-only accrues a delete marker per WAL segment,
	// and a store that implements conditional PUT but not conditional
	// COPY cannot commit a manifest at all (issue #45).
	CommitMode string `json:"commit_mode"`
	// WORM, when non-nil, is the repo's write-once-read-many
	// policy as recorded in HSREPO. Surfaced here so an operator's
	// `repo check` confirms the policy is what they expected (a
	// fleet-wide audit usually compares the WORM block across
	// every repo).
	WORM *repo.WORMPolicy `json:"worm,omitempty"`
}

// WriteText renders the operator-facing report.
func (b repoCheckBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repo check — %s\n", b.URL)
	fmt.Fprintf(bw, "  Repository ID:   %s\n", b.RepoID)
	fmt.Fprintf(bw, "  Schema:          %s\n", b.Schema)
	fmt.Fprintf(bw, "  Deployments:     %d\n", len(b.Deployments))
	fmt.Fprintf(bw, "  Live manifests:  %d\n", b.LiveManifests)
	if b.SignatureFailures > 0 {
		fmt.Fprintf(bw, "  ✗ Signature failures: %d (skipped during walk; investigate with `repair manifest`)\n",
			b.SignatureFailures)
	} else {
		fmt.Fprintln(bw, "  ✓ All manifest signatures valid")
	}
	if !b.WORM.IsZero() {
		fmt.Fprintf(bw, "  WORM policy:     %s, retention %s\n",
			b.WORM.Mode, b.WORM.Retention)
	}
	switch b.CommitMode {
	case "conditional_put":
		fmt.Fprintln(bw, "  ✓ Commit mode:    conditional PUT (append-only; nothing deleted)")
	default:
		fmt.Fprintln(bw, "  ! Commit mode:    stage + rename — every manifest commit writes a")
		fmt.Fprintln(bw, "                    temporary, COPYs it into place and DELETEs it.")
		fmt.Fprintln(bw, "                    On a versioned bucket that is a delete marker per")
		fmt.Fprintln(bw, "                    WAL segment, and it needs a conditional COPY that")
		fmt.Fprintln(bw, "                    some S3-compatible stores do not implement.")
		fmt.Fprintln(bw, "                    If your endpoint enforces If-None-Match on PUT,")
		fmt.Fprintln(bw, "                    add ?conditional_put=native to the repo URL.")
	}
	fmt.Fprintf(bw, "  Chunk references: %d distinct\n", b.ChunkRefs)
	if len(b.OrphanedReplicas) > 0 {
		fmt.Fprintf(bw, "  ✗ %d backup(s) have a redundancy copy but NO primary manifest "+
			"(invisible to list/status/restore until repaired):\n", len(b.OrphanedReplicas))
		for _, id := range b.OrphanedReplicas {
			fmt.Fprintf(bw, "      %s\n", id)
		}
		fmt.Fprintln(bw, "    recover each with `pg_hardstorage repair manifest <deployment> <backup-id>`")
	}
	if b.MissingChunks == 0 {
		fmt.Fprintln(bw, "  ✓ Every referenced chunk is present")
	} else {
		fmt.Fprintf(bw, "  ✗ %d chunk(s) referenced but MISSING from storage:\n", b.MissingChunks)
		for _, h := range b.MissingHashes {
			fmt.Fprintf(bw, "      %s\n", h)
		}
	}
	if len(b.Deployments) > 0 {
		fmt.Fprintln(bw, "  Per-deployment:")
		// Sort for stable output (Deployments() already does, but
		// re-asserting here documents the contract).
		sorted := make([]repoCheckDeployment, len(b.Deployments))
		copy(sorted, b.Deployments)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, dr := range sorted {
			fmt.Fprintf(bw, "    %s — live=%d, tombstoned=%d", dr.Name, dr.LiveManifests, dr.Tombstones)
			if dr.SignatureFailures > 0 {
				fmt.Fprintf(bw, ", sig-failed=%d", dr.SignatureFailures)
			}
			fmt.Fprintln(bw)
		}
	}
	if b.Healthy {
		fmt.Fprintln(bw, "  Verdict: ✓ HEALTHY")
	} else {
		fmt.Fprintln(bw, "  Verdict: ✗ UNHEALTHY")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// findOrphanedReplicas returns the backup IDs under
// manifests/_replicas/ that have no primary manifest in any
// deployment.
//
// The replica file is named by backup ID alone, so the deployment is
// recovered from the manifest body rather than the key — which is also
// why the check reports IDs and lets `repair manifest` take the
// deployment from the operator.
func findOrphanedReplicas(ctx context.Context, sp storage.StoragePlugin, store *backup.ManifestStore, verifier *backup.Verifier) ([]string, error) {
	deployments, err := store.Deployments(ctx)
	if err != nil {
		return nil, err
	}
	// Every backup ID that HAS a primary, live or tombstoned. A
	// tombstoned backup's primary is intact (the tombstone is a
	// separate marker), so its replica is not orphaned.
	havePrimary := map[string]struct{}{}
	for _, dep := range deployments {
		for info, lerr := range sp.List(ctx, "manifests/"+dep+"/backups/") {
			if lerr != nil {
				return nil, lerr
			}
			if !strings.HasSuffix(info.Key, "/manifest.json") {
				continue
			}
			parts := strings.Split(info.Key, "/")
			if len(parts) >= 5 {
				havePrimary[parts[3]] = struct{}{}
			}
		}
	}

	const prefix = "manifests/_replicas/"
	const suffix = ".manifest.json"
	var orphans []string
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return nil, lerr
		}
		if !strings.HasSuffix(info.Key, suffix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(info.Key, prefix), suffix)
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		if _, ok := havePrimary[id]; !ok {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}
