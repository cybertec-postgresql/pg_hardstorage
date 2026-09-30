// archivewait.go — keeping BASE_BACKUP alive while pg_backup_stop waits for WAL archiving.
package basebackup

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// ErrArchiveLag is returned (wrapped) when the stream went silent while
// PostgreSQL's pg_backup_stop was waiting for WAL to be archived: the
// backup did not hang, archive_command is failing or far behind.
var ErrArchiveLag = errors.New("basebackup: pg_backup_stop is waiting for WAL archiving")

// archiveWaitRE matches PG's do_pg_backup_stop warning (xlogfuncs /
// xlog.c): "still waiting for all required WAL segments to be archived
// (%d seconds elapsed)". PG emits it at 60 s and then at doubling
// intervals (120 s, 240 s, ...) with nothing else on the wire in
// between, so a fixed inactivity window (90 s default) aborted every
// backup whose archiving was merely slow — before this, as an opaque
// "inactivity timeout".
var archiveWaitRE = regexp.MustCompile(`still waiting for all required WAL segments to be archived \((\d+) seconds elapsed\)`)

// archiveWait tracks the pg_backup_stop archive-wait warnings of one run.
type archiveWait struct {
	elapsed atomic.Int64 // seconds PG reported in the latest warning; 0 = none seen
}

// observe is the reader's OnNotice hook. On an archive-wait warning it
// widens the inactivity window so the NEXT warning (due `elapsed`
// seconds later, since PG doubles) still lands inside it, plus the base
// window as margin. A dead server is still detected — just later.
func (a *archiveWait) observe(n *pgproto3.NoticeResponse, r *streaming.Reader, base time.Duration) {
	secs, ok := archiveWaitSeconds(n)
	if !ok {
		return
	}
	a.elapsed.Store(int64(secs))
	if r != nil {
		r.SetInactivityTimeout(archiveWaitTimeout(base, secs))
	}
}

// archiveWaitSeconds extracts the elapsed seconds from an archive-wait
// warning; ok=false for any other notice.
func archiveWaitSeconds(n *pgproto3.NoticeResponse) (int, bool) {
	if n == nil || !strings.EqualFold(n.Severity, "WARNING") {
		return 0, false
	}
	m := archiveWaitRE.FindStringSubmatch(n.Message)
	if m == nil {
		return 0, false
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil || secs <= 0 {
		return 0, false
	}
	return secs, true
}

// archiveWaitTimeout is the inactivity window after a warning at
// `secs` elapsed: the next one is due `secs` later.
func archiveWaitTimeout(base time.Duration, secs int) time.Duration {
	return base + time.Duration(secs)*time.Second
}

// explain turns an inactivity timeout that followed archive-wait
// warnings into ErrArchiveLag with the actual cause and the fix.
func (a *archiveWait) explain(err error) error {
	if err == nil || !errors.Is(err, streaming.ErrInactivityTimeout) {
		return err
	}
	secs := a.elapsed.Load()
	if secs == 0 {
		return err
	}
	return fmt.Errorf("%w (PostgreSQL reported %d s elapsed) — archive_command is failing or far behind; "+
		"check pg_stat_archiver (failed_count, last_failed_wal) and the server log, fix archiving, then retry: %w",
		ErrArchiveLag, secs, err)
}
