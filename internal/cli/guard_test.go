package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func guardCmd() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	sub := &cobra.Command{Use: "backup"}
	sub.Flags().String("pg-connection", "", "")
	sub.Flags().String("repo", "", "")
	root.AddCommand(sub)
	return sub
}

// TestRequireFlags is the contract for the conditional-requirement guard:
// same structured usage.missing_flag error + ExitMisuse as MarkFlagRequired,
// reporting every missing flag, and nil once all are set.
func TestRequireFlags(t *testing.T) {
	sub := guardCmd()

	// Both empty → both named, "are required", usage.missing_flag, ExitMisuse.
	err := requireFlags(sub, "pg-connection", "repo")
	oe, ok := output.AsOutputError(err)
	if !ok || oe.Code != "usage.missing_flag" {
		t.Fatalf("code = %v, want usage.missing_flag", err)
	}
	if output.ExitCodeFor(err) != output.ExitMisuse {
		t.Errorf("exit = %d, want ExitMisuse", output.ExitCodeFor(err))
	}
	if !strings.Contains(err.Error(), "--pg-connection, --repo are required") {
		t.Errorf("message = %q, want both flags + 'are required'", err.Error())
	}

	// One set → only the missing one, singular "is required".
	_ = sub.Flags().Set("repo", "file:///r")
	err = requireFlags(sub, "pg-connection", "repo")
	if err == nil || !strings.Contains(err.Error(), "--pg-connection is required") {
		t.Errorf("want only --pg-connection flagged, got: %v", err)
	}
	if strings.Contains(err.Error(), "--repo") {
		t.Errorf("a set flag must not be reported: %v", err)
	}

	// All set → nil.
	_ = sub.Flags().Set("pg-connection", "postgres://x")
	if err := requireFlags(sub, "pg-connection", "repo"); err != nil {
		t.Errorf("all flags set should pass, got: %v", err)
	}

	// Unknown flag name is reported (defensive: programmer typo surfaces).
	if err := requireFlags(sub, "nope"); err == nil || !strings.Contains(err.Error(), "--nope") {
		t.Errorf("unknown flag should surface, got: %v", err)
	}
}

// TestMissingFlagErr: the positional-or-flag path uses a rich label verbatim,
// scoped to the command path, with the same code + exit.
func TestMissingFlagErr(t *testing.T) {
	sub := guardCmd()
	err := missingFlagErr(sub, "--repo (or the first positional <url>)")
	if !strings.Contains(err.Error(), "backup: --repo (or the first positional <url>) is required") {
		t.Errorf("unexpected message: %q", err.Error())
	}
	if !errors.Is(err, output.ErrUsage) {
		t.Errorf("must wrap ErrUsage; got %v", err)
	}
}
