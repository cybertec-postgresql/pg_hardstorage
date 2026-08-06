// splitbrain.go — the patroni_split_brain scenario.
//
// What this drill asserts is NOT "can we make Patroni elect two
// leaders". Inducing real split-brain means partitioning a DCS and
// leaving two PostgreSQL instances accepting writes, which is not
// something a tool should do to an operator's cluster on request, and
// which R7 is explicit is an operator-only recovery.
//
// What it asserts is the guarantee the repository makes WHEN split-brain
// happens elsewhere: a divergent writer must not be able to archive over
// a segment we already hold. R7's symptom list leads with exactly that —
// `wal push` refusing a segment another writer already archived — and
// the refusal is the thing standing between "two timelines, one
// recoverable" and "an archive silently containing both".
//
// The failure mode being guarded is silent success. Without the
// verification, the loser's archive_command exits 0, PostgreSQL advances
// confirmed_flush_lsn, the slot rotates the segment off disk, and the
// operator believes the archive worked. The WAL is then gone from both
// the cluster and the repository.
//
// The drill writes into the operator's real repository, so everything it
// pushes lives under a probe deployment name and is deleted afterwards.
// Segment bodies are zero-filled apart from a few marker bytes, so what
// physically lands is a couple of compressed chunks rather than 32 MiB.

package gameday

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// splitBrainProbeDeployment namespaces everything this drill writes.
// It is not a real deployment name and cannot collide with one: the
// config schema rejects the leading underscores.
const splitBrainProbeDeployment = "__gameday_splitbrain_probe"

// splitBrainSegment is the segment number both writers contend for.
const splitBrainSegment = "000000010000000000000001"

func init() {
	Register(Scenario{
		Name: "patroni_split_brain",
		Description: "A divergent writer must not archive over a segment we already hold. " +
			"Pushes a segment, then a doppelgänger with the same name from a second " +
			"writer, and asserts the repository refuses both the content mismatch and " +
			"the foreign-cluster case.",
		Tier: "L4",
		Run:  runPatroniSplitBrain,
	})
}

func runPatroniSplitBrain(ctx context.Context, opts RunOptions) (*Result, error) {
	r := &Result{
		Schema:    SchemaResult,
		Scenario:  "patroni_split_brain",
		StartedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
	}
	defer finalize(r)

	if opts.DryRun {
		r.Evidence = append(r.Evidence, Event{
			At:   time.Now().UTC(),
			Kind: "plan",
			Message: "would push segment " + splitBrainSegment + " under the probe " +
				"deployment, then re-push it with different content from the same cluster " +
				"(expect splitbrain.content_mismatch) and with the same content from a " +
				"different cluster (expect splitbrain.system_identifier_mismatch), then " +
				"delete everything it wrote",
		})
		r.Pass = true
		return r, nil
	}

	if strings.TrimSpace(opts.RepoURL) == "" {
		r.Failure = "no repository to drill: pass --repo (the drill archives a probe " +
			"segment and then tries to archive over it)"
		r.Misconfigured = true
		r.Pass = false
		return r, nil
	}

	sp, err := storage.Open(ctx, opts.RepoURL)
	if err != nil {
		r.Failure = fmt.Sprintf("open repository %q: %v", opts.RepoURL, err)
		r.Pass = false
		return r, nil
	}
	defer sp.Close()

	// Always clean up, including on an early return: a drill that
	// leaves probe WAL in a production repository is worse than a drill
	// that did not run.
	defer cleanUpSplitBrainProbe(ctx, sp, r)

	dir, err := os.MkdirTemp("", "gameday-splitbrain-")
	if err != nil {
		r.Failure = fmt.Sprintf("create scratch dir: %v", err)
		r.Pass = false
		return r, nil
	}
	defer os.RemoveAll(dir)

	cas := casdefault.New(sp)
	const clusterA = "7000000000000000001"
	const clusterB = "7000000000000000002"

	// 1. The legitimate writer archives the segment.
	pathA, err := writeProbeSegment(dir, "a", 0xA1)
	if err != nil {
		r.Failure = err.Error()
		r.Pass = false
		return r, nil
	}
	if _, err := pushProbe(ctx, cas, sp, pathA, clusterA); err != nil {
		r.Failure = fmt.Sprintf("the first push should succeed — it is the baseline the "+
			"drill contends with: %v", err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "arranged",
		Message: "segment archived by the legitimate writer",
		Body:    map[string]any{"segment": splitBrainSegment, "system_identifier": clusterA},
	})

	// 2. Same cluster, different bytes — the classic split-brain push.
	pathB, err := writeProbeSegment(dir, "b", 0xB2)
	if err != nil {
		r.Failure = err.Error()
		r.Pass = false
		return r, nil
	}
	_, errSame := pushProbe(ctx, cas, sp, pathB, clusterA)
	if !isSplitBrainRefusal(errSame, "content_mismatch") {
		r.Failure = fmt.Sprintf("a divergent writer archived over segment %s from the SAME "+
			"cluster and the repository did not refuse with splitbrain.content_mismatch "+
			"(got %v). That is silent success: the loser's archive_command exits 0, "+
			"PostgreSQL advances confirmed_flush_lsn, the slot rotates the segment off "+
			"disk, and the WAL is gone from both the cluster and the archive",
			splitBrainSegment, errSame)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "refused",
		Message: "same-cluster doppelgänger refused",
		Body:    map[string]any{"error": errSame.Error()},
	})

	// 3. Different cluster, same segment number — the cloned-datadir
	//    case R7 calls out (cp -a of a datadir without pg_resetwal).
	_, errForeign := pushProbe(ctx, cas, sp, pathA, clusterB)
	if !isSplitBrainRefusal(errForeign, "system_identifier_mismatch") {
		r.Failure = fmt.Sprintf("a writer from a DIFFERENT cluster archived over segment "+
			"%s and the repository did not refuse with "+
			"splitbrain.system_identifier_mismatch (got %v). A cloned datadir would "+
			"interleave two clusters' WAL under one deployment",
			splitBrainSegment, errForeign)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "refused",
		Message: "foreign-cluster push refused",
		Body:    map[string]any{"error": errForeign.Error()},
	})

	// 4. Idempotency must survive: an identical re-push is archive_command
	//    retrying after a success, and refusing THAT would make PostgreSQL
	//    retry forever.
	if _, err := pushProbe(ctx, cas, sp, pathA, clusterA); err != nil {
		r.Failure = fmt.Sprintf("an identical re-push was refused (%v). archive_command "+
			"retries after a timeout it believes failed; refusing the retry wedges WAL "+
			"archiving for the whole deployment", err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:   time.Now().UTC(),
		Kind: "observed",
		Message: "an identical re-push still succeeds — the refusal discriminates on " +
			"content, not on the segment already existing",
	})

	r.Pass = true
	return r, nil
}

