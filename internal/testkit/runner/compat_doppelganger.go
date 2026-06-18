// compat_doppelganger step — split-brain archive-collision driver.
//
// Two clusters that share a system_identifier + timeline (the
// classic "operator cp -a'd a datadir, forgot pg_resetwal, both
// PGs are now archiving") will both call archive_command on
// the same segment number with DIFFERENT content.  The second
// push hits a manifest that already exists at the canonical
// key.  Without explicit verification, that race becomes
// silent-success: the loser's archive_command exits 0, PG
// advances confirmed_flush_lsn, the slot rotates the segment
// off disk, the operator believes the archive worked.
//
// This step exercises that race directly — push segment A,
// then push a doppelgänger A' (same name, same xlp_sysid,
// different body bytes), and assert the second push surfaces
// a structured error in the splitbrain.* class.  Pre-fix
// (today's main): the second push silently succeeds, the
// step's expect=error mode reports the silent-success bug.
// Post-fix: the second push errors, the step passes.

package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// runCompatDoppelganger drives the split-brain push race.
// Re-uses compat_archive's helpers (resolveShimBinary,
// buildArchivePushArgs, runShim, fetch round-trip) but
// orchestrates two distinct pushes.
//
// Step shape (yaml):
//
//   - compat_doppelganger:
//     shim: native           # or pgbackrest / barman / walg
//     deployment: prod-db
//     sink: ""               # optional sink (default file://)
//
// The step always produces fixture-A (default content) for
// the first push and fixture-A' (same name, same sysid,
// different bytes) for the second push.  Expect: the second
// push errors with `splitbrain.content_mismatch`.
func runCompatDoppelganger(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.Shim == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "compat_doppelganger: shim is required (pgbackrest | barman | barman-wal-archive | walg | native)"}
	}
	deployment := strings.TrimSpace(st.Deployment)
	if deployment == "" {
		deployment = "compat-doppelganger"
	}
	if err := ensureAgentBin(state); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}

	// Repo: file:// under artefactDir, OR a sink runtime.
	// Mirrors runCompatArchive's branching.
	var (
		repoDir   string
		repoURL   string
		sinkExtra map[string]string
		sinkEnv   map[string]string
	)
	if st.CompatSink != "" {
		rt, perr := sink.New(st.CompatSink)
		if perr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: sink %s: %v", st.CompatSink, perr)}
		}
		if perr := rt.Up(ctx); perr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: sink up: %v", perr)}
		}
		defer func() { _ = rt.Down(context.Background()) }()
		repoURL = rt.URL()
		sinkExtra = rt.Extras()
		sinkEnv = rt.EnvForAgent()
	} else {
		var aerr error
		repoDir, aerr = filepath.Abs(filepath.Join(state.artefactDir, "compat-repo"))
		if aerr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: abs repo path: %v", aerr)}
		}
		if aerr := os.MkdirAll(repoDir, 0o755); aerr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: mkdir repo: %v", aerr)}
		}
		repoURL = "file://" + repoDir
	}

	// Idempotent native repo init.
	initCmd := exec.CommandContext(ctx, state.agentBin, "repo", "init", repoURL, "--output", "json")
	initCmd.Env = mergedEnv(sinkEnv)
	if initOut, err := initCmd.CombinedOutput(); err != nil && !bytes.Contains(initOut, []byte("conflict.repo_exists")) {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: repo init: %v (output: %s)", err, truncate(initOut, 1024))}
	}

	// Two input files under different per-side dirs but
	// identical basenames — the SHIM is invoked twice with
	// different host paths that share the in-segment name.
	inputAbs, err := filepath.Abs(filepath.Join(state.artefactDir, fmt.Sprintf("compat-doppelganger-%d", idx)))
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: abs input: %v", err)}
	}
	dirA := filepath.Join(inputAbs, "side-a")
	dirB := filepath.Join(inputAbs, "side-b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: mkdir %s: %v", d, err)}
		}
	}
	segName := "000000010000000000000003"
	pathA := filepath.Join(dirA, segName)
	pathB := filepath.Join(dirB, segName)

	// Same xlp_sysid (the cloned-datadir invariant), DIFFERENT
	// body bytes.  i^0xa5 vs i^0x5a flips every byte from
	// offset 32 onward — the chunker emits a wholly different
	// chunk-hash list, so the manifests diverge in their
	// Chunks slices even though every other manifest field is
	// identical.
	if err := writeDoppelgangerSegment(pathA, 0xa5); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: write A: %v", err)}
	}
	if err := writeDoppelgangerSegment(pathB, 0x5a); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: write B: %v", err)}
	}

	shimBin, err := resolveShimBinary(st.Shim)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: resolve shim: %v", err)}
	}

	// First push — should land cleanly.
	useNetworkHost := st.CompatSink != ""
	caBundle := ""
	if sinkExtra != nil {
		caBundle = sinkExtra["ca_bundle"]
	}
	envExtra := map[string]string{}
	for k, v := range sinkEnv {
		envExtra[k] = v
	}
	argsA, err := buildSinglePushArgs(st.Shim, deployment, pathA, repoURL)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}
	if err := runShim(ctx, st.OSImage, shimBin, argsA, envExtra, repoDir, dirA, caBundle, useNetworkHost, out); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: first push (cluster A) failed: %v", err)}
	}
	emit(out, "step.compat_doppelganger.first_pushed", map[string]any{"index": idx})

	// Second push — the doppelgänger.  Capture exit code +
	// stderr so we can decide pass/fail based on the
	// post-fix contract (splitbrain.* surfaced) vs the
	// pre-fix bug (silent success).
	argsB, err := buildSinglePushArgs(st.Shim, deployment, pathB, repoURL)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}
	pushBOut, pushBErr := runShimCapture(ctx, st.OSImage, shimBin, argsB, envExtra, repoDir, dirB, caBundle, useNetworkHost)
	emit(out, "step.compat_doppelganger.second_pushed", map[string]any{
		"index":            idx,
		"second_succeeded": pushBErr == nil,
		"output":           truncate(pushBOut, 512),
	})

	// Post-fix contract: the second push MUST fail with a
	// splitbrain.* structured error code.  Any other outcome
	// (silent success OR an unrelated error) is the bug we're
	// pinning.
	if pushBErr == nil {
		// Pre-fix: silent success.  Verify by fetching the
		// archived segment and proving the repo holds A's
		// content (the loser, B, has been silently
		// discarded).
		_ = pushBErr // explicit: not nil-error path below
		fetched := filepath.Join(inputAbs, "fetched."+segName)
		if rerr := roundTripSegment(ctx, st.Shim, st.OSImage, shimBin, state.agentBin,
			deployment, segName, fetched, repoURL,
			repoDir, dirA, envExtra, caBundle, useNetworkHost, out); rerr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_doppelganger: post-collision fetch failed: %v", rerr)}
		}
		matchesA := compareFiles(pathA, fetched) == nil
		matchesB := compareFiles(pathB, fetched) == nil
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: doppelgänger push silently succeeded (split-brain undetected). Repo holds: A=%v B=%v", matchesA, matchesB)}
	}
	if !bytes.Contains(pushBOut, []byte("splitbrain.")) {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_doppelganger: second push errored but not with splitbrain.* code (output: %s)", truncate(pushBOut, 512))}
	}
	emit(out, "step.compat_doppelganger.detected", map[string]any{
		"index":  idx,
		"output": truncate(pushBOut, 512),
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("compat_doppelganger: split-brain detected (shim=%s sink=%s)", st.Shim, ifEmpty(st.CompatSink, "file://"))}
}

