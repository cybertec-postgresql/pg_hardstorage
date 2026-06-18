// Package search implements fleet-wide backup search. Given a repository
// URL and a query expression, it walks every deployment's committed
// manifests and returns those matching every predicate.
//
// v0.1 surface is deliberately small: AND-of-(key:value) tokens, no
// boolean OR, no full-text. Operators reach for fleet search to answer
// concrete questions like "show me every full backup of db1 from the
// last 7 days" — that shape is well-served by AND-only key:value, and
// the parser stays small enough to read in one sitting.
//
// Supported predicates:
//
//	deployment:<name>      exact match on Manifest.Deployment
//	tenant:<name>          exact match on Manifest.Tenant
//	type:<full|incremental> exact match on Manifest.Type
//	pg_version:<int>       exact match on Manifest.PGVersion
//	timeline:<int>         exact match on Manifest.Timeline
//	since:<duration>       Manifest.StartedAt >= now - duration
//	before:<duration>      Manifest.StartedAt <= now - duration
//	since:<RFC3339>        Manifest.StartedAt >= absolute timestamp
//	before:<RFC3339>       Manifest.StartedAt <= absolute timestamp
//
// Unknown keys parse to a typed error so the operator gets immediate
// feedback (rather than a silent empty result).
package search

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Predicate is a single AND-clause. Match returns true iff m satisfies
// it. Predicates are composed by And; the engine doesn't know about
// disjunction.
type Predicate interface {
	Match(m *backup.Manifest) bool
	String() string
}

// And composes predicates. Empty And matches everything ("no
// constraints, return all manifests").
type And []Predicate

// Match returns true iff every predicate matches.
func (a And) Match(m *backup.Manifest) bool {
	for _, p := range a {
		if !p.Match(m) {
			return false
		}
	}
	return true
}

// String concatenates predicate descriptions with " AND ".
func (a And) String() string {
	if len(a) == 0 {
		return "<all>"
	}
	parts := make([]string, len(a))
	for i, p := range a {
		parts[i] = p.String()
	}
	return strings.Join(parts, " AND ")
}

// Parse turns a raw query string into an And of predicates. Whitespace-
// separated tokens of the form key:value. Returns ErrEmptyQuery if the
// input is empty (the caller can decide whether that should mean
// "match everything" or "complain at the user").
func Parse(query string) (And, error) {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return nil, ErrEmptyQuery
	}
	out := make(And, 0, len(tokens))
	for _, tok := range tokens {
		i := strings.IndexByte(tok, ':')
		if i <= 0 || i == len(tok)-1 {
			return nil, fmt.Errorf("search: token %q is not key:value", tok)
		}
		key, val := tok[:i], tok[i+1:]
		p, err := newPredicate(key, val)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ErrEmptyQuery is returned by Parse when the query string is empty
// or whitespace-only.
var ErrEmptyQuery = fmt.Errorf("search: empty query")

func newPredicate(key, val string) (Predicate, error) {
	switch key {
	case "deployment":
		return eqString{field: "deployment", want: val, get: func(m *backup.Manifest) string { return m.Deployment }}, nil
	case "tenant":
		return eqString{field: "tenant", want: val, get: func(m *backup.Manifest) string { return m.Tenant }}, nil
	case "type":
		return eqString{field: "type", want: val, get: func(m *backup.Manifest) string { return string(m.Type) }}, nil
	case "pg_version":
		v, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("search: pg_version: %w", err)
		}
		return eqInt{field: "pg_version", want: v, get: func(m *backup.Manifest) int { return m.PGVersion }}, nil
	case "timeline":
		v, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("search: timeline: %w", err)
		}
		return eqInt{field: "timeline", want: int(v), get: func(m *backup.Manifest) int { return int(m.Timeline) }}, nil
	case "since":
		t, err := parseSinceBefore(val)
		if err != nil {
			return nil, fmt.Errorf("search: since: %w", err)
		}
		return timeAtLeast{field: "since", at: t}, nil
	case "before":
		t, err := parseSinceBefore(val)
		if err != nil {
			return nil, fmt.Errorf("search: before: %w", err)
		}
		return timeAtMost{field: "before", at: t}, nil
	}
	return nil, fmt.Errorf("search: unknown key %q (want deployment|tenant|type|pg_version|timeline|since|before)", key)
}

// parseSinceBefore accepts either an RFC3339 timestamp ("2026-04-28T00:00:00Z")
// or a duration ("7d", "24h", "30m"). Days ("d") aren't part of Go's
// time.ParseDuration so we handle them inline.
func parseSinceBefore(val string) (time.Time, error) {
	// RFC3339 first — has a 'T' or '-' shape that doesn't collide with durations.
	if t, err := time.Parse(time.RFC3339, val); err == nil {
		return t.UTC(), nil
	}
	// Days.
	if strings.HasSuffix(val, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(val, "d"))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid days duration %q", val)
		}
		return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	// Stock Go duration.
	d, err := time.ParseDuration(val)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid duration %q (want 7d, 24h, 30m, or RFC3339)", val)
	}
	return time.Now().UTC().Add(-d), nil
}

