// faults.go — first-wave fault primitives registered with
// DefaultRegistry at init time.  Each primitive is a tiny
// struct implementing the Fault interface (Name + Apply); the
// per-fault block-comment above the type documents the args
// and recovery semantics.  Second-wave primitives live in
// faults_extra.go.
package inject

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// init registers the original eleven in-tree fault primitives.
// The action prefixes match internal/testkit/config/fault.go's
// known list, which pulls live from DefaultRegistry.Names() so
// adding a primitive here makes it valid for `fault add`
// immediately.  See faults_extra.go for the second-wave
// primitives added after the 12 h soak surfaced gaps in this
// catalogue.
func init() {
	for _, f := range []Fault{
		&diskFullFault{},
		&signalFault{},
		&cgroupSqueezeFault{},
		&toxiproxyFault{},
		&sqlFault{},
		&patroniSwitchoverFault{},
		&libfaketimeFault{},
		&networkBlockFault{},
		&flipRandomByteFault{},
		&pauseArchiveFault{},
		&dockerPauseFault{},
	} {
		DefaultRegistry.Register(f)
	}
}

// --- disk_full --------------------------------------------------------

// diskFullFault fills a target's disk by writing a zero-filled
// spacer file.  Recovery removes the spacer.
//
// The fault is bounded by a `max_bytes` cap (default 256 MiB)
// so a single cell's fill cannot starve the host.  Earlier
// versions defaulted to "fill 98% of avail" with no cap and
// no host-disk-aware path, which on a parallel soak run wrote
// gigabytes into the cell's overlay layer (host
// /var/lib/docker), filling the entire host filesystem and
// cascading-failing every other cell with ENOSPC.
//
// Args:
//
//	target     — TargetSet selector, required (e.g. "repo", "pg_random")
//	max_bytes  — absolute cap on the spacer size, default 268435456 (256 MiB).
//	             Hard upper bound; never exceeded regardless of `fill`.
//	fill       — percentage of free space to consume up to max_bytes.
//	             Default "98%".  Pick min(fill% of avail, max_bytes).
//	path       — file to write the spacer into.  Default
//	             "/var/lib/pg_hardstorage/repo/.testkit-disk-fill",
//	             which is the bind-mounted repo dir — the fill
//	             stays on the operator's filesystem (typically a
//	             test-runs/ path), NOT in the container overlay
//	             on /var/lib/docker.
//	mount      — path passed to `df --output=avail` to compute
//	             free space.  Default = parent of `path`.
type diskFullFault struct{}

// Name returns "disk_full".
func (diskFullFault) Name() string { return "disk_full" }

const defaultDiskFullMaxBytes = 256 * 1024 * 1024 // 256 MiB

// Apply writes the spacer file on each picked target up to
// min(fill% of avail, max_bytes); Recovery removes it.
func (diskFullFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	fill := args["fill"]
	if fill == "" {
		fill = "98%"
	}
	path := args["path"]
	if path == "" {
		// Live in the bind-mounted repo dir so the fill goes
		// to the operator's filesystem (visible, prunable, NOT
		// shared with the docker daemon's overlay storage).
		// The earlier default "/var/lib/pg_hardstorage/.testkit-disk-fill"
		// landed in the cell's overlay → filled the host's
		// /var/lib/docker → cascade-failed every other cell.
		path = "/var/lib/pg_hardstorage/repo/.testkit-disk-fill"
	}
	mountFor := args["mount"]
	if mountFor == "" {
		// Default mount = parent dir of `path`.  Strips the
		// last `/`-segment without bringing in path/filepath
		// (the path is in the container's namespace, not the
		// operator's, so filepath would Windowsify on darwin
		// hosts driving Linux containers).
		if i := strings.LastIndex(path, "/"); i > 0 {
			mountFor = path[:i]
		} else {
			mountFor = "/var/lib/pg_hardstorage/repo"
		}
	}
	pct, err := parsePercent(fill)
	if err != nil {
		return nil, err
	}
	// `int` here matches parseDfAvail / parsePercent — those
	// helpers return int, so spacer arithmetic stays in int.
	// On 64-bit Go targets int is 64-bit anyway; the fault
	// already won't fill more than 2 GiB on 32-bit hosts.
	maxBytes := defaultDiskFullMaxBytes
	if raw := args["max_bytes"]; raw != "" {
		mb, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("disk_full: max_bytes %q: %w", raw, err)
		}
		if mb <= 0 {
			return nil, fmt.Errorf("disk_full: max_bytes must be > 0 (got %d)", mb)
		}
		maxBytes = mb
	}

	for _, t := range tgs {
		out, err := t.Exec(ctx, "df", "--output=avail", "-B1", mountFor)
		if err != nil {
			return nil, fmt.Errorf("disk_full: df %s: %w", t.Name(), err)
		}
		avail, err := parseDfAvail(string(out))
		if err != nil {
			return nil, fmt.Errorf("disk_full: parse df: %w", err)
		}
		// Spacer = min(avail × pct/100, max_bytes).  The cap
		// is the load-bearing piece: it prevents one cell
		// from monopolising the shared host filesystem.
		spacer := avail * pct / 100
		if spacer > maxBytes {
			spacer = maxBytes
		}
		// dd in 1 MiB blocks; round down to whole blocks so a
		// sub-MiB spacer (rare, but possible on tight hosts)
		// becomes 0 rather than failing on a fractional count.
		blocks := spacer / (1024 * 1024)
		if blocks <= 0 {
			// Nothing meaningful to fill; degrade to a no-op
			// rather than an error so a tightly-quota'd test
			// host still completes the soak.
			continue
		}
		if _, err := t.Exec(ctx,
			"sh", "-c",
			fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=%d 2>&1 || true",
				path, blocks)); err != nil {
			return nil, fmt.Errorf("disk_full: dd on %s: %w", t.Name(), err)
		}
	}

	// Recovery: remove every spacer.  Best-effort: if the host's
	// filesystem is so wedged that even `rm` fails (the
	// classic ENOSPC-during-runc-temp-write cascade), fall
	// back to truncating the file in place via `:` redirection
	// — `>FILE` opens for writing with O_TRUNC, releasing the
	// blocks without needing a runc temp file.  If THAT also
	// fails, surface the error so the operator sees the wedge.
	return func(ctx context.Context) error {
		for _, t := range tgs {
			if _, err := t.Exec(ctx, "rm", "-f", path); err == nil {
				continue
			} else {
				// Fallback: truncate in place.  An empty `:`
				// builtin + redirection works in dash / bash /
				// busybox sh and doesn't allocate runc temp.
				if _, err2 := t.Exec(ctx,
					"sh", "-c", fmt.Sprintf(": > %s", path)); err2 != nil {
					return fmt.Errorf("disk_full recovery: rm on %s: %w (truncate fallback also failed: %v)",
						t.Name(), err, err2)
				}
			}
		}
		return nil
	}, nil
}

