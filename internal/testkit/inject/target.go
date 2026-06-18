// target.go — Target/TargetSet: addressable soak-fleet member abstraction (docker / fake).
package inject

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
)

// Target is one addressable thing in the soak fleet — typically
// a container running PG, the host agent, the repo store, or a
// KMS / Patroni service.  Faults invoke methods on it.
//
// Implementations:
//
//   - DockerTarget: production; fronts a docker container.
//   - FakeTarget:   test fixture; records every call so tests
//     can assert what would have been done.
//
// Methods deliberately stay coarse — `Exec` covers most fault
// primitives; specialised ones (Signal, CopyFile, KillContainer)
// land here as needed.  Faults are written against this
// interface, not against docker directly, so a future k8s /
// ssh implementation drops in cleanly.
type Target interface {
	// Name returns the operator-visible identifier for log
	// lines and audit events ("u24-pg17-c0", "repo-minio-1").
	Name() string

	// Role classifies the target ("agent" | "pg" | "repo" |
	// "kms" | "patroni" | ...).  TargetSet.Pick selects on
	// this field.
	Role() string

	// Exec runs the supplied argv inside the target and
	// returns combined stdout+stderr.  Production targets
	// proxy through `docker exec`; tests record the call.
	Exec(ctx context.Context, argv ...string) ([]byte, error)

	// Signal sends a Unix signal to the main process inside
	// the target.  For container targets this is `docker kill
	// -s SIG`; for SSH targets this is `kill -SIG <pid>`.
	Signal(ctx context.Context, sig int) error

	// Start re-launches the target after Signal exits its
	// PID 1.  Idempotent: callable on a still-running target
	// as a no-op.  For container targets this is `docker
	// start <name>`; for SSH targets, the equivalent
	// service-restart command.
	//
	// Why this is part of the Target contract: Docker
	// categorises `docker kill` (any signal, not just
	// SIGKILL) as a user-initiated stop, so neither
	// `restart: unless-stopped` nor `restart: always` will
	// auto-restart the container after Signal.  Without an
	// explicit Start, every Signal leaves the cell Exited()
	// and the orchestrator's heal_window observes a wedge
	// instead of a recovery.  signalFault.Apply calls
	// Signal followed by Start; specialised faults that
	// need only Signal-without-Start can call Signal
	// directly.
	Start(ctx context.Context) error

	// CopyOut copies a file out of the target.  Used by the
	// `flip_random_byte` primitive to read a chunk before
	// flipping a byte and CopyIn-ing it back.
	CopyOut(ctx context.Context, path string) ([]byte, error)

	// CopyIn writes a file into the target.
	CopyIn(ctx context.Context, path string, body []byte) error

	// SetMemoryLimit sets the cgroup memory limit for the
	// target.  Bytes <= 0 means "remove the limit".
	//
	// Why a dedicated method (not just an Exec call to
	// echo > /sys/fs/cgroup/memory.max): Docker mounts
	// /sys/fs/cgroup read-only inside the container by
	// default — the in-container write fails on every
	// modern host.  The runtime applies the limit from
	// OUTSIDE via `docker update --memory=N <container>`,
	// which is the canonical Docker mechanism and works
	// on cgroups v1 and v2 alike.  SSH / k8s targets
	// implement the equivalent for their environment.
	SetMemoryLimit(ctx context.Context, bytes int64) error
}

// TargetSet is the population of containers / services the soak
// driver knows about.  Faults pick from this set per the args'
// "target=" key.
type TargetSet interface {
	// Pick selects targets matching the role spec.  Recognised
	// shapes:
	//
	//   "agent"          → every Target with Role()=="agent"
	//   "agent_random"   → one Target with Role()=="agent",
	//                      chosen pseudorandomly via the seed
	//   "agent_all"      → alias for "agent"
	//   "<exact-name>"   → the Target with that Name()
	//
	// An unknown role / name is an error so typos in the
	// faults.yaml surface during a soak run rather than as a
	// silent no-op.
	Pick(spec string) ([]Target, error)
}

// staticTargetSet is the default TargetSet — the soak driver
// hands it the full target list at boot and Pick filters by
// role / name.  Test cases construct it directly.
type staticTargetSet struct {
	targets []Target
	rng     *rand.Rand
	mu      sync.Mutex
}

// NewStaticTargetSet builds a TargetSet from the supplied
// list.  Seed is used by *_random selectors so different soak
// runs explore different fault → target combinations.
func NewStaticTargetSet(targets []Target, seed int64) TargetSet {
	return &staticTargetSet{
		targets: targets,
		rng:     rand.New(rand.NewSource(seed)),
	}
}

// Pick implements TargetSet.
func (s *staticTargetSet) Pick(spec string) ([]Target, error) {
	if spec == "" {
		return nil, fmt.Errorf("inject: target spec is empty")
	}

	// Exact-name match first — supports `target=u24-pg17-c0`.
	for _, t := range s.targets {
		if t.Name() == spec {
			return []Target{t}, nil
		}
	}

	// "<role>_random" — pick one matching role at random.
	if strings.HasSuffix(spec, "_random") {
		role := strings.TrimSuffix(spec, "_random")
		matched := s.byRole(role)
		if len(matched) == 0 {
			return nil, fmt.Errorf("inject: no targets with role %q", role)
		}
		s.mu.Lock()
		idx := s.rng.Intn(len(matched))
		s.mu.Unlock()
		return []Target{matched[idx]}, nil
	}

	// "<role>_all" or bare "<role>" — every matching role.
	role := strings.TrimSuffix(spec, "_all")
	matched := s.byRole(role)
	if len(matched) == 0 {
		return nil, fmt.Errorf("inject: no targets with role %q (also no target named %q)", role, spec)
	}
	return matched, nil
}

func (s *staticTargetSet) byRole(role string) []Target {
	var out []Target
	for _, t := range s.targets {
		if t.Role() == role {
			out = append(out, t)
		}
	}
	return out
}
