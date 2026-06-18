// Package sink is the testkit's storage-backend orchestration —
// the moral equivalent of the topology package, but for repos
// instead of PG clusters.
//
// Why this exists
// ---------------
// Today every soak / scenario uses `file://` for its repo,
// because that's the only backend the testkit knows how to
// bring up.  The S3 / GCS / Azure / SFTP plugins under
// internal/plugin/storage/ all exist and work — they just
// have no test surface that drives them.  This package fills
// that gap: a Runtime brings up an emulator container (MinIO
// for S3, Azurite for Azure Blob, fake-gcs-server for GCS,
// atmoz/sftp for SFTP), exposes a URL the agent's storage
// layer can dial, and tears down on exit.
//
// Self-contained, hermetic, air-gappable
// --------------------------------------
//   - Each Runtime brings up its own container; there is no
//     shared state between tests.
//   - Image tags are PINNED in the SinkImages map below; runs
//     are reproducible across time and CI environments.
//   - `pg_hardstorage_testkit images pull-sinks` (Sink-3) pre-
//     fetches every image so subsequent runs work offline.
//   - --airgap (Sink-4) refuses to fetch missing images at run
//     time, surfacing the gap as a pre-flight error rather
//     than a silent network call.
//
// Lifecycle
// ---------
//
//	r, err := sink.New("s3-minio")
//	defer r.Down(context.Background())
//	if err := r.Up(ctx); err != nil { ... }
//	repoURL := r.URL()
//	env := r.EnvForAgent()                  // creds for shell-outs
//
// Up MUST be idempotent on a fresh Runtime: the testkit may
// stop and restart a sink mid-scenario for fault-injection
// experiments.
package sink

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"time"
)

// execCommand is var'd so tests can swap in a stub.  The
// production binding goes straight to os/exec.CommandContext.
var execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// Runtime is the testkit's view of a storage backend.  Every
// method receives a context the caller can cancel to abort
// pending work.
type Runtime interface {
	// Name returns the operator-visible identifier ("s3-minio",
	// "azurite", "gcs-fake", "sftp").  Used for logging,
	// audit, and error attribution.
	Name() string

	// Up starts the backend container (or whatever the
	// underlying primitive is), waits for readiness, and
	// returns nil once the URL() is dialable.  Idempotent
	// only against a freshly-constructed Runtime; calling Up
	// twice on the same instance returns an error.
	Up(ctx context.Context) error

	// Down stops + removes the container and reclaims any
	// host-side state (temp dirs, ports).  Idempotent: safe
	// to call on a never-Up'd Runtime, safe to call twice.
	Down(ctx context.Context) error

	// URL is the storage-plugin-format URL the agent would
	// pass as its --repo flag.  Examples:
	//
	//	s3://testkit?endpoint=http://127.0.0.1:9000&path_style=true
	//	gs://testkit?endpoint=http://127.0.0.1:4443
	//	azure://devstoreaccount1/testkit?endpoint=http://127.0.0.1:10000
	//	sftp://testkit:testkit@127.0.0.1:2222/upload
	//
	// Only valid after a successful Up.
	URL() string

	// EnvForAgent returns the environment variables the
	// agent's shell-out invocations need to authenticate
	// against URL().  For S3: AWS_ACCESS_KEY_ID +
	// AWS_SECRET_ACCESS_KEY.  For SFTP: nothing (creds in URL).
	// Caller merges into the existing env when invoking
	// `pg_hardstorage backup` / `restore`.
	EnvForAgent() map[string]string

	// ContainerName returns the docker container name the
	// sink is running in, or "" when the runtime is not
	// container-backed (no current implementations are like
	// that, but a future in-process emulator could be).
	// Used by the testkit's inject framework to register a
	// `role: sink` target — `inject: { kind: sink_pause }`
	// runs `docker pause` against this name.  Only valid
	// after a successful Up.
	ContainerName() string

	// Extras returns plugin-side configuration not
	// expressible in URL form — keys go straight into
	// storage.StorageConfig.Extras at Open time.  Used
	// today only by the SFTP runtime, which exposes a
	// per-test known_hosts file and a password (the SFTP
	// plugin refuses StrictHostKeyChecking=no, correctly).
	// Other runtimes return nil.
	Extras() map[string]string
}

