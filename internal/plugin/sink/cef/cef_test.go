package cef_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/cef"
)

func newSink(t *testing.T, cfg map[string]any) (output.Sink, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.cef")
	if cfg == nil {
		cfg = map[string]any{}
	}
	if _, ok := cfg["destination"]; !ok {
		cfg["destination"] = "file://" + path
	}
	s, err := cef.NewFromSpec(output.SinkSpec{Name: "test-cef", Plugin: "cef", Config: cfg})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	return s, path
}

func TestCEF_RendersHeaderAndExtensions(t *testing.T) {
	s, path := newSink(t, nil)
	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "lag_elevated")
	ev.GeneratedAt = time.Date(2026, 4, 28, 9, 12, 0, 0, time.UTC)
	ev.Subject = output.Subject{Deployment: "db1", Tenant: "default", BackupID: "db1.full.20260428T0900Z"}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(out), "\n")
	for _, want := range []string{
		"CEF:0|pg_hardstorage|pg_hardstorage|1|wal.stream.lag_elevated|lag_elevated|6|",
		"rt=2026-04-28T09:12:00Z",
		`cs1=pg_hardstorage.v1`,
		"cs2=wal.stream",
		"cs3=lag_elevated",
		"cs4=default",
		"cs5=db1",
		"cs6=db1.full.20260428T0900Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CEF output missing %q\ngot: %s", want, got)
		}
	}
}

func TestCEF_SeverityMapping(t *testing.T) {
	cases := []struct {
		sev  output.Severity
		want int
	}{
		{output.SeverityEmergency, 10},
		{output.SeverityAlert, 10},
		{output.SeverityCritical, 10},
		{output.SeverityError, 8},
		{output.SeverityWarning, 6},
		{output.SeverityNotice, 4},
		{output.SeverityInfo, 2},
		{output.SeverityDebug, 1},
	}
	for _, tc := range cases {
		s, path := newSink(t, map[string]any{"min_severity": "debug"})
		ev := output.NewEvent(tc.sev, "test", "x")
		s.Emit(context.Background(), ev)
		s.Close()
		out, _ := os.ReadFile(path)
		want := "|" + itoa(tc.want) + "|"
		if !strings.Contains(string(out), want) {
			t.Errorf("severity %v: expected %q, got %s", tc.sev, want, out)
		}
	}
}

func TestCEF_HeaderEscaping(t *testing.T) {
	s, path := newSink(t, map[string]any{"vendor": `pg|hard\storage`})
	ev := output.NewEvent(output.SeverityInfo, "x", "y")
	if err := s.Emit(context.Background(), ev); err == nil {
		// info < notice (default) so emit drops; lower the floor.
	}
	s.Close()
	// re-build with lower floor
	s, path = newSink(t, map[string]any{
		"vendor":       `pg|hard\storage`,
		"min_severity": "info",
	})
	s.Emit(context.Background(), ev)
	s.Close()
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), `pg\|hard\\storage`) {
		t.Errorf("vendor escaping wrong: %s", out)
	}
}

func TestCEF_ExtensionEscaping(t *testing.T) {
	s, path := newSink(t, map[string]any{"min_severity": "info"})
	ev := output.NewEvent(output.SeverityInfo, "comp", "op=foo bar")
	ev.Subject = output.Subject{Deployment: "weird=name\nline"}
	s.Emit(context.Background(), ev)
	s.Close()
	out, _ := os.ReadFile(path)
	for _, want := range []string{
		`weird\=name\nline`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("extension escaping missing %q in %s", want, out)
		}
	}
}

func TestCEF_BelowSeverityDropped(t *testing.T) {
	s, path := newSink(t, map[string]any{"min_severity": "warning"})
	ev := output.NewEvent(output.SeverityInfo, "x", "y")
	s.Emit(context.Background(), ev)
	s.Close()
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Errorf("info event should be dropped under warning floor, got %s", data)
	}
}

func TestCEF_RefusesNonFileScheme(t *testing.T) {
	_, err := cef.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "cef",
		Config: map[string]any{"destination": "tcp://siem.example.com:6514"},
	})
	if err == nil {
		t.Fatal("expected error for tcp:// destination")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error should mention unsupported scheme: %v", err)
	}
}

func TestCEF_AcceptsBarePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.cef")
	s, err := cef.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "cef",
		Config: map[string]any{"destination": path, "min_severity": "info"},
	})
	if err != nil {
		t.Fatalf("bare path should be accepted: %v", err)
	}
	ev := output.NewEvent(output.SeverityInfo, "x", "y")
	s.Emit(context.Background(), ev)
	s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %s, got %v", path, err)
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "10"
}