// writeProbeSegment writes a SegmentSize file whose body is zero except
// for a marker, so the chunker compresses it to almost nothing. variant
// changes the bytes; the NAME is identical for every variant, which is
// the whole point.
func writeProbeSegment(dir, variant string, marker byte) (string, error) {
	sub := filepath.Join(dir, variant)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", fmt.Errorf("create scratch dir: %w", err)
	}
	body := make([]byte, walsink.SegmentSize)
	for i := 0; i < 64 && i < len(body); i++ {
		body[i] = marker
	}
	path := filepath.Join(sub, splitBrainSegment)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("write probe segment: %w", err)
	}
	return path, nil
}

func pushProbe(ctx context.Context, cas *repo.CAS, sp storage.StoragePlugin, path, sysID string) (*walsink.SegmentManifest, error) {
	return walsink.PushSegmentFile(ctx, cas, sp, path, walsink.PushOptions{
		Deployment:       splitBrainProbeDeployment,
		SystemIdentifier: sysID,
	})
}

// isSplitBrainRefusal reports whether err is the expected refusal.
// Matching on the code prefix rather than the whole message keeps the
// drill from breaking when the wording changes, while still failing if
// the CLASS of refusal changes — a content mismatch reported as a
// system-identifier mismatch would send the operator hunting a cloned
// datadir that does not exist.
func isSplitBrainRefusal(err error, leaf string) bool {
	return err != nil && strings.Contains(err.Error(), "splitbrain."+leaf)
}

// cleanUpSplitBrainProbe removes every OBJECT the drill wrote.
//
// On a filesystem backend the now-empty directories remain: the storage
// contract has objects and prefixes, not directories, so there is no
// operation to remove them. No data is left — `find -type f` under the
// probe prefix comes back empty — but `ls wal/` on a file:// repository
// will still show the probe deployment name. Said here rather than left
// for someone to discover.
//
// Best
// effort: a failure is recorded as evidence rather than failing the
// run, because the invariant under test has already been decided by
// the time this runs — but it IS recorded, so a repository slowly
// accumulating probe segments is visible rather than silent.
func cleanUpSplitBrainProbe(ctx context.Context, sp storage.StoragePlugin, r *Result) {
	prefix := "wal/" + splitBrainProbeDeployment + "/"
	var listed, removed, failed int
	var listErr error
	for obj, err := range sp.List(ctx, prefix) {
		if err != nil {
			listErr = err
			break
		}
		listed++
		if derr := sp.Delete(ctx, obj.Key); derr != nil {
			failed++
			continue
		}
		removed++
	}
	if listErr != nil {
		r.Evidence = append(r.Evidence, Event{
			At:      time.Now().UTC(),
			Kind:    "cleanup_failed",
			Message: fmt.Sprintf("could not list %s to clean up: %v", prefix, listErr),
		})
		return
	}
	ev := Event{
		At:      time.Now().UTC(),
		Kind:    "cleanup",
		Message: fmt.Sprintf("removed %d probe object(s) under %s", removed, prefix),
	}
	if failed > 0 {
		ev.Kind = "cleanup_failed"
		ev.Message = fmt.Sprintf("%d of %d probe object(s) under %s could not be removed; "+
			"they are inert but should be cleared by hand", failed, listed, prefix)
	}
	r.Evidence = append(r.Evidence, ev)
}
