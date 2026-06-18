package restore_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestWriteRecoveryFiles_NoOpWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{Enable: false}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"recovery.signal", "postgresql.auto.conf"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should not exist when Enable=false", f)
		}
	}
}

func TestWriteRecoveryFiles_RequiresRestoreCommand(t *testing.T) {
	dir := t.TempDir()
	err := restore.WriteRecoveryFiles(dir, restore.Recovery{Enable: true})
	if err == nil {
		t.Fatal("expected error: empty RestoreCommand with Enable=true")
	}
	if !strings.Contains(err.Error(), "RestoreCommand") {
		t.Errorf("error should mention RestoreCommand; got %v", err)
	}
}

func TestWriteRecoveryFiles_RejectsMultipleTargets(t *testing.T) {
	dir := t.TempDir()
	err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "cat",
		TargetLSN:      "0/3000028",
		TargetName:     "p",
	})
	if err == nil {
		t.Fatal("expected at-most-one error")
	}
	if !strings.Contains(err.Error(), "at most one") {
		t.Errorf("error should mention at-most-one; got %v", err)
	}
}

func TestWriteRecoveryFiles_LSN(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "/usr/bin/pg_hardstorage wal fetch db1 %f %p --repo file:///r",
		TargetLSN:      "0/3000028",
		Inclusive:      true,
		Action:         "promote",
		Timeline:       "latest",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "recovery.signal")); err != nil {
		t.Errorf("recovery.signal missing: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"# --- pg_hardstorage managed block (PITR) ---",
		"restore_command = '/usr/bin/pg_hardstorage wal fetch db1 %f %p --repo file:///r'",
		"recovery_target_lsn = '0/3000028'",
		"recovery_target_inclusive = true",
		"recovery_target_action = 'promote'",
		"recovery_target_timeline = 'latest'",
		"# --- end pg_hardstorage managed block ---",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("postgresql.auto.conf missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestWriteRecoveryFiles_Time(t *testing.T) {
	dir := t.TempDir()
	target := time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC)
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetTime:     target,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if !strings.Contains(string(body), "recovery_target_time = '2026-04-27 09:42:00+00'") {
		t.Errorf("missing time GUC; got:\n%s", body)
	}
}

func TestWriteRecoveryFiles_Name(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetName:     "before-prod-deploy",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if !strings.Contains(string(body), "recovery_target_name = 'before-prod-deploy'") {
		t.Errorf("missing name GUC; got:\n%s", body)
	}
}

func TestWriteRecoveryFiles_QuoteEscaping(t *testing.T) {
	// Pathological deployment / command containing single quotes
	// must be doubled per PG config-file rules. We test via the
	// restore_command, which is the most-likely source of user-
	// supplied content.
	dir := t.TempDir()
	bin := "/opt/it's mine/pg_hardstorage"
	cmd := bin + " wal fetch db1 %f %p"
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: cmd,
		TargetLSN:      "0/0",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	want := "restore_command = '/opt/it''s mine/pg_hardstorage wal fetch db1 %f %p'"
	if !strings.Contains(string(body), want) {
		t.Errorf("quote-escaping failed; expected %q\ngot:\n%s", want, body)
	}
}

// External review pass found that quoteSQL only escaped `'`. A
// newline / CR / NUL embedded in an operator-controlled field (e.g.
// --target-name) would terminate the GUC directive line in the auto.conf
// parser and inject whatever followed as a separate setting. Pin the
// hardened escape vocabulary.
func TestWriteRecoveryFiles_EscapesNewlinesAndControlChars(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "binary",
		// Inject a newline + a faked GUC. Pre-fix this rendered as
		// `recovery_target_name = 'evil` <NL> `bad_guc = 1'` and PG's
		// auto.conf parser would accept `bad_guc = 1` as a real GUC.
		TargetName: "evil\nbad_guc = 1",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	got := string(body)
	// The raw newline MUST NOT be present anywhere in the
	// recovery_target_name value — it must be the escaped form.
	if strings.Contains(got, "evil\nbad_guc") {
		t.Errorf("raw newline survived escape; auto.conf would inject bad_guc:\n%s", got)
	}
	if !strings.Contains(got, `'evil\nbad_guc = 1'`) {
		t.Errorf("expected backslash-n escape; got:\n%s", got)
	}
	// Carriage return + NUL hardening:
	dir2 := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir2, restore.Recovery{
		Enable:         true,
		RestoreCommand: "binary",
		TargetName:     "a\rb\x00c",
	}); err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(filepath.Join(dir2, "postgresql.auto.conf"))
	got2 := string(body2)
	if strings.Contains(got2, "\r") || strings.Contains(got2, "\x00") {
		t.Errorf("raw CR/NUL survived escape:\n%q", got2)
	}
	if !strings.Contains(got2, `'a\rb\0c'`) {
		t.Errorf("expected \\r and \\0 escape forms; got:\n%s", got2)
	}
}

