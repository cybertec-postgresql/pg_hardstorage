// Package sandbox is the verify sandbox.
//
// Given a restored PGDATA directory, the sandbox spins up an
// isolated environment, runs `pg_verifybackup` against the
// data dir, and tears down.  Operators reach for this via
// `pg_hardstorage verify --full`; the testkit reaches for it
// via `assert: pg_verifybackup: { passes: true }`.
//
// # Backends
//
// The sandbox is backend-pluggable.  Two backends ship in
// :
//
//   - **docker** (default; always built) — Docker via
//     testcontainers-go.  The image is the official
//     `postgres:<major>` (Debian, ships pg_verifybackup).
//     Requires a Docker socket on the agent host.
//   - **firecracker** (`-tags firecracker`) — Firecracker
//     microVM via firecracker-go-sdk.  Boots a stripped
//     kernel + an operator-supplied rootfs that exposes
//     pg_verifybackup; the restored PGDATA mounts as a
//     read-only virtio-blk drive.  Strongest isolation
//     posture: no shared kernel with the host, no Docker
//     daemon to attack.  Linux + KVM only.
//
// Both backends produce the same `Result` shape, so callers
// (CLI verify --full, agent verify scheduler, recovery
// drills) stay backend-agnostic.
//
// What ships:
//
//   - `pg_verifybackup` invocation against a bind/disk-mounted
//     data dir.  Returns structured pass / fail / skipped.
//   - Backend selection via `Options.Backend` ("docker" |
//     "firecracker" | "" → docker).
//
// What's deliberately NOT:
//
//   - Starting PG inside the sandbox.  The image / rootfs is
//     just a vehicle for the client tools; we never `pg_ctl
//     start` the cluster.  Starting would require a writable
//     copy of PGDATA (initial cluster start mutates pg_control
//     etc.); separate hop tracked alongside `pg_amcheck`.
//   - Smoke SQL execution.  Same reason; needs a running
//     cluster.
//   - K8s-Job sandbox backend.  Plug-in path open via the
//     same Backend interface; not in this binary's roadmap.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// Result is the structured outcome of a sandbox verify run.
//
// On-disk layout is part of the schema-compatibility
// commitment — see SchemaResult.  Callers that depend on
// this shape (manifests/<deployment>/backups/<id>/
// verification.json, the LLM evidence-bundle exporter,
// recovery-drill reports) get 24-month forward compatibility.
type Result struct {
	Schema    string    `json:"schema"`
	Backend   string    `json:"backend"`
	PGMajor   string    `json:"pg_major"`
	Image     string    `json:"image,omitempty"`
	Passed    bool      `json:"passed"`
	Tool      string    `json:"tool"` // "pg_verifybackup"
	StartedAt time.Time `json:"started_at"`
	StoppedAt time.Time `json:"stopped_at"`
	// Duration is serialised via Result.MarshalJSON as whole
	// milliseconds under the frozen JSON key "duration_ms" — the
	// struct tag is "-" because the (Un)MarshalJSON pair own the
	// field's wire representation (a raw time.Duration would
	// otherwise emit nanoseconds under that key).
	Duration   time.Duration `json:"-"`
	Stdout     string        `json:"stdout,omitempty"`
	Stderr     string        `json:"stderr,omitempty"`
	Skipped    bool          `json:"skipped,omitempty"`
	SkipReason string        `json:"skip_reason,omitempty"`
}

// SchemaResult is the JSON schema string for sandbox verify
// results.  Frozen for 24 months.
const SchemaResult = "pg_hardstorage.verify.sandbox.v1"

