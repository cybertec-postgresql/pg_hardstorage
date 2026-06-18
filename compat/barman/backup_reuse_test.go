package barman

import (
	"bytes"
	"strings"
	"testing"
)

// TestBackup_RefusesReuseBackupLink covers the honest-not-supported
// refusal for Barman's --reuse-backup={link,copy} incremental scheme.
// pg_hardstorage's PG 17 BASE_BACKUP INCREMENTAL is a different
// storage shape; the Barman shim must NOT silently fall back to a
// full when the operator's cron job explicitly asked for an
// incremental, otherwise a customer migrating from Barman would
// silently lose their incremental schedule.
func TestBackup_RefusesReuseBackupLink(t *testing.T) {
	cases := []struct {
		value        string
		wantInStderr string
	}{
		{"link", "--reuse-backup=link not supported"},
		{"copy", "--reuse-backup=copy not supported"},
		{"weird", "unknown value"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewRoot(&stdout, &stderr)
			root.SetArgs([]string{"backup", "myserver", "--reuse-backup=" + tc.value})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			_, err := root.ExecuteC()
			if err == nil {
				t.Fatalf("--reuse-backup=%s: want refusal error, got nil", tc.value)
			}
			combined := stderr.String() + " " + err.Error()
			if !strings.Contains(combined, tc.wantInStderr) {
				t.Errorf("stderr/err missing %q\n--- stderr ---\n%s\n--- err ---\n%s",
					tc.wantInStderr, stderr.String(), err.Error())
			}
		})
	}
}

// TestBackup_AcceptsReuseBackupOff confirms --reuse-backup=off is
// the no-op path (matches our default); operators migrating cron
// jobs that already set =off can keep them.  The command will
// still fail at injectDeploymentFlags because no deployment config
// exists in this unit test, but the failure must be about the
// deployment, NOT about --reuse-backup.
func TestBackup_AcceptsReuseBackupOff(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"backup", "myserver", "--reuse-backup=off"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	_, err := root.ExecuteC()
	if err == nil {
		return // unlikely — depends on whether dispatchNative succeeds
	}
	combined := stderr.String() + " " + err.Error()
	if strings.Contains(combined, "reuse-backup") {
		t.Errorf("--reuse-backup=off should be silently accepted; got refusal:\n%s",
			combined)
	}
}
