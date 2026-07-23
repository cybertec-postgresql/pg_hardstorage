// Package follower implements the Patroni leader-follow loop's
// agent integration. It composes:
//
//   - patroni.Follower — polls /cluster, fires LeaderChange events
//   - replication.EnsureSlot — Mechanism 2 slot continuity
//   - pg.TimelineHistoryFor + wal/timeline.Store — capture .history
//     files on every promotion so PITR can reconstruct timeline
//     lineage at restore time
//
// The Coordinator is the operator-facing primitive: pass it a
// Patroni REST client and a few config fields, call Run, and the
// agent transparently handles slot reconciliation + timeline
// capture across leader changes.
//
// Plan reference: §Patroni Mechanism 1 + Mechanism 2. The
// primitives shipped in earlier commits (50de7b4 / 22de1c6 /
// 8cddaca / etc.); this is the agent-side wiring that makes them
// actually run automatically.
//
// What this package does NOT do:
//
//   - Stream WAL itself. The Coordinator's job is to keep the
//     slot + timeline state coherent across leader changes; the
//     WAL streaming consumer (internal/pg/replication.Stream)
//     reconnects to the new leader on its own. Coordinator just
//     ensures the slot exists when it does.
//   - Patroni Mechanism 3 (dual-slot) or Mechanism 4
//     (synchronous-target). Both are separate primitives the
//     Coordinator can compose later.
//   - The agent's own startup/shutdown lifecycle. Run blocks
//     until ctx is cancelled, in line with the agent-lifecycle
//     pattern shared with logical.Runner.
package follower

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// gapPersistAttempts / gapPersistBackoff bound how hard persistGap
// retries a failed gap-record write. A WAL gap is detected exactly once
// (at slot recreation), so a lost record is lost forever — persistence
// is not best-effort.
const (
	gapPersistAttempts = 4
	gapPersistBackoff  = 250 * time.Millisecond
	// gapPersistFinalTimeout bounds the ONE extra Put attempt made on a
	// detached context when the caller's ctx is cancelled mid-persist
	// (agent shutdown racing failover handling). Short on purpose: it
	// delays shutdown only when storage is slow, and a healthy repo
	// answers well within it.
	gapPersistFinalTimeout = 5 * time.Second
)

// SlotRole names which Patroni cluster member a SlotSpec is
// pinned to. The Coordinator's reconcile logic uses the role to
// pick the right /cluster member for each slot.
type SlotRole string

const (
	// SlotRoleLeader pins a slot to whichever member currently
	// holds the leader DCS lock. The slot reconciles against the
	// new leader after every Patroni failover. This is the
	// Mechanism 2 single-slot default.
	SlotRoleLeader SlotRole = "leader"

	// SlotRoleReplica pins a slot to a running replica. Used by
	// Mechanism 3 dual-slot deployments to keep a redundant
	// stream attached to a replica node — when the primary
	// fails over to the replica, the replica's slot is already
	// on the new primary, so no recreation is needed.
	//
	// Replica selection: lowest-lag running replica wins
	// (`pickReplica`). Members without reported lag fall to
	// the back of the queue (Patroni sometimes omits the
	// field for stale members). Future revisions add named-
	// member preference and AZ-aware selection.
	SlotRoleReplica SlotRole = "replica"
)

// SlotSpec is one physical replication slot the Coordinator
// reconciles. Name is the slot name on the PG server; Role
// determines which /cluster member it lives on.
type SlotSpec struct {
	Name string   // physical slot name (e.g., "pg_hardstorage_db1_primary")
	Role SlotRole // "leader" or "replica"
}