// --- signal -----------------------------------------------------------

// signalFault sends a Unix signal.  Irreversible.
type signalFault struct{}

// Name returns "signal".
func (signalFault) Name() string { return "signal" }

// Apply sends the requested signal to each picked target and
// follows it with Start so docker's "killed by user" flag does
// not suppress the cell's auto-restart.  Irreversible.
func (signalFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	sigStr, err := args.Require("sig")
	if err != nil {
		return nil, err
	}
	sig, err := strconv.Atoi(sigStr)
	if err != nil {
		return nil, fmt.Errorf("signal: sig must be a number (got %q)", sigStr)
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		if err := t.Signal(ctx, sig); err != nil {
			// If the container is already down — it crashed or was
			// OOM-killed before this fault fired — the signal's
			// intended effect (process termination) has already
			// happened.  Don't mis-report a pre-existing cell
			// crash as a signal fault failure; fall through to
			// Start so the cell is recovered and the soak keeps
			// measuring real recovery semantics.  The crash itself
			// still surfaces in that cell's backup/verify health.
			if !errors.Is(err, ErrTargetNotRunning) {
				return nil, fmt.Errorf("signal: %s: %w", t.Name(), err)
			}
		}
		// Always follow Signal with Start (idempotent on a still-
		// running container — docker treats it as a no-op).  Why
		// this is load-bearing: docker categorises `docker kill`
		// (any signal, not just SIGKILL) as a user-initiated stop.
		// Both `restart: unless-stopped` and `restart: always`
		// honour that flag and DO NOT auto-restart the container
		// even though the container exited from a kill rather than
		// a docker-stop.  Without Start the cell stays Exited(137)
		// forever and every subsequent fault_apply / verify against
		// it records "container is not running" — the soak's
		// recovery metric stops measuring anything real.  Start
		// brings it back so the orchestrator's heal_window logic
		// can observe actual PG-recovery semantics.
		//
		// SIGTERM (15) targets that catch the signal and exit
		// gracefully also benefit: their PID 1 exits, container
		// goes Exited(143), unless-stopped doesn't auto-restart
		// for the same reason.  Start is correct here too.
		if err := t.Start(ctx); err != nil {
			return nil, fmt.Errorf("signal: %s: post-signal start: %w", t.Name(), err)
		}
	}
	return NoRecovery, nil
}

// --- cgroup_squeeze ---------------------------------------------------

// cgroupSqueezeFault tightens a target's cgroup memory limit
// to force back-pressure / OOM-kill.  Recovery restores the
// limit to "unlimited".
//
// We DELIBERATELY don't write /sys/fs/cgroup/memory.max from
// inside the container any more — Docker mounts cgroupfs
// read-only by default, so the in-container write fails on
// every modern host:
//
//	cgroup_squeeze: /sys/fs/cgroup/memory.max:
//	Read-only file system
//
// Instead the runtime uses `docker update --memory=N` (via
// Target.SetMemoryLimit) which is the canonical out-of-band
// path and works on cgroup-v1 and v2 alike.
type cgroupSqueezeFault struct{}

// Name returns "cgroup_squeeze".
func (cgroupSqueezeFault) Name() string { return "cgroup_squeeze" }

