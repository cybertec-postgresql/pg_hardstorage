// gcs_fake.go — Google Cloud Storage emulator sink
// (fsouza/fake-gcs-server).
package sink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

// gcsFakeRuntime brings up fsouza/fake-gcs-server, the most
// widely-used GCS emulator.  The Go GCS SDK accepts an
// `?endpoint=` override that we plumb through the gcs
// plugin's URL.
//
// The bucket is created via the `-data /data/<bucket>`
// filesystem layout — fake-gcs-server treats every
// top-level dir under -data as a pre-existing bucket, so
// no API-level CreateBucket is needed.
type gcsFakeRuntime struct {
	container string
	port      int
	dataDir   string // host tempdir bind-mounted at /data
}

const gcsFakeBucket = "testkit"

var gcsFakeCounter atomic.Uint64

func newGCSFake() *gcsFakeRuntime { return &gcsFakeRuntime{} }

// Name returns "gcs-fake".
func (g *gcsFakeRuntime) Name() string { return "gcs-fake" }

// Up runs the fake-gcs-server container on a free port with a
// bind-mounted data dir that pre-seeds the testkit bucket,
// then waits both for the TCP listener and for the bucket-
// metadata endpoint to return 200 (the data-dir scan finishes
// after the listener opens).
func (g *gcsFakeRuntime) Up(ctx context.Context) error {
	if g.container != "" {
		return errors.New("gcsFakeRuntime: already up")
	}
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("gcs-fake sink: pick port: %w", err)
	}
	g.port = port
	g.container = fmt.Sprintf("pg-hs-gcs-fake-%d-%d",
		time.Now().UnixMilli(), gcsFakeCounter.Add(1))

	// Pre-create the bucket via the filesystem layout —
	// fake-gcs-server (1.49.0) treats every top-level
	// directory under -filesystem-root as a pre-existing
	// bucket.  Same pattern MinIO uses; avoids needing an
	// API-level CreateBucket step in the sink.
	//
	// Earlier attempt with -bootstrap-bucket flag failed:
	// the flag isn't supported in 1.49.0, so the container
	// died at startup and every test op then 404'd against
	// a non-existent bucket, sending the SDK into infinite
	// retries.  The data-dir layout is the documented and
	// stable pattern.
	dir, err := os.MkdirTemp("", "pg-hs-gcs-fake-*")
	if err != nil {
		return fmt.Errorf("gcs-fake sink: tempdir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, gcsFakeBucket), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("gcs-fake sink: mkdir bucket: %w", err)
	}
	g.dataDir = dir

	// fake-gcs-server's --public-host is the URL clients
	// expect; we publish 127.0.0.1:<port> so the SDK's
	// signed-URL / host-header validation matches the
	// endpoint we hand to the plugin.
	// Backend choice matters here.  filesystem-root mode
	// has a subtle JSON-vs-XML API divergence in 1.49.0 —
	// objects written via PUT are visible to XML-API Get
	// but not to JSON-API Stat / List / Delete / Rename
	// (caught by the contract suite as 6 sub-case
	// failures).  Memory backend with `-data` seeding
	// keeps a single in-process map for both API surfaces,
	// so all operations agree on what exists.  Pre-create
	// the bucket via a same-name subdir under the seed
	// dir; fake-gcs-server walks `-data` at startup and
	// converts every top-level dir into a bucket.
	args := []string{
		"run", "-d",
		"--name", g.container,
		"-p", fmt.Sprintf("127.0.0.1:%d:4443", g.port),
		// `:z` (lowercase): SELinux-shared label so the fake-
		// gcs-server container can read the host tempdir on
		// SELinux-enforcing hosts.  No-op on non-SELinux.
		"-v", fmt.Sprintf("%s:/data:z", g.dataDir),
		SinkImages["gcs-fake"],
		"-scheme", "http",
		"-host", "0.0.0.0",
		"-port", "4443",
		"-public-host", fmt.Sprintf("127.0.0.1:%d", g.port),
		"-backend", "memory",
		"-data", "/data",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		g.container = ""
		return fmt.Errorf("gcs-fake sink: docker run: %w (output: %s)",
			err, truncate(out, 256))
	}
	if err := waitTCPReady(ctx, g.port, 30*time.Second); err != nil {
		_ = g.Down(context.Background())
		return err
	}
	// fake-gcs-server's TCP socket opens before it's
	// finished scanning the bind-mounted data dir for
	// pre-existing buckets.  If the contract test races
	// in too early, List operations 404 with "bucket
	// doesn't exist" even though the object Put / Get
	// paths work (those create bucket entries lazily).
	// Probe `GET /storage/v1/b/<bucket>` until the bucket
	// is registered; bound the wait so a misconfigured
	// run doesn't hang forever.
	if err := g.waitBucketReady(ctx, 15*time.Second); err != nil {
		_ = g.Down(context.Background())
		return err
	}
	return nil
}