// Options configures a Coordinator.
//
// Required: Client, Deployment, DSNFor, TimelineStore.
//
// Slot configuration: provide ONE of:
//   - SlotName (single-slot mode; treated as a leader-pinned slot)
//   - Slots    (multi-slot Mechanism 3 mode)
//
// Setting both is invalid. Setting neither is invalid.
type Options struct {
	// Client is the Patroni REST client to follow. Required.
	Client *patroni.Client

	// SlotName is the single-slot Mechanism 2 mode: the named
	// slot lives on whichever member is the current leader.
	// Mutually exclusive with Slots; when Slots is set this
	// field is ignored.
	SlotName string

	// Slots is the multi-slot Mechanism 3 mode: each entry
	// names a slot + the cluster role (leader or replica) it
	// lives on. The Coordinator reconciles each slot
	// independently per leader-change event. Mutually
	// exclusive with SlotName.
	Slots []SlotSpec

	// Deployment is the logical deployment name; used as the
	// timeline-history storage key prefix. Required.
	Deployment string

	// TimelineStore persists captured .history files at
	// wal/<deployment>/timelines/<tli>.history. Required.
	TimelineStore *timeline.Store

	// GapStore persists wal_gap_detected events to the repo
	// at wal/<deployment>/gaps/<tli>-<ns>.json so they survive
	// agent restarts and become visible to inspection
	// commands (`doctor`, future `wal gaps`). Optional —
	// when nil, the Coordinator still emits the structured
	// event but doesn't persist; the agent typically wires
	// this in alongside the TimelineStore so both share the
	// repo's storage plugin.
	GapStore *gapstate.Store

	// DSNFor builds a libpq DSN from the new leader's host:port.
	// The agent injects the connection user, password, sslmode,
	// connect_timeout via this callback — Patroni's /cluster only
	// surfaces host:port, not the auth details. Required.
	DSNFor func(host string, port int) string

	// LastConfirmedLSN supplies the WAL position the agent has
	// durably stored. Called once per reconcile with the new
	// leader's endpoint so the closure can scope the lookup to
	// the right timeline (the agent's repo-walking
	// implementation derives the answer from
	// wal/<deployment>/<tli>/<seg>.json).
	//
	// Hand-rolled by the agent's WAL consumer (or,,
	// by inventory.HighestArchivedLSN against the repo). The
	// callback is consulted at reconcile-time so a stale value
	// doesn't sneak in.
	//
	// Optional — when nil, gap calculation is skipped (treated
	// as first-time bootstrap). EnsureSlot's contract is that
	// lastConfirmedLSN==0 means "no prior position", so a nil
	// callback degrades safely.
	LastConfirmedLSN func(leader patroni.LeaderEndpoint) pglogrepl.LSN

	// Interval is the Patroni poll cadence; defaults to
	// patroni.DefaultFollowInterval.
	Interval time.Duration

	// OnEvent receives Coordinator events. Optional; nil
	// discards. Synchronously called on the polling goroutine,
	// so callbacks must return promptly. Use this to fan out to
	// the dispatcher's Sink pipeline.
	OnEvent func(*output.Event)

	// ReconcileSlot is the test-seam for slot reconciliation.
	// When nil, the production implementation runs
	// replication.EnsureSlot against fresh pg.Connect calls.
	// Tests override it to drive the orchestration logic
	// without standing up a real PG.
	ReconcileSlot func(ctx context.Context, dsn string) (*replication.SlotContinuityResult, error)

	// CaptureTimelineHistory is the test-seam for the
	// .history-file capture path. When nil, the production
	// implementation runs pg.TimelineHistoryFor + writes via
	// TimelineStore.Put.
	CaptureTimelineHistory func(ctx context.Context, dsn string, tli uint32) error

	// Now is the time source for event timestamps. nil → time.Now.
	Now func() time.Time
}

// Coordinator owns the leader-follow lifecycle. Construct via New;
// drive via Run.
type Coordinator struct {
	opts Options
}

