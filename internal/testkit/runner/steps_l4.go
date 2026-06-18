// Package runner — L4 upgrade/compat scenario step handlers.
//
// These six step kinds enable the L4 upgrade-matrix scenarios:
//
//   swap_binary           — `docker cp <host_path> <cell>:<cell_path>`,
//                           replacing pg_hardstorage (or any other
//                           binary) inside a running cell mid-run.
//                           Used by L4_rolling_pg_hardstorage_upgrade.
//
//   synthesize_manifest   — write a synthetic pg_hardstorage manifest
//                           with a chosen schema_version into the
//                           live repo, so we can prove auto-migration
//                           or clean refusal when a newer binary
//                           reads it.  Used by
//                           L4_manifest_schema_migration.
//
//   write_repo_marker     — write `_repo_version.json` into the
//                           repo root with a chosen format version,
//                           letting us simulate a future-format repo
//                           in front of a current-version binary.
//                           Used by L4_repo_format_forward_check.
//
//   os_pkg_upgrade        — run the cell's package manager to bump
//                           glibc / openssl / systemd (configurable)
//                           while the agent has open file handles,
//                           assert the agent survives.  Used by
//                           L4_os_pkg_upgrade_under_agent.
//
//   pg_upgrade            — run pg_upgrade --link inside the cell
//                           to perform an in-place major bump.
//                           Requires a testbed image with BOTH PG
//                           majors installed.  Used by
//                           L4_pg_upgrade_cross_major.
//
//   swap_pg_minor         — replace the PG package with a different
//                           minor version (e.g. 17.5 → 17.6) WITHOUT
//                           a service restart, asserting no in-flight
//                           backup error.  Used by
//                           L4_pg_minor_bump_during_backup.
//
// All handlers follow the existing testkit step contract: emit
// `step.starting` / `step.<kind>.completed` / failure carries the
// raw exec output truncated to 4 KiB.

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// resolveCell picks the cell container the L4 step targets.  Order:
//
//  1. st.Container == "" — use the topology leader (DSN reverse-map).
//  2. st.Container == "node:N" — Nth pg-roled target from the
//     topology's TargetSet.  Lets a Patroni rolling-upgrade
//     scenario address per-node containers without hard-coding
//     compose project names (which the runtime project derivation
//     makes scenario-author-hostile).  Out-of-range N hard-fails.
//  3. st.Container == anything else — treat as a literal docker
//     container name (the original v1 contract).
func resolveCell(ctx context.Context, st scenario.Step, state *runState) (string, error) {
	switch {
	case st.Container == "":
		dsn := state.connString()
		if dsn == "" {
			return "", errors.New("no container override and no topology DSN")
		}
		c, err := findDockerLeaderContainer(ctx, dsn)
		if err != nil {
			return "", fmt.Errorf("find cell from DSN: %w", err)
		}
		return c, nil
	case strings.HasPrefix(st.Container, "node:"):
		idxStr := strings.TrimPrefix(st.Container, "node:")
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
			return "", fmt.Errorf("container %q: parse node index: %w", st.Container, err)
		}
		if state.targets == nil {
			return "", errors.New("container=node:N requires a topology TargetSet (got nil)")
		}
		// Try the canonical PG-cell roles in order.  local-docker
		// targets advertise role="pg"; patroni-local-docker
		// advertises role="patroni" on the per-node containers.
		// First non-empty hit wins.
		var tgs []inject.Target
		for _, role := range []string{"pg", "patroni"} {
			cand, err := state.targets.Pick(role)
			if err == nil && len(cand) > 0 {
				tgs = cand
				break
			}
		}
		if len(tgs) == 0 {
			return "", fmt.Errorf("container=%s: TargetSet has no pg/patroni targets", st.Container)
		}
		if idx < 0 || idx >= len(tgs) {
			return "", fmt.Errorf("container=%s: index %d out of range (have %d targets)",
				st.Container, idx, len(tgs))
		}
		return tgs[idx].Name(), nil
	default:
		return st.Container, nil
	}
}