// writeDoppelgangerSegment writes a 16 MiB segment with the
// canonical xlp_sysid + xlp_tli (so it shares system_identifier
// with the default fixture) and a body XORed by `mask`.
// Default fixture uses 0xa5; the doppelgänger uses 0x5a; every
// byte from offset 32 onward differs, so the FastCDC chunker
// produces a fully distinct chunk-hash list — the cleanest way
// to assert "different content under the same name".
func writeDoppelgangerSegment(path string, mask byte) error {
	buf := make([]byte, compatSegmentSize)
	binary.LittleEndian.PutUint16(buf[0:2], 0xD117)
	binary.LittleEndian.PutUint16(buf[2:4], 0x0002) // XLP_LONG_HEADER
	binary.LittleEndian.PutUint64(buf[24:32], 7388123456789012345)
	for i := 32; i < len(buf); i++ {
		buf[i] = byte(i) ^ mask
	}
	return os.WriteFile(path, buf, 0o644)
}

// buildSinglePushArgs is a one-input-path variant of
// buildArchivePushArgs — keeps the doppelgänger step decoupled
// from the regular fixture-driven argv builder.
func buildSinglePushArgs(shim, deployment, segPath, repoURL string) ([]string, error) {
	switch shim {
	case "native":
		return []string{"wal", "push", deployment, segPath, "--repo", repoURL}, nil
	case "walg":
		return []string{"wal-push", segPath}, nil
	case "pgbackrest":
		repoFlags, ferr := pgbackrestRepoFlags(repoURL)
		if ferr != nil {
			return nil, ferr
		}
		out := append([]string{"--stanza=" + deployment}, repoFlags...)
		return append(out, "archive-push", segPath), nil
	case "barman", "barman-wal-archive":
		return []string{deployment, segPath}, nil
	}
	return nil, fmt.Errorf("compat_doppelganger: unsupported shim %q", shim)
}

// runShimCapture is runShim that returns (output, err) instead
// of writing failures into the emit log + nil err on success.
// The doppelgänger step needs the raw stdout/stderr to detect
// splitbrain.* error codes regardless of whether the shim
// returns the structured error directly or wraps it.
func runShimCapture(ctx context.Context, osImage, shimBin string, args []string, envExtra map[string]string, repoDir, inputDir, caBundle string, useNetworkHost bool) ([]byte, error) {
	if osImage == "" {
		cmd := exec.CommandContext(ctx, shimBin, args...)
		cmd.Env = mergedEnv(envExtra)
		return cmd.CombinedOutput()
	}
	network := "--network=none"
	if useNetworkHost {
		network = "--network=host"
	}
	// argv[0]-driven multi-call dispatcher: mount under
	// /<basename> so the in-container shim sees its real name.
	shimMount := "/" + filepath.Base(shimBin)
	dockerArgs := []string{"run", "--rm",
		network,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", shimBin + ":" + shimMount + ":ro",
		"-v", inputDir + ":" + inputDir,
	}
	if repoDir != "" {
		dockerArgs = append(dockerArgs, "-v", repoDir+":"+repoDir)
	}
	if caBundle != "" {
		dockerArgs = append(dockerArgs,
			"-v", caBundle+":"+caBundle+":ro",
			"-e", "AWS_CA_BUNDLE="+caBundle)
	}
	for k, v := range envExtra {
		if k == "PG_HARDSTORAGE_CONFIG_DIR" {
			dockerArgs = append(dockerArgs, "-v", v+":"+v)
		}
		dockerArgs = append(dockerArgs, "-e", k+"="+v)
	}
	dockerArgs = append(dockerArgs, osImage, shimMount)
	dockerArgs = append(dockerArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	return cmd.CombinedOutput()
}
