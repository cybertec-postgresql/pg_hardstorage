package cli

// `partial restore` used to exit 0 whatever it extracted. A scripted
//
//	pg_hardstorage partial restore --tables public.users ... && psql < load.sql
//
// therefore proceeded on an empty target directory whenever the table
// was absent from the backup — which happens after any VACUUM FULL,
// CLUSTER or TRUNCATE between the backup and the restore, because the
// relfilenode is resolved against the live catalog while the files come
// from the backup.
//
// `repo replicate` already carries this decision and documents why:
// "Previously any partial failure exited 0, so a scripted `repo
// replicate && rm source` would delete the source after an incomplete
// copy — a silent loss (data-loss audit round 4 #2)." The body is
// rendered first either way, so monitoring keeps its counters; only the
// exit code changes.

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
)

func TestPartialRestoreIncomplete_CleanRunExitsZero(t *testing.T) {
	res := &partial.RestoreResult{FilesWritten: 3, BytesWritten: 4096}
	if err := partialRestoreIncomplete(res); err != nil {
		t.Errorf("a complete extraction must exit 0: %v", err)
	}
}

func TestPartialRestoreIncomplete_NotInBackupExitsNonZero(t *testing.T) {
	res := &partial.RestoreResult{NotInBackup: []string{"public.users"}}
	err := partialRestoreIncomplete(res)
	if err == nil {
		t.Fatal("a restore that extracted nothing for a requested table exited 0 — a scripted " +
			"run would proceed as though the data were there")
	}
	for _, want := range []string{"public.users", "absent from this backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%v", want, err)
		}
	}
	// The remedy lives in the Suggestion, which is where this codebase
	// puts remedies (see repo replicate's incomplete-destination error).
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected a structured *output.Error, got %T", err)
	}
	if oe.Suggestion == nil {
		t.Fatal("no Suggestion — an operator told their table is missing needs to know why " +
			"and what to do, not just that it is missing")
	}
	for _, want := range []string{"VACUUM FULL", "--relfilenode-map", "older backup"} {
		if !strings.Contains(oe.Suggestion.Human, want) {
			t.Errorf("suggestion does not mention %q:\n%s", want, oe.Suggestion.Human)
		}
	}
}

// NotFound keeps its exit-0 behaviour on purpose:
// TestPartialRestore_NotFoundTable_PropagatesNotFound pins it, and a
// multi-table run that names one typo should still extract the rest.
// Asserted here so the scoping is a decision on the record rather than
// an oversight — if it is ever unified, this test is the one to delete
// deliberately.
func TestPartialRestoreIncomplete_NotFoundAloneKeepsExitZero(t *testing.T) {
	res := &partial.RestoreResult{NotFound: []string{"public.typo"}}
	if err := partialRestoreIncomplete(res); err != nil {
		t.Errorf("a catalog miss alone must keep exit 0 (an existing, tested contract): %v", err)
	}
}

// Both at once must name both, so a multi-table run reports the whole
// picture rather than the first problem found.
// Every absent-from-backup table is named, so a multi-table run reports
// the whole picture rather than the first one found.
func TestPartialRestoreIncomplete_NamesEveryAbsentTable(t *testing.T) {
	res := &partial.RestoreResult{
		NotFound:    []string{"public.typo"},
		NotInBackup: []string{"public.users", "public.orders (toast)"},
	}
	err := partialRestoreIncomplete(res)
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	for _, want := range []string{"public.users", "public.orders (toast)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %q:\n%v", want, err)
		}
	}
}

func TestPartialRestoreIncomplete_NilIsSafe(t *testing.T) {
	if err := partialRestoreIncomplete(nil); err != nil {
		t.Errorf("nil result must not error: %v", err)
	}
}
