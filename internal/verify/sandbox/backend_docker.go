// The Docker sandbox backend.  Always built; the Docker
// path is the v0.1 flavour and continues to be the default
//.  Operators on Linux + KVM who want stronger
// isolation pick the Firecracker backend (`-tags
// firecracker`) instead.

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func init() {
	register(dockerBackend{})
}

// dockerBackend is the Docker-via-testcontainers backend.
type dockerBackend struct{}

// Name returns the registered backend identifier.
func (dockerBackend) Name() string { return BackendDocker }

// Verify spins up a Docker container with the restored data dir
// mounted read-only, runs pg_verifybackup inside, and returns the
// structured Result. Returns a partial Result with the error so
// callers can still see how far the run got.
func (dockerBackend) Verify(ctx context.Context, opts Options) (*Result, error) {
	res := &Result{
		Schema:    SchemaResult,
		Backend:   BackendDocker,
		PGMajor:   opts.PGMajor,
		Image:     opts.Image,
		Tool:      "pg_verifybackup",
		StartedAt: time.Now().UTC(),
	}
	defer func() {
		res.StoppedAt = time.Now().UTC()
		res.Duration = res.StoppedAt.Sub(res.StartedAt)
	}()

	// We start the container with /bin/sleep so the PG
	// entrypoint doesn't try to initialise a cluster on top
	// of our restored data dir.  The entrypoint refuses
	// anyway because PG_VERSION is present, but starting it
	// just to fight us is a waste; sleep + Exec is the
	// cleaner pattern.
	req := tc.ContainerRequest{
		Image:      opts.Image,
		Entrypoint: []string{"/bin/sleep"},
		Cmd:        []string{"infinity"},
		WaitingFor: wait.ForExec([]string{"/bin/true"}).WithStartupTimeout(60 * time.Second),
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, opts.DataDir+":/var/lib/postgresql/data:ro")
		},
	}

	cnt, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return res, fmt.Errorf("sandbox/docker: container up: %w", err)
	}
	defer func() {
		tearCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = cnt.Terminate(tearCtx)
	}()

	// Locate the pg_verifybackup binary.  Debian PG images
	// ship it at /usr/lib/postgresql/<major>/bin/
	// pg_verifybackup; some images also expose it on PATH
	// via a symlink.  We try the versioned path first, then
	// PATH.
	binPath := "/usr/lib/postgresql/" + opts.PGMajor + "/bin/pg_verifybackup"

	rc, reader, err := cnt.Exec(ctx, []string{binPath, "/var/lib/postgresql/data"})
	if err != nil {
		rc, reader, err = cnt.Exec(ctx, []string{"pg_verifybackup", "/var/lib/postgresql/data"})
		if err != nil {
			return res, fmt.Errorf("sandbox/docker: exec pg_verifybackup: %w", err)
		}
	}

	stdout, stderr := readMultiplexed(reader)
	res.Stdout = stdout
	res.Stderr = stderr

	// pg_verifybackup exits 0 on success, non-zero on any
	// verification failure or if backup_manifest is missing.
	// Distinguish "skipped because backup_manifest is
	// absent" from "real verification failure" by looking at
	// stderr.
	if rc == 0 {
		res.Passed = true
		return res, nil
	}
	if isMissingManifestError(stderr) {
		res.Skipped = true
		res.SkipReason = "backup_manifest absent (pg_basebackup-style manifest was not captured)"
		return res, nil
	}
	res.Passed = false
	return res, nil
}

// readMultiplexed reads testcontainers' multiplexed exec
// output into stdout / stderr strings.  The multiplexed
// stream has 8-byte headers per chunk; we don't bother
// parsing — we just split the resulting text on the
// conventional headers and trust that PG client tools emit
// reasonably-clean lines.
//
// The simpler robust approach is to capture everything into
// one buffer; testcontainers' Reader returns the multiplexed
// stream and callers either de-multiplex via stdcopy or
// accept the noise.  We choose the lossy path: read
// everything into one string, treat it as combined output.
// Operators don't care about stdout vs stderr for
// pg_verifybackup — the message says what's wrong either
// way.
func readMultiplexed(r interface{ Read(p []byte) (int, error) }) (string, string) {
	if r == nil {
		return "", ""
	}
	var buf bytes.Buffer
	_, _ = bufio.NewReader(asReader(r)).WriteTo(&buf)
	body := buf.String()
	// Strip non-printable control bytes that the multiplexed
	// framing leaks into the buffer when we don't de-mux
	// properly.  The lossy approach is acceptable here: the
	// emitted text is for an operator to read.
	return strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 {
			return r
		}
		if r == '\n' || r == '\t' {
			return r
		}
		return -1
	}, body), ""
}

// asReader normalises an `interface{ Read(p []byte) (int,
// error) }` to an io.Reader-compatible wrapper.
// testcontainers' Exec returns io.Reader; this is a thin
// adapter for type-assertion clarity.
type readerLike interface{ Read(p []byte) (int, error) }

func asReader(r readerLike) *adapter { return &adapter{r: r} }

type adapter struct{ r readerLike }

// Read forwards to the wrapped readerLike, satisfying io.Reader.
func (a *adapter) Read(p []byte) (int, error) { return a.r.Read(p) }
