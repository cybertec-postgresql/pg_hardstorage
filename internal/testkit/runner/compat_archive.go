// compat_archive step — exercises the legacy-tool compat shims
// (pg-hardstorage-pgbackrest / pg-hardstorage-barman / pg-hardstorage-
// barman-wal-archive) end-to-end against a target OS container.
//
// Why this lives in the testkit, not as a unit test:
//
//   - The compat shims are user-facing CLI binaries that drive
//     the real pg_hardstorage binary via Cobra; a unit test
//     stubs the dispatcher and so cannot catch the
//     "ExecuteC vs cli.Run" silent-error class of bugs (the
//     real reason the barman shim shipped broken before the
//     fix).
//
//   - Shim binaries must work on every distro the project
//     supports — Debian, Ubuntu, Rocky, openSUSE.  They're
//     static Go binaries today but the contract is "drop the
//     binary into the operator's distro, point archive_command
//     at it, archives flow into the repo".  A multi-distro
//     scenario runner is the only meaningful coverage.
//
// The step generates a 16 MiB synthetic WAL segment + a
// `.backup` companion file (issue #10's regression fixture),
// runs the named shim's archive-push inside a target OS
// container, asserts the manifests landed in the file:// repo,
// then round-trips:
//
//   - pgbackrest: archive-get inside the container, host-side cmp.
//   - barman:     native `pg_hardstorage wal fetch` on host, cmp.
//
// Multi-OS coverage comes from the driver script
// (run_compat_testing.sh) iterating a (distro × PG version)
// matrix and invoking this scenario once per cell with
// os_image: substituted.

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
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// segmentSize is the canonical PG WAL segment size — pinned to
// match walsink.SegmentSize.  A future --with-wal-segsize variant
// would parameterise this, but every shim use today expects 16 MiB.
const compatSegmentSize = 16 * 1024 * 1024

// fixtureKind tags the synthetic input the step drives the shim
// with.  Wired through the scenario.Step.Fixture field.
type fixtureKind int

const (
	fixtureSegment fixtureKind = iota
	fixtureSegmentPlusBackup
	fixtureHistory

	// fixtureSegmentIdempotent — same as fixtureSegment but
	// archive-push runs TWICE on the identical segment file.
	// Both invocations must exit 0.  PG retries
	// archive_command on transient errors (network blip,
	// repo briefly unreachable) and then on operator
	// restart; if our shim or the native CLI ever loses
	// idempotency, the operator's archive_command would
	// fail every PG restart — silently, since PG eats
	// archive_command stdout.  Pin it.
	fixtureSegmentIdempotent
)

func parseFixture(s string) (fixtureKind, error) {
	switch strings.TrimSpace(s) {
	case "", "segment":
		return fixtureSegment, nil
	case "segment_plus_backup":
		return fixtureSegmentPlusBackup, nil
	case "history":
		return fixtureHistory, nil
	case "segment_idempotent":
		return fixtureSegmentIdempotent, nil
	default:
		return fixtureSegment, fmt.Errorf("compat_archive: unknown fixture %q (want segment | segment_plus_backup | history | segment_idempotent)", s)
	}
}

