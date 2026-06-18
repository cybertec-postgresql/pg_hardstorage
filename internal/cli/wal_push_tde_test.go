package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestWalPush_TDE_RefusesWithoutSysIDOrConn confirms the safety
// refusal we added for TDE deployments.  Under --tde we MUST NOT
// fall back to reading xlp_sysid from the segment file (it's
// ciphertext); we MUST also not silently accept "no
// system_identifier" — the manifest stamping needs it.  The
// refusal is the operator-facing signal that their archive_command
// line is incomplete.
//
// Builds a real repo so the function gets past `repo.Open`; the
// segment-path argument doesn't need to exist because the refusal
// fires before any file read.
func TestWalPush_TDE_RefusesWithoutSysIDOrConn(t *testing.T) {
	// Repo.  walsink needs a real repo.Open success before the
	// resolver runs.
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	// Segment path — file doesn't need to exist; the refusal fires
	// before readSegmentFile runs (the resolver is upstream of the
	// read).  Use a name that LOOKS like a segment so the basename
	// parser doesn't reject the input before the resolver.
	segPath := filepath.Join(t.TempDir(), "000000010000000000000001")
	// touch the file so the repo-open + parse paths can proceed
	// past their existence checks.  Empty body is fine because the
	// refusal fires before any actual content is read.
	if err := os.WriteFile(segPath, []byte{}, 0o644); err != nil {
		t.Fatalf("touch segment: %v", err)
	}

	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
	cmd.SetContext(WithDispatcher(context.Background(), d))

	err := runWalPush(cmd, walPushOptions{
		deployment:  "tde-test",
		segmentPath: segPath,
		repoURL:     repoURL,
		// pgConn intentionally empty.
		// systemID intentionally empty.
		tde: true,
	})
	if err == nil {
		t.Fatal("expected refusal; got nil error")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error; got %T: %v", err, err)
	}
	if oe.Code != "usage.missing_flag" {
		t.Errorf("error code = %q, want %q", oe.Code, "usage.missing_flag")
	}
	if !strings.Contains(oe.Message, "TDE") {
		t.Errorf("error message should mention TDE; got %q", oe.Message)
	}
	if oe.Suggestion == nil ||
		!strings.Contains(oe.Suggestion.Human, "system-identifier") {
		t.Errorf("suggestion should point at --system-identifier; got %+v", oe.Suggestion)
	}
}

// TestWalPush_TDE_AcceptsExplicitSysID confirms the happy path
// under --tde: when --system-identifier is supplied, the refusal
// MUST NOT fire (rule 1 of the precedence applies and there's no
// need to peek at the segment file).
//
// This test doesn't drive a full push — that's covered by the
// non-TDE integration tests; here we only assert that the early
// refusal doesn't trip when the operator supplied the data
// directly.  We make the function fail LATER (on the actual
// segment read) and assert the failure mode is "segment read
// failed", NOT "TDE refusal" — that's the contract proof.
func TestWalPush_TDE_AcceptsExplicitSysID(t *testing.T) {
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	segPath := filepath.Join(t.TempDir(), "000000010000000000000001")
	// Empty file → walsink push will fail at the segment-size check
	// downstream (wants exactly 16 MiB).  That's a downstream
	// failure, NOT the TDE refusal, which is what we're asserting
	// HERE doesn't fire.
	if err := os.WriteFile(segPath, []byte{}, 0o644); err != nil {
		t.Fatalf("touch segment: %v", err)
	}

	cmd := &cobra.Command{}
	var stdout2, stderr2 bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout2, &stderr2)
	cmd.SetContext(WithDispatcher(context.Background(), d))

	err := runWalPush(cmd, walPushOptions{
		deployment:  "tde-test",
		segmentPath: segPath,
		repoURL:     repoURL,
		systemID:    "7388123456789012345",
		tde:         true,
	})
	if err == nil {
		// Empty-file segment would normally fail downstream.  If
		// somehow it succeeded, the test still proves the TDE
		// refusal didn't trip; treat as pass.
		return
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		if oe.Code == "usage.missing_flag" && strings.Contains(oe.Message, "TDE") {
			t.Fatalf("TDE refusal fired despite --system-identifier being set: %v", err)
		}
	}
	// Any other error (segment size, parse, etc.) is acceptable —
	// the refusal we wanted to prove ABSENT was the TDE one.
}