// SinkImages is the canonical sink kind → docker image map.
// Tags are pinned for reproducibility; bumps land as their
// own commits with a short note explaining what changed
// upstream.
//
// `pg_hardstorage_testkit images pull-sinks` iterates this
// map.  Air-gap mode treats this as the universe of allowed
// images — anything missing locally fails pre-flight.
var SinkImages = map[string]string{
	"s3-minio":  "minio/minio:RELEASE.2025-01-20T14-49-07Z",
	"tls-minio": "minio/minio:RELEASE.2025-01-20T14-49-07Z", // same image; TLS toggled by mounting certs
	"azurite":   "mcr.microsoft.com/azure-storage/azurite:3.33.0",
	"gcs-fake":  "fsouza/fake-gcs-server:1.49.0",
	"sftp":      "atmoz/sftp:alpine-3.7",
}

// New builds a Runtime by kind name.  Unknown kinds error
// loudly so a typo in scenario YAML (`kind: s3-mino`) doesn't
// silently fall back to file://.
func New(kind string) (Runtime, error) {
	switch kind {
	case "s3-minio":
		return newMinIO(), nil
	case "tls-minio":
		return newTLSMinIO(), nil
	case "azurite":
		return newAzurite(), nil
	case "gcs-fake":
		return newGCSFake(), nil
	case "sftp":
		return newSFTP(), nil
	}
	return nil, fmt.Errorf("sink: unknown kind %q (known: %v)", kind, KnownKinds())
}

// KnownKinds returns the supported sink kinds, sorted for
// stable error messages.  Tracks SinkImages.
func KnownKinds() []string {
	out := make([]string, 0, len(SinkImages))
	for k := range SinkImages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// truncate caps a byte slice at n bytes for log output.
// Local helper — Go doesn't ship a stdlib equivalent and
// we'd rather not pull in a dep just for this.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// pickFreePort asks the kernel for an unused localhost port.
// Shared by every sink runtime.  Brief race window between
// close and docker bind is acceptable for a per-test sandbox.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitTCPReady polls 127.0.0.1:port with short-timeout TCP
// dials until one succeeds or `total` elapses.  Coarse but
// universally applicable readiness probe — good enough for
// emulators that aren't HTTP (SFTP) and ones whose HTTP
// surface returns 4xx for the root path (Azurite).
func waitTCPReady(ctx context.Context, port int, total time.Duration) error {
	deadline := time.Now().Add(total)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("sink: %s did not become ready within %s", addr, total)
}

// PreflightAirgap reports whether the docker image for the
// supplied sink kind is already present locally — a
// pre-flight check the testkit's --airgap mode runs BEFORE
// the soak / scenario starts.  Missing images become a
// pre-flight error pointing at `image pull-sinks`, rather
// than a silent docker-pull during the run that would
// violate the air-gap promise.
//
// The check shells out to `docker image inspect`; any
// non-zero exit is treated as "image absent".  Returns nil
// when the image exists, an error with operator-actionable
// detail otherwise.
func PreflightAirgap(ctx context.Context, kind string) error {
	img, ok := SinkImages[kind]
	if !ok {
		return fmt.Errorf("airgap preflight: unknown sink kind %q (known: %v)",
			kind, KnownKinds())
	}
	cmd := execCommand(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", img)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("airgap preflight: image %q (sink kind %q) not present locally — "+
			"run `pg_hardstorage_testkit image pull-sinks --only=%s` on a connected box first",
			img, kind, kind)
	}
	return nil
}
