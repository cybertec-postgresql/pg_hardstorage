package cli

// wal_fetch_sysid_test.go — the restore-side cluster-identity check.
// See wal_fetch_sysid.go: the write-side guards (`wal stream`,
// `wal push`) refuse identity changes at archive time, but an archive
// that already mixes lineages (a pre-guard mix, a reused deployment
// name, --allow-system-identifier-change) reaches recovery — and PG's
// own xlp_sysid FATAL comes mid-replay, cryptic and expensive.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestCheckFetchSystemIdentifier_MismatchRefuses(t *testing.T) {
	err := checkFetchSystemIdentifier("7000000000000000001", "7999999999999999999",
		"000000010000000000000005")
	if err == nil {
		t.Fatal("a foreign segment was handed to recovery.\n\n" +
			"PG will notice — mid-replay, after the restore's wallclock is spent, with a " +
			"FATAL that names neither the deployment nor the repair. The typed refusal " +
			"here (plus the strict restore_command tail) aborts at the FIRST foreign byte.")
	}
	if !strings.Contains(err.Error(), "system_identifier_mismatch") {
		t.Errorf("wrong code: %v", err)
	}
	if oe, ok := output.AsOutputError(err); !ok || oe.Suggestion == nil ||
		!strings.Contains(oe.Suggestion.Human, "wal audit") {
		t.Errorf("refusal's suggestion does not point at the audit: %v", err)
	}
}

func TestCheckFetchSystemIdentifier_MatchAndUnknownPass(t *testing.T) {
	for _, c := range []struct{ expect, got string }{
		{"7000000000000000001", "7000000000000000001"}, // match
		{"", "7000000000000000001"},                    // no expectation (old restore_command)
		{"7000000000000000001", ""},                    // pre-schema manifest without identity
	} {
		if err := checkFetchSystemIdentifier(c.expect, c.got, "seg"); err != nil {
			t.Errorf("expect=%q got=%q refused: %v — empty sides are no-ops by design "+
				"(upgrades must not start refusing)", c.expect, c.got, err)
		}
	}
}
