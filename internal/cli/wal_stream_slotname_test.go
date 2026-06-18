package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

func TestSlotNameCharsSafe(t *testing.T) {
	good := []string{
		"pg_hardstorage_db1",
		"slot",
		"1abc",                  // digit-start: PG allows it, so we must too
		"UPPER_case_99",         // we allow uppercase; PG decides
		strings.Repeat("a", 63), // max length
	}
	for _, s := range good {
		if !slotNameCharsSafe(s) {
			t.Errorf("slotNameCharsSafe(%q) = false; want true", s)
		}
	}
	bad := []string{
		"",                      // empty
		strings.Repeat("a", 64), // too long
		"evil; DROP",            // semicolon + space — injection surface
		"slot name",             // space
		"slot'--",               // quote
		"a\tb",                  // control char
		"a\nb",                  // newline
		"sl/ot",                 // slash
		`back\slash`,            // backslash
		"pg_hardstorage_db1.x",  // dot (from a deployment like "db1.x")
		"x`whoami`",             // backtick
	}
	for _, s := range bad {
		if slotNameCharsSafe(s) {
			t.Errorf("slotNameCharsSafe(%q) = true; want false", s)
		}
	}
}

// TestWalStream_RejectsMalformedSlotName pins that an explicit --slot
// carrying injection-surface characters is refused at the CLI boundary
// (usage.bad_slot_name) — before it reaches the unquoted
// START_REPLICATION / CREATE_REPLICATION_SLOT protocol commands.
func TestWalStream_RejectsMalformedSlotName(t *testing.T) {
	for _, badSlot := range []string{"evil; DROP", "slot name", "x'--", "a/b"} {
		t.Run(badSlot, func(t *testing.T) {
			cmd := &cobra.Command{}
			var stdout, stderr bytes.Buffer
			d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
			cmd.SetContext(WithDispatcher(context.Background(), d))

			err := runWalStream(cmd, walStreamOptions{
				deployment: "db1",
				slotName:   badSlot,
				repoURL:    "file:///nonexistent-repo", // slot gate fires before repo open
				pgConn:     "postgres://localhost/db1", // satisfy the earlier --pg-connection gate
			})
			if err == nil {
				t.Fatalf("expected refusal for --slot %q", badSlot)
			}
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != "usage.bad_slot_name" {
				t.Fatalf("want usage.bad_slot_name; got %v", err)
			}
		})
	}
}
