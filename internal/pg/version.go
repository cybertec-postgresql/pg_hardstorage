// version.go — Version: parsed server_version with major/minor/raw retained for vendor-build audit.
package pg

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Version describes the PostgreSQL server version we're talking to.
//
// Major is the user-visible release (15, 16, 17, …). Minor is the
// patch-level release. Raw is the full server_version string,
// preserved verbatim because vendor builds (RDS, Aurora, Cloud SQL)
// often add suffixes we want to surface in audit logs.
type Version struct {
	Major int
	Minor int
	Raw   string
}

// String returns "PostgreSQL <major>.<minor>" plus the raw vendor string
// when it's non-trivial (i.e. has more than the bare version).
func (v Version) String() string {
	return fmt.Sprintf("PostgreSQL %d.%d (%s)", v.Major, v.Minor, v.Raw)
}

// AtLeast reports whether v is at or above the (major, minor) release.
// Useful for guarding feature use ("incremental backups need PG 17+").
func (v Version) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// MinSupportedMajor and MaxSupportedMajor bracket the PostgreSQL
// major versions pg_hardstorage explicitly supports + tests
// against. We promise wire-protocol + manifest-format
// compatibility within this window; outside it the binary still
// connects (the replication / SQL protocols are stable across
// majors), but feature-level tests aren't part of the matrix.
//
// PG 15 is the floor because the SPEC explicitly targets PG 15+
// (non-exclusive `pg_backup_start/stop`, `archive_library`).
// PG 18 is the ceiling because that's the current upstream
// stable release as of+; bumping the ceiling is one
// constant edit + a matrix-row add.
const (
	MinSupportedMajor     = 15
	MaxSupportedMajor     = 18
	DefaultSandboxMajor   = 18 // pulled when a manifest's pg_version is unset / unreadable
	IncrementalMinMajor   = 17 // PG 17 introduced summarize_wal + BASE_BACKUP INCREMENTAL
	CombineBackupMinMajor = 17 // pg_combinebackup is a PG 17+ tool
)

// SupportedMajors returns the list of major versions in the
// declared support window, in ascending order. Used by the
// `doctor` PG-version probe + the test-matrix expander to
// ensure the in-tree "we test against these" set matches the
// tool's published support window.
func SupportedMajors() []int {
	out := make([]int, 0, MaxSupportedMajor-MinSupportedMajor+1)
	for m := MinSupportedMajor; m <= MaxSupportedMajor; m++ {
		out = append(out, m)
	}
	return out
}

// IsSupportedMajor reports whether the given major is in the
// declared support window. Out-of-window majors get a Notice-
// level "untested" warning; the binary doesn't refuse — the
// wire protocol is stable.
func IsSupportedMajor(major int) bool {
	return major >= MinSupportedMajor && major <= MaxSupportedMajor
}

// probeVersionQuery is the exact SQL the version probe runs.  It is a
// named constant (rather than an inline literal) so a regression test
// can pin it: the GUC MUST be `server_version`, never the non-existent
// `server_version_full` (issues #5, #95) nor the integer-only
// `server_version_num` (which lacks the vendor suffix we audit).
const probeVersionQuery = "SHOW server_version"

// QueryVersion runs `SHOW server_version` against c and parses the
// result. The connection MUST be in ModeRegular — replication-mode
// connections cannot run SHOW.
//
// We probe `server_version` (not `server_version_num`) because the
// string form already carries the vendor suffix on packaged
// distributions ("17.2 (Debian 17.2-1.pgdg120+1)") which is what we
// want to surface in audit logs.  An earlier draft of this code
// asked for `server_version_full` — which does not exist as a GUC
// in any PostgreSQL release; the 0.9.0 release still carried it, so a
// 0.9.0 RPM fails every probe with `unrecognized configuration
// parameter "server_version_full"` (issue #95).  Fixed in response to
// issue #5; the regression guard lives in version_internal_test.go.
func QueryVersion(ctx context.Context, c *Conn) (Version, error) {
	if c == nil || c.pg == nil {
		return Version{}, errors.New("pg: nil connection")
	}
	if c.mode != ModeRegular {
		return Version{}, output.NewError("usage.wrong_mode",
			"version probe requires ModeRegular; got "+c.mode.String()).Wrap(output.ErrUsage)
	}

	res := c.pg.ExecParams(ctx, probeVersionQuery, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return Version{}, fmt.Errorf("pg: SHOW server_version: %w", res.Err)
	}
	// SHOW returns one row, one column.
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return Version{}, errors.New("pg: empty server_version result")
	}
	raw := string(res.Rows[0][0])
	v, err := ParseVersion(raw)
	if err != nil {
		return Version{}, fmt.Errorf("pg: parse server_version %q: %w", raw, err)
	}
	return v, nil
}

// versionRE captures the leading "MAJOR" and optional ".MINOR" of a
// server_version string. PG 10+ uses a single major number;
// "17.2 (Debian 17.2-1.pgdg120+1)" is the typical RDS-style shape.
var versionRE = regexp.MustCompile(`^(\d+)(?:\.(\d+))?`)

// ParseVersion parses a server_version string into Major / Minor.
//
// PostgreSQL versions since 10 are single-number majors with a minor
// patch level: "17.2", "16.5", "15.10". The full string may carry
// vendor suffixes (Debian, RDS, etc.) which we preserve in Raw and
// ignore for parsing.
//
// Returns an error if the prefix doesn't look like a version.
func ParseVersion(raw string) (Version, error) {
	m := versionRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("no version prefix in %q", raw)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, fmt.Errorf("parse major: %w", err)
	}
	minor := 0
	if m[2] != "" {
		minor, err = strconv.Atoi(m[2])
		if err != nil {
			return Version{}, fmt.Errorf("parse minor: %w", err)
		}
	}
	return Version{Major: major, Minor: minor, Raw: raw}, nil
}
