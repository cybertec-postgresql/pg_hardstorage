package output_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// dummySink is a no-op Sink used to drive the registry tests.
type dummySink struct {
	name     string
	emitted  []*output.Event
	openErr  error
	closeErr error
}

func (s *dummySink) Name() string                                   { return s.name }
func (s *dummySink) Open(_ context.Context, _ map[string]any) error { return s.openErr }
func (s *dummySink) Emit(_ context.Context, e *output.Event) error {
	s.emitted = append(s.emitted, e)
	return nil
}
func (s *dummySink) Close() error { return s.closeErr }

func TestSinkRegistry_RegisterAndBuild(t *testing.T) {
	r := output.NewSinkRegistry()
	r.Register("dummy", func(spec output.SinkSpec) (output.Sink, error) {
		return &dummySink{name: spec.Name}, nil
	})

	s, err := r.Build(output.SinkSpec{Name: "ops", Plugin: "dummy"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "ops" {
		t.Errorf("Name = %q, want ops", s.Name())
	}
}

func TestSinkRegistry_RequiresNameAndPlugin(t *testing.T) {
	r := output.NewSinkRegistry()
	r.Register("dummy", func(spec output.SinkSpec) (output.Sink, error) {
		return &dummySink{name: spec.Name}, nil
	})

	cases := []output.SinkSpec{
		{Plugin: "dummy"},            // missing Name
		{Name: "x"},                  // missing Plugin
		{Name: "y", Plugin: "ghost"}, // unknown Plugin
	}
	for _, spec := range cases {
		if _, err := r.Build(spec); err == nil {
			t.Errorf("expected error for %+v", spec)
		}
	}
}

func TestSinkRegistry_BuildAll_PartialSuccess(t *testing.T) {
	r := output.NewSinkRegistry()
	r.Register("good", func(spec output.SinkSpec) (output.Sink, error) {
		return &dummySink{name: spec.Name}, nil
	})
	r.Register("bad", func(spec output.SinkSpec) (output.Sink, error) {
		return nil, errors.New("boom")
	})

	specs := []output.SinkSpec{
		{Name: "ops", Plugin: "good"},
		{Name: "audit", Plugin: "bad"},
		{Name: "alerts", Plugin: "good"},
	}
	sinks, errs := r.BuildAll(specs)

	if len(sinks) != 2 {
		t.Errorf("got %d sinks, want 2 (skipped the bad one)", len(sinks))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1", len(errs))
	}
	if errs[0].Spec.Name != "audit" {
		t.Errorf("error attributed to %q, want audit", errs[0].Spec.Name)
	}
	if !strings.Contains(errs[0].Error(), "boom") {
		t.Errorf("error message should include underlying cause; got %v", errs[0])
	}
}

func TestSinkRegistry_DoubleRegisterPanics(t *testing.T) {
	r := output.NewSinkRegistry()
	r.Register("p", func(_ output.SinkSpec) (output.Sink, error) { return &dummySink{}, nil })
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected panic on double-register")
		}
	}()
	r.Register("p", func(_ output.SinkSpec) (output.Sink, error) { return &dummySink{}, nil })
}

func TestSinkConfigString(t *testing.T) {
	cfg := map[string]any{"k": "v", "n": 5}
	got, err := output.SinkConfigString(cfg, "k")
	if err != nil || got != "v" {
		t.Errorf("string get: got=%q err=%v", got, err)
	}
	got, err = output.SinkConfigString(cfg, "missing")
	if err != nil || got != "" {
		t.Errorf("absent key should return empty string; got=%q err=%v", got, err)
	}
	if _, err := output.SinkConfigString(cfg, "n"); err == nil {
		t.Error("present-but-wrong-type should error")
	}
}

func TestSinkConfigStringDefault(t *testing.T) {
	cfg := map[string]any{"x": "set"}
	got, err := output.SinkConfigStringDefault(cfg, "x", "default")
	if err != nil || got != "set" {
		t.Errorf("present should override default; got=%q err=%v", got, err)
	}
	got, err = output.SinkConfigStringDefault(cfg, "missing", "default")
	if err != nil || got != "default" {
		t.Errorf("absent should return default; got=%q err=%v", got, err)
	}
}

func TestSinkBuildError_Unwrap(t *testing.T) {
	inner := errors.New("disk on fire")
	e := output.SinkBuildError{Spec: output.SinkSpec{Name: "x", Plugin: "p"}, Err: inner}
	if !errors.Is(e, inner) {
		t.Error("errors.Is should walk into Err")
	}
}
