// sourceerror.go — classify failures that come from the SOURCE database
// rather than from pg_hardstorage.
//
// A backup can fail for three quite different reasons, and an operator
// (or an automation keyed on the error code) has to be able to tell
// them apart:
//
//  1. pg_hardstorage is broken,
//  2. the environment is wrong (no repo, bad DSN, no permission),
//  3. THE DATABASE BEING BACKED UP IS DAMAGED.
//
// Only the third means "stop reading this error and go look at your
// production data". It was also the only one with no code of its own:
// a torn page detected by PostgreSQL 18's base-backup checksum
// verification surfaced as
//
//	{"code":"internal","message":"backup: BASE_BACKUP: pg ERROR [XX001]:
//	 checksum verification failure during base backup"}
//
// `internal` is the bucket for "we do not know what this is", so the
// most urgent signal the tool can emit was indistinguishable from a
// bug in the tool. Automation that routes on error codes sent real
// data corruption to whoever triages tool bugs.
//
// PostgreSQL already tells us precisely; we just were not listening.
package runner

import (
	"errors"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// PostgreSQL SQLSTATEs that mean the source cluster's own data is not
// intact (Appendix A, class XX — "Internal Error", which is
// PostgreSQL's name for "the data on disk is wrong").
const (
	sqlstateDataCorrupted  = "XX001" // torn page, failed checksum
	sqlstateIndexCorrupted = "XX002" // damaged index
)

// sqlstateOf walks an error chain for a PostgreSQL SQLSTATE. Both
// pgconn.PgError and our own streaming.ServerError implement
// SQLState(), so the method is the portable way to ask without
// importing either package here.
func sqlstateOf(err error) string {
	type sqlstater interface{ SQLState() string }
	for err != nil {
		if s, ok := err.(sqlstater); ok {
			if code := s.SQLState(); code != "" {
				return code
			}
		}
		err = errors.Unwrap(err)
	}
	return ""
}

// classifySourceError converts a raw BASE_BACKUP failure into a typed
// error when PostgreSQL told us the source data is damaged. Anything
// else is returned unchanged, so callers keep their existing wrapping.
//
// The distinction matters operationally: a source-corruption failure
// is NOT a reason to retry the backup, and it is not a reason to page
// whoever owns the backup tool. It is a reason to look at the
// database.
func classifySourceError(err error, deployment string) error {
	switch sqlstateOf(err) {
	case sqlstateDataCorrupted:
		return output.NewError("source_corruption.data_checksum",
			"backup: PostgreSQL refused to back up "+deployment+
				" because a data page failed its checksum: "+err.Error()).
			WithSuggestion(&output.Suggestion{
				Human: "This is damage in the SOURCE database, not in pg_hardstorage or the repository. " +
					"Do not retry the backup until the page is dealt with: find the relation with the " +
					"failing block from the server log, and treat this as a data-loss incident on the " +
					"primary. Backups already in the repository are unaffected — verify one and keep it. " +
					"(PostgreSQL 18 verifies page checksums during BASE_BACKUP; PG 15-17 do not, so an " +
					"older major may have been copying this page silently.)",
			}).Wrap(err)

	case sqlstateIndexCorrupted:
		return output.NewError("source_corruption.index",
			"backup: PostgreSQL refused to back up "+deployment+
				" because an index is damaged: "+err.Error()).
			WithSuggestion(&output.Suggestion{
				Human: "This is damage in the SOURCE database. REINDEX the affected index on the " +
					"primary, then re-run the backup.",
			}).Wrap(err)
	}
	if isWALRecycledDuringBackup(err) {
		return output.NewError("backup.wal_recycled",
			"backup: PostgreSQL recycled WAL that the backup of "+deployment+
				" still needed before BASE_BACKUP could send it: "+err.Error()).
			WithSuggestion(&output.Suggestion{
				Human: "The write rate outran the WAL the server kept around, and nothing was holding " +
					"it back. This is a sizing condition on the source, not a pg_hardstorage fault and " +
					"not data damage — the backup is incomplete, so do not use it, but retrying is safe. " +
					"pg_hardstorage pins this WAL with a temporary replication slot, so this happens only " +
					"when no slot could be created — look for a backup.wal_slot_unavailable warning (free a " +
					"slot or raise max_replication_slots). Otherwise, raise wal_keep_size to cover the WAL " +
					"written during a backup at peak rate. A running `wal stream` does NOT prevent it: its " +
					"slot holds only WAL the streamer has not consumed, not the backup's own start point.",
			}).Wrap(err)
	}
	return err
}

// isWALRecycledDuringBackup recognises PostgreSQL's
//
//	requested WAL segment 000000010000000200000017 has already been removed
//
// raised mid-BASE_BACKUP when the server has recycled a segment the
// backup still has to include. basebackup.c emits it without an
// errcode, so it arrives as XX000 — the same SQLSTATE as every other
// unclassified internal error — and was reported as code "internal":
// pg_hardstorage's bucket for "we do not know what this is".
//
// We do know. It is the most predictable failure a backup of a busy
// database can have, it has a two-line remedy, and it first appeared
// the first time this project's soak ran backups under sustained
// write load (enterprise_heavy, 16 writers per cell). Matched on the
// message because the SQLSTATE carries no information here.
func isWALRecycledDuringBackup(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "has already been removed") &&
		strings.Contains(err.Error(), "WAL segment")
}