// New validates opts and returns a Coordinator. Normalises
// SlotName into the equivalent single-entry Slots when needed
// so the rest of the code only deals with the slice shape.
func New(opts Options) (*Coordinator, error) {
	if opts.Client == nil {
		return nil, errors.New("follower: Options.Client is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("follower: Options.Deployment is required")
	}
	if opts.DSNFor == nil {
		return nil, errors.New("follower: Options.DSNFor is required")
	}
	if opts.TimelineStore == nil {
		return nil, errors.New("follower: Options.TimelineStore is required")
	}
	// Slot configuration: exactly one of SlotName / Slots.
	if opts.SlotName == "" && len(opts.Slots) == 0 {
		return nil, errors.New("follower: provide Options.SlotName (single-slot) or Options.Slots (multi-slot)")
	}
	if opts.SlotName != "" && len(opts.Slots) > 0 {
		return nil, errors.New("follower: Options.SlotName and Options.Slots are mutually exclusive")
	}
	if opts.SlotName != "" {
		// Normalise the single-slot mode into the same
		// internal shape multi-slot uses. The leader role is
		// the only sensible default.
		opts.Slots = []SlotSpec{{Name: opts.SlotName, Role: SlotRoleLeader}}
	}
	// Validate each slot's role + name.
	seen := map[string]struct{}{}
	for i, s := range opts.Slots {
		if s.Name == "" {
			return nil, fmt.Errorf("follower: Options.Slots[%d].Name is empty", i)
		}
		if _, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("follower: duplicate slot name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
		switch s.Role {
		case SlotRoleLeader, SlotRoleReplica:
			// ok
		case "":
			return nil, fmt.Errorf("follower: Options.Slots[%d].Role is empty (use %q or %q)",
				i, SlotRoleLeader, SlotRoleReplica)
		default:
			return nil, fmt.Errorf("follower: Options.Slots[%d].Role = %q (must be %q or %q)",
				i, s.Role, SlotRoleLeader, SlotRoleReplica)
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Coordinator{opts: opts}, nil
}

// Run blocks until ctx is cancelled. It starts a patroni.Follower
// and bridges its LeaderChange events into the slot-reconciliation
// + timeline-capture pipeline.
func (c *Coordinator) Run(ctx context.Context) error {
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c.opts.Client,
		Interval: c.opts.Interval,
		OnEvent: func(ev patroni.LeaderChange) {
			c.handleLeaderChange(ctx, ev)
		},
		OnPollError: func(err error) {
			// Include the URL the agent was hitting so the
			// operator can immediately tell whether the failure
			// is "wrong endpoint configured" vs "endpoint
			// configured correctly but unreachable from the
			// agent's network namespace" — historically (issue
			// #74) the event body had only `error`, leaving the
			// operator to guess which URL the agent picked up
			// from deployment config.
			body := map[string]any{"error": err.Error()}
			if c.opts.Client != nil {
				if u := c.opts.Client.BaseURL(); u != "" {
					body["url"] = u
				}
			}
			c.emit(output.NewEvent(output.SeverityWarning, "wal.follower", "patroni_poll_failed").
				WithSubject(output.Subject{Deployment: c.opts.Deployment}).
				WithBody(body))
		},
	})
	if err != nil {
		return fmt.Errorf("follower: start patroni follower: %w", err)
	}
	slotNames := make([]string, 0, len(c.opts.Slots))
	for _, s := range c.opts.Slots {
		slotNames = append(slotNames, s.Name)
	}
	c.emit(output.NewEvent(output.SeverityInfo, "wal.follower", "started").
		WithSubject(output.Subject{Deployment: c.opts.Deployment}).
		WithBody(map[string]any{
			"slots":      slotNames,
			"slot_count": len(slotNames),
			"interval":   c.effectiveInterval().String(),
		}))
	<-f.Done()
	c.emit(output.NewEvent(output.SeverityInfo, "wal.follower", "stopped").
		WithSubject(output.Subject{Deployment: c.opts.Deployment}))
	return nil
}

// handleLeaderChange is the per-event dispatcher. Public method
// shape so tests can drive it directly with a synthesised
// LeaderChange and observe the events emitted + the seams
// invoked.
func (c *Coordinator) HandleLeaderChange(ctx context.Context, ev patroni.LeaderChange) {
	c.handleLeaderChange(ctx, ev)
}

func (c *Coordinator) handleLeaderChange(ctx context.Context, ev patroni.LeaderChange) {
	subj := output.Subject{Deployment: c.opts.Deployment}
	c.emit(output.NewEvent(output.SeverityNotice, "wal.follower", "leader_change").
		WithSubject(subj).
		WithBody(map[string]any{
			"old": leaderDescription(ev.Old),
			"new": leaderDescription(ev.New),
		}))

	if ev.New == nil {
		// Cluster has no leader (between holders). The agent's
		// WAL streaming consumer should pause; we don't need to
		// reconcile anything here because there's no target to
		// reconcile against.
		c.emit(output.NewEvent(output.SeverityWarning, "wal.follower", "leader_gone").
			WithSubject(subj))
		return
	}

	leaderDSN := c.opts.DSNFor(ev.New.Host, ev.New.Port)
	if leaderDSN == "" {
		c.emit(output.NewEvent(output.SeverityError, "wal.follower", "dsn_build_failed").
			WithSubject(subj).
			WithBody(map[string]any{
				"host": ev.New.Host, "port": ev.New.Port,
			}))
		return
	}

	// 1. Reconcile each configured slot.
	//
	// For Mechanism 3 multi-slot deployments, we need the
	// running-replica list to find the right endpoint for each
	// SlotRoleReplica entry. We fetch /cluster ONCE per leader
	// change and use the resulting member list for every slot
	// reconcile in this iteration. The per-leader-change cost
	// is bounded — leader changes are rare events.
	var clusterMembers []patroni.Member
	for _, slot := range c.opts.Slots {
		endpoint, ok := c.resolveSlotEndpoint(ctx, ev, slot, &clusterMembers)
		if !ok {
			// resolveSlotEndpoint emits its own diagnostic
			// event; we just skip this slot and continue to
			// the next.
			continue
		}
		dsn := c.opts.DSNFor(endpoint.Host, endpoint.Port)
		if dsn == "" {
			c.emit(output.NewEvent(output.SeverityError, "wal.follower", "dsn_build_failed").
				WithSubject(output.Subject{Deployment: c.opts.Deployment}).
				WithBody(map[string]any{
					"slot_name": slot.Name,
					"slot_role": string(slot.Role),
					"host":      endpoint.Host, "port": endpoint.Port,
				}))
			continue
		}
		c.reconcileOneSlot(ctx, dsn, endpoint, slot)
	}

	// 2. Capture the new leader's timeline-history file. PG
	// returns nothing for TLI 1 (no parent); the helper
	// surfaces ErrNoHistoryForTLI1 which we treat as
	// expected-and-skip.
	//
	// Timeline history is always captured against the leader
	// (the canonical TLI source); Mechanism 3 multi-slot
	// doesn't change this — the replica's TLI is the same as
	// the leader's (Patroni keeps replicas on the same
	// timeline).
	if err := c.captureTimelineHistory(ctx, leaderDSN, ev.New.Timeline); err != nil {
		if errors.Is(err, pg.ErrNoHistoryForTLI1) {
			// Fresh cluster on TLI 1 — there's no history file
			// to capture. Notice-level event so an operator
			// scanning logs sees we tried.
			c.emit(output.NewEvent(output.SeverityNotice, "wal.follower", "timeline_no_history").
				WithSubject(subj).
				WithBody(map[string]any{"timeline": ev.New.Timeline}))
			return
		}
		c.emit(output.NewEvent(output.SeverityError, "wal.follower", "timeline_capture_failed").
			WithSubject(subj).
			WithBody(map[string]any{
				"timeline": ev.New.Timeline,
				"error":    err.Error(),
			}))
		return
	}
	c.emit(output.NewEvent(output.SeverityInfo, "wal.follower", "timeline_captured").
		WithSubject(subj).
		WithBody(map[string]any{"timeline": ev.New.Timeline}))

	// 3. Backfill any MISSING intermediate timeline-history files below
	// the new leader's TLI. The follower only observes the CURRENT leader
	// on each change, so an agent that was down across two or more
	// promotions captured the latest TLI but not the ones it slept
	// through. PG discovers recovery_target_timeline='latest' by probing
	// successive <tli>.history files via restore_command and STOPS at the
	// first one missing — so a single absent intermediate silently caps a
	// PITR at the gap (recovering to a stale timeline) or fails a target
	// on a higher TLI with "could not find timeline N". A streaming-only
	// HA deployment has no archive_command to backfill these, so we do it
	// here while a live leader (which holds every ancestor .history) can
	// still serve them.
	c.backfillTimelineHistory(ctx, leaderDSN, ev.New.Timeline)
}

// backfillTimelineHistory ensures every intermediate timeline-history
// file (TLI 2..tli-1) is committed to the repo, not just the current
// leader's. Best-effort: a fetch failure on one TLI emits a warning and
// moves on; the next leader change retries it (the Get-skip below makes
// already-committed TLIs free, so steady state costs only cheap repo
// reads). A freshly-promoted leader holds every ancestor .history
// locally, so TIMELINE_HISTORY <k> resolves for any k <= tli.
func (c *Coordinator) backfillTimelineHistory(ctx context.Context, dsn string, tli uint32) {
	if tli < 3 {
		// TLI 1 has no history; TLI 2's only ancestor is TLI 1 (also no
		// history) — nothing intermediate to backfill below TLI 3.
		return
	}
	subj := output.Subject{Deployment: c.opts.Deployment}
	for k := uint32(2); k < tli; k++ {
		// Skip already-committed files: avoids a redundant PG round-trip
		// AND keeps the captured chain contiguous. A read error other
		// than not-found is treated like a miss-we-can't-confirm; we
		// don't try to (re)capture on it because the existing bytes may
		// be fine — the next leader change re-checks.
		if _, err := c.opts.TimelineStore.Get(ctx, c.opts.Deployment, k); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err := c.captureTimelineHistory(ctx, dsn, k); err != nil {
			// ErrNoHistoryForTLI1 can't occur for k>=2, but a forced
			// rebuild could in theory leave an intermediate TLI without a
			// history file; either way one missing link shouldn't abort
			// backfilling the others.
			c.emit(output.NewEvent(output.SeverityWarning, "wal.follower", "timeline_backfill_failed").
				WithSubject(subj).
				WithBody(map[string]any{
					"timeline":        k,
					"leader_timeline": tli,
					"error":           err.Error(),
				}))
			continue
		}
		c.emit(output.NewEvent(output.SeverityInfo, "wal.follower", "timeline_backfilled").
			WithSubject(subj).
			WithBody(map[string]any{
				"timeline":        k,
				"leader_timeline": tli,
			}))
	}
}

// resolveSlotEndpoint picks the right cluster member for a
// slot's role. For SlotRoleLeader the answer is always
// ev.New (the new leader from the LeaderChange event). For
// SlotRoleReplica we lazily fetch /cluster (cached via
// *cluster — populated on first replica request, reused on
// subsequent same-LeaderChange replicas) and pick the first
// running replica.
//
// On failure (no replica available, /cluster unreachable),
// emits a structured event and returns ok=false; the caller
// skips this slot.
func (c *Coordinator) resolveSlotEndpoint(ctx context.Context, ev patroni.LeaderChange, slot SlotSpec, members *[]patroni.Member) (patroni.LeaderEndpoint, bool) {
	switch slot.Role {
	case SlotRoleLeader:
		return *ev.New, true
	case SlotRoleReplica:
		// Lazy /cluster fetch on first replica encountered for
		// this LeaderChange. Subsequent replicas in the same
		// iteration reuse the cached list.
		if *members == nil {
			cluster, err := c.opts.Client.Cluster(ctx)
			if err != nil {
				c.emit(output.NewEvent(output.SeverityWarning, "wal.follower", "cluster_fetch_failed").
					WithSubject(output.Subject{Deployment: c.opts.Deployment}).
					WithBody(map[string]any{
						"slot_name": slot.Name,
						"slot_role": string(slot.Role),
						"error":     err.Error(),
					}))
				return patroni.LeaderEndpoint{}, false
			}
			*members = cluster.Members
		}
		picked, ok := pickReplica(*members)
		if !ok {
			c.emit(output.NewEvent(output.SeverityWarning, "wal.follower", "no_replica_available").
				WithSubject(output.Subject{Deployment: c.opts.Deployment}).
				WithBody(map[string]any{
					"slot_name": slot.Name,
					"slot_role": string(slot.Role),
					"hint":      "no running replica found; the replica-pinned slot will be reconciled on the next leader change when one becomes available",
				}))
			return patroni.LeaderEndpoint{}, false
		}
		return patroni.LeaderEndpoint{
			Name:     picked.Name,
			Host:     picked.Host,
			Port:     picked.Port,
			Timeline: picked.Timeline,
			Role:     picked.Role,
		}, true
	}
	c.emit(output.NewEvent(output.SeverityError, "wal.follower", "unknown_slot_role").
		WithSubject(output.Subject{Deployment: c.opts.Deployment}).
		WithBody(map[string]any{
			"slot_name": slot.Name,
			"slot_role": string(slot.Role),
		}))
	return patroni.LeaderEndpoint{}, false
}

// reconcileOneSlot reconciles ONE configured slot. Wraps
// reconcileSlot with the per-slot event emission so a multi-slot
// deployment's events are distinguishable by slot_name + slot_role.
func (c *Coordinator) reconcileOneSlot(ctx context.Context, dsn string, endpoint patroni.LeaderEndpoint, slot SlotSpec) {
	subj := output.Subject{Deployment: c.opts.Deployment}
	cont, err := c.reconcileSlot(ctx, dsn, endpoint, slot.Name)
	if err != nil {
		c.emit(output.NewEvent(output.SeverityError, "wal.follower", "slot_reconcile_failed").
			WithSubject(subj).
			WithBody(map[string]any{
				"slot_name": slot.Name,
				"slot_role": string(slot.Role),
				"error":     err.Error(),
			}))
		return
	}
	body := map[string]any{
		"slot_name":          slot.Name,
		"slot_role":          string(slot.Role),
		"outcome":            string(cont.Outcome),
		"gap_bytes":          cont.GapBytes,
		"gap_start_lsn":      cont.GapStartLSN.String(),
		"gap_end_lsn":        cont.GapEndLSN.String(),
		"last_confirmed_lsn": cont.LastConfirmedLSN.String(),
	}
	if cont.HasGap() {
		c.emit(output.NewEvent(output.SeverityCritical, "wal.follower", "wal_gap_detected").
			WithSubject(subj).
			WithBody(body).
			WithSuggestion(&output.Suggestion{
				Human:   "the new leader's slot was recreated and has advanced past the agent's last confirmed LSN; PITR within the gap is impossible from this repo. Backup taken after this point will note the gap so PITR is refused. Investigate via `pg_hardstorage repair slot <deployment>` for diagnostics.",
				Command: "pg_hardstorage repair slot " + c.opts.Deployment,
				DocURL:  "https://docs.pghardstorage.org/runbooks/wal-gap-detected",
			}))
		// Persist the gap to the repo so it survives agent
		// restarts and becomes visible to inspection commands
		// (`doctor`, future `wal gaps`). Failure to persist is
		// logged but not fatal — the structured event already
		// fired, so subscribers are notified regardless.
		c.persistGap(ctx, endpoint, slot, cont)
		return
	}
	c.emit(output.NewEvent(output.SeverityNotice, "wal.follower", "slot_reconciled").
		WithSubject(subj).
		WithBody(body))
}

// persistGap writes a gapstate.Record for the just-detected
// gap. Failures don't propagate, but they are NOT silent: every
// exit path that leaves the record unpersisted emits a CRITICAL
// gap_persist_failed event. The same wal_gap_detected event
// already fired before this call, so the operator's alerting
// pipeline has the detection signal regardless.
func (c *Coordinator) persistGap(ctx context.Context, endpoint patroni.LeaderEndpoint, slot SlotSpec, cont *replication.SlotContinuityResult) {
	if c.opts.GapStore == nil {
		return // store not wired (tests, library-only callers)
	}
	rec := gapstate.Record{
		Deployment:  c.opts.Deployment,
		SlotName:    slot.Name,
		SlotRole:    string(slot.Role),
		Timeline:    endpoint.Timeline,
		GapStartLSN: cont.GapStartLSN.String(),
		GapEndLSN:   cont.GapEndLSN.String(),
		GapBytes:    cont.GapBytes,
		// Stamp the detection time ONCE so every retry targets the SAME
		// record key (gapstate keys embed DetectedAt's unix-nano). Without
		// this a retry would write a DUPLICATE gap record.
		DetectedAt: c.opts.Now().UTC(),
	}
	// The gap is detected EXACTLY ONCE — at slot recreation; the next
	// reconcile finds the slot present and computes no gap. A lost record
	// is lost forever, and restore's preflight can then no longer refuse a
	// PITR into [gap_start, gap_end). So persistence is NOT best-effort:
	// retry with bounded backoff; on total failure escalate to CRITICAL so
	// the operator records it by hand and fixes the repo write path.
	var lastErr error
	for attempt := 0; attempt < gapPersistAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				// Shutdown raced the retry loop. Bailing out here would
				// silently drop the once-only record, so make ONE final
				// attempt on a short context detached from the cancelled
				// ctx — when storage is healthy the record still lands.
				c.persistGapFinal(ctx, endpoint, slot, rec, lastErr)
				return
			case <-time.After(gapPersistBackoff * time.Duration(attempt)):
			}
		}
		_, err := c.opts.GapStore.Put(ctx, rec)
		if err == nil || errors.Is(err, storage.ErrAlreadyExists) {
			return // persisted (already-exists ⇒ a prior attempt's write landed)
		}
		lastErr = err
		if ctx.Err() != nil {
			// The attempt itself failed under an already-cancelled ctx —
			// a ctx-aware store fails every further retry the same way,
			// including the very FIRST attempt when shutdown races the
			// failover handling. Same detached final attempt as the
			// backoff-select path above.
			c.persistGapFinal(ctx, endpoint, slot, rec, lastErr)
			return
		}
	}
	c.emitGapPersistFailed(endpoint, slot, rec, lastErr)
}