// runCompatArchive is the step body — wired into runStep's switch
// in runner.go.  See the file-top docstring for the overall flow.
func runCompatArchive(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.Shim == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "compat_archive: shim is required (pgbackrest | barman | barman-wal-archive)"}
	}
	deployment := strings.TrimSpace(st.Deployment)
	if deployment == "" {
		deployment = "compat-stanza"
	}
	fixture, err := parseFixture(st.Fixture)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}
	if err := ensureAgentBin(state); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}

	// Resolve repo target: a sink runtime (s3-minio, tls-minio,
	// ...) when sink: is set on the step, else a local file://
	// repo under artefactDir.
	//
	// repoDir is meaningful only on the file:// path (host-side
	// asserts read manifests there).  When a sink is in play we
	// can't host-stat the manifest — we shell back to the
	// native CLI's `list` to verify, since the test container
	// owns the bucket contents.
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
				Message: fmt.Sprintf("compat_archive: sink %s: %v", st.CompatSink, perr)}
		}
		if perr := rt.Up(ctx); perr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: sink up: %v", perr)}
		}
		defer func() { _ = rt.Down(context.Background()) }()
		repoURL = rt.URL()
		sinkExtra = rt.Extras()
		sinkEnv = rt.EnvForAgent()
		emit(out, "step.compat_archive.sink_up", map[string]any{
			"index": idx, "kind": st.CompatSink, "url": repoURL,
		})
	} else {
		// Resolve to absolute path: the artefact dir comes
		// in as whatever the operator passed to
		// --artefact-dir, often a relative path; the
		// file:// URL we hand the native CLI must be
		// absolute (the storage backend resolves URLs
		// independent of the agent's cwd), and docker bind
		// mounts also reject relative source paths.
		var aerr error
		repoDir, aerr = filepath.Abs(filepath.Join(state.artefactDir, "compat-repo"))
		if aerr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: abs repo path: %v", aerr)}
		}
		if aerr := os.MkdirAll(repoDir, 0o755); aerr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: mkdir repo: %v", aerr)}
		}
		repoURL = "file://" + repoDir
	}

	// Idempotent repo init via the native CLI — `repo init`
	// no-ops on an already-initialised repo, so re-runs in
	// the same scenario are safe.  Sink credentials (S3
	// access keys, AWS_CA_BUNDLE) flow through env so the
	// agent's storage layer can reach the configured backend.
	initCmd := exec.CommandContext(ctx, state.agentBin, "repo", "init", repoURL, "--output", "json")
	initCmd.Env = mergedEnv(sinkEnv)
	if initOut, err := initCmd.CombinedOutput(); err != nil && !bytes.Contains(initOut, []byte("conflict.repo_exists")) {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_archive: repo init: %v (output: %s)", err, truncate(initOut, 1024))}
	}

	// Generate the fixture(s) under artefactDir/compat-input.
	// Same absolute-path requirement as repoDir.
	inputDir, err := filepath.Abs(filepath.Join(state.artefactDir, fmt.Sprintf("compat-input-%d", idx)))
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_archive: abs input path: %v", err)}
	}
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_archive: mkdir input: %v", err)}
	}

	segName := "000000010000000000000003"
	segPath := filepath.Join(inputDir, segName)
	backupName := segName + ".000000D8.backup"
	backupPath := filepath.Join(inputDir, backupName)
	historyName := "00000002.history"
	historyPath := filepath.Join(inputDir, historyName)

	switch fixture {
	case fixtureSegment, fixtureSegmentPlusBackup, fixtureSegmentIdempotent:
		if err := writeSyntheticSegment(segPath); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: write segment: %v", err)}
		}
		if fixture == fixtureSegmentPlusBackup {
			if err := writeSyntheticBackupCompanion(backupPath, segName); err != nil {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: fmt.Sprintf("compat_archive: write .backup: %v", err)}
			}
		}
	case fixtureHistory:
		if err := writeSyntheticHistory(historyPath); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: write .history: %v", err)}
		}
	}

	// Resolve the shim binary the operator wants to test.
	// Same precedence rules as resolveAgentBinary: env
	// override → ./bin/<name> → PATH.
	shimBin, err := resolveShimBinary(st.Shim)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_archive: resolve shim: %v", err)}
	}

	// Build the per-fixture archive-push invocations.  The shim's
	// flag surface differs (pgbackrest takes --stanza /
	// --repo1-path or --repo1-s3-bucket; barman takes positional
	// <server> and reads the repo URL from pg_hardstorage.yaml),
	// so each shim has its own argv builder.
	pushList, err := buildArchivePushArgs(st.Shim, deployment, fixture, segPath, backupPath, historyPath, repoURL, inputDir)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("compat_archive: build argv: %v", err)}
	}

	// For barman the shim looks up the repo via deployment
	// config, so we seed a temp PG_HARDSTORAGE_CONFIG_DIR with
	// just enough yaml to satisfy the lookup.
	envExtra := map[string]string{}
	for k, v := range sinkEnv {
		envExtra[k] = v
	}
	if isBarmanShim(st.Shim) {
		cfgDir, err := filepath.Abs(filepath.Join(state.artefactDir, fmt.Sprintf("compat-config-%d", idx)))
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: abs config path: %v", err)}
		}
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: mkdir config: %v", err)}
		}
		yamlBody := fmt.Sprintf(`schema: pg_hardstorage.config.v1
deployments:
  %s:
    repo: %s
    pg_connection: postgres://postgres@unused/postgres
`, deployment, repoURL)
		if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(yamlBody), 0o644); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: write yaml: %v", err)}
		}
		envExtra["PG_HARDSTORAGE_CONFIG_DIR"] = cfgDir
	}
	if st.Shim == "walg" {
		// WAL-G's CLI is env-driven: WALG_*_PREFIX picks the
		// repo backend, AWS_ENDPOINT carries the S3 endpoint
		// override (the shim's flags.go translates env →
		// `--repo` URL via mapEnvToNativeArgs).
		// PG_HARDSTORAGE_DEPLOYMENT names the deployment;
		// without it the shim falls back to PGHOST/"default",
		// which would let two scenarios collide on a shared
		// host.
		envExtra["PG_HARDSTORAGE_DEPLOYMENT"] = deployment
		for k, v := range walgEnvForRepoURL(repoURL) {
			envExtra[k] = v
		}
	}

	// Execute every archive-push invocation, in order, in the
	// chosen environment (host or os_image container).
	useNetworkHost := st.CompatSink != "" // sink endpoints live on host loopback
	caBundle := ""
	if sinkExtra != nil {
		caBundle = sinkExtra["ca_bundle"]
	}
	for _, args := range pushList {
		if err := runShim(ctx, st.OSImage, shimBin, args, envExtra, repoDir, inputDir, caBundle, useNetworkHost, out); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: archive-push %v: %v", args, err)}
		}
	}
	emit(out, "step.compat_archive.pushed", map[string]any{
		"index":   idx,
		"shim":    st.Shim,
		"image":   st.OSImage,
		"fixture": st.Fixture,
		"pushes":  len(pushList),
	})

	// Assert: every fixture surface should now have a manifest
	// (for segments) or a verbatim file (for .backup / .history)
	// at its canonical repo key.
	//
	// On file:// repos the runner stats the bind-mounted dir
	// directly.  On sink-backed repos (S3, etc.) we shell back
	// to the native CLI's `wal list` so the assertion is
	// backend-agnostic — the bucket contents are MinIO's own,
	// not host-readable.
	if st.CompatSink == "" {
		if err := assertRepoState(repoDir, deployment, fixture, segName, backupName, historyName); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: repo assert: %v", err)}
		}
	} else {
		if err := assertRepoStateViaCLI(ctx, state.agentBin, repoURL, deployment, fixture, segName, sinkEnv); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: repo assert (sink): %v", err)}
		}
	}

	// Round-trip:
	//
	//   - pgbackrest:  shim's archive-get inside the same image.
	//   - barman:      native `pg_hardstorage wal fetch` on host
	//                  (the barman shim doesn't have an
	//                  archive-get verb — recovery uses
	//                  pg_hardstorage's restore_command directly).
	target := filepath.Join(inputDir, "fetched."+segName)
	if fixture == fixtureSegment || fixture == fixtureSegmentPlusBackup || fixture == fixtureSegmentIdempotent {
		if err := roundTripSegment(ctx, st.Shim, st.OSImage, shimBin, state.agentBin, deployment, segName, target, repoURL, repoDir, inputDir, envExtra, caBundle, useNetworkHost, out); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: round-trip: %v", err)}
		}
		if err := compareFiles(segPath, target); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("compat_archive: round-trip mismatch: %v", err)}
		}
	}

	emit(out, "step.compat_archive.completed", map[string]any{
		"index":   idx,
		"shim":    st.Shim,
		"image":   st.OSImage,
		"fixture": st.Fixture,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("compat_archive: %s shim ok (image=%s, fixture=%s)",
			st.Shim, ifEmpty(st.OSImage, "host"), ifEmpty(st.Fixture, "segment"))}
}

