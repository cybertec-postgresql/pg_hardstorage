// Package paths resolves where pg_hardstorage's config, state, cache,
// runtime sockets, logs, and shared read-only data live on the host.
//
// Two ideas drive the design:
//
//  1. Concerns are separated in code (Paths.Config / .State / .Cache / ...).
//     The same binary respects FHS on Debian + RHEL, XDG when run as a user,
//     and a single-tree consolidation when the operator wants RHEL-appliance
//     style (paths.root: /opt/pg_hardstorage).
//
//  2. Every path records the Source that produced it (flag, env, root-
//     override, XDG, FHS, default). doctor reports that source so users
//     always know "why is the config here?".
//
// Precedence (highest to lowest):
//
//	override map           // explicit per-domain Path; e.g. --config-dir
//	per-domain env         // PG_HARDSTORAGE_CONFIG_DIR, _STATE_DIR, ...
//	root-override          // PG_HARDSTORAGE_ROOT or paths.root: ...
//	XDG (user mode)        // $XDG_CONFIG_HOME, $XDG_STATE_HOME, ...
//	FHS (system mode)      // /etc/pg_hardstorage, /var/lib/pg_hardstorage, ...
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Source describes how a Path was chosen.
type Source string

const (
	// SourceFlag indicates an explicit override (CLI flag or config key).
	SourceFlag Source = "flag"
	// SourceEnv indicates a per-domain environment variable.
	SourceEnv Source = "env"
	// SourceRoot indicates PG_HARDSTORAGE_ROOT or paths.root.
	SourceRoot Source = "root-override"
	// SourceXDG indicates an XDG base-directory default (user mode).
	SourceXDG Source = "xdg"
	// SourceFHS indicates an FHS default (system mode).
	SourceFHS Source = "fhs"
	// SourceWindows indicates a Windows Known Folder (APPDATA / LOCALAPPDATA / PROGRAMDATA).
	SourceWindows Source = "windows"
	// SourceDerived indicates a Path computed from a parent Path.
	SourceDerived Source = "derived"
	// SourceDefault indicates the fallback built-in default.
	SourceDefault Source = "default"
)

// Mode controls system vs user resolution. Auto picks based on UID.
type Mode int

const (
	// ModeAuto resolves system vs user based on UID (root => system).
	ModeAuto Mode = iota
	// ModeSystem forces FHS / system-wide paths.
	ModeSystem
	// ModeUser forces XDG / per-user paths.
	ModeUser
)

// Path is a resolved filesystem location with audit metadata.
type Path struct {
	Value  string `json:"value"`
	Source Source `json:"source"`
	Reason string `json:"reason,omitempty"`
}

// String returns the path value for fmt.Sprintf compatibility.
func (p Path) String() string { return p.Value }

// Domain identifies one logical location category.
type Domain string

// The Domain* constants enumerate the categories Resolve fills in.
// Each maps to a specific FHS / XDG / root-override anchor; see the
// package comment for the precedence chain.
const (
	// DomainConfig holds operator-edited YAML (pg_hardstorage.yaml,
	// conf.d/*.yaml, deployments/<name>.yaml). Read-mostly.
	DomainConfig Domain = "config"
	// DomainState holds mutable runtime state survived across
	// restarts (inflight buffers, SQLite coordinator DB, crash
	// reports).
	DomainState Domain = "state"
	// DomainCache holds rebuildable derived data (bloom filters,
	// manifest indexes). Safe to wipe; the agent rebuilds on demand.
	DomainCache Domain = "cache"
	// DomainLogs holds operator-readable log files when journald is
	// unavailable. On journald hosts this is unused.
	DomainLogs Domain = "logs"
	// DomainRuntime holds ephemeral runtime artefacts — primarily
	// Unix-domain sockets (archive_library endpoint, agent control
	// socket). Cleared on host reboot per FHS /run semantics.
	DomainRuntime Domain = "runtime"
	// DomainSharedData holds read-only package-shipped resources
	// (runbooks, OpenAPI spec, CRDs, completions). Owned by the
	// distribution package; never written by the agent.
	DomainSharedData Domain = "shared_data"
)

