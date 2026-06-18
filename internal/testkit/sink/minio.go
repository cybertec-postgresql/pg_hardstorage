// minio.go — minioRuntime: single-node MinIO container as S3-API endpoint for testkit runs.
package sink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

// minioRuntime brings up a single-node MinIO container as an
// S3-API endpoint for the testkit.  Bucket creation is done
// via filesystem layout — MinIO treats every top-level dir
// under /data as a bucket — which avoids needing to install
// `mc` or wire up the AWS SDK in this package.
//
// Lifecycle:
//
//  1. Up()
//     - reserves a free localhost port (kernel-assigned)
//     - creates a host temp dir + a `<bucket>/` subdir
//     (this IS the pre-created bucket; no init step)
//     - docker run -d minio/minio server /data
//     - polls /minio/health/ready until 200 or timeout
//
//  2. URL() returns:
//     s3://<bucket>?endpoint=http://127.0.0.1:<port>&path_style=true
//
//  3. EnvForAgent() returns the S3 access keys MinIO is
//     configured with; the agent's invocation merges them
//     into its env so the AWS SDK's default credential
//     chain finds them.
//
//  4. Down()
//     - docker rm -f <container>
//     - os.RemoveAll(<temp dir>)
//
// Idempotency notes
// -----------------
// Up returns an error if called twice on the same instance —
// the container name and port are bound to the first run.
// Down is safe to call multiple times and on a never-Up'd
// instance (both ops short-circuit on empty state).
type minioRuntime struct {
	bucket    string
	accessKey string
	secretKey string

	// Lifecycle state.  All set by Up, cleared by Down.
	container string // docker container name (empty when Down)
	dataDir   string // host tempdir bind-mounted at /data
	port      int    // host port → 9000 inside the container
}

// minioCounter dedupes container names within a single
// process when multiple Runtimes are constructed back-to-back
// (e.g., a soak with --parallel slots).  The wall-clock part
// of the name is millisecond-resolution, which can collide on
// a fast process; the atomic counter eliminates that risk.
var minioCounter atomic.Uint64

func newMinIO() *minioRuntime {
	return &minioRuntime{
		bucket:    "testkit",
		accessKey: "testkit",
		secretKey: "testkitsecret",
	}
}

// Name implements Runtime.
func (m *minioRuntime) Name() string { return "s3-minio" }

// Up starts the MinIO container and waits for readiness.
// Returns the first error encountered; partial state is
// cleaned up before return so the caller can retry without
// leftover containers / temp dirs.
func (m *minioRuntime) Up(ctx context.Context) error {
	if m.container != "" {
		return errors.New("minioRuntime: already up (call Down first)")
	}

	// Free localhost port.  We close the listener immediately;
	// the brief race window before docker binds is acceptable
	// for a per-test sandbox.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("minio sink: pick port: %w", err)
	}
	m.port = l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// Pre-create the bucket via filesystem layout — top-level
	// directories under /data are buckets in MinIO's
	// filesystem-mode storage.  Avoids needing mc / AWS SDK
	// in this package just to issue a CreateBucket.
	dir, err := os.MkdirTemp("", "pg-hs-minio-*")
	if err != nil {
		return fmt.Errorf("minio sink: tempdir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, m.bucket), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("minio sink: mkdir bucket: %w", err)
	}
	m.dataDir = dir

	// Container name composition:
	//   - UnixNano:  ~1 ns resolution within a process.
	//   - os.Getpid: distinguishes between forked test processes
	//                (run_compat_testing.sh --parallel N spawns
	//                disjoint shell+testkit processes; each had its
	//                own counter starting at 1, and a millisecond-
	//                resolution clock occasionally returned the
	//                same tick on two processes — producing
	//                `Conflict. The container name "pg-hs-minio-
	//                <millis>-2" is already in use` collisions
	//                under --parallel 2 / 3 in past soak runs).
	//   - counter:   defence-in-depth within a single process.
	m.container = fmt.Sprintf("pg-hs-minio-%d-%d-%d",
		time.Now().UnixNano(), os.Getpid(), minioCounter.Add(1))

	// `:z` (lowercase) on the data-dir bind-mount: SELinux-
	// enforcing hosts (Fedora, RHEL, Alma, Rocky) deny the
	// MinIO container's writes to a host tempdir labelled
	// `tmp_t` / `user_tmp_t`.  Without this, the container
	// starts but every PUT to /data fails silently, the
	// MINIO_ROOT_USER bootstrap never completes, and the
	// readiness probe times out after 60s.  The flag is a
	// no-op on systems without SELinux.
	args := []string{
		"run", "-d",
		"--name", m.container,
		// See tls_minio.go for the rationale: MinIO recommends
		// nofile≥65536, and the inherited Docker daemon default
		// (1024 soft on many distros) causes startup failures
		// under parallel-cell campaign load.
		"--ulimit", "nofile=65536:65536",
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", m.port),
		"-v", fmt.Sprintf("%s:/data:z", m.dataDir),
		"-e", "MINIO_ROOT_USER=" + m.accessKey,
		"-e", "MINIO_ROOT_PASSWORD=" + m.secretKey,
		SinkImages["s3-minio"],
		"server", "/data",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Cleanup the temp dir — the container creation
		// failed, so there's no container to remove.
		_ = os.RemoveAll(m.dataDir)
		m.dataDir = ""
		container := m.container
		m.container = ""
		return fmt.Errorf("minio sink: docker run %s: %w (output: %s)",
			container, err, truncate(out, 256))
	}

	// 120s budget (was 60s).  Docker daemon contention with 4
	// parallel campaigns (compat + soak + scenario-sweep + k8s
	// minikube) observed on 2026-05-09 stretched bring-up past
	// 60s on multiple cells.  120s gives a 24× margin over warm-
	// host startup (~5s) while still catching a real hang well
	// within human patience.
	if err := m.waitReady(ctx, 120*time.Second); err != nil {
		// Best-effort cleanup; preserve the underlying error
		// for the caller (the container may hold logs the
		// caller wants).  Down handles the rm.
		_ = m.Down(context.Background())
		return err
	}
	return nil
}