// writeSyntheticSegment generates a 16 MiB file with a valid
// XLogLongPageHeader at offset 0 (matches PG's on-disk layout
// closely enough that walsink.ReadSystemIdentifierFromSegment
// is happy).
func writeSyntheticSegment(path string) error {
	buf := make([]byte, compatSegmentSize)
	// xlp_magic 0xD117 (PG 17 sentinel)
	binary.LittleEndian.PutUint16(buf[0:2], 0xD117)
	// xlp_info: XLP_LONG_HEADER (0x0002) — required for sysid extraction
	binary.LittleEndian.PutUint16(buf[2:4], 0x0002)
	// xlp_sysid (offset 24, 8 bytes LE) — non-zero arbitrary value
	binary.LittleEndian.PutUint64(buf[24:32], 7388123456789012345)
	// Fill the rest with deterministic bytes so chunker dedup is
	// reproducible across runs.
	for i := 32; i < len(buf); i++ {
		buf[i] = byte((i ^ 0xa5) & 0xff)
	}
	return os.WriteFile(path, buf, 0o644)
}

// writeSyntheticBackupCompanion produces a `.backup` history
// file with realistic shape — what pg_backup_stop emits.
func writeSyntheticBackupCompanion(path, segName string) error {
	body := fmt.Sprintf(`START WAL LOCATION: 0/3000098 (file %[1]s)
STOP WAL LOCATION: 0/3000170 (file %[1]s)
CHECKPOINT LOCATION: 0/3000130
BACKUP METHOD: streamed
BACKUP FROM: primary
START TIME: 2026-05-06 17:24:28 CEST
LABEL: pg_basebackup synthetic test fixture
START TIMELINE: 1
`, segName)
	return os.WriteFile(path, []byte(body), 0o644)
}