// waitBucketReady polls the bucket-metadata endpoint until
// fake-gcs-server returns 200, or until the deadline
// elapses.  Required because the container's TCP listener
// opens before the data-dir scan completes.
func (g *gcsFakeRuntime) waitBucketReady(ctx context.Context, total time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/storage/v1/b/%s", g.port, gcsFakeBucket)
	deadline := time.Now().Add(total)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("gcs-fake sink: bucket %s not registered within %s", gcsFakeBucket, total)
}

// Down removes the container, deletes the bind-mounted data
// tempdir, and clears the recorded port.  Idempotent.
func (g *gcsFakeRuntime) Down(ctx context.Context) error {
	if g.container != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", g.container).Run()
		g.container = ""
	}
	if g.dataDir != "" {
		_ = os.RemoveAll(g.dataDir)
		g.dataDir = ""
	}
	g.port = 0
	return nil
}

// URL points at the emulator using ONLY the plain
// gcs://<bucket> form — endpoint routing is left entirely
// to STORAGE_EMULATOR_HOST (set in EnvForAgent).
//
// Why not also pass `endpoint=` URL param: the Go GCS
// SDK treats STORAGE_EMULATOR_HOST and WithEndpoint
// (which the plugin maps `endpoint=` onto) as different
// modes.  The env-var path puts the SDK in "emulator
// mode" (HTTP, no auth, no signed URLs); the
// WithEndpoint path keeps it in production mode (HTTPS,
// auth required, signed URLs).  Setting both at once
// produces split-brain behaviour — Put goes through one
// path, Stat through another — which is what surfaced
// as the 6 contract sub-case failures originally
// attributed to fake-gcs-server JSON/XML divergence.
//
// Production deployments hitting real GCS continue to
// use `gcs://<bucket>?endpoint=<custom>` for endpoint
// override; the emulator path is the testkit-only
// shortcut that goes via env.
func (g *gcsFakeRuntime) URL() string {
	return fmt.Sprintf("gcs://%s", gcsFakeBucket)
}

// EnvForAgent — fake-gcs-server doesn't validate
// credentials, so any string in GOOGLE_APPLICATION_CREDENTIALS
// or anonymous auth is fine.  We set
// STORAGE_EMULATOR_HOST as the documented fallback the SDK
// recognises for emulator detection; the gcs plugin's
// endpoint= URL parameter takes precedence in practice.
func (g *gcsFakeRuntime) EnvForAgent() map[string]string {
	return map[string]string{
		"STORAGE_EMULATOR_HOST": fmt.Sprintf("http://127.0.0.1:%d", g.port),
	}
}

// ContainerName implements Runtime.
func (g *gcsFakeRuntime) ContainerName() string { return g.container }

// Extras implements Runtime.  GCS plugin reads creds
// from URL query params + env vars; no Extras needed.
func (g *gcsFakeRuntime) Extras() map[string]string { return nil }
