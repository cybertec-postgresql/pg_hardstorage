package postverify_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in    string
		want  postverify.Mode
		isErr bool
	}{
		{"", postverify.ModeAuto, false},
		{"auto", postverify.ModeAuto, false},
		{"AUTO", postverify.ModeAuto, false},
		{"off", postverify.ModeOff, false},
		{"none", postverify.ModeOff, false},
		{"required", postverify.ModeRequired, false},
		{"strict", postverify.ModeRequired, false},
		{"dump", postverify.ModeDump, false},
		{"bogus", "", true},
		{"sound", "", true},
	}
	for _, c := range cases {
		got, err := postverify.ParseMode(c.in)
		if c.isErr {
			if err == nil {
				t.Errorf("ParseMode(%q): expected error, got %s", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestVerify_OffMode_AlwaysSkips(t *testing.T) {
	res, err := postverify.Verify(context.Background(), postverify.Options{
		Mode:    postverify.ModeOff,
		DataDir: t.TempDir(), // doesn't exist as a real PGDATA, doesn't matter
	})
	if err != nil {
		t.Fatalf("ModeOff should never error: %v", err)
	}
	if !res.Skipped {
		t.Error("ModeOff should set Skipped")
	}
}

func TestVerify_AutoMode_SoftSkipsOnMissingPGControl(t *testing.T) {
	// Empty TempDir has no global/pg_control → auto mode
	// soft-skips with a clear reason rather than firing
	// pg_ctl against a non-PGDATA.
	res, err := postverify.Verify(context.Background(), postverify.Options{
		Mode:    postverify.ModeAuto,
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("auto mode on empty dir should soft-skip, got %v", err)
	}
	if !res.Skipped {
		t.Fatal("expected Skipped=true")
	}
	if !strings.Contains(res.SkipReason, "pg_control") {
		t.Errorf("SkipReason %q should mention pg_control", res.SkipReason)
	}
}

func TestVerify_RequiredMode_HardFailsOnMissingPGControl(t *testing.T) {
	_, err := postverify.Verify(context.Background(), postverify.Options{
		Mode:    postverify.ModeRequired,
		DataDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected hard fail in Required mode without pg_control")
	}
	if !strings.Contains(err.Error(), "pg_control") {
		t.Errorf("error %q should mention pg_control", err.Error())
	}
}

func TestVerify_ErrNoEnvironment_IsSentinel(t *testing.T) {
	// errors.Is wiring sanity — caller code switches on this.
	if !errors.Is(postverify.ErrNoEnvironment, postverify.ErrNoEnvironment) {
		t.Fatal("ErrNoEnvironment must be its own match")
	}
}

// TestStageForRecovery_PITRArmed_NoImmediateTarget locks the issue-#56
// fix: when the restore already armed a PITR recovery_target_* block,
// postverify must not append its own `recovery_target = 'immediate'`
// (PG rejects two targets with "multiple recovery targets specified").
// It must still wire a restore_command so redo can reach the target.
func TestStageForRecovery_PITRArmed_NoImmediateTarget(t *testing.T) {
	dir := t.TempDir()
	// Mimic restore.WriteRecoveryFiles: a PITR target block + signal.
	autoConf := filepath.Join(dir, "postgresql.auto.conf")
	if err := os.WriteFile(autoConf, []byte(
		"recovery_target_time = '2026-05-22 13:50:45+00'\n"+
			"recovery_target_inclusive = true\n"+
			"recovery_target_action = 'pause'\n"+
			"recovery_target_timeline = 'latest'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recovery.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := postverify.StageForRecoveryForTest(
		dir, "file:///tmp/repo", "pg18", "/usr/bin/true", true); err != nil {
		t.Fatalf("stageForRecovery (PITR armed): %v", err)
	}

	got, err := os.ReadFile(autoConf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "recovery_target = 'immediate'") {
		t.Errorf("PITR-armed restore must NOT get recovery_target='immediate'; auto.conf:\n%s", s)
	}
	if !strings.Contains(s, "restore_command =") {
		t.Errorf("PITR-armed restore should still get a restore_command; auto.conf:\n%s", s)
	}
	// Exactly one recovery target — the operator's time target.
	if !strings.Contains(s, "recovery_target_time =") {
		t.Errorf("operator's recovery_target_time must survive; auto.conf:\n%s", s)
	}
}

// TestStageForRecovery_NonPITR_GetsImmediateTarget is the control: a
// plain "restore latest" smoke test still gets recovery_target='immediate'
// + promote so postverify can boot and probe the cluster.
func TestStageForRecovery_NonPITR_GetsImmediateTarget(t *testing.T) {
	dir := t.TempDir()
	if err := postverify.StageForRecoveryForTest(
		dir, "file:///tmp/repo", "pg18", "/usr/bin/true", false); err != nil {
		t.Fatalf("stageForRecovery (non-PITR): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "recovery_target = 'immediate'") {
		t.Errorf("non-PITR restore should get recovery_target='immediate'; auto.conf:\n%s", s)
	}
	if !strings.Contains(s, "recovery_target_action = 'promote'") {
		t.Errorf("non-PITR restore should get recovery_target_action='promote'; auto.conf:\n%s", s)
	}
}
