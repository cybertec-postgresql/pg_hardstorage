package paths

import (
	"path/filepath"
	"testing"
)

// withForcedWindows pins isWindows to true for the
// duration of fn and restores the original afterwards.
// We use this rather than an exported Options field so
// the production API surface stays clean.
func withForcedWindows(t *testing.T, fn func()) {
	t.Helper()
	orig := isWindows
	isWindows = func() bool { return true }
	defer func() { isWindows = orig }()
	fn()
}

func TestWindows_UserMode_AppDataDefaults(t *testing.T) {
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode:    ModeUser,
			HomeDir: `C:\Users\hs`,
			Env: func(k string) string {
				switch k {
				case "APPDATA":
					return `C:\Users\hs\AppData\Roaming`
				case "LOCALAPPDATA":
					return `C:\Users\hs\AppData\Local`
				case "PROGRAMDATA":
					return `C:\ProgramData`
				}
				return ""
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"config":      filepath.Join(`C:\Users\hs\AppData\Roaming`, "pg_hardstorage"),
			"state":       filepath.Join(`C:\Users\hs\AppData\Local`, "pg_hardstorage", "state"),
			"cache":       filepath.Join(`C:\Users\hs\AppData\Local`, "pg_hardstorage", "cache"),
			"logs":        filepath.Join(`C:\Users\hs\AppData\Local`, "pg_hardstorage", "logs"),
			"runtime":     filepath.Join(`C:\Users\hs\AppData\Local`, "pg_hardstorage", "run"),
			"shared_data": filepath.Join(`C:\ProgramData`, "pg_hardstorage"),
		}
		got := map[string]Path{
			"config":      p.Config,
			"state":       p.State,
			"cache":       p.Cache,
			"logs":        p.Logs,
			"runtime":     p.Runtime,
			"shared_data": p.SharedData,
		}
		for name, w := range want {
			if got[name].Value != w {
				t.Errorf("%s = %q, want %q", name, got[name].Value, w)
			}
			if got[name].Source != SourceWindows {
				t.Errorf("%s source = %q, want windows", name, got[name].Source)
			}
		}
	})
}

func TestWindows_UserMode_FallbackWhenEnvUnset(t *testing.T) {
	// APPDATA + LOCALAPPDATA unset — service container,
	// stripped-down test rig, what have you.  We must
	// still resolve to *something* under HomeDir so the
	// resolver doesn't crash with empty strings.
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode:    ModeUser,
			HomeDir: `C:\Users\hs`,
			Env:     func(string) string { return "" },
		})
		if err != nil {
			t.Fatal(err)
		}
		wantConfig := filepath.Join(`C:\Users\hs`, "AppData", "Roaming", "pg_hardstorage")
		if p.Config.Value != wantConfig {
			t.Errorf("config without APPDATA = %q, want %q", p.Config.Value, wantConfig)
		}
		wantState := filepath.Join(`C:\Users\hs`, "AppData", "Local", "pg_hardstorage", "state")
		if p.State.Value != wantState {
			t.Errorf("state without LOCALAPPDATA = %q, want %q", p.State.Value, wantState)
		}
		// PROGRAMDATA fallback — every Windows install
		// has C:\ProgramData, so this is the safe miss.
		if p.SharedData.Value != filepath.Join(`C:\ProgramData`, "pg_hardstorage") {
			t.Errorf("shared_data without PROGRAMDATA = %q", p.SharedData.Value)
		}
	})
}

