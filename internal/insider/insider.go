// Package insider implements insider-threat detection on top of the
// hash-chained audit log.  The SPEC commitment is "insider-threat
// anomaly detection — unusual download patterns, novel IAM principals,
// off-hours bulk reads → alert."
//
// Architecture: a *baseline window* (e.g. the prior 30 days) defines
// "normal" — which actors do what, in what tenants, at which hours.
// A *target window* (e.g. the last 24 h) is then scored against that
// baseline.  Findings are produced when something in the target
// breaks the baseline pattern: an actor never seen before performing
// a destructive action; an actor performing destructive ops at a
// time-of-day they never have; a sudden volume spike; a tenant
// boundary crossed for the first time.
//
// Design principles:
//
//   - We never call out to a generic "machine-learning model" — every
//     rule is auditable and explainable.  An operator gets a reason
//     they can read and verify against the audit log.
//   - The set of destructive actions is configurable but ships with
//     sensible defaults derived from the codebase's known governance
//     primitives (kms.shred, backup.delete, repo.gc, threshold.*,
//     jit.revoke, …).
//   - A scan is itself recorded.  Future commits can layer threshold
//     attestations on a scan so multiple operators sign-off that
//     "no insider-threat findings as of T" when blessing a release.
//
// Storage layout:
//
//	insider/scans/<id>.json
//
// Scan IDs are lex-sortable: <020d-unix-seconds>-<8-hex-fnv>.
package insider

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema is the on-disk version tag for a Scan body.
const SchemaScan = "pg_hardstorage.insider.scan.v1"

// FindingType enumerates the rules.  Stable strings (24-month
// backward-compat).
type FindingType string

const (
	// FindingNovelPrincipal flags an actor seen in the target window
	// but absent from the baseline window — a previously unknown
	// principal touching the audit log.
	FindingNovelPrincipal FindingType = "novel_principal"
	// FindingFirstDestructive flags the first time an actor performs
	// a destructive action that they never performed in the baseline.
	FindingFirstDestructive FindingType = "first_destructive_action"
	// FindingOffHoursDestructive flags a destructive action performed
	// at a UTC hour-of-day the actor never used for destructive ops
	// in the baseline.
	FindingOffHoursDestructive FindingType = "off_hours_destructive"
	// FindingVolumeSpike flags a per-(actor,action) target rate that
	// exceeds the baseline rate by Options.VolumeSpikeFactor.
	FindingVolumeSpike FindingType = "volume_spike"
	// FindingCrossTenantNovel flags an actor touching a tenant they
	// never touched in the baseline window.
	FindingCrossTenantNovel FindingType = "cross_tenant_novel"
	// FindingPostJITDestructive flags a destructive action performed
	// within one hour of a jit.issue for the same actor — the
	// canonical break-glass pattern, logged for the audit trail.
	FindingPostJITDestructive FindingType = "post_jit_destructive"
)

// Severity mirrors the RFC 5424 levels we use elsewhere.
type Severity string

const (
	// SeverityInfo is the lowest finding severity — informational
	// only, surfaced for completeness in scan listings.
	SeverityInfo Severity = "info"
	// SeverityNotice is a logged-but-non-alarming finding (e.g. the
	// break-glass post-JIT destructive pattern).
	SeverityNotice Severity = "notice"
	// SeverityWarning is an unusual-but-not-clearly-malicious finding
	// (novel principal, off-hours destructive, volume spike).
	SeverityWarning Severity = "warning"
	// SeverityCritical is the strongest finding severity — a first-
	// ever destructive action by an actor and similar patterns that
	// warrant immediate operator attention.
	SeverityCritical Severity = "critical"
)

// DefaultDestructiveActions is the action set we treat as "destructive"
// out of the box.  Operators override via Options.DestructiveActions.
var DefaultDestructiveActions = []string{
	"kms.shred",
	"kms.rotate",
	"backup.delete",
	"repo.gc",
	"repo.set_mode",
	"jit.revoke",
	"threshold.roster_create",
	"hold.remove",
}

// Defaults for time- and volume-based heuristics.
const (
	DefaultBaselineDuration  = 30 * 24 * time.Hour
	DefaultTargetDuration    = 24 * time.Hour
	DefaultVolumeSpikeFactor = 5.0
	DefaultMinBaselineSize   = 10 // skip volume-spike under low-N noise
)

