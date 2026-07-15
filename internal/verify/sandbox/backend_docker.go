// The Docker sandbox backend.  Always built; the Docker
// path is the v0.1 flavour and continues to be the default
//.  Operators on Linux + KVM who want stronger
// isolation pick the Firecracker backend (`-tags
// firecracker`) instead.

package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
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

	rc, reader, err := cnt.Exec(ctx, verifyBackupArgs(binPath))
	if err != nil {
		rc, reader, err = cnt.Exec(ctx, verifyBackupArgs("pg_verifybackup"))
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
// output and de-multiplexes it into separate stdout / stderr
// strings.
//
// Docker's exec stream (when not TTY-allocated, which is the
// case here) frames each chunk with an 8-byte header:
//
//	byte 0     stream type (1 = stdout, 2 = stderr)
//	bytes 1-3  reserved (zero)
//	bytes 4-7  big-endian uint32 payload length
//
// De-muxing matters because the caller classifies a
// missing-backup_manifest run as Skipped by matching
// pg_verifybackup's message — which PG writes to STDERR.  The
// old implementation folded everything into stdout and
// returned an empty stderr, so isMissingManifestError could
// never match and a manifest-less backup was mis-reported as a
// verification FAILURE instead of Skipped.
//
// If the buffer doesn't look framed (e.g. a TTY-allocated
// stream, or a backend that returns raw combined output), we
// fall back to treating the whole thing as stderr so the
// classifier still gets a chance to see the message — the same
// place PG would have written it.
func readMultiplexed(r interface{ Read(p []byte) (int, error) }) (string, string) {
	if r == nil {
		return "", ""
	}
	var raw bytes.Buffer
	_, _ = io.Copy(&raw, asReader(r))
	body := raw.Bytes()

	var outBuf, errBuf bytes.Buffer
	if !demuxDockerStream(body, &outBuf, &errBuf) {
		// Not a framed stream — treat everything as stderr so
		// message-matching (missing manifest) still works.
		return "", stripControl(string(body))
	}
	return stripControl(outBuf.String()), stripControl(errBuf.String())
}

// demuxDockerStream parses Docker's 8-byte-header multiplexed
// exec stream, appending stdout payloads to out and stderr
// payloads to errW.  Returns false when the buffer does not
// conform to the framing (in which case out/errW are left
// untouched and the caller falls back to combined handling).
func demuxDockerStream(body []byte, out, errW *bytes.Buffer) bool {
	const headerLen = 8
	if len(body) < headerLen {
		return false
	}
	i := 0
	sawFrame := false
	for i+headerLen <= len(body) {
		streamType := body[i]
		// Valid stream types are 0 (stdin), 1 (stdout), 2
		// (stderr).  Anything else means we're not looking at a
		// real frame header — bail so the caller falls back.
		if streamType > 2 || body[i+1] != 0 || body[i+2] != 0 || body[i+3] != 0 {
			return false
		}
		n := int(binary.BigEndian.Uint32(body[i+4 : i+8]))
		i += headerLen
		if n < 0 || i+n > len(body) {
			// Truncated / malformed frame — not a clean stream.
			return false
		}
		payload := body[i : i+n]
		i += n
		switch streamType {
		case 2:
			errW.Write(payload)
		default:
			out.Write(payload)
		}
		sawFrame = true
	}
	// Trailing bytes that don't form a full header means the
	// buffer isn't cleanly framed.
	if i != len(body) {
		return false
	}
	return sawFrame
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