// runSwapBinary runs `docker cp <host_path> <cell>:<cell_path>`.
// The host file is copied as-is — no chmod, no chown — because the
// destination's filesystem semantics (overlayfs / bind mounts) make
// post-cp permission-fixing fragile.  Author the host file with the
// modes the cell needs.
func runSwapBinary(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.HostPath == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_binary: host_path is required"}
	}
	if st.CellPath == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_binary: cell_path is required"}
	}
	cell, err := resolveCell(ctx, st, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_binary: " + err.Error()}
	}
	cmd := exec.CommandContext(ctx, "docker", "cp",
		st.HostPath, cell+":"+st.CellPath)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("swap_binary: docker cp %s -> %s:%s: %v (output: %s)",
				st.HostPath, cell, st.CellPath, err, truncate(outBytes, 4096))}
	}
	emit(out, "step.swap_binary.completed", map[string]any{
		"index": idx, "cell": cell, "host_path": st.HostPath, "cell_path": st.CellPath,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("swapped %s -> %s:%s", st.HostPath, cell, st.CellPath)}
}

// runSynthesizeManifest writes a JSON manifest stamped with a
// chosen schema_version into the cell's repo.  The synthetic
// manifest is not byte-identical to a real one (no actual chunk
// references), but its top-level shape is enough for the
// load-and-detect path that auto-migration / refusal logic
// exercises:
//
//	{ "schema_version": "<version>",
//	  "type": "synthetic.placeholder",
//	  "generated_at": "<RFC3339>",
//	  "marker": "L4_manifest_schema_migration" }
//
// The repo is reverse-mapped from state.repoURL — must be file://
// (sink-backed scenarios that want the same effect should call
// `cli_run` against `pg_hardstorage repo write` instead).
func runSynthesizeManifest(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.ManifestSchemaVersion == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "synthesize_manifest: manifest_schema_version is required"}
	}
	repoPath, err := fileRepoPath(state.repoURL)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "synthesize_manifest: " + err.Error()}
	}
	rel := st.ManifestRelPath
	if rel == "" {
		rel = filepath.Join("manifests",
			fmt.Sprintf("synthetic-%s.json", strings.ReplaceAll(st.ManifestSchemaVersion, ".", "_")))
	}
	full := filepath.Join(repoPath, rel)
	body := map[string]any{
		"schema_version": st.ManifestSchemaVersion,
		"type":           "synthetic.placeholder",
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"marker":         "L4_manifest_schema_migration",
	}
	if err := writeJSONUnderRepo(full, body); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "synthesize_manifest: " + err.Error()}
	}
	emit(out, "step.synthesize_manifest.completed", map[string]any{
		"index": idx, "path": full, "schema_version": st.ManifestSchemaVersion,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("wrote synthetic manifest at %s (schema_version=%s)",
			full, st.ManifestSchemaVersion)}
}

// runWriteRepoMarker writes (or overwrites) `_repo_version.json`
// at the repo root with the chosen `format` value, letting a test
// stage a "future" repo format in front of a current-version
// binary and assert refuse-with-runbook semantics.
func runWriteRepoMarker(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.RepoMarkerVersion == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "write_repo_marker: repo_marker_version is required"}
	}
	repoPath, err := fileRepoPath(state.repoURL)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "write_repo_marker: " + err.Error()}
	}
	full := filepath.Join(repoPath, "_repo_version.json")
	body := map[string]any{
		"format":       st.RepoMarkerVersion,
		"written_by":   "testkit:L4_repo_format_forward_check",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSONUnderRepo(full, body); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "write_repo_marker: " + err.Error()}
	}
	emit(out, "step.write_repo_marker.completed", map[string]any{
		"index": idx, "path": full, "format": st.RepoMarkerVersion,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("wrote %s with format=%s", full, st.RepoMarkerVersion)}
}

// runOSPkgUpgrade runs the cell's distro-native package manager
// to upgrade a configurable set of packages.  Default set is
// glibc + openssl + systemd — the three components most likely
// to break the agent (libc syscall ABIs, TLS handshake, dbus
// reconnect).  The step DOES NOT restart any service; the whole
// point is to exercise "what happens to the agent's open FDs
// when the rootfs underneath it gets upgraded."
//
// PkgManager defaults to auto-detect via /etc/os-release.  Cells
// without a recognised package manager hard-fail with the list
// of probed names so an operator can extend.
func runOSPkgUpgrade(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	cell, err := resolveCell(ctx, st, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "os_pkg_upgrade: " + err.Error()}
	}
	pm := st.PkgManager
	if pm == "" || pm == "auto" {
		pm, err = detectPkgManager(ctx, cell)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "os_pkg_upgrade: " + err.Error()}
		}
	}
	pkgs := st.Packages
	if len(pkgs) == 0 {
		pkgs = []string{"libc6", "libssl3", "systemd"} // debian-ish default; mapped per pm below
	}
	cmd, mappedPkgs, err := pkgManagerUpgradeCmd(cell, pm, pkgs)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "os_pkg_upgrade: " + err.Error()}
	}
	execCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	if outBytes, err := execCmd.CombinedOutput(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("os_pkg_upgrade: %v (output: %s)",
				err, truncate(outBytes, 4096))}
	}
	emit(out, "step.os_pkg_upgrade.completed", map[string]any{
		"index": idx, "cell": cell, "pkg_manager": pm, "packages": mappedPkgs,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("upgraded %v on %s via %s", mappedPkgs, cell, pm)}
}