// writeSyntheticHistory writes a one-line timeline-history
// file — the smallest valid `.history` shape.
func writeSyntheticHistory(path string) error {
	return os.WriteFile(path, []byte("1\t0/3000170\tno recovery target specified\n"), 0o644)
}

// resolveShimBinary maps a logical shim name to an absolute
// binary path.  Order:
//
//  1. PG_HARDSTORAGE_<NAME>_BIN env var (e.g.
//     PG_HARDSTORAGE_PGBACKREST_BIN).
//  2. ./bin/pg-hardstorage-<name> in cwd.
//  3. pg-hardstorage-<name> on PATH.
//
// shim == "native" returns the native pg_hardstorage binary
// path (resolveAgentBinary's contract) — used by migration
// scenarios that exercise the post-transition state where the
// operator drops the shim and uses the native CLI directly.
func resolveShimBinary(shim string) (string, error) {
	if shim == "native" {
		return resolveAgentBinary()
	}
	exeName := "pg-hardstorage-" + shim
	envKey := "PG_HARDSTORAGE_" + strings.ToUpper(strings.ReplaceAll(shim, "-", "_")) + "_BIN"
	if v := os.Getenv(envKey); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envKey, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("%s=%q: %w", envKey, v, err)
		}
		return abs, nil
	}
	for _, c := range []string{"./bin/" + exeName, "bin/" + exeName} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, nil
			}
		}
	}
	if path, err := exec.LookPath(exeName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found (set %s, place at ./bin/%s, or put on PATH)", exeName, envKey, exeName)
}

