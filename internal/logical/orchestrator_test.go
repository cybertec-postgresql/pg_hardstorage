package logical_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
)

func TestManager_AddListRemove(t *testing.T) {
	m := logical.NewManager(filepath.Join(t.TempDir(), "logical_streams.json"))

	// Add succeeds.
	s, err := m.Add(logical.AddOptions{
		Name:        "events-cdc",
		Deployment:  "db1",
		Publication: "events_pub",
		RepoURL:     "file:///srv/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Slot == "" {
		t.Error("Slot should default to pg_hardstorage_logical_<name>")
	}
	if s.Plugin != "pgoutput" {
		t.Errorf("Plugin = %q, want pgoutput", s.Plugin)
	}
	if s.SinkKind != "chunked" {
		t.Errorf("SinkKind = %q, want chunked", s.SinkKind)
	}

	// Re-add fails with ErrAlreadyExists.
	if _, err := m.Add(logical.AddOptions{
		Name:        "events-cdc",
		Deployment:  "db1",
		Publication: "events_pub",
		RepoURL:     "file:///srv/repo",
	}); !errors.Is(err, logical.ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}

	// List returns it.
	out, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "events-cdc" {
		t.Errorf("List returned %+v", out)
	}

	// Get returns it.
	got, err := m.Get("events-cdc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "events-cdc" {
		t.Errorf("Get returned %+v", got)
	}

	// Remove succeeds.
	if err := m.Remove("events-cdc"); err != nil {
		t.Fatal(err)
	}

	// List is now empty.
	out, _ = m.List()
	if len(out) != 0 {
		t.Errorf("after Remove, List returned %+v", out)
	}

	// Get the removed name returns ErrNotFound.
	if _, err := m.Get("events-cdc"); !errors.Is(err, logical.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestManager_RejectsMissingFields(t *testing.T) {
	m := logical.NewManager(filepath.Join(t.TempDir(), "logical_streams.json"))
	cases := []logical.AddOptions{
		{},                             // missing everything
		{Name: "x"},                    // missing deployment
		{Name: "x", Deployment: "db1"}, // missing publication
		{Name: "x", Deployment: "db1", Publication: "p"}, // missing repo
	}
	for _, c := range cases {
		if _, err := m.Add(c); err == nil {
			t.Errorf("Add(%+v) — expected error", c)
		}
	}
}

func TestManager_StateFilePersistsAcrossInstances(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "logical_streams.json")
	m1 := logical.NewManager(statePath)
	if _, err := m1.Add(logical.AddOptions{
		Name:        "x",
		Deployment:  "db1",
		Publication: "p",
		RepoURL:     "file:///r",
	}); err != nil {
		t.Fatal(err)
	}
	m2 := logical.NewManager(statePath)
	out, err := m2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("second manager didn't see persisted entry: %+v", out)
	}
}
