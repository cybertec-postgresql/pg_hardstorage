package agent

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestVerifyModeFromArg is the regression for bug #53: the documented
// verify_after restore option was never read, so a verify-gated
// restore reported success without verifying. verifyModeFromArg is the
// wiring that turns the arg into a real post-restore gate; Execute
// then runs restore.Verify with the resulting mode and fails the job
// on a verification failure. Here we assert the mapping: a truthy
// verify_after must produce VerifyRequire (a hard gate), never an
// empty/no-op mode.
func TestVerifyModeFromArg(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    restore.VerifyMode
		wantErr bool
	}{
		{"absent", nil, "", false},
		{"bool_true_is_hard_gate", true, restore.VerifyRequire, false},
		{"bool_false_is_noop", false, "", false},
		{"string_require", "require", restore.VerifyRequire, false},
		{"string_auto", "auto", restore.VerifyAuto, false},
		{"string_skip_is_noop", "skip", "", false},
		{"string_empty_is_auto", "", restore.VerifyAuto, false},
		{"string_bad", "sometimes", "", true},
		{"number_rejected", float64(1), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verifyModeFromArg(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("verifyModeFromArg(%v) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyModeFromArg(%v): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("verifyModeFromArg(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