// persistGapFinal is persistGap's shutdown path: the retry loop's ctx was
// cancelled while the record was still unpersisted. A WAL gap is detected
// exactly once — at slot recreation — so agent shutdown must not lose the
// record when storage is actually healthy. Make ONE final Put attempt on a
// short context detached from the cancelled ctx (values retained,
// cancellation dropped); if even that fails, the record really is lost and
// we escalate CRITICAL exactly like the exhausted-retries path.
func (c *Coordinator) persistGapFinal(ctx context.Context, endpoint patroni.LeaderEndpoint, slot SlotSpec, rec gapstate.Record, lastErr error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gapPersistFinalTimeout)
	defer cancel()
	_, err := c.opts.GapStore.Put(fctx, rec)
	if err == nil || errors.Is(err, storage.ErrAlreadyExists) {
		return // persisted (already-exists ⇒ a prior attempt's write landed)
	}
	if lastErr != nil {
		err = fmt.Errorf("final attempt on detached context: %w (last error before shutdown: %v)", err, lastErr)
	}
	// emit is synchronous and does not consult any context, so the
	// CRITICAL escalation is delivered even though ctx is cancelled.
	c.emitGapPersistFailed(endpoint, slot, rec, err)
}

// emitGapPersistFailed emits the CRITICAL escalation for a gap record that
// could not be persisted. Fired on BOTH unpersisted exit paths — retries
// exhausted, and shutdown with the detached final attempt also failing.
func (c *Coordinator) emitGapPersistFailed(endpoint patroni.LeaderEndpoint, slot SlotSpec, rec gapstate.Record, lastErr error) {
	c.emit(output.NewEvent(output.SeverityCritical, "wal.follower", "gap_persist_failed").
		WithSubject(output.Subject{Deployment: c.opts.Deployment}).
		WithBody(map[string]any{
			"slot_name":     slot.Name,
			"slot_role":     string(slot.Role),
			"timeline":      endpoint.Timeline,
			"gap_start_lsn": rec.GapStartLSN,
			"gap_end_lsn":   rec.GapEndLSN,
			"gap_bytes":     rec.GapBytes,
			"attempts":      gapPersistAttempts,
			"error":         lastErr.Error(),
			"impact":        "the WAL gap was NOT persisted; restore preflight cannot refuse a PITR into [gap_start, gap_end), and PG will instead halt mid-recovery at the missing segment.",
		}).
		WithSuggestion(&output.Suggestion{
			Human:   "the gap is real (WAL was lost across the failover) but the agent could not record it durably — investigate the repo write path. Until the record exists, a PITR spanning the gap fails mid-recovery instead of being refused up front.",
			Command: "pg_hardstorage repair slot " + c.opts.Deployment,
		}))
}