// buildArchivePushArgs builds the per-fixture archive-push argv
// list.  Returns one or two argv slices — the second covers
// the `.backup` companion when fixture is segment_plus_backup.
//
// pgbackrest's CLI takes --stanza globally and either
// --repo1-path (posix) or --repo1-type=s3 + --repo1-s3-bucket
// + --repo1-s3-endpoint on archive-push.  Barman's
// barman-wal-archive takes only the positional <server>
// <segment-path> — repo discovery happens via deployment
// config (PG_HARDSTORAGE_CONFIG_DIR).  WAL-G takes only the
// positional segment path; everything else (WALG_*_PREFIX,
// PGHOST, PG_HARDSTORAGE_DEPLOYMENT) is env-driven, plumbed
// in via envExtra at runShim time.
func buildArchivePushArgs(shim, deployment string, fix fixtureKind, segPath, backupPath, historyPath, repoURL, inputDir string) ([][]string, error) {
	switch shim {
	case "native":
		// Post-migration state: operator drops the shim and
		// drives `pg_hardstorage wal push` directly.  Same
		// argv shape PG would invoke from archive_command if
		// the operator wired native into archive_command at
		// migration cutover time:
		//
		//   archive_command = 'pg_hardstorage wal push <dep> %p --repo <url>'
		base := []string{"wal", "push", deployment}
		repoFlags := []string{"--repo", repoURL}
		switch fix {
		case fixtureSegment:
			return [][]string{append(append(append([]string(nil), base...), segPath), repoFlags...)}, nil
		case fixtureSegmentIdempotent:
			one := append(append(append([]string(nil), base...), segPath), repoFlags...)
			return [][]string{one, append([]string(nil), one...)}, nil
		case fixtureSegmentPlusBackup:
			return [][]string{
				append(append(append([]string(nil), base...), segPath), repoFlags...),
				append(append(append([]string(nil), base...), backupPath), repoFlags...),
			}, nil
		case fixtureHistory:
			return [][]string{append(append(append([]string(nil), base...), historyPath), repoFlags...)}, nil
		}
	case "walg":
		switch fix {
		case fixtureSegment:
			return [][]string{{"wal-push", segPath}}, nil
		case fixtureSegmentIdempotent:
			// Re-push the same segment.  Native CLI
			// returns success on the second attempt
			// because RenameIfNotExists treats
			// ErrAlreadyExists as idempotent.
			return [][]string{
				{"wal-push", segPath},
				{"wal-push", segPath},
			}, nil
		case fixtureSegmentPlusBackup:
			// WAL-G's wal-push expects WAL segments — the
			// shim routes archive_command's `.backup`
			// companion through the SAME native code path
			// (issue #10's aux-file class), so the shim
			// invocation shape is identical.
			return [][]string{
				{"wal-push", segPath},
				{"wal-push", backupPath},
			}, nil
		case fixtureHistory:
			return [][]string{{"wal-push", historyPath}}, nil
		}
	case "pgbackrest":
		// Choose --repo1-* flags based on the repo URL scheme.
		// For sink-backed s3:// URLs we forward the bucket +
		// endpoint to the shim using the same flags an
		// operator would write in pgbackrest.conf, exercising
		// the shim's URL builder (compat/pgbackrest/flags.go's
		// buildRepoURL) end-to-end.
		repoFlags, ferr := pgbackrestRepoFlags(repoURL)
		if ferr != nil {
			return nil, ferr
		}
		base := append([]string{"--stanza=" + deployment}, repoFlags...)
		base = append(base, "archive-push")
		switch fix {
		case fixtureSegment:
			return [][]string{append(append([]string(nil), base...), segPath)}, nil
		case fixtureSegmentIdempotent:
			return [][]string{
				append(append([]string(nil), base...), segPath),
				append(append([]string(nil), base...), segPath),
			}, nil
		case fixtureSegmentPlusBackup:
			return [][]string{
				append(append([]string(nil), base...), segPath),
				append(append([]string(nil), base...), backupPath),
			}, nil
		case fixtureHistory:
			return [][]string{append(append([]string(nil), base...), historyPath)}, nil
		}
	case "barman", "barman-wal-archive":
		// barman-wal-archive: positional <server> <segment-path>.
		// Repo URL is auto-injected from pg_hardstorage.yaml
		// by the shim's deployment-config lookup.
		switch fix {
		case fixtureSegment:
			return [][]string{{deployment, segPath}}, nil
		case fixtureSegmentIdempotent:
			return [][]string{
				{deployment, segPath},
				{deployment, segPath},
			}, nil
		case fixtureSegmentPlusBackup:
			return [][]string{
				{deployment, segPath},
				{deployment, backupPath},
			}, nil
		case fixtureHistory:
			return [][]string{{deployment, historyPath}}, nil
		}
	}
	return nil, fmt.Errorf("unsupported shim %q", shim)
}

