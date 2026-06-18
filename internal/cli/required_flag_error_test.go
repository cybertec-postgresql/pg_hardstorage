package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestEnrichRequiredFlagError pins the contract that makes the
// MarkFlagRequired sweep safe: cobra's required-flag failure is translated
// into the same structured usage.missing_flag error + ExitMisuse that the
// old hand-written "X is required" checks produced, with a "--flag is
// required" message shell scripts and tests already match.
func TestEnrichRequiredFlagError(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	sub := &cobra.Command{Use: "backup"}
	root.AddCommand(sub)

	// Single missing flag → "is required".
	got := enrichRequiredFlagError(sub, errors.New(`required flag(s) "pg-connection" not set`))
	oe, ok := output.AsOutputError(got)
	if !ok || oe.Code != "usage.missing_flag" {
		t.Fatalf("code = %v, want usage.missing_flag", got)
	}
	if !errors.Is(got, output.ErrUsage) {
		t.Errorf("must wrap ErrUsage (→ ExitMisuse); got %v", got)
	}
	if output.ExitCodeFor(got) != output.ExitMisuse {
		t.Errorf("exit = %d, want ExitMisuse", output.ExitCodeFor(got))
	}
	if !strings.Contains(got.Error(), "--pg-connection is required") {
		t.Errorf("message = %q, want '--pg-connection is required'", got.Error())
	}

	// Multiple missing flags → "are required", each named.
	multi := enrichRequiredFlagError(sub, errors.New(`required flag(s) "repo", "pg-connection" not set`))
	for _, want := range []string{"--repo", "--pg-connection", "are required"} {
		if !strings.Contains(multi.Error(), want) {
			t.Errorf("multi message %q missing %q", multi.Error(), want)
		}
	}

	// A non-required-flag error passes through untouched.
	other := errors.New("connect.replication: boom")
	if enrichRequiredFlagError(sub, other) != other {
		t.Error("non-required-flag error must pass through unchanged")
	}
	// nil cmd / nil err are safe.
	if enrichRequiredFlagError(nil, other) != other {
		t.Error("nil cmd should pass error through")
	}
	if enrichRequiredFlagError(sub, nil) != nil {
		t.Error("nil err should stay nil")
	}
}
