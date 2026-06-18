// corrupt_repo_object step — adversarial mutation of stored
// repo state followed by an assertion that the next operation
// detects the corruption with a structured verify.* error.
//
// Three classes of mutation, each catching a different invariant
// the read path relies on:
//
//   - chunk_random  : flip a bit / truncate / overwrite a CAS
//                     chunk's envelope.  The plaintext-hash
//                     verification in CAS.GetChunkBytes (after
//                     envelope decode) MUST detect this.
//                     Pre-fix the doppelgänger commit changed
//                     the write path; the read path's hash
//                     check predates that and is what this
//                     pin guards.
//   - manifest_random: same, against a per-segment WAL manifest
//                      OR a backup manifest.  Restore must
//                      refuse on a corrupt manifest, not
//                      silently skip the affected file.
//   - hsrepo         : flip a byte in HSREPO.  Any subsequent
//                      repo.Open should refuse with a clear
//                      error rather than silently treat the
//                      repo as uninitialised.
//
// The step is file://-only by design — its job is to mutate
// the on-disk bytes, which only makes sense against a local
// filesystem repo.  Running against an s3:// repo produces a
// clear refusal at step entry.
//
// Pairs with `expect_error_prefix` on the step: when set, the
// step DOES NOT itself probe — it just mutates.  The next
// operation in the scenario (a restore, a verify, a wal fetch)
// is the actual assertion; the runner's overall exit is
// success iff that next step's error code starts with the
// configured prefix.  Today's runner doesn't have a generic
// "expect-error-on-next-step" hook; corrupt_repo_object's
// own implementation runs a probe immediately so the
// detection is deterministic and locally testable.

package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

const (
	corruptTargetChunkRandom    = "chunk_random"
	corruptTargetManifestRandom = "manifest_random"
	corruptTargetHSREPO         = "hsrepo"

	mutationFlipBitAt      = "flip_bit_at"
	mutationTruncate       = "truncate"
	mutationOverwriteZeros = "overwrite_zeros"
)

// runCorruptRepoObject mutates a stored repo object then
// (when ExpectErrorPrefix is set) probes the integrity path
// to confirm the corruption surfaces as a structured error.
func runCorruptRepoObject(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if state.repoURL == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "corrupt_repo_object: no repo initialised yet (run a take_backup first)"}
	}
	if !strings.HasPrefix(state.repoURL, "file://") {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("corrupt_repo_object: only file:// repos supported (got %q); s3 / azure / gcs corruption tests live at the storage-plugin layer", state.repoURL)}
	}
	repoDir := strings.TrimPrefix(state.repoURL, "file://")

	target := strings.TrimSpace(st.RepoTarget)
	if target == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "corrupt_repo_object: repo_target is required (chunk_random | manifest_random | hsrepo)"}
	}
	mutation := strings.TrimSpace(st.Mutation)
	if mutation == "" {
		mutation = mutationFlipBitAt
	}

	path, err := pickCorruptionTarget(repoDir, target)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("corrupt_repo_object: pick %s: %v", target, err)}
	}
	if err := applyMutation(path, mutation, st.Offset); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("corrupt_repo_object: %s on %s: %v", mutation, path, err)}
	}
	emit(out, "step.corrupt_repo_object.applied", map[string]any{
		"index":    idx,
		"target":   target,
		"path":     path,
		"mutation": mutation,
		"offset":   st.Offset,
	})

	if st.ExpectErrorPrefix == "" {
		// Mutate-only mode: the corruption is the step's
		// product, the assertion is left to a follow-up
		// step (a restore: that's expected to fail).
		return StepResult{Index: idx, Kind: st.Kind, Pass: true,
			Message: fmt.Sprintf("corrupt_repo_object: mutated %s (%s)", path, mutation)}
	}

	// Probe: shell out to the appropriate detection path.
	// `repo scrub --apply` walks every chunk and re-verifies
	// SHA-256 — that's the operator's bit-rot probe and the
	// only diagnostic command that actually reads chunk
	// content (repo check just walks signatures + manifest
	// counts; for content-level mutation we need scrub).
	//
	// Manifest mutations that shape parses cleanly (json
	// still loads) but fields are wrong → caught by signature
	// verification path.  `repo audit verify-signatures`
	// covers that surface; for now we route both the
	// chunk_random and manifest_random classes through the
	// same scrub command since scrub also re-verifies
	// manifest signatures along the way.
	probeArgs := []string{"repo", "scrub", state.repoURL, "--full", "--output", "json"}
	cmd := state.agentCmd(ctx, probeArgs...)
	probeOut, _ := cmd.CombinedOutput()
	if !bytes.Contains(probeOut, []byte(st.ExpectErrorPrefix)) {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("corrupt_repo_object: probe `repo scrub` did not report %q (output: %s)",
				st.ExpectErrorPrefix, truncate(probeOut, 512))}
	}
	emit(out, "step.corrupt_repo_object.detected", map[string]any{
		"index":  idx,
		"prefix": st.ExpectErrorPrefix,
		"output": truncate(probeOut, 256),
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("corrupt_repo_object: detected (%s)", st.ExpectErrorPrefix)}
}