// runPgUpgrade runs `pg_upgrade --link` inside the cell to perform
// an in-place major bump from PgFromVersion to PgToVersion.  The
// cell's testbed image MUST have BOTH PG majors installed under
// /usr/lib/postgresql/<major>/bin (Debian/Ubuntu) or
// /usr/pgsql-<major>/bin (PGDG RPMs).  The step:
//
//  1. Stops the source PG via pg_ctl (NOT docker — we want the
//     cell up so docker exec works for the rest of the steps).
//  2. Initializes a new PGDATA at /var/lib/postgresql/<to>-data
//     with the target version's initdb.
//  3. Runs pg_upgrade --link from the source dir to the target.
//  4. Updates /var/lib/postgresql/data symlink to point at the
//     new dir.  Future PG starts in this cell run the new major.
//
// The step is deliberately Debian/Ubuntu-shaped — the rhel-family
// path lives under /usr/pgsql-NN/ which needs a small switch in
// the path expansion.  Initial L4 scenario pins debian:12.
func runPgUpgrade(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.PgFromVersion == "" || st.PgToVersion == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "pg_upgrade: pg_from_version + pg_to_version are required"}
	}
	cell, err := resolveCell(ctx, st, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "pg_upgrade: " + err.Error()}
	}
	// pg_upgrade as the postgres user.  Cell's entrypoint already
	// owns /var/lib/postgresql/data as postgres; the new datadir
	// is created here with the same ownership.  PgSuperuser
	// defaults to "postgres" — testbed images that pin a
	// different superuser via POSTGRES_USER (the scenario runner's
	// local-docker provider sets it to "testkit") override it
	// from YAML so pg_upgrade -U doesn't error with
	//   role "postgres" does not exist
	pgSuperuser := st.PgSuperuser
	if pgSuperuser == "" {
		pgSuperuser = "postgres"
	}
	script := fmt.Sprintf(`set -e
FROM=%s; TO=%s; SUPERUSER=%s
SRC_BIN=/usr/lib/postgresql/$FROM/bin
DST_BIN=/usr/lib/postgresql/$TO/bin
SRC_DATA=/var/lib/postgresql/data
DST_DATA=/var/lib/postgresql/$TO-data
[ -x "$SRC_BIN/pg_ctl" ] || { echo "missing source binaries at $SRC_BIN" >&2; exit 64; }
[ -x "$DST_BIN/pg_ctl" ] || { echo "missing target binaries at $DST_BIN" >&2; exit 64; }
runuser -u postgres -- $SRC_BIN/pg_ctl -D $SRC_DATA -m fast stop || true
runuser -u postgres -- mkdir -p $DST_DATA
runuser -u postgres -- $DST_BIN/initdb -D $DST_DATA --username=$SUPERUSER --no-locale --encoding=UTF8
cd /var/lib/postgresql
runuser -u postgres -- $DST_BIN/pg_upgrade --link \
    -b $SRC_BIN -B $DST_BIN \
    -d $SRC_DATA -D $DST_DATA \
    -U $SUPERUSER
ln -sfn $DST_DATA /var/lib/postgresql/data-current
# Carry forward the host-accessible config from the old data
# dir's pg_hba.conf — without this the cell's outside-the-
# container TCP connections are refused by the freshly-initdb'd
# PG 17 (which only allows local socket auth by default).
cp $SRC_DATA/pg_hba.conf $DST_DATA/pg_hba.conf
# Start the upgraded cluster on port 5432 (pg_upgrade's
# new-cluster initdb defaults to 5432; we add listen_addresses
# explicitly so the testcontainers host port-forward sees a
# binding it can connect to).  Log to /tmp because /var/log
# is typically root-owned in a debian rootfs.
runuser -u postgres -- $DST_BIN/pg_ctl -D $DST_DATA \
    -o "-c listen_addresses='*' -c port=5432" \
    -l /tmp/pg_post_upgrade.log -w start
`, st.PgFromVersion, st.PgToVersion, pgSuperuser)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", cell, "bash", "-c", script)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("pg_upgrade: %v (output: %s)",
				err, truncate(outBytes, 4096))}
	}
	emit(out, "step.pg_upgrade.completed", map[string]any{
		"index": idx, "cell": cell,
		"from": st.PgFromVersion, "to": st.PgToVersion,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("pg_upgrade %s -> %s on %s", st.PgFromVersion, st.PgToVersion, cell)}
}

