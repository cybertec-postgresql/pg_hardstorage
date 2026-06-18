// Package gameday is the v0.1 chaos-automation surface.
//
// A "game day" is a scheduled, opt-in failure simulation: kill the
// agent mid-backup, simulate an S3 503 storm, force a Patroni
// failover. The goal is empirical evidence that the system recovers
// without operator intervention — the kind of evidence a regulator's
// auditor wants to see and a tired SRE wants to trust.
//
// v0.1 ships:
//
//   - A scenario registry with three scripted scenarios:
//
//   - agent_kill — SIGKILL the local agent process; assert
//     self-supervised recovery within `recover_within`.
//
//   - s3_throttle — wrap the storage plugin with a fault-injecting
//     middleware that returns 503 for `duration` and asserts the
//     operation completes (under the bandwidth/retry budget).
//
//   - patroni_failover — declarative-only in v0.1 (we don't
//     actually drive Patroni in v0.1; the scenario records the
//     intended invariant + manual steps).
//
//   - `Run(ctx, scenario, opts)` — runs one scenario, returning a
//     structured Result with pass/fail and the evidence captured.
//
//   - `List() []Scenario` — registry walk, used by `gameday list`
//     in the CLI.
//
//   - `Report(ctx, repoURL)` — recent results from the audit log
//     (when run with `--audit`); v0.1 returns the in-memory cache
//     since the audit-log integration lands with the verifier
//     subsystem.
//
// What deliberately doesn't ship in v0.1:
//
//   - Scheduled (cron-driven) game days. The scheduler is a separate
//     subsystem; gameday consumes its trigger but doesn't invent it.
//   - Real Patroni driver. Calling Patroni's REST `/switchover` from
//     a chaos test belongs to the verifier sandbox, where the
//     test owns its own Patroni cluster.
//   - Cross-region failover simulation. Same reason — needs the
//     replicate subsystem to be live.
package gameday

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SchemaResult is the JSON schema string for gameday Run results.
// 24-month back-compat per the project-wide commitment.
const SchemaResult = "pg_hardstorage.gameday.v1"

// Scenario is one named chaos experiment. Implementations live in
// scenario_*.go and register themselves in init() via Register.
type Scenario struct {
	Name        string
	Description string
	Tier        string // L1 | L2 | L3 — same tier ladder as the testkit

	// Run executes the scenario. The implementation owns the failure
	// injection and the recovery assertion; it returns Pass=true iff
	// the system recovered within the scenario's invariants.
	Run func(ctx context.Context, opts RunOptions) (*Result, error)
}

// RunOptions configures one Run call. Each scenario consults a subset
// of these fields; unused ones are ignored.
type RunOptions struct {
	// Deployment is the logical deployment to target. Some scenarios
	// (s3_throttle, patroni_failover) are deployment-scoped; others
	// (agent_kill) are agent-scoped and ignore this.
	Deployment string

	// RepoURL is the repository under test.
	RepoURL string

	// RecoverWithin is the upper bound for "did it recover?" — beyond
	// this, the scenario fails regardless of the underlying outcome.
	// Defaults are scenario-specific (60s for agent_kill, 5m for
	// s3_throttle).
	RecoverWithin time.Duration

	// FaultDuration controls how long the fault is held active.
	// Scenario-specific defaults apply when zero.
	FaultDuration time.Duration

	// DryRun reports the planned actions without executing the
	// fault injection or asserting recovery. Useful for previewing
	// what `gameday run agent_kill` would do.
	DryRun bool
}

// Result is the structured outcome of one Run.
type Result struct {
	Schema       string        `json:"schema"`
	Scenario     string        `json:"scenario"`
	Pass         bool          `json:"pass"`
	StartedAt    time.Time     `json:"started_at"`
	StoppedAt    time.Time     `json:"stopped_at"`
	Duration     time.Duration `json:"duration_ms"`
	DryRun       bool          `json:"dry_run,omitempty"`
	RecoveryTime time.Duration `json:"recovery_time_ms,omitempty"`
	Evidence     []Event       `json:"evidence,omitempty"`
	Failure      string        `json:"failure,omitempty"`
}

// Event is one observation captured during a Run. Scenarios append
// events as they go; the JSON output preserves them so a post-mortem
// can replay the timeline.
type Event struct {
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Message string         `json:"message,omitempty"`
	Body    map[string]any `json:"body,omitempty"`
}

// --- registry --------------------------------------------------------

var (
	registryMu sync.RWMutex
	registry   = map[string]Scenario{}
)

// Register adds a scenario to the registry. Re-registration with the
// same name overwrites; this is intentional — operator overrides in
// /etc/pg_hardstorage/gameday/<name>.scenario.yaml win over the
// in-tree default once that loader lands.
func Register(s Scenario) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if s.Name == "" {
		panic("gameday: cannot register a scenario without a name")
	}
	if s.Run == nil {
		panic("gameday: cannot register a scenario without a Run function")
	}
	registry[s.Name] = s
}

// List returns every registered scenario, sorted by name. Used by
// `gameday list` and by the configuration validator.
func List() []Scenario {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the named scenario or ErrNoSuchScenario.
func Get(name string) (Scenario, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[name]
	if !ok {
		return Scenario{}, fmt.Errorf("gameday: %w: %q", ErrNoSuchScenario, name)
	}
	return s, nil
}

// ErrNoSuchScenario is returned by Get when the name isn't registered.
var ErrNoSuchScenario = errors.New("no such gameday scenario")

// Run executes the named scenario with opts. Convenience wrapper
// over Get + Run that handles the ErrNoSuchScenario case so callers
// don't have to.
func Run(ctx context.Context, name string, opts RunOptions) (*Result, error) {
	s, err := Get(name)
	if err != nil {
		return nil, err
	}
	res, err := s.Run(ctx, opts)
	if err != nil {
		// Even on a Run() error we want a partial Result back so the
		// caller can record what was observed before the abort.
		if res == nil {
			res = &Result{
				Schema:    SchemaResult,
				Scenario:  name,
				StartedAt: time.Now().UTC(),
				Failure:   err.Error(),
			}
			res.StoppedAt = res.StartedAt
		}
		return res, err
	}
	if res == nil {
		// Defensive: a scenario that returns (nil, nil) is a
		// programming error.
		return nil, fmt.Errorf("gameday: scenario %q returned nil Result with no error", name)
	}
	return res, nil
}