// pickCorruptionTarget locates a single concrete file under
// repoDir matching the operator-supplied target class.  For
// the random classes we walk the repo and pick a uniformly-
// random match — deterministic per-run only if the operator
// also pinned a Seed; otherwise the choice is fresh per run,
// which is fine for chaos-style coverage.
func pickCorruptionTarget(repoDir, target string) (string, error) {
	switch target {
	case corruptTargetHSREPO:
		p := filepath.Join(repoDir, "HSREPO")
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("HSREPO not found at %q: %w", p, err)
		}
		return p, nil
	case corruptTargetChunkRandom:
		return pickRandomFile(filepath.Join(repoDir, "chunks"), ".chk")
	case corruptTargetManifestRandom:
		// On-disk layout: backup manifests live under
		// `manifests/<deployment>/backups/<backup-id>/manifest.json`
		// (with a sibling under `manifests/_replicas/`).  WAL
		// segment manifests live under `manifests/<dep>/wal/`
		// when streaming is configured.  We walk the whole
		// `manifests/` subtree so any *.json manifest is fair
		// game, regardless of whether the scenario ran a
		// backup, a wal-stream, or both.
		return pickRandomFile(filepath.Join(repoDir, "manifests"), ".json")
	}
	return "", fmt.Errorf("unknown repo_target %q (want one of: chunk_random, manifest_random, hsrepo)", target)
}

// pickRandomFile walks `root` and returns one entry chosen
// uniformly at random whose name ends with `suffix`.  Returns
// fs.ErrNotExist if no matches exist.  Skips paths containing
// `/_replicas/` — those are bookkeeping copies that the
// canonical read paths (scrub, restore, verify-chain) don't
// traverse, so a mutation there would never surface as an
// operator-visible error.
func pickRandomFile(root, suffix string) (string, error) {
	var matches []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.Contains(p, string(os.PathSeparator)+"_replicas"+string(os.PathSeparator)) {
			return nil
		}
		if strings.HasSuffix(p, suffix) {
			matches = append(matches, p)
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return "", fmt.Errorf("walk %s: %w", root, walkErr)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no %s files under %s", suffix, root)
	}
	pick, err := rand.Int(rand.Reader, big.NewInt(int64(len(matches))))
	if err != nil {
		return "", err
	}
	return matches[pick.Int64()], nil
}

// applyMutation rewrites the file in place per the requested
// mutation.  The file's modtime advances; downstream consumers
// that cache by modtime are forced to re-read.
func applyMutation(path, mutation string, offset int64) error {
	switch mutation {
	case mutationFlipBitAt:
		return mutateFlipBit(path, offset)
	case mutationTruncate:
		return mutateTruncate(path, offset)
	case mutationOverwriteZeros:
		return mutateOverwriteZeros(path, offset)
	}
	return fmt.Errorf("unknown mutation %q (want: flip_bit_at, truncate, overwrite_zeros)", mutation)
}

// mutateFlipBit XORs the byte at `offset` with a non-zero
// random byte.  When offset is 0 (operator passed no offset)
// we pick a random byte position so the mutation is
// non-trivially detectable but doesn't always hit the file's
// header where some plugins might short-circuit on
// magic-byte mismatch.
func mutateFlipBit(path string, offset int64) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("file %q is empty; nothing to flip", path)
	}
	pos := offset
	if pos <= 0 || pos >= int64(len(body)) {
		// Pick a random offset in the back half of the
		// file to avoid header / magic-byte regions.
		r, _ := rand.Int(rand.Reader, big.NewInt(int64(len(body)/2)))
		pos = int64(len(body)/2) + r.Int64()
	}
	body[pos] ^= 0xff
	return atomicReplace(path, body)
}

// mutateTruncate cuts the file off at `offset`, defaulting to
// half its length when offset == 0.  Common backup-tool
// failure mode: a chunk file partially written before a power
// cut + an fsync race that left the file at non-zero length
// but truncated.
func mutateTruncate(path string, offset int64) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("file %q is empty; nothing to truncate", path)
	}
	keep := offset
	if keep <= 0 || keep >= int64(len(body)) {
		keep = int64(len(body) / 2)
	}
	return atomicReplace(path, body[:keep])
}

// mutateOverwriteZeros nulls out 64 bytes starting at offset
// (or at len/4 when offset == 0).  Survives length-only
// integrity checks; only a content hash catches it.
func mutateOverwriteZeros(path string, offset int64) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("file %q is empty; nothing to overwrite", path)
	}
	// 64 bytes is a comfortable default for typical chunk sizes
	// (~4 MiB-class), but the random repo-target picker can land
	// on a 12-byte INCREMENTAL.NNNN header or a backup_label-sized
	// chunk, where the default span overruns the buffer. Cap the
	// span at the file length so a small chunk gets zeroed end-to-
	// end (still invalidates its content-addressed hash, which is
	// what the verify.* assertion needs).
	span := int64(64)
	if span > int64(len(body)) {
		span = int64(len(body))
	}
	pos := offset
	if pos <= 0 || pos+span > int64(len(body)) {
		pos = int64(len(body) / 4)
		if pos+span > int64(len(body)) {
			pos = 0
		}
	}
	for i := int64(0); i < span && pos+i < int64(len(body)); i++ {
		body[pos+i] = 0
	}
	return atomicReplace(path, body)
}

// atomicReplace writes via tmp+rename so a torn write here
// doesn't make the test itself a flake.  The mode is whatever
// the previous file had — preserves the agent's own write
// mode for a believable replay.
func atomicReplace(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".corrupt-tmp-"+randSuffix())
	prev, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = prev.Mode()
	}
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// The state.agentCmd helper is used elsewhere; assert here
// that the import-graph dependency stays pulled in.
var _ = exec.Command