type eqString struct {
	field, want string
	get         func(*backup.Manifest) string
}

// Match reports whether m's field equals the wanted string.
func (p eqString) Match(m *backup.Manifest) bool { return p.get(m) == p.want }

// String renders the predicate as `field="want"`.
func (p eqString) String() string { return fmt.Sprintf("%s=%q", p.field, p.want) }

type eqInt struct {
	field string
	want  int
	get   func(*backup.Manifest) int
}

// Match reports whether m's field equals the wanted int.
func (p eqInt) Match(m *backup.Manifest) bool { return p.get(m) == p.want }

// String renders the predicate as `field=want`.
func (p eqInt) String() string { return fmt.Sprintf("%s=%d", p.field, p.want) }

type timeAtLeast struct {
	field string
	at    time.Time
}

// Match reports whether m.StartedAt is at or after the lower bound.
func (p timeAtLeast) Match(m *backup.Manifest) bool { return !m.StartedAt.Before(p.at) }

// String renders the predicate as `field>=RFC3339`.
func (p timeAtLeast) String() string {
	return fmt.Sprintf("%s>=%s", p.field, p.at.Format(time.RFC3339))
}

type timeAtMost struct {
	field string
	at    time.Time
}

// Match reports whether m.StartedAt is at or before the upper bound.
func (p timeAtMost) Match(m *backup.Manifest) bool { return !m.StartedAt.After(p.at) }

// String renders the predicate as `field<=RFC3339`.
func (p timeAtMost) String() string {
	return fmt.Sprintf("%s<=%s", p.field, p.at.Format(time.RFC3339))
}

// Hit is one matching manifest, with the fields the CLI text/JSON
// renderers need. We project only what's useful — embedding the full
// Manifest would balloon NDJSON output for large fleet scans.
type Hit struct {
	Deployment string    `json:"deployment"`
	BackupID   string    `json:"backup_id"`
	Tenant     string    `json:"tenant,omitempty"`
	Type       string    `json:"type"`
	PGVersion  int       `json:"pg_version"`
	Timeline   uint32    `json:"timeline"`
	StartLSN   string    `json:"start_lsn"`
	StopLSN    string    `json:"stop_lsn"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	Files      int       `json:"files"`
}

// SearchOptions configures a single Search call.
type SearchOptions struct {
	// Limit caps the number of returned hits. <= 0 means "no limit".
	// Useful in CLI mode so a fleet with 50k backups doesn't render
	// 50k lines of text.
	Limit int

	// Verifier, when set, is used to verify each manifest's signature
	// before yielding it. nil means accept whatever's on disk —
	// suitable for a quick query, but real audits should always pass
	// a Verifier.
	Verifier *backup.Verifier
}

// Search walks every deployment's manifests and returns those matching
// the query. Returns hits in deterministic order (deployment ASC,
// backup_id ASC) so two runs against the same repo produce identical
// output regardless of the storage backend's listing order.
//
// Unreadable / un-verifiable manifests are silently skipped — fleet
// search is a discovery tool, not a forensic one. Use `repair manifest`
// to chase a specific bad manifest.
func Search(ctx context.Context, sp storage.StoragePlugin, q And, opts SearchOptions) ([]Hit, error) {
	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("search: list deployments: %w", err)
	}
	var hits []Hit
	for _, dep := range deployments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for m, lerr := range ms.List(ctx, dep, opts.Verifier) {
			if lerr != nil {
				// Skip unreadable manifests — see doc comment.
				continue
			}
			if m == nil {
				continue
			}
			if !q.Match(m) {
				continue
			}
			hits = append(hits, manifestToHit(m))
			if opts.Limit > 0 && len(hits) >= opts.Limit {
				return sortHits(hits), nil
			}
		}
	}
	return sortHits(hits), nil
}

func manifestToHit(m *backup.Manifest) Hit {
	return Hit{
		Deployment: m.Deployment,
		BackupID:   m.BackupID,
		Tenant:     m.Tenant,
		Type:       string(m.Type),
		PGVersion:  m.PGVersion,
		Timeline:   m.Timeline,
		StartLSN:   m.StartLSN,
		StopLSN:    m.StopLSN,
		StartedAt:  m.StartedAt,
		StoppedAt:  m.StoppedAt,
		Files:      len(m.Files),
	}
}

func sortHits(h []Hit) []Hit {
	sort.Slice(h, func(i, j int) bool {
		if h[i].Deployment != h[j].Deployment {
			return h[i].Deployment < h[j].Deployment
		}
		return h[i].BackupID < h[j].BackupID
	})
	return h
}