// Options drives one Scan.
type Options struct {
	BaselineWindow     time.Duration
	TargetWindow       time.Duration
	Now                time.Time
	DestructiveActions []string

	// VolumeSpikeFactor: target-window per-action-type count must be
	// at least this multiple of baseline-window per-action-type rate
	// (per equivalent window length) to flag.  Default 5.0.
	VolumeSpikeFactor float64

	// MinBaselineSize: minimum # of baseline events of an action type
	// before we report volume spikes (avoids low-N noise).  Default 10.
	MinBaselineSize int

	// Tenant restricts the scan to events in this tenant only when
	// non-empty; otherwise scans every tenant.
	Tenant string

	// Note is recorded with the scan body.
	Note string
}

// Scan is the result of one detection pass.
type Scan struct {
	Schema     string    `json:"schema"`
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Tenant     string    `json:"tenant,omitempty"`
	Note       string    `json:"note,omitempty"`

	BaselineFrom time.Time `json:"baseline_from"`
	BaselineTo   time.Time `json:"baseline_to"`
	TargetFrom   time.Time `json:"target_from"`
	TargetTo     time.Time `json:"target_to"`

	BaselineEvents int `json:"baseline_events"`
	TargetEvents   int `json:"target_events"`
	BaselineActors int `json:"baseline_actors"`
	TargetActors   int `json:"target_actors"`

	DestructiveActions []string `json:"destructive_actions"`
	VolumeSpikeFactor  float64  `json:"volume_spike_factor"`

	Findings []Finding `json:"findings,omitempty"`
}

// Finding is one rule-trigger.
type Finding struct {
	Type     FindingType `json:"type"`
	Severity Severity    `json:"severity"`
	Actor    string      `json:"actor,omitempty"`
	Tenant   string      `json:"tenant,omitempty"`
	Action   string      `json:"action,omitempty"`
	Reason   string      `json:"reason"`
	// EventIDs lists the audit event(s) that triggered this finding.
	EventIDs []string `json:"event_ids,omitempty"`
	// Hour-of-day if the rule keys on time-of-day; -1 for n/a.
	HourOfDay int `json:"hour_of_day,omitempty"`
	// Counts: target / baseline (for volume spike).
	TargetCount  int     `json:"target_count,omitempty"`
	BaselineRate float64 `json:"baseline_rate,omitempty"`
}

// HighestSeverity returns the strongest severity in the scan's
// findings; "" if no findings.
func (s *Scan) HighestSeverity() Severity {
	rank := func(sv Severity) int {
		switch sv {
		case SeverityCritical:
			return 4
		case SeverityWarning:
			return 3
		case SeverityNotice:
			return 2
		case SeverityInfo:
			return 1
		}
		return 0
	}
	var best Severity
	for _, f := range s.Findings {
		if rank(f.Severity) > rank(best) {
			best = f.Severity
		}
	}
	return best
}

// Sentinel errors.
var (
	ErrScanNotFound  = errors.New("insider: scan not found")
	ErrInvalidWindow = errors.New("insider: invalid window")
)

// Detector executes scans against an audit.Store.
type Detector struct {
	store *audit.Store
}

// NewDetector wraps an audit.Store.
func NewDetector(store *audit.Store) *Detector {
	return &Detector{store: store}
}

