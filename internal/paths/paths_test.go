package paths_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// stubEnv builds an Options-friendly env lookup from a map.
func stubEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSystemMode_FHSDefaults(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeSystem,
		Env:  stubEnv(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		got  paths.Path
		want string
	}{
		"config":      {p.Config, "/etc/pg_hardstorage"},
		"state":       {p.State, "/var/lib/pg_hardstorage"},
		"cache":       {p.Cache, "/var/cache/pg_hardstorage"},
		"logs":        {p.Logs, "/var/log/pg_hardstorage"},
		"runtime":     {p.Runtime, "/run/pg_hardstorage"},
		"shared_data": {p.SharedData, "/usr/share/pg_hardstorage"},
	}
	for name, tc := range cases {
		if tc.got.Value != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got.Value, tc.want)
		}
		if tc.got.Source != paths.SourceFHS {
			t.Errorf("%s source = %q, want fhs", name, tc.got.Source)
		}
	}
}

func TestUserMode_XDGFallbacks(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode:    paths.ModeUser,
		HomeDir: "/home/alice",
		UID:     1000,
		Env:     stubEnv(nil), // no XDG_* set; defaults apply
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		got  paths.Path
		want string
	}{
		"config":      {p.Config, "/home/alice/.config/pg_hardstorage"},
		"state":       {p.State, "/home/alice/.local/share/pg_hardstorage"},
		"cache":       {p.Cache, "/home/alice/.cache/pg_hardstorage"},
		"logs":        {p.Logs, "/home/alice/.local/state/pg_hardstorage"},
		"runtime":     {p.Runtime, "/run/user/1000/pg_hardstorage"},
		"shared_data": {p.SharedData, "/home/alice/.local/share/pg_hardstorage/share"},
	}
	for name, tc := range cases {
		if tc.got.Value != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got.Value, tc.want)
		}
		if tc.got.Source != paths.SourceXDG {
			t.Errorf("%s source = %q, want xdg", name, tc.got.Source)
		}
	}
}