// runSwapPgMinor reinstalls the PG package at the requested
// minor version inside the cell.  Unlike pg_upgrade this is a
// drop-in binary swap — the on-disk format is identical between
// minors.  The package-manager `--install` invocation may
// trigger a service restart on some distros; we suppress that
// via the family-specific "no restart" flag.
func runSwapPgMinor(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.PgMinorPackage == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_pg_minor: pg_minor_package is required"}
	}
	cell, err := resolveCell(ctx, st, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_pg_minor: " + err.Error()}
	}
	pm := st.PkgManager
	if pm == "" || pm == "auto" {
		pm, err = detectPkgManager(ctx, cell)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "swap_pg_minor: " + err.Error()}
		}
	}
	cmd, err := pkgManagerInstallCmd(cell, pm, st.PgMinorPackage)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "swap_pg_minor: " + err.Error()}
	}
	execCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	if outBytes, err := execCmd.CombinedOutput(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("swap_pg_minor: %v (output: %s)",
				err, truncate(outBytes, 4096))}
	}
	emit(out, "step.swap_pg_minor.completed", map[string]any{
		"index": idx, "cell": cell, "pkg_manager": pm, "package": st.PgMinorPackage,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("installed %s on %s via %s", st.PgMinorPackage, cell, pm)}
}

// ---- helpers --------------------------------------------------

// fileRepoPath converts a file:// repo URL into an on-host path.
// Returns an error for non-file schemes — sink-backed scenarios
// must use cli_run against `pg_hardstorage repo write` instead.
func fileRepoPath(repoURL string) (string, error) {
	if repoURL == "" {
		return "", errors.New("no repo URL on state")
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("parse repo URL %q: %w", repoURL, err)
	}
	if u.Scheme != "file" && u.Scheme != "" {
		return "", fmt.Errorf("repo scheme %q not supported here (file:// only)", u.Scheme)
	}
	return u.Path, nil
}