// Apply tightens each picked target's memory limit to
// max_bytes via `docker update --memory`; Recovery restores
// the limit to unlimited.
func (cgroupSqueezeFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	maxBytesRaw := args["max_bytes"]
	if maxBytesRaw == "" {
		maxBytesRaw = "67108864" // 64 MiB
	}
	maxBytes, err := strconv.ParseInt(maxBytesRaw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cgroup_squeeze: max_bytes %q: %w", maxBytesRaw, err)
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}

	for _, t := range tgs {
		if err := t.SetMemoryLimit(ctx, maxBytes); err != nil {
			return nil, fmt.Errorf("cgroup_squeeze: %s: %w", t.Name(), err)
		}
	}

	return func(ctx context.Context) error {
		// Recovery has two jobs:
		//
		//  1. -1 → "no limit": lift the squeeze.
		//
		//  2. Restart PG if the squeeze OOM-killed it.  A 32 MiB
		//     memory.max is far below PostgreSQL's shared-memory
		//     footprint, so the kernel OOM killer reliably kills
		//     the postmaster mid-fault.  The testbed entrypoint
		//     starts PG exactly once (`pg_ctl start` then `exec
		//     tail -F`) — there is NO supervisor — so a killed
		//     postmaster stays dead for the rest of the cell's
		//     life.  Every subsequent `pg_hardstorage backup`
		//     then fails with `storage.unreachable`, which is
		//     pg_hardstorage behaving CORRECTLY (you cannot back
		//     up a down database) but is scored as a spurious
		//     cell-failure.  Lifting the limit alone is not a
		//     complete recovery: the fault must put the cell
		//     back the way it found it, PG included.
		//
		// Order matters: lift the limit FIRST so the restarted
		// postmaster has memory to start in.
		var errs []string
		for _, t := range tgs {
			if err := t.SetMemoryLimit(ctx, -1); err != nil {
				errs = append(errs, fmt.Sprintf("%s: lift limit: %v", t.Name(), err))
			}
		}
		for _, t := range tgs {
			// Only PG cells have a postmaster to restart.  A
			// cgroup_squeeze aimed at a repo / agent / kms
			// target has nothing to do here.
			if t.Role() != "pg" {
				continue
			}
			if err := restartPGIfDown(ctx, t); err != nil {
				// A heavy squeeze can OOM-kill not just the
				// postmaster but the whole container (docker's
				// OOM watchdog kills PID 1's process tree).
				// When that happens `docker exec` fails with
				// "container is not running" and restartPGIfDown
				// can't even open a shell to bring PG back up.
				// Detect the typed sentinel, Start the container
				// first, RE-APPLY the unlimit (docker update --memory
				// on a stopped container has been observed to not
				// always persist across Start — the soak's 4th
				// run got the freshly-Start'd container with the
				// pre-squeeze limit still in effect and the next
				// docker exec was OOM-killed mid-script with
				// exit 137), then retry restartPGIfDown.
				if errors.Is(err, ErrTargetNotRunning) {
					if serr := t.Start(ctx); serr != nil {
						errs = append(errs, fmt.Sprintf("%s: start container: %v", t.Name(), serr))
						continue
					}
					if merr := t.SetMemoryLimit(ctx, -1); merr != nil {
						errs = append(errs, fmt.Sprintf("%s: re-lift limit after Start: %v", t.Name(), merr))
						continue
					}
					if err2 := restartPGIfDown(ctx, t); err2 != nil {
						errs = append(errs, fmt.Sprintf("%s: restart PG after container Start: %v", t.Name(), err2))
					}
					continue
				}
				errs = append(errs, fmt.Sprintf("%s: restart PG: %v", t.Name(), err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("cgroup_squeeze recovery: %s", strings.Join(errs, "; "))
		}
		return nil
	}, nil
}

// restartPGIfDown brings PostgreSQL back up inside a cell when it
// is no longer accepting connections — the recovery path for
// cgroup_squeeze, whose memory clamp OOM-kills the postmaster.
//
// The shell snippet handles the two container shapes this codebase
// runs PG in:
//
//   - stock postgres:N image (local-docker scenario provider):
//     the postmaster is PID 1.  It MUST NOT be SIGKILLed — that
//     would kill the container.  PG here is supervised by Docker's
//     restart policy, so the snippet just waits for connections
//     once the squeeze is lifted.
//   - testbed family image (soak fleet): PID 1 is a non-PG reaper
//     ("exec tail -F" in entrypoint-pg.sh) and there is no
//     supervisor, so the snippet owns the full kill-orphans /
//     clear-lockfiles / pg_ctl-start restart.
//
// For the testbed path the snippet mirrors entrypoint-pg.sh: it
// walks the same pg_ctl search path (pgdg-apt, distro-packaged,
// RHEL /usr/pgsql-*) and starts PG as $PGUSER with the same
// logfile.
//
// Liveness is decided by a real `psql -c 'select 1'` probe, NOT
// `pg_ctl status`.  `pg_ctl status` only checks whether the PID in
// postmaster.pid is alive — and in a container's small, fast-
// recycling PID namespace the OOM-killed postmaster's PID is
// routinely reassigned to an unrelated process within the 60 s
// fault window, so `pg_ctl status` reports "server running" for a
// postmaster that is actually dead.  A connection probe cannot be
// fooled this way.
//
// When PG is down a plain `pg_ctl start` is NOT enough.  The OOM
// killer SIGKILLs the postmaster without cleanup, which leaves a
// fresh postmaster three booby traps:
//
//   - orphaned backend / auxiliary processes that outlive the
//     postmaster (a backend only notices postmaster death when it
//     next checks for interrupts).  While alive they hold the
//     shared-memory interlock; once SIGKILL'd they turn into
//     zombies, because the testbed's PID 1 ("exec tail -F" in
//     entrypoint-pg.sh) is not a reaping init.  A zombie holds no
//     resources but DOES keep its PID allocated.
//   - a stale $PGDATA/postmaster.pid, and
//   - stale socket lock files ($socketdir/.s.PGSQL.<port>.lock).
//
// Both lock files record the dead postmaster's PID.  Because a
// zombie keeps that PID allocated, `kill -0 <pid>` still succeeds,
// so a fresh postmaster believes the old one is alive and aborts
// with a "lock file ... already exists" FATAL.
//
// So before starting we SIGKILL every LIVE `postgres` process
// (matched on comm, which leaves pg_ctl/psql alone; zombies are
// skipped — they cannot be killed and hold nothing) to release
// the shared-memory interlock, then delete every stale lock file.
// We only reach this path once the probe has proven PG is
// unreachable, so there is no live server to disturb.  `-w -t 120`
// then blocks pg_ctl until crash recovery completes and PG accepts
// connections; the final probe loop confirms it before we return.
func restartPGIfDown(ctx context.Context, t Target) error {
	const script = `
set -u
PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PGUSER="${PGUSER:-postgres}"
PG_CTL=""
PG_BIN=""
for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /usr/bin; do
    if [ -x "$d/pg_ctl" ]; then PG_CTL="$d/pg_ctl"; PG_BIN="$d"; break; fi
done
[ -n "$PG_CTL" ] || { echo "pg_ctl not found" >&2; exit 1; }
# Liveness probe: pg_isready, not a psql select.  pg_isready
# reports whether the server accepts connections BEFORE role/auth
# is checked, so it needs neither a valid DB role nor a privilege
# drop -- the local-docker provider initdb's its cluster with the
# superuser "testkit" (not "postgres"), so a psql probe hard-coded
# to role postgres would always fail and wrongly conclude PG is
# down.  pg_isready ships in the same bin dir as pg_ctl.
probe() { "$PG_BIN/pg_isready" -q >/dev/null 2>&1; }
# is_live_pg PID -> succeeds for a non-zombie process named
# "postgres".  /proc is read directly so no procps/pkill
# dependency is assumed across distros; comm is "postgres" for
# server processes and "pg_ctl"/"psql" for the tools, so the
# restart helpers themselves are never matched.  Zombies (State
# Z) are excluded: they cannot be killed and hold no resources.
# PID 1 is NEVER matched: SIGKILLing the container's init kills
# the container itself (fatal on the stock postgres:N image,
# where the postmaster IS PID 1 — see the supervised-image
# branch below).
is_live_pg() {
    [ "$1" = 1 ] && return 1
    [ "$(cat /proc/$1/comm 2>/dev/null)" = postgres ] || return 1
    case "$(sed -n 's/^State:[[:space:]]*//p' /proc/$1/status 2>/dev/null)" in
        Z*) return 1 ;;
        '') return 1 ;;
        *)  return 0 ;;
    esac
}
count_live_pg() {
    n=0
    for p in /proc/[0-9]*; do
        is_live_pg "${p#/proc/}" && n=$((n + 1))
    done
    echo "$n"
}
kill_live_pg() {
    for p in /proc/[0-9]*; do
        pid="${p#/proc/}"
        is_live_pg "$pid" && kill -9 "$pid" 2>/dev/null
    done
    return 0
}
# Wait up to 60 s for PG to come back on its own first.  The
# testbed compose stanza uses restart: unless-stopped, so
# Docker auto-restarts a whole-cell OOM kill; the entrypoint
# then re-runs initdb / pg_ctl and PG becomes ready typically
# within 20-30 s.  Racing the entrypoint's own pg_ctl machinery
# with our SIGKILL-everything-named-postgres script is the
# worst thing this recovery can do — it kills the entrypoint's
# pg_ctl child, the entrypoint exits non-zero, the container
# exits again, our docker exec session dies with SIGKILL
# (exit 137).  The soak runs 4 and 5 both reproduced this race
# at the same opensuse-leap-15-pg15 iteration.  Polling lets
# the entrypoint finish on its own before we resort to the
# cleanup-and-restart dance.
i=0
while [ "$i" -lt 60 ]; do
    if probe; then
        echo "pg accepting connections (after ${i}s wait)"
        exit 0
    fi
    i=$((i + 1))
    sleep 1
done
echo "pg unreachable after 60 s wait — cleaning up and restarting"
# Two container shapes reach this point:
#
#   - stock postgres:N image (the local-docker scenario
#     provider): the postmaster is PID 1, supervised by Docker's
#     restart policy.  We MUST NOT kill it — SIGKILLing PID 1
#     kills the whole container, taking this docker-exec session
#     down with it (exit 137).  The kill/rm/pg_ctl dance below
#     also makes no sense when PG is the init process.  Once the
#     cgroup squeeze is lifted the postmaster self-heals, or — if
#     it was OOM-killed — Docker restarts the container; either
#     way the right move is simply to wait for connections.
#
#   - testbed family image: PID 1 is a non-PG reaper
#     (entrypoint-pg.sh's "exec tail -F"), there is no
#     supervisor, and a killed postmaster stays dead — so we
#     own the full restart, below.
if [ "$(cat /proc/1/comm 2>/dev/null)" = postgres ]; then
    echo "postmaster is PID 1 (supervised image) — waiting for it to recover"
    i=0
    while [ "$i" -lt 90 ]; do
        if probe; then
            echo "pg accepting connections"
            exit 0
        fi
        i=$((i + 1))
        sleep 1
    done
    echo "pg still unreachable after 90s wait" >&2
    exit 1
fi
# SIGKILL every live postgres process so the shared-memory
# interlock is released, looping until none remain (re-kill each
# pass to catch a straggler that was in an uninterruptible
# syscall on the first sweep).
j=0
while [ "$j" -lt 20 ]; do
    kill_live_pg
    sleep 1
    [ "$(count_live_pg)" -eq 0 ] && break
    j=$((j + 1))
done
# Drop every stale lock file the SIGKILL'd postmaster could not
# clear: the data-dir pidfile plus the per-socket lock files in
# whichever socket directories this build uses.  Unmatched globs
# stay literal and 'rm -f' swallows them, so listing all the
# usual socket dirs is harmless.
rm -f "$PGDATA/postmaster.pid"
rm -f /run/postgresql/.s.PGSQL.*.lock \
      /var/run/postgresql/.s.PGSQL.*.lock \
      /tmp/.s.PGSQL.*.lock 2>/dev/null
su -s /bin/sh -c "$PG_CTL -D $PGDATA -l /var/log/postgresql.log -w -t 120 start" "$PGUSER" || true
i=0
while [ "$i" -lt 30 ]; do
    if probe; then
        echo "pg restarted and accepting connections"
        exit 0
    fi
    i=$((i + 1))
    sleep 1
done
echo "pg still unreachable after restart attempt" >&2
su -s /bin/sh -c "tail -n 30 /var/log/postgresql.log" "$PGUSER" 2>/dev/null >&2 || true
exit 1
`
	if out, err := t.Exec(ctx, "sh", "-c", script); err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- toxiproxy --------------------------------------------------------

// toxiproxyFault enables a Toxiproxy toxic on the named proxy.
// The soak driver must have provisioned a toxiproxy server +
// proxy in front of the storage URL; this primitive only
// flips toxics on/off via the toxiproxy CLI.
type toxiproxyFault struct{}

// Name returns "toxiproxy".
func (toxiproxyFault) Name() string { return "toxiproxy" }

// Apply enables the named toxic on the configured proxy via
// `toxiproxy-cli toxic add`; Recovery removes it.
func (toxiproxyFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	proxy := args["proxy"]
	if proxy == "" {
		proxy = "repo"
	}
	toxicName := args["toxic"]
	if toxicName == "" {
		toxicName = "soak-induced"
	}
	kind := args["type"]
	if kind == "" {
		// Default to a 50% latency toxic — the most-asked-for
		// shape from operators.
		kind = "latency"
	}
	// Pick the toxiproxy admin target.  Soak driver wires it
	// in with role="toxiproxy".
	tgs, err := ts.Pick("toxiproxy")
	if err != nil {
		return nil, err
	}
	if len(tgs) == 0 {
		return nil, fmt.Errorf("toxiproxy: no target with role=toxiproxy in fleet")
	}
	// `toxiproxy-cli toxic add -t <type> -n <name> -a <attr=val>... <proxy>`
	cli := []string{"toxiproxy-cli", "toxic", "add", "-t", kind, "-n", toxicName}
	for k, v := range args {
		switch k {
		case "proxy", "toxic", "type", "_positional":
			continue
		}
		cli = append(cli, "-a", k+"="+v)
	}
	cli = append(cli, proxy)

	for _, t := range tgs {
		if _, err := t.Exec(ctx, cli...); err != nil {
			return nil, fmt.Errorf("toxiproxy: enable %s on %s: %w", toxicName, t.Name(), err)
		}
	}
	return func(ctx context.Context) error {
		for _, t := range tgs {
			_, _ = t.Exec(ctx, "toxiproxy-cli", "toxic", "remove", "-n", toxicName, proxy)
		}
		return nil
	}, nil
}

// --- sql --------------------------------------------------------------

// sqlFault runs a single SQL statement against the target's
// PG via psql.  Used for slot drops, lock acquisition, etc.
// Irreversible at the inject layer; recovery is the
// caller's job (e.g. by running another `sql` fault).
type sqlFault struct{}

// Name returns "sql".
func (sqlFault) Name() string { return "sql" }

// Apply runs the single positional statement (or stmt= arg)
// via psql against the picked target's PG.  Irreversible at
// the inject layer.
func (sqlFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	stmt := args["_positional"]
	if stmt == "" {
		stmt = args["stmt"]
	}
	if stmt == "" {
		return nil, fmt.Errorf("sql: missing statement (use sql(\"...\") or sql(stmt=\"...\"))")
	}
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	user := args["user"]
	if user == "" {
		user = "postgres"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// Run psql as `user` via `su` — the testbed images don't
	// ship `sudo`, and `-s /bin/sh` is required because the
	// postgres account's login shell is nologin.
	cmd := "psql -c " + shSingleQuote(stmt)
	for _, t := range tgs {
		if _, err := t.Exec(ctx, "su", "-s", "/bin/sh", "-c", cmd, user); err != nil {
			return nil, fmt.Errorf("sql on %s: %w", t.Name(), err)
		}
	}
	return NoRecovery, nil
}

// shSingleQuote wraps s in single quotes safe for a POSIX sh
// command line, rendering any embedded single quote as the
// standard '\” escape.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- patroni_switchover ----------------------------------------------

// patroniSwitchoverFault triggers a Patroni leader switch via
// the cluster's REST API.  Reversibility: the cluster will
// settle into a new leader; calling switchover again can
// promote any specific node, but "revert to the previous
// leader" is not generally meaningful in a Patroni topology
// where the prior leader may already be a healthy replica.
//
// Wire shape (Patroni 3.x+):
//
//	GET  /cluster                                    → JSON cluster state
//	POST /failover  {"leader":"X","candidate":"Y"}   → asynchronous failover
//
// We pre-discover the cluster from one node, pick a healthy
// replica as the candidate, and POST /failover with the body
// Patroni requires.  Earlier code did a body-less POST to
// /switchover which Patroni 4.x rejects with HTTP 411
// (Length Required) — and even on older Patroni the body-less
// form returns "candidate must be specified".
//
// Targeting:
//   - target=patroni / patroni_random / patroni_all → fault uses
//     ANY one of the matched targets to issue the API call.
//     (Patroni REST exposes the same cluster view on every node;
//     one call is enough — this is not a per-node fault.)
//   - target=<exact-name> → uses that node's REST endpoint.
type patroniSwitchoverFault struct{}

// Name returns "patroni_switchover".
func (patroniSwitchoverFault) Name() string { return "patroni_switchover" }

// Apply polls /cluster, picks a (leader, streaming-replica)
// pair, and POSTs /failover until Patroni accepts the request
// or the 90s deadline elapses.  Irreversible — the cluster
// settles on the new leader.
func (patroniSwitchoverFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	role := args["target"]
	if role == "" {
		role = "patroni"
	}
	tgs, err := ts.Pick(role)
	if err != nil {
		return nil, err
	}
	port := args["port"]
	if port == "" {
		port = "8008"
	}
	if len(tgs) == 0 {
		return nil, fmt.Errorf("patroni_switchover: no targets matched %q", role)
	}
	// Issue the API calls through the first target — Patroni
	// REST returns the same cluster view from any node, and one
	// /failover call is sufficient to drive the leader change.
	t := tgs[0]
	clusterURL := fmt.Sprintf("http://127.0.0.1:%s/cluster", port)
	failoverURL := fmt.Sprintf("http://127.0.0.1:%s/failover", port)

	// Patroni rejects /failover with HTTP 503 ("failover is not
	// possible") whenever the chosen candidate is not a caught-up,
	// streaming replica — a routine transient right after a seed /
	// load burst while the replica drains its replication backlog.
	// Re-poll /cluster and retry the POST until the cluster is
	// switchover-ready or the deadline elapses, so a scenario that
	// injects a switchover immediately after load does not flake.
	const switchoverDeadline = 90 * time.Second
	deadline := time.Now().Add(switchoverDeadline)
	var lastErr error
	for attempt := 1; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		clusterBody, cerr := t.Exec(ctx, "curl", "-fsS", clusterURL)
		if cerr != nil {
			lastErr = fmt.Errorf("GET /cluster on %s: %w", t.Name(), cerr)
		} else if leader, candidate, perr := pickPatroniFailoverPair(clusterBody); perr != nil {
			lastErr = fmt.Errorf("parse cluster from %s: %w", t.Name(), perr)
		} else {
			body, _ := json.Marshal(map[string]string{
				"leader":    leader,
				"candidate": candidate,
			})
			if _, perr := t.Exec(ctx,
				"curl", "-fsS", "-XPOST",
				"-H", "Content-Type: application/json",
				"-d", string(body),
				failoverURL); perr == nil {
				return NoRecovery, nil
			} else {
				lastErr = fmt.Errorf("POST /failover on %s (leader=%s candidate=%s): %w",
					t.Name(), leader, candidate, perr)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("patroni_switchover: cluster not switchover-ready within %s (last attempt %d: %v)",
				switchoverDeadline, attempt, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// pickPatroniFailoverPair parses Patroni's /cluster JSON and
// picks (current leader, healthy replica) for a /failover body.
// Returns an error if the cluster doesn't have at least one
// leader and one running replica — without both, /failover has
// no useful work to do and Patroni would reject the request.
func pickPatroniFailoverPair(body []byte) (leader, candidate string, err error) {
	var cluster struct {
		Members []struct {
			Name  string `json:"name"`
			Role  string `json:"role"`
			State string `json:"state"`
		} `json:"members"`
	}
	if jerr := json.Unmarshal(body, &cluster); jerr != nil {
		return "", "", fmt.Errorf("decode /cluster: %w", jerr)
	}
	for _, m := range cluster.Members {
		if m.Role == "leader" && m.State == "running" {
			leader = m.Name
			break
		}
	}
	if leader == "" {
		return "", "", fmt.Errorf("no running leader in cluster (members: %d)", len(cluster.Members))
	}
	// Patroni reports replicas as role="replica" with
	// state="streaming" (caught up) or "running" (briefly, during
	// state transitions / while still draining backlog).  Prefer a
	// streaming replica: Patroni rejects /failover with HTTP 503
	// when the candidate lags beyond maximum_lag_on_failover, and a
	// merely "running" replica is the common lagging case.  Fall
	// back to a "running" replica only when no streaming one exists.
	var fallback string
	for _, m := range cluster.Members {
		if m.Name == leader || m.Role != "replica" {
			continue
		}
		if m.State == "streaming" {
			candidate = m.Name
			break
		}
		if m.State == "running" && fallback == "" {
			fallback = m.Name
		}
	}
	if candidate == "" {
		candidate = fallback
	}
	if candidate == "" {
		return "", "", fmt.Errorf("no healthy replica candidate (cluster has only the leader %q)", leader)
	}
	return leader, candidate, nil
}

// --- libfaketime ------------------------------------------------------

// libfaketimeFault writes a faketime envelope to /etc/faketimerc
// inside the target so subsequent agent / PG restarts inherit
// the skew.  Recovery wipes /etc/faketimerc.
//
// Note: an already-running process will not pick up the new
// value until restart — by design, faketime via /etc/faketimerc
// is process-start-time only.  For mid-flight skew the inject
// driver pairs this fault with a `signal sig=15` to force
// restart.
type libfaketimeFault struct{}

// Name returns "libfaketime".
func (libfaketimeFault) Name() string { return "libfaketime" }

// Apply writes the requested skew into /etc/faketimerc on
// each picked target; Recovery deletes the file.  New skew
// takes effect on the next process restart.
func (libfaketimeFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	skew, err := args.Require("skew")
	if err != nil {
		return nil, err
	}
	target := args["target"]
	if target == "" {
		target = "agent"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		if err := t.CopyIn(ctx, "/etc/faketimerc", []byte(skew+"\n")); err != nil {
			return nil, fmt.Errorf("libfaketime on %s: %w", t.Name(), err)
		}
	}
	return func(ctx context.Context) error {
		for _, t := range tgs {
			_, _ = t.Exec(ctx, "rm", "-f", "/etc/faketimerc")
		}
		return nil
	}, nil
}

// --- network_block ----------------------------------------------------

// networkBlockFault adds an iptables OUTPUT DROP rule for the
// configured destination.  The container must have
// CAP_NET_ADMIN — the soak driver wires this when constructing
// the cell.  Recovery deletes the rule.
type networkBlockFault struct{}

// Name returns "network_block".
func (networkBlockFault) Name() string { return "network_block" }

// Apply appends an iptables OUTPUT DROP rule for the
// destination on each picked source target; Recovery deletes
// the same rule.
func (networkBlockFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	dst, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	source := args["source"]
	if source == "" {
		source = "agent"
	}
	tgs, err := ts.Pick(source)
	if err != nil {
		return nil, err
	}

	// We add the rule and remember the exact argv so Recovery
	// can replay it with -D instead of -A.
	addArgv := []string{"iptables", "-A", "OUTPUT", "-d", dst, "-j", "DROP"}
	delArgv := []string{"iptables", "-D", "OUTPUT", "-d", dst, "-j", "DROP"}

	for _, t := range tgs {
		if _, err := t.Exec(ctx, addArgv...); err != nil {
			return nil, fmt.Errorf("network_block: %s: %w", t.Name(), err)
		}
	}
	return func(ctx context.Context) error {
		for _, t := range tgs {
			_, _ = t.Exec(ctx, delArgv...)
		}
		return nil
	}, nil
}

// --- flip_random_byte -------------------------------------------------

// flipRandomByteFault picks a file matching prefix on the repo
// target, flips a random byte, and writes it back.  This
// simulates bit-rot or partial corruption mid-flight.  No
// recovery — the bit is now flipped.
type flipRandomByteFault struct{}

// Name returns "flip_random_byte".
func (flipRandomByteFault) Name() string { return "flip_random_byte" }

// Apply picks a random file under prefix on each picked
// target, XORs a random byte, and writes it back.
// Irreversible.
func (flipRandomByteFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	prefix, err := args.Require("prefix")
	if err != nil {
		return nil, err
	}
	target := args["target"]
	if target == "" {
		target = "repo"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		// Pick a random file under the prefix.  We use
		// `find ... | sort -R | head -1` rather than a Go
		// implementation so the choice happens inside the
		// target's filesystem view.
		out, err := t.Exec(ctx, "sh", "-c",
			fmt.Sprintf("find %s -type f 2>/dev/null | shuf -n 1", prefix))
		if err != nil {
			return nil, fmt.Errorf("flip_random_byte: locate %s on %s: %w", prefix, t.Name(), err)
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			return nil, fmt.Errorf("flip_random_byte: no files under %s on %s", prefix, t.Name())
		}
		body, err := t.CopyOut(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("flip_random_byte: read %s: %w", path, err)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("flip_random_byte: %s is empty", path)
		}
		// Crypto-rand the offset so different soak runs flip
		// different bits.  Mod in unsigned space and convert
		// after — Go's `%` preserves dividend sign so casting
		// the uint64 to int first risks a negative offset.
		offsetBuf := make([]byte, 8)
		_, _ = rand.Read(offsetBuf)
		off64 := uint64(offsetBuf[0])<<56 | uint64(offsetBuf[1])<<48 |
			uint64(offsetBuf[2])<<40 | uint64(offsetBuf[3])<<32 |
			uint64(offsetBuf[4])<<24 | uint64(offsetBuf[5])<<16 |
			uint64(offsetBuf[6])<<8 | uint64(offsetBuf[7])
		offset := int(off64 % uint64(len(body)))
		body[offset] ^= 0x01
		if err := t.CopyIn(ctx, path, body); err != nil {
			return nil, fmt.Errorf("flip_random_byte: write %s: %w", path, err)
		}
	}
	return NoRecovery, nil
}

// --- pause_archive ----------------------------------------------------

// pauseArchiveFault touches a sentinel file the agent watches.
// The file's presence pauses WAL archiving until removed;
// Recovery removes it.  The agent's archive loop honouring
// the sentinel is the runtime contract — it does not need to
// be present for this primitive to "succeed" at the inject
// layer (the touch / rm are the assertions).
type pauseArchiveFault struct{}

// Name returns "pause_archive".
func (pauseArchiveFault) Name() string { return "pause_archive" }

// Apply touches the agent's archive-pause sentinel on each
// picked target; Recovery removes it.
func (pauseArchiveFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "agent"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	sentinel := args["sentinel"]
	if sentinel == "" {
		sentinel = "/var/lib/pg_hardstorage/.archive-paused"
	}
	for _, t := range tgs {
		if _, err := t.Exec(ctx, "touch", sentinel); err != nil {
			return nil, fmt.Errorf("pause_archive: %s: %w", t.Name(), err)
		}
	}
	return func(ctx context.Context) error {
		for _, t := range tgs {
			_, _ = t.Exec(ctx, "rm", "-f", sentinel)
		}
		return nil
	}, nil
}

// --- shared parsing helpers -------------------------------------------

// parsePercent accepts "98" or "98%" and returns 98.
func parsePercent(s string) (int, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a percentage: %q", s)
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("percentage out of range: %d", n)
	}
	return n, nil
}

// parseDfAvail extracts the availability number from
// `df --output=avail -B1` output, which looks like:
//
//	Avail
//	     12345678
func parseDfAvail(out string) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "Avail" {
			continue
		}
		// First numeric line wins.
		n, err := strconv.Atoi(line)
		if err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no numeric availability in df output: %q", out)
}

// --- docker_pause -----------------------------------------------------

// dockerPauseFault freezes a target container via
// `docker pause <name>` for the duration of the scenario
// step's heal window, then unpauses on Recovery.  The
// canonical use case is "make the sink unreachable for N
// seconds" — a docker-paused container holds open TCP
// connections (no RST) but stops processing IO, simulating
// the network-storm / GC-pause class of cloud-storage
// outage that backup-tool retry budgets must survive.
//
// Action shape:
//
//	docker_pause(target=sink)
//	docker_pause(target=sink_random)
//	docker_pause(target=<exact-container-name>)
//
// Operates on docker targets only; SSH-style targets (when
// they land) get a separate primitive that doesn't pretend
// to be docker pause.
type dockerPauseFault struct{}

// Name returns "docker_pause".
func (dockerPauseFault) Name() string { return "docker_pause" }

// Apply freezes each picked DockerTarget via `docker pause`;
// Recovery unpauses the same containers in order.
func (dockerPauseFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	if len(tgs) == 0 {
		return nil, fmt.Errorf("docker_pause: no targets matched %q", target)
	}

	// Capture the (container, dockerBin) pairs we paused
	// so Recovery unpauses the same set, in order.
	type paused struct {
		container string
		dockerBin string
	}
	var done []paused
	for _, t := range tgs {
		dt, ok := t.(*DockerTarget)
		if !ok {
			return nil, fmt.Errorf("docker_pause: target %q is not a DockerTarget (got %T)", t.Name(), t)
		}
		bin := dt.DockerBin
		if bin == "" {
			bin = "docker"
		}
		out, err := exec.CommandContext(ctx, bin, "pause", dt.Container).CombinedOutput()
		if err != nil {
			// Unpause whatever we already paused so we
			// don't leave half-frozen state on the way out.
			for _, p := range done {
				_ = exec.Command(p.dockerBin, "unpause", p.container).Run()
			}
			return nil, fmt.Errorf("docker_pause: %s: %w (output: %s)",
				dt.Container, err, strings.TrimSpace(string(out)))
		}
		done = append(done, paused{container: dt.Container, dockerBin: bin})
	}

	return func(ctx context.Context) error {
		// Best-effort unpause across the whole captured
		// set — surface the FIRST error but continue so
		// transient docker hiccups don't strand multiple
		// containers.
		var firstErr error
		for _, p := range done {
			if err := exec.CommandContext(ctx, p.dockerBin, "unpause", p.container).Run(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("docker_pause recovery (unpause %s): %w", p.container, err)
			}
		}
		return firstErr
	}, nil
}
