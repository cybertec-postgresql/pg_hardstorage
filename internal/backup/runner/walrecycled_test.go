package runner

import (
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// First seen when the soak finally ran backups under sustained write
// load (enterprise_heavy, 16 pgbench writers per cell):
//
//	backup: BASE_BACKUP: pg ERROR [XX000]: requested WAL segment
//	00000001000000020000001A has already been removed
//
// reported as code "internal". PostgreSQL gives this no errcode of its
// own, so SQLSTATE is useless and the message is the only signal.

type fakeServerErr struct{ state, msg string }

func (e fakeServerErr) Error() string    { return "pg ERROR [" + e.state + "]: " + e.msg }
func (e fakeServerErr) SQLState() string { return e.state }

func TestWALRecycledIsNotInternal(t *testing.T) {
	raw := errors.Join(errors.New("backup: BASE_BACKUP"),
		fakeServerErr{"XX000", "requested WAL segment 00000001000000020000001A has already been removed"})

	got := classifySourceError(raw, "db1")
	var oe *output.Error
	if !errors.As(got, &oe) {
		t.Fatalf("not classified: %v", got)
	}
	if oe.Code != "backup.wal_recycled" {
		t.Errorf("code = %q, want backup.wal_recycled", oe.Code)
	}
	if oe.Suggestion == nil || oe.Suggestion.Human == "" {
		t.Error("the whole value of classifying this is the remedy; it must carry one")
	}
	if !errors.Is(got, raw) {
		t.Error("the original error must stay in the chain")
	}
}

// Other XX000 failures must NOT be swept into this class — that would
// hand a real internal fault a confident, wrong diagnosis.
func TestOtherXX000StaysUnclassified(t *testing.T) {
	for _, msg := range []string{
		"unexpected tuple in pg_class",
		"could not read block 7 in file base/5/1259",
		"WAL segment size mismatch",
	} {
		raw := fakeServerErr{"XX000", msg}
		if got := classifySourceError(raw, "db1"); got != error(raw) {
			t.Errorf("%q was classified (%v); only the recycled-WAL message should be", msg, got)
		}
	}
}