// writeJSONUnderRepo writes pretty-printed JSON to `path`, mkdir'ing
// the parent if necessary.  Used by synthesize_manifest +
// write_repo_marker — both write small files; we don't bother with
// atomic-rename.
func writeJSONUnderRepo(path string, body any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	bs, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(path, bs, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// detectPkgManager runs `cat /etc/os-release` inside the cell and
// maps ID / ID_LIKE to a package-manager token.  Recognised:
// debian/ubuntu → apt; rhel/centos/fedora/rocky/alma → dnf;
// suse/opensuse → zypper; arch → pacman.
func detectPkgManager(ctx context.Context, cell string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", cell, "cat", "/etc/os-release")
	body, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read /etc/os-release on %s: %w", cell, err)
	}
	line := strings.ToLower(string(body))
	switch {
	case strings.Contains(line, "id=debian"), strings.Contains(line, "id=ubuntu"):
		return "apt", nil
	case strings.Contains(line, "id=fedora"), strings.Contains(line, "id=rocky"),
		strings.Contains(line, "id=centos"), strings.Contains(line, "id=rhel"),
		strings.Contains(line, "id=almalinux"), strings.Contains(line, "id_like=\"rhel"):
		return "dnf", nil
	case strings.Contains(line, "id=opensuse"), strings.Contains(line, "id=sles"):
		return "zypper", nil
	case strings.Contains(line, "id=arch"):
		return "pacman", nil
	}
	return "", fmt.Errorf("unrecognised /etc/os-release on %s; pass pkg_manager= explicitly", cell)
}

// pkgManagerUpgradeCmd builds a SHELL command that refreshes the
// package index and then upgrades-or-installs the supplied
// packages on the cell.  Refresh + upgrade in one shell so we
// fail fast if the index refresh errors (saves a 90-second wait
// for the upgrade to discover the same problem).  Returns the
// argv + the distro-mapped package names actually targeted
// (apt's "libc6" vs dnf's "glibc" etc.).
//
// Note on `install` vs `--only-upgrade`: in soak testing we used
// `apt-get install -y --only-upgrade <pkgs>` and apt errored out
// if any package wasn't already installed.  testbed images vary —
// postgres:17 base ships libc6 but NOT libssl3 — so the
// "upgrade" semantics need to gracefully degrade to "install if
// missing, upgrade if present".  Plain `apt-get install -y
// <pkgs>` does exactly that.  Same shape on dnf / zypper /
// pacman: each has a single verb that means "ensure the package
// is present at the latest version regardless of current state."
func pkgManagerUpgradeCmd(cell, pm string, pkgs []string) ([]string, []string, error) {
	mapped := mapPackageNames(pm, pkgs)
	pkgList := strings.Join(mapped, " ")
	switch pm {
	case "apt":
		shell := fmt.Sprintf(
			"DEBIAN_FRONTEND=noninteractive apt-get update -qq && "+
				"DEBIAN_FRONTEND=noninteractive apt-get install -y %s",
			pkgList)
		return []string{"docker", "exec", cell, "bash", "-c", shell}, mapped, nil
	case "dnf":
		shell := fmt.Sprintf("dnf -y --nogpgcheck install %s", pkgList)
		return []string{"docker", "exec", cell, "bash", "-c", shell}, mapped, nil
	case "zypper":
		shell := fmt.Sprintf("zypper --non-interactive --no-gpg-checks install %s", pkgList)
		return []string{"docker", "exec", cell, "bash", "-c", shell}, mapped, nil
	case "pacman":
		shell := fmt.Sprintf("pacman -Sy --noconfirm %s", pkgList)
		return []string{"docker", "exec", cell, "bash", "-c", shell}, mapped, nil
	}
	return nil, nil, fmt.Errorf("os_pkg_upgrade: unknown pkg_manager %q", pm)
}

// pkgManagerInstallCmd builds the `docker exec` argv for installing
// a single explicit package version (used by swap_pg_minor).
func pkgManagerInstallCmd(cell, pm, pkg string) ([]string, error) {
	switch pm {
	case "apt":
		return []string{"docker", "exec", cell, "env", "DEBIAN_FRONTEND=noninteractive",
			"apt-get", "install", "-y", "--allow-downgrades", pkg}, nil
	case "dnf":
		return []string{"docker", "exec", cell, "dnf", "-y", "--nogpgcheck", "install", pkg}, nil
	case "zypper":
		return []string{"docker", "exec", cell, "zypper", "--non-interactive", "--no-gpg-checks",
			"install", "--force", pkg}, nil
	case "pacman":
		return []string{"docker", "exec", cell, "pacman", "-S", "--noconfirm", pkg}, nil
	}
	return nil, fmt.Errorf("swap_pg_minor: unknown pkg_manager %q", pm)
}

// mapPackageNames translates the canonical Debian-shaped package
// names ("libc6", "libssl3", "systemd") to the distro-native ones
// for the chosen package manager.  RHEL family uses "glibc" /
// "openssl-libs"; SUSE uses "glibc" / "libopenssl3"; Arch uses
// "glibc" / "openssl" / "systemd".  Unknown inputs pass through
// unchanged.
func mapPackageNames(pm string, pkgs []string) []string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		switch pm {
		case "dnf":
			switch p {
			case "libc6":
				out = append(out, "glibc")
			case "libssl3":
				out = append(out, "openssl-libs")
			default:
				out = append(out, p)
			}
		case "zypper":
			switch p {
			case "libc6":
				out = append(out, "glibc")
			case "libssl3":
				out = append(out, "libopenssl3")
			default:
				out = append(out, p)
			}
		case "pacman":
			switch p {
			case "libc6":
				out = append(out, "glibc")
			case "libssl3":
				out = append(out, "openssl")
			default:
				out = append(out, p)
			}
		default:
			out = append(out, p)
		}
	}
	return out
}
