// Package topology defines the testkit's infrastructure-provider
// abstraction. A Topology brings up a PG (single instance, replica
// pair, K8s cluster, ...), exposes a connection string, and is
// responsible for tearing it down again.
//
// v0.1 ships:
//
//	local-docker / testcontainers — single PG via testcontainers-go
//	stub:kind                     — placeholder (real impl)
//	stub:k8s-remote               — placeholder
//	stub:ssh-inventory            — placeholder
//	stub:cloud-vms                — placeholder
//	stub:firecracker              — placeholder
//
// The stub providers return a structured error when Up is called, so a
// scenario referencing an unimplemented provider fails fast with a
// clear message rather than running against an unrelated topology.
package topology

import (
	"context"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// Topology is one running infrastructure stack. Up brings it up; Down
// tears it down. ConnString returns a libpq DSN to the primary PG;
// the caller is free to open as many connections as the scenario needs.
type Topology interface {
	// Name returns the provider's name, for logging.
	Name() string

	// Up brings the topology up. ctx cancellation aborts the bring-up.
	Up(ctx context.Context, opts UpOptions) error

	// ConnString returns a libpq DSN to the primary. Only valid after
	// Up has returned nil.
	ConnString() string

	// Targets returns the inject.Target list the topology owns —
	// containers, hosts, services that fault primitives can act on.
	// The scenario runner builds a TargetSet from this so an
	// `inject` step can run against the running infrastructure
	// without the runner caring whether it's docker, k8s, or ssh
	// underneath.  Returns an empty slice when the topology hasn't
	// been Up'd or has no targetable surface (e.g. SkipTopology /
	// stubs).
	Targets() []inject.Target

	// Down tears it down. Idempotent — safe to call multiple times,
	// safe to call on a topology that's never been Up'd.
	Down(ctx context.Context) error
}

// UpOptions configures bring-up. Provider-specific fields live in
// Provider — UpOptions covers what every topology needs.
type UpOptions struct {
	// PGVersion is the PG major version ("15", "16", "17", "18").
	// Topologies that pin a specific image consult this; topologies
	// that target an existing infra block ignore it. Empty defaults
	// to pg.MaxSupportedMajor — the current upstream-stable major.
	PGVersion string

	// Filesystem ("ext4", "xfs", "zfs", "btrfs"). Local providers
	// ignore this; SSH-inventory providers may filter to matching
	// hosts.
	Filesystem string

	// Operator ("cnpg", "zalando", "crunchy") for K8s providers.
	Operator string

	// Image overrides the provider's default container image.
	// Empty falls back to the provider's choice (e.g. local-docker
	// uses `postgres:<PGVersion>`).
	Image string

	// Replicas requested (0 = single primary).
	Replicas int

	// InventoryFile for ssh-inventory provider.
	InventoryFile string

	// ExtraGUCs are postgresql.conf settings to apply at server
	// start via `postgres -c <name>=<value>`.  These override the
	// topology's default GUCs AND cannot be reset later by
	// `ALTER SYSTEM` (per PG's PGC_S_ARGV precedence over
	// PGC_S_FILE) — exactly what scenarios exercising
	// out-of-the-box bad-config postures need.  Honoured by
	// local-docker; ignored by topologies that don't own the
	// postmaster.
	ExtraGUCs map[string]string
}

// Build returns a Topology by name. Returns ErrUnknownProvider for
// names not known.
func Build(name string) (Topology, error) {
	switch name {
	case "local-docker", "testcontainers":
		return newLocalDocker(), nil
	case "patroni-local-docker":
		// Multi-node Patroni cluster (etcd + 3 Spilo nodes)
		// brought up via docker compose CLI.  The right
		// topology to pick when the scenario exercises
		// failover / switchover invariants — local-docker
		// runs a single PG with no failover surface.
		return newPatroniLocalDocker(), nil
	case "kind":
		return newStub("kind", "kind topology lands; v0.1 ships local-docker / testcontainers / patroni-local-docker"), nil
	case "k8s-remote":
		return newStub("k8s-remote", "k8s-remote topology lands"), nil
	case "ssh-inventory":
		return newStub("ssh-inventory", "ssh-inventory topology lands"), nil
	case "cloud-vms":
		return newStub("cloud-vms", "cloud-vms topology lands"), nil
	case "firecracker":
		return newStub("firecracker", "firecracker topology lands"), nil
	}
	return nil, fmt.Errorf("topology: unknown provider %q (v0.1 ships local-docker, testcontainers, patroni-local-docker; stubs for kind, k8s-remote, ssh-inventory, cloud-vms, firecracker)", name)
}

// stubTopology returns an error from Up, recording why in its message.
type stubTopology struct {
	name   string
	reason string
}

func newStub(name, reason string) *stubTopology { return &stubTopology{name: name, reason: reason} }

// Name returns the not-yet-implemented provider name.
func (s *stubTopology) Name() string { return s.name }

// ConnString returns "" — the stub never reaches a usable DSN.
func (s *stubTopology) ConnString() string { return "" }

// Targets returns nil — the stub has no inject surface.
func (s *stubTopology) Targets() []inject.Target { return nil }

// Up always errors with the stub's "not yet implemented"
// reason so callers fail loudly rather than silently no-op.
func (s *stubTopology) Up(_ context.Context, _ UpOptions) error {
	return fmt.Errorf("topology %s: %s", s.name, s.reason)
}

// Down is a no-op — nothing was brought up.
func (s *stubTopology) Down(_ context.Context) error { return nil }