// reconcileSlot dispatches to the test-seam if set, else runs
// the production EnsureSlot path against fresh connections.
//
// The endpoint parameter scopes the LastConfirmedLSN lookup to
// the right timeline (the agent's repo-walking implementation
// queries wal/<deployment>/<tli>/<seg>.json). slotName is the
// per-call slot name so multi-slot deployments don't all
// reconcile against the same name.
func (c *Coordinator) reconcileSlot(ctx context.Context, dsn string, endpoint patroni.LeaderEndpoint, slotName string) (*replication.SlotContinuityResult, error) {
	if c.opts.ReconcileSlot != nil {
		return c.opts.ReconcileSlot(ctx, dsn)
	}
	regConn, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return nil, fmt.Errorf("regular conn: %w", err)
	}
	defer regConn.Close(ctx)
	replConn, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return nil, fmt.Errorf("replication conn: %w", err)
	}
	defer replConn.Close(ctx)
	var lastLSN pglogrepl.LSN
	if c.opts.LastConfirmedLSN != nil {
		lastLSN = c.opts.LastConfirmedLSN(endpoint)
	}
	return replication.EnsureSlot(ctx, regConn, replConn, slotName, lastLSN)
}

// captureTimelineHistory dispatches to the test-seam if set,
// else runs TimelineHistoryFor + TimelineStore.Put.
//
// TLI 1 → returns pg.ErrNoHistoryForTLI1 (PG has nothing to
// emit for the initial timeline); the caller treats this as
// expected and emits a notice rather than an error.
func (c *Coordinator) captureTimelineHistory(ctx context.Context, dsn string, tli uint32) error {
	if c.opts.CaptureTimelineHistory != nil {
		return c.opts.CaptureTimelineHistory(ctx, dsn, tli)
	}
	if tli == 0 {
		return errors.New("follower: TLI 0 is invalid")
	}
	repl, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return fmt.Errorf("replication conn for timeline: %w", err)
	}
	defer repl.Close(ctx)
	th, err := pg.TimelineHistoryFor(ctx, repl, tli)
	if err != nil {
		return err // includes ErrNoHistoryForTLI1 when applicable
	}
	if err := c.opts.TimelineStore.Put(ctx, c.opts.Deployment, tli, th.Content); err != nil {
		return fmt.Errorf("write timeline %d: %w", tli, err)
	}
	return nil
}

