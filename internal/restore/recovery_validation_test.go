// recovery_validation_test.go — table-driven coverage for
// validateRecovery and reachability-gate edge cases not pinned by
// the higher-level WriteRecoveryFiles / CheckTargetReachable tests.
//
// validateRecovery is the last-line defence between a CLI-built
// Recovery and the GUC-rendering code that writes
// postgresql.auto.conf.  Any branch of it that escapes test
// coverage is a branch where a malformed Recovery could quietly
// produce a broken recovery.conf — the exact failure mode #99
// surfaced, just one layer up the stack.
package restore

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestValidateRecovery_TableDriven covers every Validate branch in
// one place so a future invariant addition shows up immediately if
// it's not table-rowed.
func TestValidateRecovery_TableDriven(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		r       Recovery
		wantErr string // substring; empty = expect nil
	}{
		{
			name:    "happy LSN target",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3000028", Action: "promote", Timeline: "latest"},
			wantErr: "",
		},
		{
			name:    "happy time target",
			r:       Recovery{RestoreCommand: "x", TargetTime: now, Action: "pause"},
			wantErr: "",
		},
		{
			name:    "happy name target",
			r:       Recovery{RestoreCommand: "x", TargetName: "before_drop", Action: "shutdown"},
			wantErr: "",
		},
		{
			name:    "missing RestoreCommand",
			r:       Recovery{TargetLSN: "0/3000028"},
			wantErr: "RestoreCommand is required",
		},
		{
			name:    "two targets: LSN + time",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3000028", TargetTime: now},
			wantErr: "at most one of TargetLSN, TargetTime, TargetName",
		},
		{
			name:    "two targets: LSN + name",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3000028", TargetName: "y"},
			wantErr: "at most one of TargetLSN, TargetTime, TargetName",
		},
		{
			name:    "two targets: time + name",
			r:       Recovery{RestoreCommand: "x", TargetTime: now, TargetName: "y"},
			wantErr: "at most one of TargetLSN, TargetTime, TargetName",
		},
		{
			name:    "three targets",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", TargetTime: now, TargetName: "y"},
			wantErr: "at most one of TargetLSN, TargetTime, TargetName",
		},
		{
			name:    "standby + LSN target",
			r:       Recovery{RestoreCommand: "x", StandbyMode: true, TargetLSN: "0/3000028"},
			wantErr: "StandbyMode is mutually exclusive",
		},
		{
			name:    "standby + time target",
			r:       Recovery{RestoreCommand: "x", StandbyMode: true, TargetTime: now},
			wantErr: "StandbyMode is mutually exclusive",
		},
		{
			name:    "standby + name target",
			r:       Recovery{RestoreCommand: "x", StandbyMode: true, TargetName: "x"},
			wantErr: "StandbyMode is mutually exclusive",
		},
		{
			name:    "standby alone is OK",
			r:       Recovery{RestoreCommand: "x", StandbyMode: true},
			wantErr: "",
		},
		{
			name:    "bad action enum",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Action: "burn-it-down"},
			wantErr: "Action",
		},
		{
			name:    "empty action defaults at render time (validate accepts)",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Action: ""},
			wantErr: "",
		},
		{
			name:    "malformed TargetLSN",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "garbage"},
			wantErr: "TargetLSN",
		},
		{
			name:    "bad timeline string",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Timeline: "foo"},
			wantErr: "Timeline",
		},
		{
			name:    "timeline zero rejected",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Timeline: "0"},
			wantErr: "Timeline",
		},
		{
			name:    "timeline positive int OK",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Timeline: "42"},
			wantErr: "",
		},
		{
			name:    "timeline empty (renders 'latest' default)",
			r:       Recovery{RestoreCommand: "x", TargetLSN: "0/3", Timeline: ""},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRecovery(tc.r)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v; want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestCheckTargetReachable_InclusiveZeroValue documents the
// programmatic-API trap: a caller building Recovery{TargetLSN: X}
// without explicitly setting Inclusive gets Go's zero value
// (false), which my gate treats as exclusive semantics — stricter
// than PG's actual default (inclusive=true).
//
// The CLI's buildRecovery always sets Inclusive=true|false
// explicitly from --to-exclusive, so this only bites direct
// programmatic users of the restore package.  This test pins the
// current behaviour so anyone refactoring sees the trap and
// either makes it API-explicit (Mode enum / *bool) or normalises
// the default inside CheckTargetReachable.
func TestCheckTargetReachable_InclusiveZeroValue(t *testing.T) {
	// Zero-value Inclusive at the equality boundary refuses
	// (treated as exclusive).  If a future change makes the gate
	// default-to-inclusive, this test needs updating — and the
	// CLI's buildRecovery behaviour should be reviewed in the
	// same pass to keep the layers consistent.
	r := &Recovery{TargetLSN: "0/3000028" /* Inclusive: unset → false */}
	if err := CheckTargetReachable("0/3000028", r); err == nil {
		t.Error("zero-value Inclusive at equality currently treated as exclusive (refuses); " +
			"if you're updating the default, also update buildRecovery + this test together")
	}
}

// TestCheckTargetReachable_SkipGapCheck_DoesNotBypass: the
// reachability gate is independent of the WAL-gap pre-flight.
// --skip-gap-check is the operator's "I've validated WAL gaps
// some other way" override — it must NOT silence the reachability
// refusal, which is a logical-vs-physical impossibility, not an
// operator-tolerable risk.
func TestCheckTargetReachable_SkipGapCheck_DoesNotBypass(t *testing.T) {
	r := &Recovery{TargetLSN: "0/3000000", Inclusive: true, SkipGapCheck: true}
	err := CheckTargetReachable("0/30001A0", r)
	if err == nil {
		t.Error("SkipGapCheck must not bypass reachability — gap-check and reachability are independent")
	}
}

// TestCheckTargetReachable_StandbyMode_NotGated: standby mode
// has no stop point, so reachability doesn't apply.  Pre-existing
// validateRecovery enforces standby + target mutual exclusion;
// CheckTargetReachable should pass cleanly for a standby Recovery
// regardless of stop_lsn.
func TestCheckTargetReachable_StandbyMode_NotGated(t *testing.T) {
	r := &Recovery{StandbyMode: true}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("standby mode shouldn't trip reachability: %v", err)
	}
}

