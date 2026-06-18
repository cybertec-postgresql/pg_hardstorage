package runner

import (
	"strings"
	"testing"
)

// TestSubstitutePlaceholders covers the handful of substitution
// rules cli_run depends on.  We don't unit-test runCLIRun itself
// — that's a real exec.Command shell-out and lives in the scenario
// integration tests (smoke_cli_version.scenario.yaml).
func TestSubstitutePlaceholders(t *testing.T) {
	st := &runState{
		repoURL:     "file:///tmp/repo",
		deployment:  "myapp",
		lastBackup:  "myapp.full.20260509T120000Z.abcd",
		artefactDir: "/tmp/artefacts",
		agentBin:    "/usr/bin/pg_hardstorage",
	}

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{
			name: "no_placeholder",
			in:   "literal-flag-value",
			want: "literal-flag-value",
		},
		{
			name: "single_repo",
			in:   "$REPO",
			want: "file:///tmp/repo",
		},
		{
			name: "embedded_placeholder",
			in:   "--repo=$REPO/sub",
			want: "--repo=file:///tmp/repo/sub",
		},
		{
			name: "multiple",
			in:   "$DEPLOYMENT/$LAST_BACKUP",
			want: "myapp/myapp.full.20260509T120000Z.abcd",
		},
		{
			name:    "unknown_placeholder_typo",
			in:      "$LASTBACKUP", // missing underscore
			wantErr: "unrecognised placeholder",
		},
		{
			name:    "unknown_short_token",
			in:      "$XYZ",
			wantErr: "unrecognised placeholder",
		},
		{
			name: "shell_dollar_one_passes_through",
			// "$1" is not an alpha-prefixed token so the
			// guard ignores it.  Cli_run isn't a shell, but
			// argv tokens that happen to look like "$1"
			// shouldn't trip the typo guard.
			in:   "$1",
			want: "$1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := substitutePlaceholders(tc.in, st)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result %q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q missing substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstitutePlaceholders_EmptyValueErrors(t *testing.T) {
	// $LAST_BACKUP referenced before any take_backup ran is the
	// canonical scenario-authoring mistake.  The substitution
	// guard catches it as an explicit error rather than silently
	// passing an empty string to the CLI (which would surface as
	// a confusing "argument required" or similar).
	st := &runState{
		repoURL:    "file:///tmp/repo",
		deployment: "myapp",
		// lastBackup intentionally empty
	}
	if _, err := substitutePlaceholders("$LAST_BACKUP", st); err == nil {
		t.Fatal("expected error for empty $LAST_BACKUP, got nil")
	} else if !strings.Contains(err.Error(), "$LAST_BACKUP is empty") {
		t.Fatalf("error %q should mention empty $LAST_BACKUP", err)
	}
}

func TestAppendEnv_OverridesBaseKey(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "AWS_REGION=us-west-2"}
	overlay := map[string]string{"AWS_REGION": "eu-central-1", "AWS_ACCESS_KEY_ID": "x"}
	got := appendEnv(base, overlay)

	region := ""
	for _, kv := range got {
		if strings.HasPrefix(kv, "AWS_REGION=") {
			region = kv
		}
	}
	if region != "AWS_REGION=eu-central-1" {
		t.Fatalf("AWS_REGION should be overridden, got %q", region)
	}
	// PATH/HOME should be preserved.
	hasPath, hasHome := false, false
	for _, kv := range got {
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
		if kv == "HOME=/root" {
			hasHome = true
		}
	}
	if !hasPath || !hasHome {
		t.Fatalf("base env not preserved: %v", got)
	}
}
