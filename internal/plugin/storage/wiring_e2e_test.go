// wiring_e2e_test.go — every registered scheme must be reachable
// through the PRODUCTION entry point, using only what production
// actually supplies.
//
// Every backend's own suite (fs, s3, gcs, azblob, sftp, scp) builds a
// storage.StorageConfig by hand and populates Extras before calling
// plugin.Open directly. That validates the plugin, but it skips the one
// line that differs in production:
//
//	storage.Open → plugin.Open(ctx, StorageConfig{URL: u})
//
// Extras is EMPTY there, and nothing anywhere populates it. The scp
// backend was consequently unusable — every operation failed at open
// with "extras.known_hosts is required" — while its contract suite
// passed, because the suite supplied the Extras the real caller never
// does. A plugin can be flawless and still unreachable.
//
// So these tests open through storage.Open with the empty Extras that
// `repo init` passes, configuring each backend only by URL and
// environment. A backend reachable solely from a test harness fails
// here.
//
// TestAllRegisteredSchemesAreWired is the companion guard: it fails
// when a scheme is registered without a row in the table below, so
// adding a seventh backend cannot silently reopen the gap.
//
//go:build integration

package storage_test

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func requireDockerE2E(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

// wiredScheme describes how production configures one backend: a URL,
// plus environment. Never Extras — production cannot supply it.
type wiredScheme struct {
	scheme string
	// sinkKind is the testkit runtime backing it, or "" for a backend
	// that needs no container (file://).
	sinkKind string
	// env maps the runtime's Extras/credentials onto the environment
	// variables the plugin actually reads in production. Nil when the
	// runtime's own EnvForAgent is sufficient.
	env func(rt sink.Runtime) map[string]string
}

// wiredSchemes is the source of truth for
// TestAllRegisteredSchemesAreWired. One row per registered scheme.
var wiredSchemes = []wiredScheme{
	{scheme: "file"},
	{scheme: "s3", sinkKind: "s3-minio"},
	{scheme: "gcs", sinkKind: "gcs-fake"},
	{scheme: "azblob", sinkKind: "azurite"},
	{
		scheme:   "sftp",
		sinkKind: "sftp",
		// The runtime exposes known_hosts through Extras, which
		// production never reads; the plugin's env fallback is the only
		// working channel.
		env: func(rt sink.Runtime) map[string]string {
			e := map[string]string{}
			ex := rt.Extras()
			if v := ex["known_hosts"]; v != "" {
				e["PG_HARDSTORAGE_SFTP_KNOWN_HOSTS"] = v
			}
			if v := ex["password"]; v != "" {
				e["PG_HARDSTORAGE_SFTP_PASSWORD"] = v
			}
			return e
		},
	},
	{scheme: "scp", sinkKind: "ssh-exec"},
}

// productionSchemes is the registry snapshot taken at package-init
// time, which is exactly the set of schemes the SHIPPED plugins
// register.
//
// storage.Schemes() cannot be read at test time for this purpose: the
// registry is process-global, and storage_test.go's own registry unit
// tests register fakes into it from inside test bodies — "fake-dup-1",
// "fake-known-2", "file-test-3". Reading it live made this test pass
// under a -run filter that excluded those tests and fail on a full
// package run, which is a worse failure mode than not having the test.
//
// Imported packages are fully initialised before the importing
// package's variables, so every plugin's init() has already run here
// while no test function has. That is precisely the production set.
var productionSchemes = storage.Schemes()

// TestAllRegisteredSchemesAreWired fails when a plugin registers a
// scheme that no wiring test covers.
//
// Without it, the wiring guarantee decays silently: someone adds a
// backend, its own contract suite passes (built by hand, with Extras),
// and nothing ever checks it against storage.Open. That is precisely
// how scp:// shipped unusable.
func TestAllRegisteredSchemesAreWired(t *testing.T) {
	covered := map[string]bool{}
	for _, w := range wiredSchemes {
		covered[w.scheme] = true
	}
	registered := productionSchemes
	if len(registered) == 0 {
		t.Fatal("no schemes registered — the blank imports above are not taking effect")
	}
	var missing []string
	for _, s := range registered {
		if !covered[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("scheme(s) %v are registered but have no row in wiredSchemes — "+
			"add one so the backend is proven reachable through storage.Open, "+
			"not merely through a hand-built StorageConfig", missing)
	}
	// The inverse: a stale row for a scheme nobody registers any more.
	reg := map[string]bool{}
	for _, s := range registered {
		reg[s] = true
	}
	for _, w := range wiredSchemes {
		if !reg[w.scheme] {
			t.Errorf("wiredSchemes lists %q, which is no longer registered — remove the row", w.scheme)
		}
	}
	t.Logf("registered schemes: %s", strings.Join(registered, ", "))
}

// TestWiring_EveryScheme opens each registered backend the way
// `repo init` does and runs an object round-trip through it.
func TestWiring_EveryScheme(t *testing.T) {
	for _, w := range wiredSchemes {
		t.Run(w.scheme, func(t *testing.T) {
			url := wiringURL(t, w)
			ctx := context.Background()
			sp, err := storage.Open(ctx, url)
			if err != nil {
				t.Fatalf("storage.Open(%s): %v\n"+
					"%s is not reachable through the production entry point "+
					"with URL + environment alone", url, w.scheme, w.scheme)
			}
			defer sp.Close()
			roundTrip(t, sp)
		})
	}
}

// wiringURL brings up whatever the scheme needs and returns its URL,
// applying the environment production would read.
func wiringURL(t *testing.T, w wiredScheme) string {
	t.Helper()
	if w.sinkKind == "" {
		// file:// needs no container.
		return "file://" + t.TempDir()
	}
	requireDockerE2E(t)
	rt, err := sink.New(w.sinkKind)
	if err != nil {
		t.Fatalf("sink.New(%q): %v", w.sinkKind, err)
	}
	if err := rt.Up(context.Background()); err != nil {
		t.Fatalf("%s container: %v", w.sinkKind, err)
	}
	t.Cleanup(func() { _ = rt.Down(context.Background()) })

	for k, v := range rt.EnvForAgent() {
		t.Setenv(k, v)
	}
	if w.env != nil {
		for k, v := range w.env(rt) {
			t.Setenv(k, v)
		}
	}
	return rt.URL()
}

// roundTrip exercises the object contract through whatever plugin
// storage.Open handed back: put, read back, stat, delete.
func roundTrip(t *testing.T, sp storage.StoragePlugin) {
	t.Helper()
	ctx := context.Background()
	const key = "wiring/e2e.txt"
	body := []byte("reachable through storage.Open")

	if _, err := sp.Put(ctx, key, strings.NewReader(string(body)), storage.PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := sp.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := make([]byte, len(body))
	n, _ := rc.Read(got)
	_ = rc.Close()
	if string(got[:n]) != string(body) {
		t.Errorf("Get = %q, want %q", got[:n], body)
	}
	if _, err := sp.Stat(ctx, key); err != nil {
		t.Errorf("Stat: %v", err)
	}
	if err := sp.Delete(ctx, key); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// TestWiring_UnknownScheme keeps the negative case honest: a typo'd
// scheme must be refused, not silently treated as a filesystem path.
func TestWiring_UnknownScheme(t *testing.T) {
	if _, err := storage.Open(context.Background(), "s4://bucket/prefix"); err == nil {
		t.Fatal("unknown scheme accepted")
	}
}