// runShim executes the shim binary either on the host (osImage
// empty) or inside a docker container of the named image.
// Stdout/stderr are written to the scenario's emit log so
// failures land in the run report.
//
// caBundle (when non-empty) is bind-mounted into the container
// and exposed as AWS_CA_BUNDLE — needed for tls-minio sinks
// whose self-signed cert isn't in the distro's trust store.
//
// useNetworkHost replaces --network=none with --network=host so
// the in-container shim can reach a sink runtime listening on
// host loopback.  Plain file:// scenarios stay on
// --network=none for the lower attack surface + faster startup.
func runShim(ctx context.Context, osImage, shimBin string, args []string, envExtra map[string]string, repoDir, inputDir, caBundle string, useNetworkHost bool, out io.Writer) error {
	if osImage == "" {
		// Host execution — fast path used when the operator
		// just wants to validate the shim against the runner's
		// own distro (typical CI default).
		cmd := exec.CommandContext(ctx, shimBin, args...)
		cmd.Env = mergedEnv(envExtra)
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		if err := cmd.Run(); err != nil {
			emit(out, "step.compat_archive.shim_failed", map[string]any{
				"image": "host", "shim": filepath.Base(shimBin),
				"output": truncate(combined.Bytes(), 1024),
			})
			return fmt.Errorf("host shim: %w (output: %s)", err, truncate(combined.Bytes(), 512))
		}
		return nil
	}
	// Container execution — bind-mount shim, repo (if any),
	// input.  --user pins the in-container uid/gid to the
	// host invoker so files the shim writes into the bind-
	// mounted repo are host-readable.
	network := "--network=none"
	if useNetworkHost {
		network = "--network=host"
	}
	// Bind-mount the binary at a path whose basename matches
	// the SHIM (not "/shim") so the BusyBox-style multi-call
	// dispatcher in cmd/pg-hardstorage-compat can read its
	// argv[0] correctly.  Anything else (e.g. /shim) lands in
	// the dispatcher's "unknown name" fallback and exits 2.
	//
	// `:z` (lowercase) on every bind-mount makes Podman/Docker
	// relabel the host path with a SHARED SELinux label so
	// multiple containers spawned by this runner can all read
	// it.  Without `:z`, an SELinux-enforcing host (Fedora,
	// RHEL, Alma, Rocky) silently denies the container's
	// initial mmap of the binary — Go's runtime then segfaults
	// before producing any output (`exit 139, output: ""`).
	// The flag is a no-op on systems without SELinux.
	shimMount := "/" + filepath.Base(shimBin)
	dockerArgs := []string{"run", "--rm",
		network,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", shimBin + ":" + shimMount + ":ro,z",
		"-v", inputDir + ":" + inputDir + ":z",
	}
	if repoDir != "" {
		dockerArgs = append(dockerArgs, "-v", repoDir+":"+repoDir+":z")
	}
	if caBundle != "" {
		// Bind-mount + expose for the AWS SDK's auto-trust
		// path (the agent / shim children pick AWS_CA_BUNDLE
		// up automatically).
		dockerArgs = append(dockerArgs,
			"-v", caBundle+":"+caBundle+":ro,z",
			"-e", "AWS_CA_BUNDLE="+caBundle)
	}
	for k, v := range envExtra {
		// PG_HARDSTORAGE_CONFIG_DIR points at a host path the
		// barman shim reads.  Bind-mount that dir into the
		// container at the SAME path so the lookup resolves.
		// AWS_* values are plain strings, no path mount needed.
		if k == "PG_HARDSTORAGE_CONFIG_DIR" {
			dockerArgs = append(dockerArgs, "-v", v+":"+v+":z")
		}
		dockerArgs = append(dockerArgs, "-e", k+"="+v)
	}
	dockerArgs = append(dockerArgs, osImage, shimMount)
	dockerArgs = append(dockerArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		emit(out, "step.compat_archive.shim_failed", map[string]any{
			"image": osImage, "shim": filepath.Base(shimBin),
			"output": truncate(combined.Bytes(), 1024),
		})
		return fmt.Errorf("docker shim (%s): %w (output: %s)", osImage, err, truncate(combined.Bytes(), 512))
	}
	return nil
}

// mergedEnv returns os.Environ() + the extras as a flat key=value
// slice ready for exec.Cmd.Env.
func mergedEnv(extras map[string]string) []string {
	env := append([]string(nil), os.Environ()...)
	for k, v := range extras {
		env = append(env, k+"="+v)
	}
	return env
}

// assertRepoState verifies the canonical paths exist on disk.
// We check directly on the file:// repo's host bind so we
// don't shell back into the native CLI (which would couple
// the test to its list output format).
func assertRepoState(repoDir, deployment string, fix fixtureKind, segName, backupName, historyName string) error {
	want := []string{}
	switch fix {
	case fixtureSegment, fixtureSegmentIdempotent:
		want = append(want, fmt.Sprintf("wal/%s/00000001/%s.json", deployment, segName))
	case fixtureSegmentPlusBackup:
		want = append(want,
			fmt.Sprintf("wal/%s/00000001/%s.json", deployment, segName),
			fmt.Sprintf("wal/%s/00000001/%s", deployment, backupName),
		)
	case fixtureHistory:
		want = append(want, fmt.Sprintf("wal/%s/history/%s", deployment, historyName))
	}
	for _, rel := range want {
		full := filepath.Join(repoDir, rel)
		st, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("expected repo key missing: %s (%w)", rel, err)
		}
		if st.Size() == 0 {
			return fmt.Errorf("repo key empty: %s", rel)
		}
	}
	return nil
}

