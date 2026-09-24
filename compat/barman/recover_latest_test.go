package barman

import (
	"strings"
	"testing"
)

// `barman recover` without a target copies every archived WAL file and
// PostgreSQL replays all of it. The native default stops at the
// backup's consistency point (recovery_target='immediate'), silently
// dropping everything archived after the backup — so the shim must
// ask for --to-latest.
func TestRecoverNoTargetReplaysAllWAL(t *testing.T) {
	got, _, _, err := runShim(t, "recover", "db1", "latest", "/srv/pg17")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !containsArg(got, "--to-latest") {
		t.Fatalf("untargeted recover must pass --to-latest; got %v", got)
	}
}

// Only an explicit target (or --target-immediate) replaces it.
func TestRecoverTargetsDoNotReplayToLatest(t *testing.T) {
	for _, flag := range []string{
		"--target-immediate",
		"--target-time=2026-04-27 09:42:00+02",
		"--target-name=cut",
	} {
		got, _, _, err := runShim(t, "recover", "db1", "latest", "/srv/pg17", flag)
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if containsArg(got, "--to-latest") {
			t.Errorf("%s: must not pass --to-latest; got %v", flag, got)
		}
	}
}

// Barman hands --target-time to PostgreSQL, which reads an offset-less
// literal in the server's TimeZone; native would read UTC and stop at
// a different instant. Refuse instead of guessing.
func TestRecoverOffsetLessTargetTimeRefused(t *testing.T) {
	got, _, _, err := runShim(t, "recover", "db1", "latest", "/srv/pg17",
		"--target-time=2026-04-27 09:42:00")
	if err == nil || !strings.Contains(err.Error(), "UTC offset") {
		t.Fatalf("want offset refusal, got err=%v argv=%v", err, got)
	}
	if got != nil && len(got) != 0 {
		t.Fatalf("refused recover still dispatched %v", got)
	}
}

func containsArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}
