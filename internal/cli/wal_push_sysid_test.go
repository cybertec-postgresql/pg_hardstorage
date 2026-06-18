package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

func TestValidSystemIdentifier(t *testing.T) {
	good := []string{"1", "7388123456789012345", "18446744073709551615"} // up to max uint64
	for _, s := range good {
		if !validSystemIdentifier(s) {
			t.Errorf("validSystemIdentifier(%q) = false; want true", s)
		}
	}
	bad := []string{
		"0",                    // no real cluster reports 0; header path refuses it too
		"deadbeef",             // hex letters
		"0x1234",               // hex prefix
		"-1",                   // signed
		"12.5",                 // not an integer
		"12 34",                // embedded space
		"18446744073709551616", // overflows uint64
		"  7",                  // leading space
		"abc",
	}
	for _, s := range bad {
		if validSystemIdentifier(s) {
			t.Errorf("validSystemIdentifier(%q) = true; want false", s)
		}
	}
}

// TestWalPush_RejectsMalformedSystemIdentifier pins that a non-decimal
// --system-identifier is refused up front with usage.bad_system_identifier
// — before the repo round-trip and before it can be stamped onto a
// manifest (where it would later trip a spurious split-brain mismatch
// against a header-derived push).
func TestWalPush_RejectsMalformedSystemIdentifier(t *testing.T) {
	for _, badID := range []string{"deadbeef", "0", "0x10", "-5"} {
		t.Run(badID, func(t *testing.T) {
			cmd := &cobra.Command{}
			var stdout, stderr bytes.Buffer
			d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
			cmd.SetContext(WithDispatcher(context.Background(), d))

			// repoURL is non-empty so the only-other-earlier check
			// passes; the sysid gate fires before repo.Open, so the
			// URL never has to resolve.
			err := runWalPush(cmd, walPushOptions{
				deployment:  "db1",
				segmentPath: "/tmp/000000010000000000000001",
				repoURL:     "file:///nonexistent-repo",
				systemID:    badID,
			})
			if err == nil {
				t.Fatalf("expected refusal for --system-identifier %q", badID)
			}
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != "usage.bad_system_identifier" {
				t.Fatalf("want usage.bad_system_identifier; got %v", err)
			}
		})
	}
}
