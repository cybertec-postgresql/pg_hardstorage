package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

// recordingSink captures every Event emitted through a dispatcher
// so tests can assert presence / absence of a particular event.
type recordingSink struct {
	mu     sync.Mutex
	events []*output.Event
}

func (r *recordingSink) Name() string                                   { return "recording" }
func (r *recordingSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (r *recordingSink) Close() error                                   { return nil }
func (r *recordingSink) Emit(_ context.Context, ev *output.Event) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

// TestLoadConfigBestEffort_SuppressesPermissionDeniedWarning pins
// the fix for GH issue #20: when the config file exists but is
// unreadable due to permissions (the classic shape is PG's
// restore_command forking `pg_hardstorage wal fetch` as the
// postgres user against /root/.config/pg_hardstorage/... that root
// previously created), loadConfigBestEffort must NOT emit a
// `config.load_failed` warning event.  Pre-fix that warning landed
// in pg_ctl start's stderr once per fetched WAL segment, flooding
// recovery output and burying actionable signal.
func TestLoadConfigBestEffort_SuppressesPermissionDeniedWarning(t *testing.T) {
	// Plant a HOME with an unreadable config file.  The paths
	// resolver honours HOME / XDG_CONFIG_HOME; setting
	// XDG_CONFIG_HOME makes the resolved config path
	// $XDG_CONFIG_HOME/pg_hardstorage/pg_hardstorage.yaml.
	home := t.TempDir()
	xdgConfig := filepath.Join(home, ".config")
	configDir := filepath.Join(xdgConfig, "pg_hardstorage")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(configDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(configFile, []byte("airgapped: off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the file unreadable.  We are the only user on the
	// test host so 0000 is a permission denial for ourselves
	// too — the EACCES path runs uniformly.
	if err := os.Chmod(configFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configFile, 0o644) }) // let TempDir cleanup work

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	rec := &recordingSink{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
	d.AddSink(rec)

	got := loadConfigBestEffort(context.Background(), d)
	if got != nil {
		t.Errorf("loadConfigBestEffort returned non-nil on permission denied; want nil (defaults)")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if ev.Component == "config" && ev.Op == "load_failed" {
			t.Errorf("config.load_failed event must be suppressed on EACCES (GH #20); got %+v", ev)
		}
	}
}

// TestLoadConfigBestEffort_StillWarnsOnMalformedYAML pins the
// negative side of the fix: actionable failures (YAML syntax
// errors, drop-in dir issues) STILL emit the warning.  An operator
// staring at a malformed config needs to see the diagnostic.
func TestLoadConfigBestEffort_StillWarnsOnMalformedYAML(t *testing.T) {
	home := t.TempDir()
	xdgConfig := filepath.Join(home, ".config")
	configDir := filepath.Join(xdgConfig, "pg_hardstorage")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant readable but syntactically-invalid YAML.  The
	// loader returns a parse error wrapped through fmt.Errorf;
	// it does NOT wrap fs.ErrPermission, so our gate at
	// loadConfigBestEffort lets it through to the warning emit.
	configFile := filepath.Join(configDir, "pg_hardstorage.yaml")
	// Mismatched brackets are unambiguous YAML syntax errors —
	// the loader's yaml.Unmarshal returns a parse error, NOT
	// fs.ErrPermission, so the gate in loadConfigBestEffort
	// lets the warning fire.
	if err := os.WriteFile(configFile, []byte("airgapped: {[\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	rec := &recordingSink{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
	d.AddSink(rec)

	_ = loadConfigBestEffort(context.Background(), d)
	// Dispatcher.Event fans out to sinks via goroutines; Close
	// drains the in-flight emit calls so the test sees the
	// final state.
	_ = d.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	sawLoadFailed := false
	for _, ev := range rec.events {
		if ev.Component == "config" && ev.Op == "load_failed" {
			sawLoadFailed = true
			break
		}
	}
	if !sawLoadFailed {
		t.Errorf("config.load_failed event MUST fire on a malformed-YAML load failure (actionable diagnostic)")
	}
}