// TestCheckTargetReachable_NilTarget_NotGated: the CLI's
// buildRecovery returns nil when no PITR flag is set; downstream
// passes that nil to CheckTargetReachable.  Must be a no-op.
func TestCheckTargetReachable_NilTarget_NotGated(t *testing.T) {
	if err := CheckTargetReachable("0/3000028", nil); err != nil {
		t.Errorf("nil Recovery must pass: %v", err)
	}
}

// TestCheckTargetReachable_LowercaseLSN: pglogrepl accepts both
// upper and lower hex.  A user-supplied lowercase --to-lsn must
// compare against an uppercase stop_lsn correctly (and vice
// versa).  Without this test, a future "normalise to upper before
// compare" refactor that's bug-free in upper-vs-upper might
// silently break lower-vs-upper or vice versa.
func TestCheckTargetReachable_LowercaseLSN(t *testing.T) {
	r := &Recovery{TargetLSN: "0/40000a0", Inclusive: true} // lowercase
	if err := CheckTargetReachable("0/30001A0" /* uppercase */, r); err != nil {
		t.Errorf("lowercase target after uppercase stop should pass: %v", err)
	}
	r = &Recovery{TargetLSN: "0/300010a", Inclusive: true} // lowercase, before uppercase stop
	if err := CheckTargetReachable("0/30001A0", r); err == nil {
		t.Error("lowercase target before uppercase stop must refuse")
	}
}

// TestCheckTargetReachable_ErrorSuggestion: the structured error
// must carry an operator-actionable suggestion.  Without this, a
// future refactor that drops the Suggestion (e.g. while
// simplifying the error constructor) silently degrades the
// 3am-on-call experience.
func TestCheckTargetReachable_ErrorCarriesSuggestion(t *testing.T) {
	r := &Recovery{TargetLSN: "0/3000000", Inclusive: true}
	err := CheckTargetReachable("0/30001A0", r)
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *output.Error
	if !errAs(err, &ce) {
		t.Fatalf("not a structured error: %T %v", err, err)
	}
	if ce.Suggestion == nil || ce.Suggestion.Human == "" {
		t.Errorf("error must carry an operator suggestion: %+v", ce)
	}
	// The suggestion should mention both recovery paths (later
	// LSN, earlier backup) — operators reach for either depending
	// on which fact is fixed.
	for _, want := range []string{"earlier backup", "stop_lsn"} {
		if !strings.Contains(ce.Suggestion.Human, want) {
			t.Errorf("suggestion missing %q: %q", want, ce.Suggestion.Human)
		}
	}
}

// errAs is a tiny errors.As wrapper that keeps the test file's
// imports minimal — errors.As needs the &target pattern and
// importing "errors" just for it would crowd the package header.
func errAs(err error, target any) bool {
	type asErr interface{ Unwrap() error }
	for err != nil {
		if ce, ok := target.(**output.Error); ok {
			if oe, ok := err.(*output.Error); ok {
				*ce = oe
				return true
			}
		}
		u, ok := err.(asErr)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