// Paths is the result of Resolve. Each field carries its Source.
type Paths struct {
	Mode     Mode `json:"-"`
	ModeName string
	Config   Path
	// ConfigFileOverride, when non-empty, is the explicit config file
	// named by -c/--config (via PG_HARDSTORAGE_CONFIG_FILE). Empty means
	// "use the well-known <Config>/pg_hardstorage.yaml".
	ConfigFileOverride string `json:"-"`
	ConfigDropIn       Path   // <Config>/conf.d
	Deployments        Path   // <Config>/deployments
	Sinks              Path   // <Config>/sinks
	Skills             Path   // <Config>/skills (operator-overrides; user dirs handled separately)
	Keyring            Path   // <Config>/keyring (must be mode 0700)
	State              Path
	Inflight           Path // <State>/inflight
	Crashes            Path // <State>/crashes
	// StateDSN points at the host-local bookkeeping store. Pre-v0.4
	// this was a path to a SQLite file; we've since collapsed onto
	// PG / etcd / K8s `Lease` for any persistent state and kept this
	// field as the historical placeholder. The Path's Value is now
	// the JSON-state directory beneath State (`<State>/bookkeeping`)
	// — small per-package files (standbys.json, timetravel.json,
	// logical_streams.json, ...) live under it and the field gives
	// callers one canonical "where does my host-local bookkeeping
	// go?" answer without re-deriving from State each time.
	StateDSN   Path
	Cache      Path
	Logs       Path
	Runtime    Path
	SharedData Path // /usr/share/pg_hardstorage; runbooks live here
}

// Options drives Resolve. Use DefaultOptions() for production; tests
// inject overrides directly.
type Options struct {
	Mode      Mode
	UID       int                 // used when Mode == ModeAuto
	HomeDir   string              // overrides $HOME
	Env       func(string) string // env-var lookup; defaults to os.Getenv
	Root      string              // when set, overrides everything (paths.root)
	Overrides map[Domain]string   // per-domain explicit override (e.g. --config-dir)
	// ConfigFile, when set, names an explicit config FILE (the CLI's
	// -c/--config flag, forwarded via PG_HARDSTORAGE_CONFIG_FILE). It
	// overrides the well-known <Config>/pg_hardstorage.yaml lookup for
	// both reads (config.Load) and write-back (configio).
	ConfigFile string
}

// DefaultOptions reads the environment for the running process. Most
// callers (CLI, agent, server) use this; tests construct Options directly.
func DefaultOptions() Options {
	return Options{
		Mode:       ModeAuto,
		UID:        os.Geteuid(),
		HomeDir:    getHome(),
		Env:        os.Getenv,
		Root:       os.Getenv("PG_HARDSTORAGE_ROOT"),
		ConfigFile: os.Getenv("PG_HARDSTORAGE_CONFIG_FILE"),
	}
}