// Run executes one scan.  Pulls baseline + target events from the
// audit store, computes findings, returns the populated Scan
// (unsigned/unsaved — caller persists via ScanStore).
func (d *Detector) Run(ctx context.Context, opts Options) (*Scan, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	baselineWindow := opts.BaselineWindow
	if baselineWindow == 0 {
		baselineWindow = DefaultBaselineDuration
	}
	targetWindow := opts.TargetWindow
	if targetWindow == 0 {
		targetWindow = DefaultTargetDuration
	}
	if baselineWindow <= 0 || targetWindow <= 0 {
		return nil, fmt.Errorf("%w: baseline %s / target %s",
			ErrInvalidWindow, baselineWindow, targetWindow)
	}
	destructive := opts.DestructiveActions
	if len(destructive) == 0 {
		destructive = DefaultDestructiveActions
	}
	spikeFactor := opts.VolumeSpikeFactor
	if spikeFactor <= 0 {
		spikeFactor = DefaultVolumeSpikeFactor
	}
	minBaselineSize := opts.MinBaselineSize
	if minBaselineSize <= 0 {
		minBaselineSize = DefaultMinBaselineSize
	}

	targetTo := now.UTC()
	targetFrom := targetTo.Add(-targetWindow)
	baselineTo := targetFrom
	baselineFrom := baselineTo.Add(-baselineWindow)

	scan := &Scan{
		Schema:             SchemaScan,
		ID:                 newScanID(now, opts.Tenant),
		StartedAt:          now.UTC(),
		Tenant:             opts.Tenant,
		Note:               opts.Note,
		BaselineFrom:       baselineFrom,
		BaselineTo:         baselineTo,
		TargetFrom:         targetFrom,
		TargetTo:           targetTo,
		DestructiveActions: append([]string(nil), destructive...),
		VolumeSpikeFactor:  spikeFactor,
	}

	baseline, err := d.store.Search(ctx, audit.ListFilters{
		Tenant: opts.Tenant,
		Since:  baselineFrom,
		Until:  baselineTo,
	})
	if err != nil {
		scan.FinishedAt = time.Now().UTC()
		return scan, fmt.Errorf("insider: baseline search: %w", err)
	}
	target, err := d.store.Search(ctx, audit.ListFilters{
		Tenant: opts.Tenant,
		Since:  targetFrom,
		Until:  targetTo,
	})
	if err != nil {
		scan.FinishedAt = time.Now().UTC()
		return scan, fmt.Errorf("insider: target search: %w", err)
	}
	scan.BaselineEvents = len(baseline)
	scan.TargetEvents = len(target)

	// Profile the baseline for fast lookup.
	bp := buildProfile(baseline, destructive)
	scan.BaselineActors = len(bp.actorEvents)
	tpActors := uniqueActors(target)
	scan.TargetActors = len(tpActors)

	// Apply rules.
	scan.Findings = appendFindings(scan.Findings,
		detectNovelPrincipal(target, bp))
	scan.Findings = appendFindings(scan.Findings,
		detectFirstDestructive(target, bp, destructive))
	scan.Findings = appendFindings(scan.Findings,
		detectOffHoursDestructive(target, bp, destructive))
	scan.Findings = appendFindings(scan.Findings,
		detectVolumeSpike(target, bp, baselineWindow, targetWindow,
			destructive, spikeFactor, minBaselineSize))
	scan.Findings = appendFindings(scan.Findings,
		detectCrossTenantNovel(target, bp))
	scan.Findings = appendFindings(scan.Findings,
		detectPostJITDestructive(target, destructive))

	scan.FinishedAt = time.Now().UTC()
	return scan, nil
}

// ----- profile building -----

// profile is the aggregate of one event window per actor.
type profile struct {
	actorEvents          map[string][]*audit.Event
	actorTenants         map[string]map[string]struct{}
	actorActions         map[string]map[string]int
	actorDestructive     map[string]map[string]struct{}
	actorDestructiveHour map[string]map[int]struct{}
}

func buildProfile(events []*audit.Event, destructive []string) *profile {
	dest := stringSetFromSlice(destructive)
	p := &profile{
		actorEvents:          make(map[string][]*audit.Event),
		actorTenants:         make(map[string]map[string]struct{}),
		actorActions:         make(map[string]map[string]int),
		actorDestructive:     make(map[string]map[string]struct{}),
		actorDestructiveHour: make(map[string]map[int]struct{}),
	}
	for _, ev := range events {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		p.actorEvents[actor] = append(p.actorEvents[actor], ev)
		if _, ok := p.actorTenants[actor]; !ok {
			p.actorTenants[actor] = make(map[string]struct{})
		}
		p.actorTenants[actor][ev.Tenant] = struct{}{}
		if _, ok := p.actorActions[actor]; !ok {
			p.actorActions[actor] = make(map[string]int)
		}
		p.actorActions[actor][ev.Action]++
		if _, isDest := dest[ev.Action]; isDest {
			if _, ok := p.actorDestructive[actor]; !ok {
				p.actorDestructive[actor] = make(map[string]struct{})
			}
			p.actorDestructive[actor][ev.Action] = struct{}{}
			if _, ok := p.actorDestructiveHour[actor]; !ok {
				p.actorDestructiveHour[actor] = make(map[int]struct{})
			}
			p.actorDestructiveHour[actor][ev.Timestamp.UTC().Hour()] = struct{}{}
		}
	}
	return p
}