// resultJSON is the wire form of Result.  It mirrors every
// Result field EXCEPT Duration, which it replaces with an
// integer-milliseconds field under the same frozen JSON key
// ("duration_ms").
//
// Why the custom form: Result.Duration is a time.Duration,
// whose natural JSON encoding is its nanosecond count.  Emitting
// that under the key "duration_ms" told every consumer the run
// took 1e6x longer than it did (a 5s run serialised as
// 5000000000, read back as "5 000 000 000 ms").  We can't rename
// the key — it's part of the 24-month-frozen schema — so we keep
// the KEY and fix the VALUE: divide nanoseconds down to whole
// milliseconds on the way out, and multiply back on the way in.
type resultJSON struct {
	Schema     string    `json:"schema"`
	Backend    string    `json:"backend"`
	PGMajor    string    `json:"pg_major"`
	Image      string    `json:"image,omitempty"`
	Passed     bool      `json:"passed"`
	Tool       string    `json:"tool"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"`
	SkipReason string    `json:"skip_reason,omitempty"`
}

// MarshalJSON emits Result with duration_ms carrying whole
// milliseconds instead of the raw nanosecond count of the
// underlying time.Duration.  The "duration_ms" key is
// preserved verbatim.
func (r Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(resultJSON{
		Schema:     r.Schema,
		Backend:    r.Backend,
		PGMajor:    r.PGMajor,
		Image:      r.Image,
		Passed:     r.Passed,
		Tool:       r.Tool,
		StartedAt:  r.StartedAt,
		StoppedAt:  r.StoppedAt,
		DurationMS: r.Duration.Milliseconds(),
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		Skipped:    r.Skipped,
		SkipReason: r.SkipReason,
	})
}

// UnmarshalJSON reads a Result back, interpreting duration_ms
// as whole milliseconds (the inverse of MarshalJSON).
func (r *Result) UnmarshalJSON(b []byte) error {
	var j resultJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	r.Schema = j.Schema
	r.Backend = j.Backend
	r.PGMajor = j.PGMajor
	r.Image = j.Image
	r.Passed = j.Passed
	r.Tool = j.Tool
	r.StartedAt = j.StartedAt
	r.StoppedAt = j.StoppedAt
	r.Duration = time.Duration(j.DurationMS) * time.Millisecond
	r.Stdout = j.Stdout
	r.Stderr = j.Stderr
	r.Skipped = j.Skipped
	r.SkipReason = j.SkipReason
	return nil
}

// Options configures one verify run.  The same struct is
// passed to every Backend; backend-specific fields (e.g.
// Firecracker's kernel + rootfs paths) live on this struct
// and are ignored by backends that don't consume them.
type Options struct {
	// DataDir is the host-side path to the restored PGDATA.
	// Must contain backup_manifest if pg_verifybackup is to
	// do meaningful work; we surface a structured "skipped"
	// result if it doesn't.
	DataDir string

	// PGMajor is the major version of the source PG ("15",
	// "16", "17").  The Docker image / Firecracker rootfs
	// must match so the pg_verifybackup binary's protocol
	// expectations align with what was captured.  Defaults to
	// pg.DefaultSandboxMajor when empty.
	PGMajor string

	// Image overrides the default `postgres:<major>` Docker
	// image.  Useful for air-gapped environments with a
	// private mirror.  Ignored by non-Docker backends.
	Image string

	// Backend selects the sandbox backend.  Valid values:
	//
	//   ""           → "docker" (default)
	//   "docker"     → Docker via testcontainers
	//   "firecracker" → Firecracker microVM (requires
	//                  -tags firecracker build flavour)
	//
	// Unknown values surface a clear refusal with the list
	// of available backends.
	Backend string

	// FirecrackerKernel is the path to a Linux kernel image
	// (typically `vmlinux`) the Firecracker microVM boots.
	// Required when Backend == "firecracker".
	FirecrackerKernel string

	// FirecrackerRootfs is the path to a rootfs ext4 image
	// containing pg_verifybackup at /usr/bin/pg_verifybackup
	// (or PATH-discoverable).  Required when Backend ==
	// "firecracker".  Mounted read-only.
	FirecrackerRootfs string

	// FirecrackerBin is the absolute path to the
	// `firecracker` binary.  Defaults to `firecracker` on
	// PATH.
	FirecrackerBin string

	// FirecrackerVCPU + FirecrackerMemMiB cap the microVM's
	// resources.  Defaults: 2 vCPU, 1024 MiB.
	FirecrackerVCPU   int64
	FirecrackerMemMiB int64
}

