// preflight.go — fatal/warning preflight findings that gate `wal stream` startup.
package replication

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// PreflightSeverity tags a single Preflight finding.  Fatal findings
// fail the preflight unless the operator explicitly skips; warnings
// surface in the result for awareness but do not block streaming.
type PreflightSeverity string

const (
	// PreflightFatal: the streamer cannot operate correctly under
	// this configuration.  Default behaviour is to refuse to start
	// streaming; operator can override with --skip-preflight.
	PreflightFatal PreflightSeverity = "fatal"

	// PreflightWarning: the configuration is technically allowed
	// but introduces a footgun the operator should know about
	// (e.g. max_slot_wal_keep_size capping slot retention).
	// Streaming proceeds; the warning lands in the start event.
	PreflightWarning PreflightSeverity = "warning"

	// PreflightInfo: informational only; surfaces values the
	// operator may want to see in the start event without any
	// implied judgement.
	PreflightInfo PreflightSeverity = "info"
)

// PreflightFinding is one structured observation from the preflight
// checks.  Code is a short stable identifier (e.g.
// "wal_level.too_low") that consumers can match on; Message is the
// human-readable summary; Suggestion is the remediation hint when
// the finding is fatal.
type PreflightFinding struct {
	Severity   PreflightSeverity `json:"severity"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	Suggestion string            `json:"suggestion,omitempty"`
	// Observed is the actual value the check read from PG (e.g.
	// "logical" for wal_level).  Empty when the check is binary
	// (e.g. user role membership).
	Observed string `json:"observed,omitempty"`
	// Required is the value the check expects when applicable
	// (e.g. "replica or higher" for wal_level).  Empty when the
	// check is binary.
	Required string `json:"required,omitempty"`
}

// PreflightResult is what Preflight returns: a structured set of
// findings the operator-facing CLI surfaces verbatim.  HasFatal()
// is the gate for "should we refuse to start streaming?".
type PreflightResult struct {
	Findings []PreflightFinding `json:"findings"`
	// PgVersion is the parsed server_version_num (e.g. 170000 for
	// PG 17.0).  Useful for callers that branch on PG version
	// (e.g. idle_replication_slot_timeout is only PG 17+).
	PgVersionNum int `json:"pg_version_num,omitempty"`
}

// HasFatal reports whether any finding in the result is fatal.
// Callers that want to block on warnings too can iterate Findings
// directly.
func (r *PreflightResult) HasFatal() bool {
	for _, f := range r.Findings {
		if f.Severity == PreflightFatal {
			return true
		}
	}
	return false
}

// Preflight runs the WAL-stream readiness checks against the
// regular-mode connection in c.  The connection must be in
// ModeRegular — the checks query pg_settings, pg_roles, and
// pg_replication_slots, none of which are reachable from a
// replication-mode connection.
//
// Checks performed (priority order, fatal first):
//
//   - wal_level >= replica (fatal): physical replication is
//     impossible at lower levels.
//   - max_replication_slots > current_count (fatal): a full slot
//     table refuses CREATE_REPLICATION_SLOT.
//   - max_wal_senders > current_active (fatal): a saturated
//     wal-sender pool refuses START_REPLICATION.
//   - connecting role has REPLICATION attribute (fatal):
//     pg_roles.rolreplication must be true.
//   - max_slot_wal_keep_size > 0 (warning): caps slot retention,
//     so a backed-up streamer can still lose WAL.
//   - max_slot_wal_keep_size unbounded (info): surfaces the
//     opposite disk-fill risk so the operator picks a policy
//     explicitly.  See docs/how-to/operating/slot-disk-safety.md.
//   - idle_replication_slot_timeout > 0 (warning, PG 17+):
//     idle slots get dropped silently.
//   - wal_keep_size (info): surfaces the value for diagnostics.
//
// connectingRole is the role the streamer connects as.  Pass empty
// to skip the per-role REPLICATION-attribute check (e.g. when the
// caller doesn't know the role yet); the slot-create attempt will
// surface the missing attribute later, but with a less actionable
// error.
//
// appName is the application_name the streamer's replication
// connection uses.  When non-empty, Preflight checks whether it is
// listed in synchronous_standby_names — see the sync_standby.*
// findings.  Pass empty to skip that check.
func Preflight(ctx context.Context, c *pg.Conn, connectingRole, appName string) (*PreflightResult, error) {
	if c == nil {
		return nil, errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeRegular {
		return nil, fmt.Errorf("replication: Preflight requires ModeRegular; got %s", c.Mode())
	}

	res := &PreflightResult{}

	if v, err := readIntSetting(ctx, c, "server_version_num"); err == nil {
		res.PgVersionNum = v
	}

	walLevel, err := readSetting(ctx, c, "wal_level")
	if err != nil {
		return res, fmt.Errorf("replication: read wal_level: %w", err)
	}
	if !walLevelOK(walLevel) {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightFatal,
			Code:       "wal_level.too_low",
			Message:    fmt.Sprintf("wal_level is %q; physical replication requires replica or higher", walLevel),
			Suggestion: "set wal_level = replica (or logical) in postgresql.conf and restart PostgreSQL",
			Observed:   walLevel,
			Required:   "replica or logical",
		})
	}

	maxSlots, err := readIntSetting(ctx, c, "max_replication_slots")
	if err != nil {
		return res, fmt.Errorf("replication: read max_replication_slots: %w", err)
	}
	curSlots, err := countReplicationSlots(ctx, c)
	if err != nil {
		return res, fmt.Errorf("replication: count slots: %w", err)
	}
	if maxSlots <= 0 {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightFatal,
			Code:       "max_replication_slots.zero",
			Message:    "max_replication_slots is 0; the server cannot hold any replication slots",
			Suggestion: "set max_replication_slots = 10 (or higher) in postgresql.conf and restart PostgreSQL",
			Observed:   strconv.Itoa(maxSlots),
			Required:   "> 0",
		})
	} else if curSlots >= maxSlots {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightFatal,
			Code:       "max_replication_slots.full",
			Message:    fmt.Sprintf("all %d replication slots are in use; CREATE_REPLICATION_SLOT will fail", maxSlots),
			Suggestion: "raise max_replication_slots and restart PostgreSQL, or drop unused slots from pg_replication_slots",
			Observed:   strconv.Itoa(curSlots),
			Required:   fmt.Sprintf("< %d (max_replication_slots)", maxSlots),
		})
	}

	maxSenders, err := readIntSetting(ctx, c, "max_wal_senders")
	if err != nil {
		return res, fmt.Errorf("replication: read max_wal_senders: %w", err)
	}
	curSenders, err := countActiveWalSenders(ctx, c)
	if err != nil {
		return res, fmt.Errorf("replication: count active wal senders: %w", err)
	}
	if maxSenders <= 0 {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightFatal,
			Code:       "max_wal_senders.zero",
			Message:    "max_wal_senders is 0; START_REPLICATION cannot connect",
			Suggestion: "set max_wal_senders = 10 (or higher) in postgresql.conf and restart PostgreSQL",
			Observed:   strconv.Itoa(maxSenders),
			Required:   "> 0",
		})
	} else if curSenders >= maxSenders {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightFatal,
			Code:       "max_wal_senders.saturated",
			Message:    fmt.Sprintf("all %d wal senders are active; START_REPLICATION will fail", maxSenders),
			Suggestion: "raise max_wal_senders and restart PostgreSQL, or terminate idle replication connections",
			Observed:   strconv.Itoa(curSenders),
			Required:   fmt.Sprintf("< %d (max_wal_senders)", maxSenders),
		})
	}

	if connectingRole != "" {
		hasRepl, err := roleHasReplication(ctx, c, connectingRole)
		if err != nil {
			return res, fmt.Errorf("replication: probe role %q: %w", connectingRole, err)
		}
		if !hasRepl {
			res.Findings = append(res.Findings, PreflightFinding{
				Severity:   PreflightFatal,
				Code:       "role.no_replication",
				Message:    fmt.Sprintf("role %q does not have the REPLICATION attribute; replication-mode connections will fail authentication", connectingRole),
				Suggestion: fmt.Sprintf("ALTER ROLE %s WITH REPLICATION; (requires superuser) — and confirm pg_hba.conf has a `replication` entry that matches this role + host", connectingRole),
			})
		}
	}

	if v, err := readIntSetting(ctx, c, "max_slot_wal_keep_size"); err == nil {
		switch {
		case v > 0:
			res.Findings = append(res.Findings, PreflightFinding{
				Severity:   PreflightWarning,
				Code:       "max_slot_wal_keep_size.set",
				Message:    fmt.Sprintf("max_slot_wal_keep_size = %d MB; PG will recycle WAL even when the slot is behind, so a sustained streamer outage CAN still lose WAL", v),
				Suggestion: "pair the cap with a streamer-lag alert (pg_hardstorage_wal_archive_lag_bytes) sized at ~75%% of the cap; see docs/how-to/operating/slot-disk-safety.md for the full trade-off",
				Observed:   strconv.Itoa(v) + "MB",
			})
		default:
			// v == -1 (unlimited, the PG default) or v == 0
			// (also unlimited in PG's grammar).  Surface the
			// disk-fill risk so the policy is explicit, but
			// don't escalate — it's a valid choice.
			res.Findings = append(res.Findings, PreflightFinding{
				Severity:   PreflightInfo,
				Code:       "max_slot_wal_keep_size.unbounded",
				Message:    "max_slot_wal_keep_size = -1 (PG default); the slot will retain WAL until the streamer reconnects, even if pg_wal/ fills the partition",
				Suggestion: "pair with a disk-free alert on the partition holding pg_wal/ AND a streamer-lag alert; or set max_slot_wal_keep_size to bound the slot — see docs/how-to/operating/slot-disk-safety.md for the full trade-off",
				Observed:   strconv.Itoa(v),
			})
		}
	}

	// PG 17+ ships idle_replication_slot_timeout (silently drops
	// idle slots).  Older PGs don't have the setting; silently
	// skip the check there.
	if res.PgVersionNum >= 170000 {
		if v, err := readIntSetting(ctx, c, "idle_replication_slot_timeout"); err == nil && v > 0 {
			res.Findings = append(res.Findings, PreflightFinding{
				Severity:   PreflightWarning,
				Code:       "idle_replication_slot_timeout.set",
				Message:    fmt.Sprintf("idle_replication_slot_timeout = %ds; idle slots get dropped silently and PG will recycle WAL afterwards", v),
				Suggestion: "set idle_replication_slot_timeout = 0 to disable; otherwise size it generously enough to cover scheduled streamer downtime",
				Observed:   strconv.Itoa(v) + "s",
			})
		}
	}

	if v, err := readSetting(ctx, c, "wal_keep_size"); err == nil {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity: PreflightInfo,
			Code:     "wal_keep_size",
			Message:  fmt.Sprintf("wal_keep_size = %s (informational; the slot is the primary retention guarantee)", v),
			Observed: v,
		})
	}

	// wal_sender_timeout = 0 disables PG's primary-keepalive
	// emission entirely.  When that's the case the streamer's
	// client-side inactivity watchdog is a footgun: PG sends
	// nothing during idle periods, the read deadline fires, the
	// stream errors out (and with auto-reconnect, loops forever).
	// Issue #12 documents the exact failure mode.  Surface as a
	// warning so operators know to pair the setting with
	// `--no-inactivity-timeout` on the streamer.
	if v, err := readIntSetting(ctx, c, "wal_sender_timeout"); err == nil && v == 0 {
		res.Findings = append(res.Findings, PreflightFinding{
			Severity:   PreflightWarning,
			Code:       "wal_sender_timeout.zero",
			Message:    "wal_sender_timeout = 0 disables PG's primary keepalives; the streamer's default inactivity watchdog will fire after 5 minutes of idle WAL and reconnect in a loop",
			Suggestion: "either set wal_sender_timeout to a non-zero value (the PG default of 60s is a sane choice), OR pass --no-inactivity-timeout to the streamer to disable the client-side watchdog",
			Observed:   "0",
		})
	}

	// Synchronous-standby detection. If the operator has named this
	// agent in synchronous_standby_names, PG commits BLOCK on its
	// flush feedback — and pg_hardstorage's WAL streamer commits at
	// 16 MiB-segment granularity, so it would add a whole segment's
	// fill + archive time to every dependent commit. Worse, with
	// synchronous_commit = remote_apply a WAL archiver can never
	// report an apply LSN, so the primary's commits would hang
	// forever. Both are surfaced here.
	if appName != "" {
		if ssn, err := readSetting(ctx, c, "synchronous_standby_names"); err == nil &&
			syncStandbyNamesContains(ssn, appName) {
			sc, _ := readSetting(ctx, c, "synchronous_commit")
			if strings.EqualFold(strings.TrimSpace(sc), "remote_apply") {
				res.Findings = append(res.Findings, PreflightFinding{
					Severity: PreflightFatal,
					Code:     "sync_standby.remote_apply",
					Message: fmt.Sprintf("application_name %q is in synchronous_standby_names AND synchronous_commit = remote_apply; "+
						"a WAL archiver never reports an apply LSN, so the primary's commits would hang forever", appName),
					Suggestion: "remove this agent from synchronous_standby_names, or set synchronous_commit to remote_write/on — a WAL archiver cannot satisfy remote_apply",
					Observed:   "remote_apply",
				})
			} else {
				res.Findings = append(res.Findings, PreflightFinding{
					Severity: PreflightWarning,
					Code:     "sync_standby.named",
					Message: fmt.Sprintf("application_name %q is listed in synchronous_standby_names; pg_hardstorage's WAL streamer commits whole 16 MiB segments, "+
						"so every dependent commit waits for a segment to fill and archive — substantial added commit latency", appName),
					Suggestion: "if you did not intend pg_hardstorage to gate commit durability, remove it from synchronous_standby_names — it is a segment-granular archiver, not a low-latency synchronous replica",
					Observed:   ssn,
				})
			}
		}
	}

	return res, nil
}

// syncStandbyNamesContains reports whether name appears in a
// synchronous_standby_names GUC value. It handles the documented
// shapes: a wildcard "*"; a bare comma list ("s1, s2"); and the
// FIRST k (...) / ANY k (...) method forms. Names may be
// double-quoted. This is a pragmatic textual match — exactly what
// PG itself does on a name (PG lower-cases unquoted identifiers,
// but pg_hardstorage's app name is already lower-case).
func syncStandbyNamesContains(guc, name string) bool {
	s := strings.TrimSpace(guc)
	if s == "" || name == "" {
		return false
	}
	// Strip an optional "FIRST k" / "ANY k" method prefix + parens.
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "first ") || strings.HasPrefix(low, "any ") {
		if i := strings.Index(s, "("); i >= 0 {
			if j := strings.LastIndex(s, ")"); j > i {
				s = s[i+1 : j]
			}
		}
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.Trim(strings.TrimSpace(part), `"`)
		if p == "*" || p == name {
			return true
		}
	}
	return false
}

