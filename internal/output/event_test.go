package output

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewEvent_Defaults(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	ev := NewEvent(SeverityWarning, "backup", "started")
	after := time.Now().UTC().Add(time.Second)

	if ev.Schema != Schema {
		t.Errorf("Schema = %q, want %q", ev.Schema, Schema)
	}
	if ev.Severity != SeverityWarning {
		t.Errorf("Severity = %s, want warning", ev.Severity)
	}
	if ev.SeverityName != "warning" {
		t.Errorf("SeverityName = %q, want warning", ev.SeverityName)
	}
	if ev.Component != "backup" || ev.Op != "started" {
		t.Errorf("Component=%q Op=%q", ev.Component, ev.Op)
	}
	if ev.GeneratedAt.Before(before) || ev.GeneratedAt.After(after) {
		t.Errorf("GeneratedAt %v outside [%v, %v]", ev.GeneratedAt, before, after)
	}
}

func TestEvent_BuildersJSON(t *testing.T) {
	ev := NewEvent(SeverityNotice, "wal.stream", "slot_recreated").
		WithSubject(Subject{Deployment: "db1", Timeline: 3, LSN: "0/300"}).
		WithBody(map[string]any{"gap_bytes": 0}).
		WithSuggestion(&Suggestion{Human: "no action"})

	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"schema":"pg_hardstorage.v1"`,
		`"severity_name":"notice"`,
		`"component":"wal.stream"`,
		`"op":"slot_recreated"`,
		`"deployment":"db1"`,
		`"timeline":3`,
		`"gap_bytes":0`,
		`"suggestion":{"human":"no action"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("JSON missing %q\nfull: %s", want, s)
		}
	}
}

func TestEvent_OmitsEmptySubject(t *testing.T) {
	ev := NewEvent(SeverityInfo, "version", "show")
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), `"subject"`) {
		t.Errorf("zero subject should be omitted (omitzero); got %s", b)
	}
}

func TestSubject_IsZero(t *testing.T) {
	if !(Subject{}).IsZero() {
		t.Error("zero Subject should report IsZero")
	}
	if (Subject{Deployment: "db1"}).IsZero() {
		t.Error("non-zero Subject should not report IsZero")
	}
}

func TestNewResult(t *testing.T) {
	r := NewResult("status").WithBody(map[string]any{"deployments": []string{"db1"}})
	if r.Schema != Schema || r.Command != "status" {
		t.Errorf("Schema=%q Command=%q", r.Schema, r.Command)
	}
	if r.IsError() {
		t.Error("body Result must not be error")
	}
	r2 := NewResult("backup").WithError(NewError("backup.failed", "boom"))
	if !r2.IsError() {
		t.Error("error Result must be error")
	}
	if r2.Result != nil {
		t.Error("WithError must clear Result")
	}
}

func TestError_AsErrorInterface(t *testing.T) {
	var err error = NewError("wal.slot_missing", "slot dropped").
		WithSubject(Subject{Deployment: "db1"}).
		WithSuggestion(&Suggestion{Command: "pg_hardstorage wal repair db1"})
	if got := err.Error(); got != "wal.slot_missing: slot dropped" {
		t.Errorf("Error() = %q", got)
	}
	oe, ok := AsOutputError(err)
	if !ok {
		t.Fatal("AsOutputError returned !ok")
	}
	if oe.Subject.Deployment != "db1" {
		t.Errorf("subject lost on AsOutputError")
	}
}

func TestError_UnwrapAndAs(t *testing.T) {
	cause := errors.New("connection refused")
	oe := NewError("pg.unreachable", "could not connect").Wrap(cause)
	if !errors.Is(oe, cause) {
		t.Error("errors.Is should find cause through Unwrap")
	}
	wrapped := errors.Join(oe, errors.New("other"))
	if got, ok := AsOutputError(wrapped); !ok || got.Code != "pg.unreachable" {
		t.Error("AsOutputError must work through errors.Join chains")
	}
}

func TestToError_Passthrough(t *testing.T) {
	original := NewError("x", "y")
	got := ToError(original)
	if got != original {
		t.Error("ToError must return the same *Error if already structured")
	}
}

func TestToError_WrapsAdHoc(t *testing.T) {
	got := ToError(errors.New("boom"))
	if got.Code != "internal" {
		t.Errorf("code = %q, want internal", got.Code)
	}
	if got.Message != "boom" {
		t.Errorf("message = %q", got.Message)
	}
	if got.Severity != SeverityError {
		t.Errorf("severity = %s, want error", got.Severity)
	}
	if got.Cause == nil {
		t.Error("cause must be retained for errors.Is/As")
	}
}

func TestToError_NilStaysNil(t *testing.T) {
	if ToError(nil) != nil {
		t.Error("ToError(nil) must return nil")
	}
}

func TestError_CauseNotSerialized(t *testing.T) {
	oe := NewError("x", "y").Wrap(errors.New("internal detail"))
	b, _ := json.Marshal(oe)
	if strings.Contains(string(b), "internal detail") {
		t.Errorf("cause must not appear in JSON: %s", b)
	}
}