func (c *Coordinator) effectiveInterval() time.Duration {
	if c.opts.Interval > 0 {
		return c.opts.Interval
	}
	return patroni.DefaultFollowInterval
}

func (c *Coordinator) emit(ev *output.Event) {
	if c.opts.OnEvent == nil {
		return
	}
	c.opts.OnEvent(ev)
}

// leaderDescription builds a stable map shape for event bodies.
// Used twice (old + new) per LeaderChange so a sink subscriber
// sees a consistent payload regardless of nil-ness.
func leaderDescription(e *patroni.LeaderEndpoint) map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{
		"name":     e.Name,
		"host":     e.Host,
		"port":     e.Port,
		"timeline": e.Timeline,
		"role":     e.Role,
	}
}

// replicaCandidate pairs a Patroni member with its parsed lag
// for the picker. Package-private; only pickReplica + sortLag
// touch it.
type replicaCandidate struct {
	m   patroni.Member
	lag int64
}

// pickReplica selects the best replica for a SlotRoleReplica
// reconcile. Preference order:
//
//  1. Running replicas with reported lag, ascending by lag bytes
//     (lowest-lag first).
//  2. Running replicas WITHOUT reported lag (Patroni sometimes
//     omits it for stale or just-rejoined members).
//
// Members in non-running states are skipped entirely. The
// leader is also skipped — the function's contract is "pick a
// replica."
//
// Returns (member, true) on success; (zero, false) when no
// eligible replica exists. Caller emits no_replica_available
// in the failure case.
//
// Why lag-aware: a replica that's caught up to the leader is
// the safest dual-slot pin — its restart_lsn tracks the leader
// closely, so a future failover-to-this-replica means the slot
// continuity gap is small or zero. A lagging replica's slot
// would have a restart_lsn behind the leader's and would
// surface a false-positive gap on every reconcile.
func pickReplica(members []patroni.Member) (patroni.Member, bool) {
	var withLag []replicaCandidate
	var withoutLag []replicaCandidate
	for _, m := range members {
		if m.IsLeader() {
			continue
		}
		if m.State != "running" {
			continue
		}
		if m.Lag != nil {
			withLag = append(withLag, replicaCandidate{m: m, lag: *m.Lag})
		} else {
			withoutLag = append(withoutLag, replicaCandidate{m: m})
		}
	}
	// Lowest-lag wins. Stable sort so ties between replicas
	// with identical lag preserve the /cluster member order
	// (which Patroni reports deterministically across calls).
	sortLag(withLag)
	if len(withLag) > 0 {
		return withLag[0].m, true
	}
	if len(withoutLag) > 0 {
		return withoutLag[0].m, true
	}
	return patroni.Member{}, false
}

// sortLag sorts the candidate slice ascending by lag, stable.
// Extracted so a future change (preference for same-AZ, named
// member, etc.) drops in here without touching pickReplica.
//
// Insertion sort — bounded N (cluster member count is typically
// 3–5; rarely above 10). Avoids the heavy weight of sort.Slice +
// the closure allocation.
func sortLag(s []replicaCandidate) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1].lag > s[j].lag {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}