// walLevelOK reports whether the wal_level value is sufficient for
// physical replication.  The valid values are "replica" and
// "logical" (logical implies replica).  Anything else (most
// importantly "minimal") is fatal.
func walLevelOK(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "replica", "logical":
		return true
	}
	return false
}

// readSetting fetches one row from pg_settings.  Returns the empty
// string with no error when the setting doesn't exist (older PGs
// missing forward-introduced settings); a real query error is
// surfaced.
func readSetting(ctx context.Context, c *pg.Conn, name string) (string, error) {
	const q = `SELECT setting FROM pg_settings WHERE name = $1`
	res := c.PgConn().ExecParams(ctx, q, [][]byte{[]byte(name)}, nil, nil, nil).Read()
	if res.Err != nil {
		return "", res.Err
	}
	if len(res.Rows) == 0 {
		return "", nil
	}
	return string(res.Rows[0][0]), nil
}

// readIntSetting wraps readSetting with strconv.Atoi.  Returns
// (0, nil) when the setting is absent so callers can branch on
// the value rather than the absence.
func readIntSetting(ctx context.Context, c *pg.Conn, name string) (int, error) {
	v, err := readSetting(ctx, c, name)
	if err != nil {
		return 0, err
	}
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// countReplicationSlots returns the number of rows in
// pg_replication_slots.  Compared against max_replication_slots
// to detect a full slot table.
func countReplicationSlots(ctx context.Context, c *pg.Conn) (int, error) {
	const q = `SELECT count(*)::text FROM pg_replication_slots`
	res := c.PgConn().ExecParams(ctx, q, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return 0, res.Err
	}
	if len(res.Rows) == 0 {
		return 0, nil
	}
	return strconv.Atoi(string(res.Rows[0][0]))
}

// countActiveWalSenders returns the number of rows in
// pg_stat_replication (which is one per active wal sender).
func countActiveWalSenders(ctx context.Context, c *pg.Conn) (int, error) {
	const q = `SELECT count(*)::text FROM pg_stat_replication`
	res := c.PgConn().ExecParams(ctx, q, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return 0, res.Err
	}
	if len(res.Rows) == 0 {
		return 0, nil
	}
	return strconv.Atoi(string(res.Rows[0][0]))
}

// roleHasReplication checks pg_roles.rolreplication for the named
// role.  Returns false if the role doesn't exist, with no error —
// the streamer's connection attempt will surface the missing role
// in a more informative way than the preflight could.
func roleHasReplication(ctx context.Context, c *pg.Conn, role string) (bool, error) {
	const q = `SELECT rolreplication::text FROM pg_roles WHERE rolname = $1`
	res := c.PgConn().ExecParams(ctx, q, [][]byte{[]byte(role)}, nil, nil, nil).Read()
	if res.Err != nil {
		return false, res.Err
	}
	if len(res.Rows) == 0 {
		return false, nil
	}
	return string(res.Rows[0][0]) == "true", nil
}