// resolved returns the Options with defaults applied.
func (o Options) resolved() Options {
	out := o
	if out.PGMajor == "" {
		out.PGMajor = fmt.Sprintf("%d", pg.DefaultSandboxMajor)
	}
	if out.Image == "" {
		out.Image = "postgres:" + out.PGMajor
	}
	if out.Backend == "" {
		out.Backend = BackendDocker
	}
	if out.FirecrackerVCPU <= 0 {
		out.FirecrackerVCPU = 2
	}
	if out.FirecrackerMemMiB <= 0 {
		out.FirecrackerMemMiB = 1024
	}
	if out.FirecrackerBin == "" {
		out.FirecrackerBin = "firecracker"
	}
	return out
}

// Backend is the sandbox-backend contract.  Implementations
// (docker, firecracker, k8s-job, …) accept Options and
// produce a Result.  Backends self-register through the
// internal `register` function in the same package.
type Backend interface {
	// Name is the canonical lowercase backend name
	// ("docker", "firecracker") — stable identifier that
	// surfaces in Result.Backend, audit events, and the
	// Options.Backend selector.
	Name() string

	// Verify runs `pg_verifybackup` against opts.DataDir.
	// Errors only when the sandbox itself failed to come up
	// (Docker unavailable, kernel image missing, KVM denied,
	// etc.); a "verify ran but found a problem" outcome is
	// still a successful invocation that returns
	// Passed=false.
	Verify(ctx context.Context, opts Options) (*Result, error)
}

// Backend names.  Constants so callers don't typo strings.
const (
	BackendDocker      = "docker"
	BackendFirecracker = "firecracker"
)

// backendRegistry holds the available backends.  Populated
// at init() time by the per-backend files.  Lookup is
// case-insensitive on the operator-supplied name.
var backendRegistry = map[string]Backend{}

// register adds a Backend to the registry.  Called from each
// backend's init().
func register(b Backend) {
	backendRegistry[strings.ToLower(b.Name())] = b
}

// availableBackends returns the registered backend names,
// sorted, lowercased.  Used by error messages so operators
// know what their build flavour supports.
func availableBackends() []string {
	out := make([]string, 0, len(backendRegistry))
	for n := range backendRegistry {
		out = append(out, n)
	}
	// Stable order: docker first if present, then alphabetical.
	if _, ok := backendRegistry[BackendDocker]; ok {
		out = moveFirst(out, BackendDocker)
	}
	return out
}

func moveFirst(s []string, first string) []string {
	for i, v := range s {
		if v == first {
			s[0], s[i] = s[i], s[0]
			break
		}
	}
	return s
}

// Backends returns the public-facing list of registered
// backend names — what the current binary supports.
// Operators consume this via `pg_hardstorage doctor` to
// confirm their build flavour.
func Backends() []string {
	return availableBackends()
}

// Verify dispatches to the backend named in opts.Backend
// (default: docker) and runs `pg_verifybackup` against the
// supplied data dir.
//
// API-stable since: the public surface didn't change
// when we added the Backend interface — callers that pass
// only DataDir + PGMajor + Image keep working without
// modification.
func Verify(ctx context.Context, opts Options) (*Result, error) {
	if opts.DataDir == "" {
		return nil, errors.New("sandbox: DataDir is required")
	}
	resolved := opts.resolved()

	b, ok := backendRegistry[strings.ToLower(resolved.Backend)]
	if !ok {
		return nil, fmt.Errorf(
			"sandbox: unknown backend %q (available: %s)",
			resolved.Backend, strings.Join(availableBackends(), ", "),
		)
	}

	return b.Verify(ctx, resolved)
}

// isMissingManifestError detects the specific stderr pattern
// PG emits when backup_manifest is absent.  Shared between
// backends so the "skipped because no manifest" surface is
// identical regardless of how pg_verifybackup was invoked.
func isMissingManifestError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "backup_manifest") &&
		(strings.Contains(s, "no such file") || strings.Contains(s, "could not open"))
}