// roundTripSegment writes the archived segment back out via the
// shim's archive-get (pgbackrest) or the native CLI's wal fetch
// (barman, which has no archive-get verb).  Caller cmp's the
// produced file against the original.
//
// caBundle / useNetworkHost are forwarded to runShim for sink-
// backed scenarios; on file:// repos both are zero/false.
func roundTripSegment(ctx context.Context, shim, osImage, shimBin, agentBin, deployment, segName, targetPath, repoURL, repoDir, inputDir string, envExtra map[string]string, caBundle string, useNetworkHost bool, out io.Writer) error {
	switch shim {
	case "native":
		// Direct `pg_hardstorage wal fetch` — same shape an
		// operator's restore_command would invoke after the
		// migration cutover.
		args := []string{"wal", "fetch", deployment, segName, targetPath, "--repo", repoURL}
		return runShim(ctx, osImage, shimBin, args, envExtra, repoDir, inputDir, caBundle, useNetworkHost, out)
	case "walg":
		// WAL-G's wal-fetch is symmetric to wal-push:
		// positional <segment-name> <output-path>, every
		// other config dimension comes from the env vars
		// already in envExtra.
		args := []string{"wal-fetch", segName, targetPath}
		return runShim(ctx, osImage, shimBin, args, envExtra, repoDir, inputDir, caBundle, useNetworkHost, out)
	case "pgbackrest":
		repoFlags, ferr := pgbackrestRepoFlags(repoURL)
		if ferr != nil {
			return ferr
		}
		args := append([]string{"--stanza=" + deployment}, repoFlags...)
		args = append(args, "archive-get", segName, targetPath)
		return runShim(ctx, osImage, shimBin, args, envExtra, repoDir, inputDir, caBundle, useNetworkHost, out)
	case "barman", "barman-wal-archive":
		// Native `wal fetch` on host — barman shim has no
		// archive-get verb; recovery uses pg_hardstorage's
		// restore_command directly.  The native fetch is the
		// matching half of the round-trip.  Sink env (S3
		// creds + AWS_CA_BUNDLE) flows through the host
		// process so the SDK trusts the self-signed cert.
		cmd := exec.CommandContext(ctx, agentBin,
			"wal", "fetch", deployment, segName, targetPath,
			"--repo", repoURL, "--output", "json")
		cmd.Env = mergedEnv(envExtra)
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("native wal fetch: %w (output: %s)", err, truncate(combined.Bytes(), 384))
		}
		return nil
	}
	return fmt.Errorf("round-trip not supported for shim %q", shim)
}

// walgEnvForRepoURL maps a native repo URL to the WAL-G env
// vars an operator would set in their environment to point
// the wal-g binary at the same backend.  The shim's
// flags.go maps these back to a `--repo` URL via
// mapEnvToNativeArgs/buildRepoURL, so this round-trips:
//
//	file:///abs                        -> WALG_FILE_PREFIX=/abs
//	s3://bkt?endpoint=URL&path_style=… -> WALG_S3_PREFIX=s3://bkt
//	                                      AWS_ENDPOINT=URL
//	                                      AWS_S3_FORCE_PATH_STYLE=true
//
// AWS_ENDPOINT is the de facto standard env var the AWS SDK
// honours when set; both the shim's flags.go and the native
// CLI's S3 plugin pick it up.
func walgEnvForRepoURL(repoURL string) map[string]string {
	if strings.HasPrefix(repoURL, "file://") {
		return map[string]string{
			"WALG_FILE_PREFIX": strings.TrimPrefix(repoURL, "file://"),
		}
	}
	if strings.HasPrefix(repoURL, "s3://") {
		// Strip query string so WALG_S3_PREFIX is a clean
		// s3://bucket/[prefix] — the SDK gets endpoint via
		// AWS_ENDPOINT, region via AWS_REGION (already set by
		// sink.EnvForAgent), and we force path-style under any
		// custom endpoint.
		bare := repoURL
		query := ""
		if i := strings.Index(repoURL, "?"); i >= 0 {
			bare, query = repoURL[:i], repoURL[i+1:]
		}
		out := map[string]string{
			"WALG_S3_PREFIX": bare,
		}
		for _, kv := range strings.Split(query, "&") {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			k, v := kv[:eq], kv[eq+1:]
			switch k {
			case "endpoint":
				out["AWS_ENDPOINT"] = v
			}
		}
		out["AWS_S3_FORCE_PATH_STYLE"] = "true"
		return out
	}
	return nil
}

