package barmancloud

import (
	"reflect"
	"testing"
)

// barman-cloud-restore only lays down the base backup; the caller
// (CNPG's recovery bootstrap, or an operator) configures
// restore_command, the signal file and any recovery target, and
// PostgreSQL replays every archived segment unless told otherwise. A
// plain native restore instead arms recovery_target='immediate' +
// standby.signal, dropping all WAL archived after the backup and
// colliding with CNPG's own recovery_target_* ("multiple recovery
// targets specified"). --to-latest arms restore_command with no
// target; --to-action promote matches what CNPG always asks for.
func TestRestore_ReplaysAllWAL(t *testing.T) {
	got, rc := runWithStubbedDispatch(t, map[string]string{}, nil, ExecuteRestore,
		[]string{"--cloud-provider", "aws-s3", "s3://bucket/prefix", "discovery", "20260508T101757", "/pgdata"})
	if rc != 0 {
		t.Fatalf("exit %d", rc)
	}
	want := []string{
		"restore", "discovery", "20260508T101757",
		"--target", "/pgdata",
		"--repo", "s3://bucket/prefix",
		"--to-latest", "--to-action", "promote",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %v\nwant %v", got, want)
	}
}