// waitReady polls MinIO's /minio/health/ready AND a real
// S3-shaped endpoint (HEAD /<bucket>/) until BOTH return a
// non-503.  Why both: /minio/health/ready only checks that
// MinIO's HTTP server is up — it returns 200 well before the
// IAM + storage backend finishes initialising, so PutObject
// from a fast caller hits 503
//
//	XMinioServerNotInitialized: Server not initialized yet,
//	please try again.
//
// even though /health/ready is happy.  Adding a HEAD on the
// bucket URL exercises the same path PutObject uses; once
// that returns anything other than 503 the server is ready
// for real traffic.  HEAD on a not-yet-created bucket
// correctly returns 404 once IAM is up — that's the pass
// signal we wait for.  Per-poll timeout is short (1 s) so a
// slow container start doesn't translate into a stuck poll.
func (m *minioRuntime) waitReady(ctx context.Context, total time.Duration) error {
	deadline := time.Now().Add(total)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/minio/health/ready", m.port)
	bucketURL := fmt.Sprintf("http://127.0.0.1:%d/%s/", m.port, m.bucket)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Stage 1: liveness — HTTP server up.
		hresp, err := client.Get(healthURL)
		if err != nil || hresp.StatusCode != http.StatusOK {
			if hresp != nil {
				_ = hresp.Body.Close()
			}
			goto sleep
		}
		_ = hresp.Body.Close()
		// Stage 2: real S3-shaped op.  HEAD on bucket: 404
		// (NoSuchBucket) is fine — that means IAM resolved the
		// auth+path and only the bucket itself doesn't exist
		// yet.  503 means MinIO is still bringing the storage /
		// IAM layer up; keep polling.
		{
			req, _ := http.NewRequestWithContext(ctx, "HEAD", bucketURL, nil)
			bresp, berr := client.Do(req)
			if berr == nil {
				code := bresp.StatusCode
				_ = bresp.Body.Close()
				if code != http.StatusServiceUnavailable {
					return nil
				}
			}
		}
	sleep:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("minio sink: %s did not become ready within %s",
		m.container, total)
}

// Down stops + removes the container and reclaims the temp
// dir.  Idempotent.  All errors are swallowed to best-effort
// because Down is typically called from a defer and we'd
// rather leak an orphan container than mask the original
// scenario error.  Operators can `docker rm -f $(docker ps
// -q --filter name=pg-hs-minio-)` to clean up leaks.
func (m *minioRuntime) Down(ctx context.Context) error {
	if m.container != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", m.container).Run()
		m.container = ""
	}
	if m.dataDir != "" {
		_ = os.RemoveAll(m.dataDir)
		m.dataDir = ""
	}
	m.port = 0
	return nil
}

// URL implements Runtime.  path_style=true is required for
// MinIO (and any non-AWS S3 endpoint) — vhost-style addressing
// would try to dial bucket.127.0.0.1, which doesn't resolve.
func (m *minioRuntime) URL() string {
	return fmt.Sprintf("s3://%s?endpoint=http://127.0.0.1:%d&path_style=true&region=us-east-1",
		m.bucket, m.port)
}

// EnvForAgent returns the S3 access keys MinIO is configured
// with.  The agent's storage layer (internal/plugin/storage/s3)
// uses the AWS SDK's default credential chain, which reads
// AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY from the
// environment first.  AWS_REGION pre-empts the SDK's
// default-region resolution to keep test runs deterministic
// across boxes that may have different AWS_DEFAULT_REGION.
func (m *minioRuntime) EnvForAgent() map[string]string {
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     m.accessKey,
		"AWS_SECRET_ACCESS_KEY": m.secretKey,
		"AWS_REGION":            "us-east-1",
	}
}

// ContainerName implements Runtime.
func (m *minioRuntime) ContainerName() string { return m.container }

// Extras implements Runtime.  S3 plugin needs nothing
// beyond URL params + env vars.
func (m *minioRuntime) Extras() map[string]string { return nil }