// Resolve computes a Paths from the given Options. It does not create any
// directories; callers materialize on demand.
func Resolve(opts Options) (*Paths, error) {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	mode := opts.Mode
	if mode == ModeAuto {
		if opts.UID == 0 {
			mode = ModeSystem
		} else {
			mode = ModeUser
		}
	}

	r := &Paths{Mode: mode, ModeName: modeName(mode)}

	r.Config = resolveDomain(DomainConfig, mode, opts)
	r.ConfigFileOverride = opts.ConfigFile
	r.State = resolveDomain(DomainState, mode, opts)
	r.Cache = resolveDomain(DomainCache, mode, opts)
	r.Logs = resolveDomain(DomainLogs, mode, opts)
	r.Runtime = resolveDomain(DomainRuntime, mode, opts)
	r.SharedData = resolveDomain(DomainSharedData, mode, opts)

	// Derived paths inherit Source = SourceDerived to make their lineage
	// explicit in doctor reports.
	r.ConfigDropIn = derive(r.Config, "conf.d")
	r.Deployments = derive(r.Config, "deployments")
	r.Sinks = derive(r.Config, "sinks")
	r.Skills = derive(r.Config, "skills")
	r.Keyring = derive(r.Config, "keyring")
	r.Inflight = derive(r.State, "inflight")
	r.Crashes = derive(r.State, "crashes")

	// Per-derived-domain env overrides.  Tests + ops occasionally
	// want to point the keyring (or any other derived path) at a
	// non-default location without redefining the whole config dir
	// — useful when sharing keys across an operator's profile while
	// keeping per-deployment config files separate.  The override
	// is post-derive, so a missing env var falls back to the
	// <Config>/keyring default.  Only keyring is currently
	// supported via env; deployments / sinks / skills derivations
	// are stable enough that no caller has needed to override them
	// in practice.
	if v := opts.Env("PG_HARDSTORAGE_KEYRING_DIR"); v != "" {
		r.Keyring = Path{
			Value:  v,
			Source: SourceEnv,
			Reason: "PG_HARDSTORAGE_KEYRING_DIR",
		}
	}
	r.StateDSN = Path{
		Value:  filepath.Join(r.State.Value, "bookkeeping"),
		Source: SourceDerived,
		Reason: "<state>/bookkeeping (per-package JSON state)",
	}

	if err := r.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// resolveDomain applies the precedence chain for a single domain.
func resolveDomain(d Domain, mode Mode, opts Options) Path {
	// 1) explicit override
	if v, ok := opts.Overrides[d]; ok && v != "" {
		return Path{Value: v, Source: SourceFlag, Reason: "explicit override for " + string(d)}
	}
	// 2) per-domain env var
	envKey := "PG_HARDSTORAGE_" + strings.ToUpper(string(d)) + "_DIR"
	if v := opts.Env(envKey); v != "" {
		return Path{Value: v, Source: SourceEnv, Reason: envKey}
	}
	// 3) root override
	if opts.Root != "" {
		sub := domainSubdir(d)
		return Path{
			Value:  filepath.Join(opts.Root, sub),
			Source: SourceRoot,
			Reason: "PG_HARDSTORAGE_ROOT=" + opts.Root + " + /" + sub,
		}
	}
	// 4) Platform defaults.
	//    - Linux / *BSD / macOS: XDG (user) or FHS (system).
	//    - Windows: Known Folders (APPDATA / LOCALAPPDATA / PROGRAMDATA).
	//      Picked via runtime.GOOS rather than a build tag so the
	//      same package builds for every supported target without
	//      a per-OS file split — the cost is one cheap string
	//      compare per Resolve call.
	if isWindows() {
		return windowsPath(d, mode, opts)
	}
	if mode == ModeUser {
		return xdgPath(d, opts)
	}
	return fhsPath(d)
}

// isWindows is split out so tests can pin the OS without
// touching runtime.GOOS.  The only callers are
// resolveDomain (production) and the Windows test file.
var isWindows = func() bool { return runtime.GOOS == "windows" }

// domainSubdir returns the canonical name we tack onto $ROOT for each domain.
func domainSubdir(d Domain) string {
	switch d {
	case DomainConfig:
		return "etc"
	case DomainState:
		return "var/lib"
	case DomainCache:
		return "var/cache"
	case DomainLogs:
		return "var/log"
	case DomainRuntime:
		return "run"
	case DomainSharedData:
		return "share"
	}
	return string(d)
}

// fhsPath maps a domain to its FHS default for system-mode (root) deployments.
func fhsPath(d Domain) Path {
	switch d {
	case DomainConfig:
		return Path{Value: "/etc/pg_hardstorage", Source: SourceFHS, Reason: "FHS /etc"}
	case DomainState:
		return Path{Value: "/var/lib/pg_hardstorage", Source: SourceFHS, Reason: "FHS /var/lib"}
	case DomainCache:
		return Path{Value: "/var/cache/pg_hardstorage", Source: SourceFHS, Reason: "FHS /var/cache"}
	case DomainLogs:
		return Path{Value: "/var/log/pg_hardstorage", Source: SourceFHS, Reason: "FHS /var/log"}
	case DomainRuntime:
		return Path{Value: "/run/pg_hardstorage", Source: SourceFHS, Reason: "FHS /run"}
	case DomainSharedData:
		return Path{Value: "/usr/share/pg_hardstorage", Source: SourceFHS, Reason: "FHS /usr/share"}
	}
	return Path{Value: "/var/lib/pg_hardstorage", Source: SourceDefault, Reason: "fallback"}
}

// xdgPath maps a domain to its XDG base-directory location.
//
// We follow https://specifications.freedesktop.org/basedir-spec/ exactly:
//   - $XDG_CONFIG_HOME      defaults to $HOME/.config
//   - $XDG_DATA_HOME        defaults to $HOME/.local/share
//   - $XDG_CACHE_HOME       defaults to $HOME/.cache
//   - $XDG_STATE_HOME       defaults to $HOME/.local/state  (logs go here too)
//   - $XDG_RUNTIME_DIR      no documented fallback; if missing we synthesize
//     /run/user/<uid> (the systemd convention).
func xdgPath(d Domain, opts Options) Path {
	home := opts.HomeDir

	xdg := func(envName, def string) (string, Source, string) {
		if v := opts.Env(envName); v != "" {
			return filepath.Join(v, "pg_hardstorage"), SourceXDG, envName + "=" + v
		}
		return filepath.Join(def, "pg_hardstorage"), SourceXDG, "XDG default (" + envName + " unset)"
	}

	switch d {
	case DomainConfig:
		v, src, why := xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		return Path{Value: v, Source: src, Reason: why}
	case DomainState:
		v, src, why := xdg("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
		return Path{Value: v, Source: src, Reason: why}
	case DomainCache:
		v, src, why := xdg("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
		return Path{Value: v, Source: src, Reason: why}
	case DomainLogs:
		// XDG_STATE_HOME is the spec'd location for logs.
		v, src, why := xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
		return Path{Value: v, Source: src, Reason: why}
	case DomainRuntime:
		// XDG_RUNTIME_DIR has no documented fallback. We synthesize
		// /run/user/<uid> (the systemd convention) so the caller has a
		// uid-private place to put sockets even when the env var is unset.
		if v := opts.Env("XDG_RUNTIME_DIR"); v != "" {
			return Path{Value: filepath.Join(v, "pg_hardstorage"), Source: SourceXDG, Reason: "XDG_RUNTIME_DIR=" + v}
		}
		uid := strconv.Itoa(opts.UID)
		try := filepath.Join("/run/user", uid, "pg_hardstorage")
		return Path{Value: try, Source: SourceXDG, Reason: "synthesized: /run/user/" + uid + " (XDG_RUNTIME_DIR unset)"}
	case DomainSharedData:
		// User-mode shared data lives next to user state (read-only assets
		// installed by package manager are still under /usr/share and
		// always reachable; this is the user-writable copy).
		v, src, why := xdg("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
		return Path{Value: filepath.Join(v, "share"), Source: src, Reason: why}
	}
	return Path{Value: filepath.Join(home, ".local", "share", "pg_hardstorage"), Source: SourceDefault, Reason: "fallback"}
}

// windowsPath maps a domain to its Windows Known-Folders
// location.  We follow Microsoft's guidance for "where
// should an application put its data":
//
//	User mode (the common case — Service, scheduled task,
//	or interactive operator):
//	  Config       %APPDATA%\pg_hardstorage         (roaming, follows the user across machines)
//	  State        %LOCALAPPDATA%\pg_hardstorage\state    (machine-local user state)
//	  Cache        %LOCALAPPDATA%\pg_hardstorage\cache
//	  Logs         %LOCALAPPDATA%\pg_hardstorage\logs
//	  Runtime      %LOCALAPPDATA%\pg_hardstorage\run     (no Windows equivalent of /run; LocalAppData is the closest)
//	  SharedData   %PROGRAMDATA%\pg_hardstorage          (system-wide read-only assets, like /usr/share)
//
//	System mode (Administrator / LocalSystem service —
//	chosen explicitly via --mode system; on Windows there
//	is no UID-zero equivalent so ModeAuto stays in user
//	mode):
//	  <every domain>   %PROGRAMDATA%\pg_hardstorage\<sub>
//
// Env-var fallbacks mirror what `os.UserConfigDir` /
// `os.UserCacheDir` use: when APPDATA / LOCALAPPDATA are
// unset (e.g. inside a stripped-down service container)
// we synthesise paths under HomeDir so resolution still
// returns *something*.  A missing PROGRAMDATA falls
// through to `C:\ProgramData` — Windows installs
// universally have this directory.
func windowsPath(d Domain, mode Mode, opts Options) Path {
	get := func(envKey, fallback string) (string, string) {
		if v := opts.Env(envKey); v != "" {
			return v, "%" + envKey + "%=" + v
		}
		return fallback, "%" + envKey + "% unset, fallback " + fallback
	}

	// System mode is one shape: everything under
	// PROGRAMDATA\pg_hardstorage\<sub>.  Operators who
	// want this set --mode system explicitly; it isn't
	// the default because most Windows installs run as
	// the interactive user.
	if mode == ModeSystem {
		base, why := get("PROGRAMDATA", `C:\ProgramData`)
		sub := windowsSystemSubdir(d)
		return Path{
			Value:  filepath.Join(base, "pg_hardstorage", sub),
			Source: SourceWindows,
			Reason: why + ` + \pg_hardstorage\` + sub,
		}
	}

	// User mode — config in APPDATA, state/cache/logs/run
	// in LOCALAPPDATA, shared data in PROGRAMDATA.
	switch d {
	case DomainConfig:
		base, why := get("APPDATA", filepath.Join(opts.HomeDir, "AppData", "Roaming"))
		return Path{Value: filepath.Join(base, "pg_hardstorage"), Source: SourceWindows, Reason: why}
	case DomainState:
		base, why := get("LOCALAPPDATA", filepath.Join(opts.HomeDir, "AppData", "Local"))
		return Path{Value: filepath.Join(base, "pg_hardstorage", "state"), Source: SourceWindows, Reason: why}
	case DomainCache:
		base, why := get("LOCALAPPDATA", filepath.Join(opts.HomeDir, "AppData", "Local"))
		return Path{Value: filepath.Join(base, "pg_hardstorage", "cache"), Source: SourceWindows, Reason: why}
	case DomainLogs:
		base, why := get("LOCALAPPDATA", filepath.Join(opts.HomeDir, "AppData", "Local"))
		return Path{Value: filepath.Join(base, "pg_hardstorage", "logs"), Source: SourceWindows, Reason: why}
	case DomainRuntime:
		base, why := get("LOCALAPPDATA", filepath.Join(opts.HomeDir, "AppData", "Local"))
		return Path{Value: filepath.Join(base, "pg_hardstorage", "run"), Source: SourceWindows, Reason: why}
	case DomainSharedData:
		base, why := get("PROGRAMDATA", `C:\ProgramData`)
		return Path{Value: filepath.Join(base, "pg_hardstorage"), Source: SourceWindows, Reason: why}
	}
	// Should be unreachable — every Domain is enumerated
	// above — but keep a defensive default for a future
	// domain added without a Windows mapping.
	return Path{
		Value:  filepath.Join(opts.HomeDir, "AppData", "Local", "pg_hardstorage"),
		Source: SourceDefault,
		Reason: "fallback (unmapped domain)",
	}
}

// windowsSystemSubdir is the system-mode equivalent of
// domainSubdir: collapse FHS verbiage into Windows-style
// subdirectory names that read well under
// PROGRAMDATA\pg_hardstorage\.
func windowsSystemSubdir(d Domain) string {
	switch d {
	case DomainConfig:
		return "config"
	case DomainState:
		return "state"
	case DomainCache:
		return "cache"
	case DomainLogs:
		return "logs"
	case DomainRuntime:
		return "run"
	case DomainSharedData:
		return "share"
	}
	return string(d)
}

// derive constructs a Path that is a subdirectory of a parent. Source is
// always SourceDerived so doctor reports the lineage clearly.
func derive(parent Path, sub string) Path {
	return Path{
		Value:  filepath.Join(parent.Value, sub),
		Source: SourceDerived,
		Reason: "<" + filepath.Base(parent.Value) + ">/" + sub,
	}
}

// validate sanity-checks the resolved Paths. We surface trivially-bad
// shapes early (empty Value) rather than letting them reach a syscall.
func (p *Paths) validate() error {
	for name, v := range map[string]string{
		"config":      p.Config.Value,
		"state":       p.State.Value,
		"cache":       p.Cache.Value,
		"logs":        p.Logs.Value,
		"runtime":     p.Runtime.Value,
		"shared_data": p.SharedData.Value,
	} {
		if v == "" {
			return fmt.Errorf("paths: empty %s after resolution", name)
		}
	}
	return nil
}

// All returns the (Domain, Path) tuples in stable order — useful for
// doctor's report and JSON output.
func (p *Paths) All() []DomainPath {
	return []DomainPath{
		{Domain: "config", Label: "Config", Path: p.Config},
		{Domain: "config.drop_in", Label: "  drop-in", Path: p.ConfigDropIn},
		{Domain: "config.deployments", Label: "  deployments", Path: p.Deployments},
		{Domain: "config.sinks", Label: "  sinks", Path: p.Sinks},
		{Domain: "config.skills", Label: "  skills", Path: p.Skills},
		{Domain: "config.keyring", Label: "  keyring", Path: p.Keyring},
		{Domain: "state", Label: "State", Path: p.State},
		{Domain: "state.bookkeeping", Label: "  bookkeeping", Path: p.StateDSN},
		{Domain: "state.inflight", Label: "  inflight", Path: p.Inflight},
		{Domain: "state.crashes", Label: "  crashes", Path: p.Crashes},
		{Domain: "cache", Label: "Cache", Path: p.Cache},
		{Domain: "logs", Label: "Logs", Path: p.Logs},
		{Domain: "runtime", Label: "Runtime", Path: p.Runtime},
		{Domain: "shared_data", Label: "Shared data", Path: p.SharedData},
	}
}

// DomainPath is a labelled Path for ordered iteration.
type DomainPath struct {
	Domain string `json:"domain"`
	Label  string `json:"-"`
	Path   Path   `json:"path"`
}

func modeName(m Mode) string {
	switch m {
	case ModeSystem:
		return "system"
	case ModeUser:
		return "user"
	case ModeAuto:
		return "auto"
	}
	return "unknown"
}

func getHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}