// External review pass found a pre-fix order bug: signal was written
// FIRST, GUCs SECOND. If the GUC append failed after the signal
// landed, PG would enter recovery without recovery_target_* set and
// replay all available WAL — past the operator's intended PITR
// target. Post-fix: GUCs first, signal last → signal-write failure
// leaves an unsignalled cluster with stale-GUCs-in-auto.conf, which
// is harmless at non-recovery startup. Pin the order.
func TestWriteRecoveryFiles_AutoConfWrittenBeforeSignal(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "binary",
		TargetLSN:      "0/3000028",
	}); err != nil {
		t.Fatal(err)
	}
	autoStat, err := os.Stat(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatalf("auto.conf missing: %v", err)
	}
	sigStat, err := os.Stat(filepath.Join(dir, "recovery.signal"))
	if err != nil {
		t.Fatalf("recovery.signal missing: %v", err)
	}
	// auto.conf's mtime should be ≤ signal's mtime (i.e. it was
	// written first or simultaneously). Filesystems with second-
	// granularity timestamps can tie, so we use !After.
	if autoStat.ModTime().After(sigStat.ModTime()) {
		t.Errorf("auto.conf written AFTER signal — partial-failure order is dangerous: auto=%v sig=%v",
			autoStat.ModTime(), sigStat.ModTime())
	}
}

func TestWriteRecoveryFiles_DefaultsAppliedAtRender(t *testing.T) {
	// Empty Action / Timeline should render as "pause" / "latest".
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/0",
		// Action and Timeline are empty strings.
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if !strings.Contains(string(body), "recovery_target_action = 'pause'") {
		t.Errorf("default action not 'pause'; got:\n%s", body)
	}
	if !strings.Contains(string(body), "recovery_target_timeline = 'latest'") {
		t.Errorf("default timeline not 'latest'; got:\n%s", body)
	}
}

func TestWriteRecoveryFiles_AppendsToExistingAutoConf(t *testing.T) {
	dir := t.TempDir()
	autoConf := filepath.Join(dir, "postgresql.auto.conf")
	preExisting := "# Do not edit this file manually!\nlisten_addresses = '127.0.0.1'\n"
	if err := os.WriteFile(autoConf, []byte(preExisting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/0",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(autoConf)
	got := string(body)
	if !strings.HasPrefix(got, preExisting) {
		t.Errorf("pre-existing content lost; got:\n%s", got)
	}
	if !strings.Contains(got, "managed block") {
		t.Errorf("managed block missing; got:\n%s", got)
	}
}

func TestRecovery_IsTargetSet(t *testing.T) {
	cases := []struct {
		name string
		r    restore.Recovery
		want bool
	}{
		{"nothing", restore.Recovery{}, false},
		{"lsn", restore.Recovery{TargetLSN: "0/0"}, true},
		{"time", restore.Recovery{TargetTime: time.Now()}, true},
		{"name", restore.Recovery{TargetName: "x"}, true},
	}
	for _, c := range cases {
		if got := c.r.IsTargetSet(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWriteRecoveryFiles_RejectsBadAction(t *testing.T) {
	dir := t.TempDir()
	err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/0",
		Action:         "explode",
	})
	if err == nil {
		t.Fatal("expected error on bad action")
	}
	if !strings.Contains(err.Error(), "Action") {
		t.Errorf("error should mention Action; got %v", err)
	}
}