func uniqueActors(events []*audit.Event) []string {
	seen := make(map[string]struct{})
	for _, ev := range events {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		seen[actor] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normaliseActor(actor string) string {
	return strings.TrimSpace(actor)
}

func stringSetFromSlice(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// ----- rule implementations -----

// detectNovelPrincipal: actor in target with no events at all in
// baseline.  Severity: warning.
func detectNovelPrincipal(target []*audit.Event, bp *profile) []Finding {
	tpActors := uniqueActors(target)
	var findings []Finding
	seen := make(map[string]bool)
	for _, actor := range tpActors {
		if _, ok := bp.actorEvents[actor]; ok {
			continue
		}
		// Only one finding per actor (multiple events fold into one).
		if seen[actor] {
			continue
		}
		seen[actor] = true
		eventIDs := []string{}
		actions := make(map[string]struct{})
		for _, ev := range target {
			if normaliseActor(ev.Actor) == actor {
				eventIDs = append(eventIDs, ev.ID)
				actions[ev.Action] = struct{}{}
			}
		}
		findings = append(findings, Finding{
			Type:     FindingNovelPrincipal,
			Severity: SeverityWarning,
			Actor:    actor,
			Reason: fmt.Sprintf(
				"actor %q not seen in baseline window; performed %d action(s) of %d distinct kind(s)",
				actor, len(eventIDs), len(actions)),
			EventIDs:  capIDs(eventIDs),
			HourOfDay: -1,
		})
	}
	return findings
}

// detectFirstDestructive: actor in target performs a destructive
// action; in baseline the same actor never performed any
// destructive action (or wasn't present at all — which is ALSO
// covered by NovelPrincipal but we de-duplicate on type+actor).
// Severity: critical.
func detectFirstDestructive(target []*audit.Event, bp *profile, destructive []string) []Finding {
	dest := stringSetFromSlice(destructive)
	var findings []Finding
	seen := make(map[string]map[string]bool) // actor → action → bool
	for _, ev := range target {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		if _, isDest := dest[ev.Action]; !isDest {
			continue
		}
		if seen[actor] == nil {
			seen[actor] = make(map[string]bool)
		}
		if seen[actor][ev.Action] {
			continue
		}
		seen[actor][ev.Action] = true
		// The actor's baseline destructive set:
		if _, ok := bp.actorDestructive[actor][ev.Action]; ok {
			continue // already done this destructive action before
		}
		findings = append(findings, Finding{
			Type:      FindingFirstDestructive,
			Severity:  SeverityCritical,
			Actor:     actor,
			Tenant:    ev.Tenant,
			Action:    ev.Action,
			Reason:    fmt.Sprintf("actor %q performed destructive action %q for the first time in the scanned history", actor, ev.Action),
			EventIDs:  []string{ev.ID},
			HourOfDay: ev.Timestamp.UTC().Hour(),
		})
	}
	return findings
}

// detectOffHoursDestructive: actor's destructive event in target
// happened in an hour-of-day they never performed destructive ops
// in during the baseline.  Severity: warning.
func detectOffHoursDestructive(target []*audit.Event, bp *profile, destructive []string) []Finding {
	dest := stringSetFromSlice(destructive)
	var findings []Finding
	for _, ev := range target {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		if _, isDest := dest[ev.Action]; !isDest {
			continue
		}
		hours := bp.actorDestructiveHour[actor]
		if hours == nil {
			continue // no baseline destructive history → covered by FirstDestructive
		}
		hr := ev.Timestamp.UTC().Hour()
		if _, ok := hours[hr]; ok {
			continue
		}
		findings = append(findings, Finding{
			Type:      FindingOffHoursDestructive,
			Severity:  SeverityWarning,
			Actor:     actor,
			Tenant:    ev.Tenant,
			Action:    ev.Action,
			Reason:    fmt.Sprintf("actor %q performed destructive action %q at UTC hour %d, outside the baseline hours %v", actor, ev.Action, hr, sortedHours(hours)),
			EventIDs:  []string{ev.ID},
			HourOfDay: hr,
		})
	}
	return findings
}

// detectVolumeSpike: per-(actor, action), if the target rate
// (count / target_window) exceeds the baseline rate
// (count / baseline_window) by Factor, flag.  Skipped when
// baseline count < MinBaselineSize.  Severity: warning.
func detectVolumeSpike(target []*audit.Event, bp *profile,
	baselineWindow, targetWindow time.Duration,
	destructive []string,
	factor float64, minBaselineSize int) []Finding {

	// Build target-window per-(actor,action) counts.
	type key struct{ actor, action string }
	tCount := make(map[key]int)
	tEventIDs := make(map[key][]string)
	for _, ev := range target {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		k := key{actor, ev.Action}
		tCount[k]++
		tEventIDs[k] = append(tEventIDs[k], ev.ID)
	}
	// Normalise the rates to a common per-second scale.
	bSec := baselineWindow.Seconds()
	tSec := targetWindow.Seconds()

	var findings []Finding
	for k, tc := range tCount {
		bc := bp.actorActions[k.actor][k.action]
		if bc < minBaselineSize {
			// Low baseline-N: skip to avoid noise unless brand-new
			// (which FirstDestructive / NovelPrincipal already covers).
			continue
		}
		baselineRate := float64(bc) / bSec
		targetRate := float64(tc) / tSec
		if baselineRate <= 0 {
			continue
		}
		if targetRate < factor*baselineRate {
			continue
		}
		findings = append(findings, Finding{
			Type:     FindingVolumeSpike,
			Severity: SeverityWarning,
			Actor:    k.actor,
			Action:   k.action,
			Reason: fmt.Sprintf(
				"actor %q performed %q %d time(s) in target (%.0f/s) vs baseline mean rate %.4f/s — exceeds %.1f× threshold",
				k.actor, k.action, tc, targetRate, baselineRate, factor),
			EventIDs:     capIDs(tEventIDs[k]),
			TargetCount:  tc,
			BaselineRate: baselineRate,
			HourOfDay:    -1,
		})
	}
	return findings
}

// detectCrossTenantNovel: actor touches a tenant in target they
// haven't touched in baseline.  Severity: warning.
func detectCrossTenantNovel(target []*audit.Event, bp *profile) []Finding {
	var findings []Finding
	seen := make(map[string]map[string]bool)
	for _, ev := range target {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		baseTenants := bp.actorTenants[actor]
		if baseTenants == nil {
			continue // covered by NovelPrincipal
		}
		if _, ok := baseTenants[ev.Tenant]; ok {
			continue
		}
		if seen[actor] == nil {
			seen[actor] = make(map[string]bool)
		}
		if seen[actor][ev.Tenant] {
			continue
		}
		seen[actor][ev.Tenant] = true
		findings = append(findings, Finding{
			Type:      FindingCrossTenantNovel,
			Severity:  SeverityWarning,
			Actor:     actor,
			Tenant:    ev.Tenant,
			Action:    ev.Action,
			Reason:    fmt.Sprintf("actor %q touched tenant %q for the first time", actor, ev.Tenant),
			EventIDs:  []string{ev.ID},
			HourOfDay: -1,
		})
	}
	return findings
}

// detectPostJITDestructive: a destructive action immediately
// preceded by a jit.issue for the same actor in the target window.
// This is the canonical break-glass pattern; we surface it as
// notice (informational) so cron alerts aren't noisy by default,
// but the audit trail unambiguously records the chain.  Severity:
// notice.
func detectPostJITDestructive(target []*audit.Event, destructive []string) []Finding {
	dest := stringSetFromSlice(destructive)
	// Group target events by actor in chronological order.
	byActor := make(map[string][]*audit.Event)
	for _, ev := range target {
		actor := normaliseActor(ev.Actor)
		if actor == "" {
			continue
		}
		byActor[actor] = append(byActor[actor], ev)
	}
	var findings []Finding
	for actor, evs := range byActor {
		sort.Slice(evs, func(i, j int) bool {
			return evs[i].Timestamp.Before(evs[j].Timestamp)
		})
		var lastJIT *audit.Event
		for _, ev := range evs {
			if ev.Action == "jit.issue" {
				lastJIT = ev
				continue
			}
			if _, isDest := dest[ev.Action]; !isDest {
				continue
			}
			if lastJIT == nil {
				continue
			}
			// Within 1 h of a jit.issue → record.
			if ev.Timestamp.Sub(lastJIT.Timestamp) > time.Hour {
				continue
			}
			findings = append(findings, Finding{
				Type:     FindingPostJITDestructive,
				Severity: SeverityNotice,
				Actor:    actor,
				Tenant:   ev.Tenant,
				Action:   ev.Action,
				Reason: fmt.Sprintf(
					"actor %q performed destructive %q within 1h of a jit.issue (event %s) — break-glass pattern logged",
					actor, ev.Action, lastJIT.ID),
				EventIDs:  []string{lastJIT.ID, ev.ID},
				HourOfDay: ev.Timestamp.UTC().Hour(),
			})
			lastJIT = nil // one chain per JIT
		}
	}
	return findings
}

// ----- helpers -----

func appendFindings(out []Finding, more []Finding) []Finding {
	return append(out, more...)
}

// capIDs caps event-ID slices at 10 to avoid noisy huge findings.
func capIDs(in []string) []string {
	if len(in) <= 10 {
		return in
	}
	out := make([]string, 10)
	copy(out, in[:9])
	out[9] = fmt.Sprintf("…+%d more", len(in)-9)
	return out
}

func sortedHours(hours map[int]struct{}) []int {
	out := make([]int, 0, len(hours))
	for h := range hours {
		out = append(out, h)
	}
	sort.Ints(out)
	return out
}

// newScanID is lex-sortable.
func newScanID(at time.Time, tenant string) string {
	hasher := fnv.New32a()
	hasher.Write([]byte(at.UTC().Format(time.RFC3339Nano)))
	hasher.Write([]byte(tenant))
	short := fmt.Sprintf("%08x", hasher.Sum32())
	return fmt.Sprintf("%020d-%s", at.UTC().Unix(), short)
}

// ----- storage -----

// ScanStore reads + writes scans.
type ScanStore struct {
	sp storage.StoragePlugin
}

// NewScanStore wraps sp.
func NewScanStore(sp storage.StoragePlugin) *ScanStore {
	return &ScanStore{sp: sp}
}

func scanKey(id string) string { return "insider/scans/" + id + ".json" }

// Put persists a scan.  Refuses to overwrite (lex-sortable IDs
// include a timestamp; collision is a programmer error).
func (s *ScanStore) Put(ctx context.Context, scan *Scan) error {
	body, err := stdjson.MarshalIndent(scan, "", "  ")
	if err != nil {
		return err
	}
	key := scanKey(scan.ID)
	tmp := key + ".tmp"
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("insider: put tmp: %w", err)
	}
	return s.sp.RenameIfNotExists(ctx, tmp, key)
}

// Get reads + decodes one scan.
func (s *ScanStore) Get(ctx context.Context, id string) (*Scan, error) {
	rd, err := s.sp.Get(ctx, scanKey(id))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrScanNotFound, id, err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("insider: scan read: %w", err)
	}
	var scan Scan
	if err := stdjson.Unmarshal(body, &scan); err != nil {
		return nil, fmt.Errorf("insider: scan decode: %w", err)
	}
	return &scan, nil
}

// ListFilter filters scans on read.
type ListFilter struct {
	Since           *time.Time
	MinSeverity     Severity
	Tenant          string
	HasFindingsOnly bool
}

// List returns scans newest-first matching the filter.
func (s *ScanStore) List(ctx context.Context, f ListFilter) ([]*Scan, error) {
	const prefix = "insider/scans/"
	var out []*Scan
	for obj, err := range s.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("insider: list: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(base, ".json")
		scan, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		if f.Since != nil && scan.StartedAt.Before(*f.Since) {
			continue
		}
		if f.Tenant != "" && scan.Tenant != f.Tenant {
			continue
		}
		if f.HasFindingsOnly && len(scan.Findings) == 0 {
			continue
		}
		if f.MinSeverity != "" && !severityAtLeast(scan.HighestSeverity(), f.MinSeverity) {
			continue
		}
		out = append(out, scan)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func severityAtLeast(have, want Severity) bool {
	rank := map[Severity]int{
		SeverityInfo:     1,
		SeverityNotice:   2,
		SeverityWarning:  3,
		SeverityCritical: 4,
	}
	return rank[have] >= rank[want]
}