func TestWindows_SystemMode_ProgramData(t *testing.T) {
	// --mode system on Windows = everything under
	// PROGRAMDATA\pg_hardstorage\<sub>, the equivalent
	// of FHS /var/lib + /etc + /var/log unified.
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode: ModeSystem,
			Env: func(k string) string {
				if k == "PROGRAMDATA" {
					return `D:\ProgramData`
				}
				return ""
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"config":      filepath.Join(`D:\ProgramData`, "pg_hardstorage", "config"),
			"state":       filepath.Join(`D:\ProgramData`, "pg_hardstorage", "state"),
			"cache":       filepath.Join(`D:\ProgramData`, "pg_hardstorage", "cache"),
			"logs":        filepath.Join(`D:\ProgramData`, "pg_hardstorage", "logs"),
			"runtime":     filepath.Join(`D:\ProgramData`, "pg_hardstorage", "run"),
			"shared_data": filepath.Join(`D:\ProgramData`, "pg_hardstorage", "share"),
		}
		got := map[string]Path{
			"config":      p.Config,
			"state":       p.State,
			"cache":       p.Cache,
			"logs":        p.Logs,
			"runtime":     p.Runtime,
			"shared_data": p.SharedData,
		}
		for name, w := range want {
			if got[name].Value != w {
				t.Errorf("%s = %q, want %q", name, got[name].Value, w)
			}
			if got[name].Source != SourceWindows {
				t.Errorf("%s source = %q, want windows", name, got[name].Source)
			}
		}
	})
}

func TestWindows_PerDomainEnvWins(t *testing.T) {
	// PG_HARDSTORAGE_CONFIG_DIR / _STATE_DIR / etc. are
	// platform-agnostic — they must take precedence over
	// Windows defaults the same way they do over FHS /
	// XDG.  Locks in that the platform branch sits BELOW
	// the per-domain env in the precedence chain.
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode:    ModeUser,
			HomeDir: `C:\Users\hs`,
			Env: func(k string) string {
				if k == "PG_HARDSTORAGE_CONFIG_DIR" {
					return `D:\custom\config`
				}
				return ""
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if p.Config.Value != `D:\custom\config` {
			t.Errorf("config = %q, want D:\\custom\\config", p.Config.Value)
		}
		if p.Config.Source != SourceEnv {
			t.Errorf("config source = %q, want env", p.Config.Source)
		}
	})
}

func TestWindows_RootOverride(t *testing.T) {
	// PG_HARDSTORAGE_ROOT collapses everything under one
	// tree; on Windows we keep the same {etc, var/lib,
	// var/cache, ...} subdir naming as Linux so a
	// Windows install pointed at e.g. D:\pg_hardstorage
	// looks identical to a Linux paths.root tree.  A
	// pure-Windows operator who'd prefer Windows-style
	// subdirs can layer per-domain overrides on top.
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode:    ModeUser,
			HomeDir: `C:\Users\hs`,
			Root:    `D:\pg_hardstorage`,
			Env:     func(string) string { return "" },
		})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(`D:\pg_hardstorage`, "etc")
		if p.Config.Value != want {
			t.Errorf("config under root = %q, want %q", p.Config.Value, want)
		}
		if p.Config.Source != SourceRoot {
			t.Errorf("config source = %q, want root-override", p.Config.Source)
		}
	})
}

func TestWindows_AutoModeStaysUser(t *testing.T) {
	// Windows has no UID-zero equivalent, and Geteuid
	// returns -1 on Windows.  ModeAuto must therefore
	// resolve to ModeUser — a Windows install running
	// without explicit --mode should land in
	// %APPDATA%/%LOCALAPPDATA%, not PROGRAMDATA.
	withForcedWindows(t, func() {
		p, err := Resolve(Options{
			Mode:    ModeAuto,
			UID:     -1, // os.Geteuid() on Windows
			HomeDir: `C:\Users\hs`,
			Env:     func(string) string { return "" },
		})
		if err != nil {
			t.Fatal(err)
		}
		if p.Mode != ModeUser {
			t.Errorf("ModeAuto on Windows should stay ModeUser; got %v", p.Mode)
		}
		// And Config should land under AppData\Roaming,
		// not ProgramData (the system-mode shape).
		if filepath.Base(filepath.Dir(p.Config.Value)) != "Roaming" {
			t.Errorf("config not under Roaming: %q", p.Config.Value)
		}
	})
}