func TestUserMode_XDGOverrides(t *testing.T) {
	env := stubEnv(map[string]string{
		"XDG_CONFIG_HOME": "/srv/cfg",
		"XDG_DATA_HOME":   "/srv/data",
		"XDG_CACHE_HOME":  "/srv/cache",
		"XDG_STATE_HOME":  "/srv/state",
		"XDG_RUNTIME_DIR": "/run/srv",
	})
	p, err := paths.Resolve(paths.Options{
		Mode:    paths.ModeUser,
		HomeDir: "/home/alice",
		UID:     1000,
		Env:     env,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.Value != "/srv/cfg/pg_hardstorage" {
		t.Errorf("config = %q", p.Config.Value)
	}
	if p.State.Value != "/srv/data/pg_hardstorage" {
		t.Errorf("state = %q", p.State.Value)
	}
	if p.Cache.Value != "/srv/cache/pg_hardstorage" {
		t.Errorf("cache = %q", p.Cache.Value)
	}
	if p.Logs.Value != "/srv/state/pg_hardstorage" {
		t.Errorf("logs = %q", p.Logs.Value)
	}
	if p.Runtime.Value != "/run/srv/pg_hardstorage" {
		t.Errorf("runtime = %q", p.Runtime.Value)
	}
}

func TestPerDomainEnvOverridesXDG(t *testing.T) {
	env := stubEnv(map[string]string{
		"PG_HARDSTORAGE_CONFIG_DIR": "/opt/cfg",
		"PG_HARDSTORAGE_STATE_DIR":  "/opt/state",
	})
	p, err := paths.Resolve(paths.Options{
		Mode:    paths.ModeUser,
		HomeDir: "/home/alice",
		UID:     1000,
		Env:     env,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.Value != "/opt/cfg" || p.Config.Source != paths.SourceEnv {
		t.Errorf("config = %+v", p.Config)
	}
	if p.State.Value != "/opt/state" || p.State.Source != paths.SourceEnv {
		t.Errorf("state = %+v", p.State)
	}
	// Cache should still be XDG.
	if p.Cache.Source != paths.SourceXDG {
		t.Errorf("cache should be XDG, got %s", p.Cache.Source)
	}
}

func TestRootOverride(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeSystem,
		Root: "/opt/pg_hardstorage",
		Env:  stubEnv(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		got  paths.Path
		want string
	}{
		"config":      {p.Config, "/opt/pg_hardstorage/etc"},
		"state":       {p.State, "/opt/pg_hardstorage/var/lib"},
		"cache":       {p.Cache, "/opt/pg_hardstorage/var/cache"},
		"logs":        {p.Logs, "/opt/pg_hardstorage/var/log"},
		"runtime":     {p.Runtime, "/opt/pg_hardstorage/run"},
		"shared_data": {p.SharedData, "/opt/pg_hardstorage/share"},
	}
	for name, tc := range cases {
		if tc.got.Value != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got.Value, tc.want)
		}
		if tc.got.Source != paths.SourceRoot {
			t.Errorf("%s source = %q, want root-override", name, tc.got.Source)
		}
		if !strings.Contains(tc.got.Reason, "/opt/pg_hardstorage") {
			t.Errorf("%s reason should mention root: %q", name, tc.got.Reason)
		}
	}
}

func TestExplicitOverrideWins(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeSystem,
		Root: "/opt/pg_hardstorage",
		Env: stubEnv(map[string]string{
			"PG_HARDSTORAGE_CONFIG_DIR": "/from/env",
		}),
		Overrides: map[paths.Domain]string{
			paths.DomainConfig: "/from/flag",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.Value != "/from/flag" {
		t.Errorf("explicit override should win: %q", p.Config.Value)
	}
	if p.Config.Source != paths.SourceFlag {
		t.Errorf("source should be flag; got %s", p.Config.Source)
	}
}

func TestDerivedPaths(t *testing.T) {
	p, err := paths.Resolve(paths.Options{Mode: paths.ModeSystem, Env: stubEnv(nil)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"conf.d":      "/etc/pg_hardstorage/conf.d",
		"deployments": "/etc/pg_hardstorage/deployments",
		"sinks":       "/etc/pg_hardstorage/sinks",
		"skills":      "/etc/pg_hardstorage/skills",
		"keyring":     "/etc/pg_hardstorage/keyring",
		"inflight":    "/var/lib/pg_hardstorage/inflight",
		"crashes":     "/var/lib/pg_hardstorage/crashes",
		"bookkeeping": "/var/lib/pg_hardstorage/bookkeeping",
	}
	got := map[string]string{
		"conf.d":      p.ConfigDropIn.Value,
		"deployments": p.Deployments.Value,
		"sinks":       p.Sinks.Value,
		"skills":      p.Skills.Value,
		"keyring":     p.Keyring.Value,
		"inflight":    p.Inflight.Value,
		"crashes":     p.Crashes.Value,
		"bookkeeping": p.StateDSN.Value,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if p.ConfigDropIn.Source != paths.SourceDerived {
		t.Errorf("derived paths must be tagged: got %s", p.ConfigDropIn.Source)
	}
}

func TestAuto_RootIsSystem(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeAuto,
		UID:  0,
		Env:  stubEnv(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != paths.ModeSystem {
		t.Errorf("auto + uid 0 should pick system; got %s", p.ModeName)
	}
	if p.Config.Value != "/etc/pg_hardstorage" {
		t.Errorf("expected FHS config; got %q", p.Config.Value)
	}
}

func TestAuto_NonRootIsUser(t *testing.T) {
	p, err := paths.Resolve(paths.Options{
		Mode:    paths.ModeAuto,
		UID:     1000,
		HomeDir: "/home/alice",
		Env:     stubEnv(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != paths.ModeUser {
		t.Errorf("auto + uid 1000 should pick user; got %s", p.ModeName)
	}
	if !strings.HasPrefix(p.Config.Value, "/home/alice/.config/") {
		t.Errorf("expected XDG config; got %q", p.Config.Value)
	}
}

func TestAll_StableOrder(t *testing.T) {
	p, err := paths.Resolve(paths.Options{Mode: paths.ModeSystem, Env: stubEnv(nil)})
	if err != nil {
		t.Fatal(err)
	}
	all := p.All()
	if len(all) < 10 {
		t.Fatalf("All returned only %d entries", len(all))
	}
	if all[0].Domain != "config" {
		t.Errorf("first entry should be config; got %q", all[0].Domain)
	}
}
