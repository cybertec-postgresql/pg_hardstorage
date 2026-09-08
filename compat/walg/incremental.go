// incremental.go — decide whether `backup-push` can use the PG17
// incremental protocol, or must fall back to a full backup.
//
// WAL-G's `backup-push` is delta-by-default and works against every
// PostgreSQL version, because wal-g's delta scheme is tool-level: it
// diffs pages itself and never asks the server for anything. The shim
// mapped that default onto pg_hardstorage's `--incremental-from`,
// which is the PostgreSQL 17 incremental-backup protocol and carries
// two server-side prerequisites wal-g never had:
//
//   - PostgreSQL 17 or newer (PG15/16 fail outright with
//     backup.incremental_unsupported), and
//   - summarize_wal = on, which is OFF by default even on PG17
//     (PG answers 55000: "incremental backups cannot be taken unless
//     WAL summarization is enabled").
//
// So the single most canonical WAL-G command — `wal-g backup-push
// $PGDATA`, no flags — failed out of the box for every PG15/16 user
// and for every PG17 user who had not turned summarization on. It
// failed on the FIRST push into a fresh repository too: the
// "no prior full, promote to full" path never engaged, because the
// refusal comes from PostgreSQL before the repository is consulted.
//
// Rather than let the operator discover that from a failed backup,
// probe the server once and choose. A full backup is always correct —
// deduplication against what is already in the repository means the
// stored cost of a "full" here is close to the incremental one — so
// falling back costs the operator very little, and saying nothing
// would have cost them the backup.
package walg

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// incrementalProbeTimeout bounds the capability probe. backup-push is
// invoked from cron; a black-holed host must fail on the backup
// itself, with the backup's own error, rather than hang here.
const incrementalProbeTimeout = 10 * time.Second

// minIncrementalPGVersion is the first major that has the incremental
// protocol (pg_combinebackup / WAL summarization).
const minIncrementalPGVersion = 170000

// canUseIncremental reports whether the deployment's PostgreSQL can
// serve an incremental backup right now, plus a human reason when it
// cannot.
//
// A probe that cannot reach the server returns true: the connection
// is about to be made again by the backup itself, and the backup's
// own error is a better one to show than a speculative downgrade.
//
// Split out as a variable so tests can drive both branches without a
// live PostgreSQL.
var canUseIncremental = func(ctx context.Context, dsn string) (ok bool, reason string) {
	if strings.TrimSpace(dsn) == "" {
		// No DSN to probe. The backup will fail on its own terms.
		return true, ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, incrementalProbeTimeout)
	defer cancel()

	conn, err := pg.Connect(probeCtx, dsn, pg.ModeRegular)
	if err != nil {
		return true, ""
	}
	defer func() { _ = conn.PgConn().Close(probeCtx) }()

	// server_version_num is a parameter status, so it costs no query.
	raw := conn.PgConn().ParameterStatus("server_version_num")
	if raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil && n < minIncrementalPGVersion {
			return false, fmt.Sprintf("PostgreSQL %s does not have incremental backups (they need PG 17+)",
				conn.PgConn().ParameterStatus("server_version"))
		}
	}

	// summarize_wal is off by default even on PG17, and without it
	// PostgreSQL refuses the incremental with SQLSTATE 55000.
	res := conn.PgConn().ExecParams(probeCtx, "SHOW summarize_wal", nil, nil, nil, nil).Read()
	if res.Err != nil {
		// Unknown parameter (pre-17) or no permission: treat as "not
		// available" only when we already know the version is old;
		// otherwise let the backup speak for itself.
		return true, ""
	}
	if len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
		if v := strings.ToLower(string(res.Rows[0][0])); v != "on" {
			return false, "summarize_wal is " + v + " on the server (incremental backups need summarize_wal = on)"
		}
	}
	return true, ""
}

// resolveIncremental decides the backup shape for a `backup-push`
// that did not pass --full, and warns when it had to downgrade.
func resolveIncremental(ctx context.Context, stderr io.Writer, dsn string) (useIncremental bool) {
	ok, reason := canUseIncremental(ctx, dsn)
	if ok {
		return true
	}
	fmt.Fprintf(stderr,
		"pg-hardstorage-walg: backup-push: taking a FULL backup instead of a delta — %s.\n"+
			"pg-hardstorage-walg: backup-push: this is not a downgrade in practice: every backup is "+
			"deduplicated against the repository, so a full stores about what a delta would. "+
			"To get PG-native incrementals, run PostgreSQL 17+ with summarize_wal = on.\n",
		reason)
	return false
}

// pgConnectionFrom extracts the --pg-connection value the arg mapper
// produced, so the probe uses exactly the DSN the backup will.
func pgConnectionFrom(native []string) string {
	for i := 0; i+1 < len(native); i++ {
		if native[i] == "--pg-connection" {
			return native[i+1]
		}
	}
	return ""
}
