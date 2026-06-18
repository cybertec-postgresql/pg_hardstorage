package pgbackrest

import (
	"reflect"
	"strings"
	"testing"
)

// captureDispatch swaps dispatchNative for a recorder that
// stores the args and returns 0.  Tests defer the restore.
func captureDispatch(t *testing.T) *[]string {
	t.Helper()
	captured := []string{}
	prev := dispatchNative
	dispatchNative = func(a []string) int {
		captured = a
		return 0
	}
	t.Cleanup(func() { dispatchNative = prev })
	t.Cleanup(resetGlobalArgs)
	resetGlobalArgs()
	return &captured
}

func TestBackup_Full_Default(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{
		stanza:     "db1",
		pg1Host:    "db.example.com",
		pg1Port:    5432,
		pg1User:    "pgbackup",
		repo1Type:  "posix",
		repo1Path:  "/var/lib/pgbackrest",
		backupType: "full",
	}
	if err := runBackup(globalArgs); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"backup", "db1",
		"--pg-connection", "postgres://pgbackup@db.example.com:5432/postgres",
		"--repo", "file:///var/lib/pgbackrest",
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("args:\n got %v\nwant %v", *got, want)
	}
}

func TestBackup_Incr_AppendsParent(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
		backupType: "incr",
	}
	if err := runBackup(globalArgs); err != nil {
		t.Fatal(err)
	}
	if !sliceContainsPair(*got, "--incremental-from", "latest") {
		t.Errorf("incr should append --incremental-from latest; got %v", *got)
	}
}

func TestBackup_Diff_Refused(t *testing.T) {
	captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", backupType: "diff"}
	err := runBackup(globalArgs)
	if err == nil || !strings.Contains(err.Error(), "--type=diff") {
		t.Fatalf("expected refusal for --type=diff, got %v", err)
	}
	if !strings.Contains(err.Error(), "use --type=incr") {
		t.Fatalf("expected remediation suggesting --type=incr, got %v", err)
	}
}

func TestBackup_UnknownType_Refused(t *testing.T) {
	captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", backupType: "weekly"}
	err := runBackup(globalArgs)
	if err == nil || !strings.Contains(err.Error(), "--type=weekly") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestBackup_RequiresStanza(t *testing.T) {
	captureDispatch(t)
	globalArgs = pgbackrestArgs{}
	if err := runBackup(globalArgs); err == nil {
		t.Fatalf("expected error when stanza is missing")
	}
}

func sliceContainsPair(s []string, a, b string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == a && s[i+1] == b {
			return true
		}
	}
	return false
}