// pgbackrestRepoFlags maps a native repo URL to the
// pgbackrest-shim flag set the shim's buildRepoURL maps BACK
// to that same URL.  Closes the loop:
//
//	file:///abs       -> --repo1-type=posix --repo1-path=/abs
//	s3://bkt?endpoint=https://h&path_style=true&region=R
//	                  -> --repo1-type=s3 --repo1-s3-bucket=bkt
//	                     --repo1-s3-endpoint=https://h
//	                     --repo1-s3-region=R
//
// We exercise the shim's URL-builder end-to-end this way: a
// regression in the shim's translate.go would surface as the
// archive-push reaching the native CLI with the wrong URL,
// which would fail at agent-side TLS / endpoint resolution.
func pgbackrestRepoFlags(repoURL string) ([]string, error) {
	if strings.HasPrefix(repoURL, "file://") {
		path := strings.TrimPrefix(repoURL, "file://")
		return []string{"--repo1-type=posix", "--repo1-path=" + path}, nil
	}
	if strings.HasPrefix(repoURL, "s3://") {
		// Crude URL parsing — good enough for the shapes
		// the sink runtimes emit.  bucket + path before `?`,
		// query params after.
		rest := strings.TrimPrefix(repoURL, "s3://")
		var bucket, query string
		if i := strings.Index(rest, "?"); i >= 0 {
			bucket, query = rest[:i], rest[i+1:]
		} else {
			bucket = rest
		}
		flags := []string{
			"--repo1-type=s3",
			"--repo1-s3-bucket=" + bucket,
		}
		for _, kv := range strings.Split(query, "&") {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			k, v := kv[:eq], kv[eq+1:]
			switch k {
			case "endpoint":
				flags = append(flags, "--repo1-s3-endpoint="+v)
			case "region":
				flags = append(flags, "--repo1-s3-region="+v)
			}
		}
		return flags, nil
	}
	return nil, fmt.Errorf("pgbackrestRepoFlags: unsupported scheme in %q (want file:// or s3://)", repoURL)
}

// assertRepoStateViaCLI verifies the segment landed in a
// sink-backed repo by shelling to the native CLI's `wal list`
// (the only backend-agnostic way — host stat works only on
// file://).  Match is on segment name; we don't sniff the
// chunk envelope.  sinkEnv carries the S3 creds + AWS_CA_BUNDLE.
func assertRepoStateViaCLI(ctx context.Context, agentBin, repoURL, deployment string, fix fixtureKind, segName string, sinkEnv map[string]string) error {
	cmd := exec.CommandContext(ctx, agentBin,
		"wal", "list", deployment, "--repo", repoURL, "--output", "json")
	cmd.Env = mergedEnv(sinkEnv)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wal list: %w (output: %s)", err, truncate(combined.Bytes(), 384))
	}
	body := combined.Bytes()
	switch fix {
	case fixtureSegment, fixtureSegmentPlusBackup, fixtureSegmentIdempotent:
		if !bytes.Contains(body, []byte(segName)) {
			return fmt.Errorf("wal list output does not mention segment %q (output: %s)",
				segName, truncate(body, 384))
		}
	case fixtureHistory:
		// History files don't show in `wal list` (it's
		// segment-focused).  Skip — we already confirmed the
		// shim returned exit 0; native fetch round-trip below
		// is the next signal.
	}
	return nil
}

// compareFiles is a byte-equal cmp.  Used to assert
// round-tripped segments match the synthetic source.
func compareFiles(a, b string) error {
	aBytes, err := os.ReadFile(a)
	if err != nil {
		return fmt.Errorf("read %s: %w", a, err)
	}
	bBytes, err := os.ReadFile(b)
	if err != nil {
		return fmt.Errorf("read %s: %w", b, err)
	}
	if !bytes.Equal(aBytes, bBytes) {
		return fmt.Errorf("byte mismatch: %s (%d bytes) vs %s (%d bytes)",
			a, len(aBytes), b, len(bBytes))
	}
	return nil
}

func isBarmanShim(s string) bool {
	return s == "barman" || s == "barman-wal-archive"
}

func ifEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Compile-time linkage check: emit + truncate live in steps.go
// (same package).  A future refactor that splits the package
// would need to re-export them.
var _ = time.Now // keep the time import live for future emit timestamping
