// faults_extra.go — second-wave fault primitives, added on
// top of the original eleven (disk_full, signal, cgroup_squeeze,
// toxiproxy, sql, patroni_switchover, libfaketime, network_block,
// flip_random_byte, pause_archive, docker_pause).  Each
// primitive here was prioritised after a 12 h continuous-fault
// soak as a real failure mode the existing set could not
// reproduce.  See the file-level doc comments per fault for the
// design rationale and any honest limitations.
package inject

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrCapSysResource reports that a fault could not be applied
// because the target container's user namespace lacks
// CAP_SYS_RESOURCE.  Used by fd_exhaustion (prlimit on another
// process needs SYS_RESOURCE, which Docker's default capability
// set drops).  Callers that classify fault outcomes — the soak
// driver in particular — can treat this as "needs SYS_RESOURCE
// wiring" rather than a generic apply failure.
var ErrCapSysResource = errors.New("target lacks CAP_SYS_RESOURCE")

// init registers the second-wave primitives.  Go permits
// multiple init() per package; the central one in faults.go
// stays untouched so it keeps reading as the original catalogue.
func init() {
	for _, f := range []Fault{
		&checkpointStormFault{},
		&dropRelationFault{},
		&tablespaceUnmountFault{},
		&inodeExhaustionFault{},
		&fdExhaustionFault{},
		&manifestCorruptionFault{},
		&concurrentRepoWriterFault{},
		&truncatedWALSegmentFault{},
		&missingWALSegmentFault{},
		&tornPageFault{},
	} {
		DefaultRegistry.Register(f)
	}
}

// --- checkpoint_storm -------------------------------------------------

// checkpointStormFault floods CHECKPOINT against PG to exercise
// the WAL→backup interaction window.  Every CHECKPOINT forces a
// flush + (potentially) segment rotation, surfacing races in
// archiving / wal-streaming and backup-tool bugs that assume
// checkpoint pressure is bounded.
//
// Args:
//
//	target    — TargetSet selector, default "pg_random"
//	user      — OS user to run psql as (NOT a PG role).  Default
//	            "postgres" — matches the testbed images' fixed
//	            OS user; peer auth picks up the cluster's
//	            superuser role from there.  CHECKPOINT requires
//	            a PG superuser, and the soak's `postgres` peer-auth
//	            role IS a superuser on every testbed image we
//	            ship.  Setting this to "testkit" or any other PG
//	            role fails — `su` needs an OS user (found
//	            empirically: 3 min into the first 4-slot run).
//	count     — number of CHECKPOINT statements, default 20
//	sleep_ms  — milliseconds between statements (GNU coreutils
//	            sleep fractional form, e.g. sleep 0.250).  Default
//	            0 (back-to-back, bounded only by how fast PG can
//	            complete each checkpoint).
//
// Apply runs the storm synchronously; the orchestrator's fault
// timeline expects Apply to return when the stressor is done.
// Recovery is a no-op — CHECKPOINT is a normal PG operation and
// leaves no state to revert.
type checkpointStormFault struct{}

// Name returns "checkpoint_storm".
func (checkpointStormFault) Name() string { return "checkpoint_storm" }

// Apply runs `count` back-to-back CHECKPOINT statements (with
// optional sleep_ms between) against the picked target's PG.
// Synchronous; NoRecovery.
func (checkpointStormFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	user := args["user"]
	if user == "" {
		user = "postgres"
	}
	count := 20
	if raw := args["count"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("checkpoint_storm: count must be a positive int (got %q)", raw)
		}
		count = n
	}
	sleepMs := 0
	if raw := args["sleep_ms"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("checkpoint_storm: sleep_ms must be a non-negative int (got %q)", raw)
		}
		sleepMs = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// Single shell exec runs the whole loop inside the cell;
	// a per-statement docker exec would amortise to >100 ms per
	// call and make the "storm" pace meaningless.  Use coreutils
	// `sleep` fractional form (e.g. sleep 0.250) — supported in
	// every distro the testbed images ship from (coreutils >= 8.30).
	sleepCmd := ""
	if sleepMs > 0 {
		sleepCmd = fmt.Sprintf(" sleep %d.%03d;", sleepMs/1000, sleepMs%1000)
	}
	script := fmt.Sprintf(
		`i=0; while [ "$i" -lt %d ]; do psql -c CHECKPOINT >/dev/null 2>&1 || true;%s i=$((i+1)); done`,
		count, sleepCmd)
	for _, t := range tgs {
		if _, err := t.Exec(ctx, "su", "-s", "/bin/sh", "-c", script, user); err != nil {
			return nil, fmt.Errorf("checkpoint_storm on %s: %w", t.Name(), err)
		}
	}
	return NoRecovery, nil
}

// --- drop_relation_mid_backup ----------------------------------------

// dropRelationFault creates a sizeable relation, forces it to
// disk, then drops it — all under the cover of a running backup
// (the soak's iteration loop runs backups continuously).  The
// product invariant under test: a backup either includes the
// table cleanly or refuses the snapshot — never "manifest names
// a relfilenode that no longer exists in the catalog."
//
// Args:
//
//	target — TargetSet selector, default "pg_random"
//	user   — OS user to run psql as (NOT a PG role).  Default
//	         "postgres" — matches the existing sql fault and
//	         the testbed images' fixed OS user (the cluster's
//	         PG role for peer auth derives from the OS user).
//	         Setting this to a PG role (e.g. "testkit") fails
//	         with `su: user testkit does not exist` because su
//	         needs an OS user — the soak found this within 3
//	         minutes of the first 4-slot run.
//	rows   — INSERT row count, default 50000 (≈50 MiB at the
//	         1 KiB-per-row shape below — large enough that the
//	         relation has several physical pages on disk and a
//	         concurrent backup has work to copy)
//
// Apply is a single shell pipeline:
//  1. CREATE TABLE testkit_drop_<rand> (...);
//  2. INSERT bulk rows;
//  3. CHECKPOINT (flush relfilenode to disk so the backup has
//     real data to race against);
//  4. DROP TABLE.
//
// Recovery: DROP TABLE IF EXISTS — idempotent cleanup in case
// the Apply died between step 1 and step 4.
type dropRelationFault struct{}

// Name returns "drop_relation_mid_backup".
func (dropRelationFault) Name() string { return "drop_relation_mid_backup" }

// Apply CREATEs a randomly-named relation, bulk-INSERTs `rows`
// rows, CHECKPOINTs, then DROPs the table.  Recovery DROP TABLE
// IF EXISTS reaps any leftover from a mid-script abort.
func (dropRelationFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	user := args["user"]
	if user == "" {
		user = "postgres"
	}
	rows := 50000
	if raw := args["rows"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("drop_relation_mid_backup: rows must be a positive int (got %q)", raw)
		}
		rows = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// Random suffix so concurrent applications in a fleet never
	// collide.  hex is shell-safe and forms a valid PG identifier.
	suffixBuf := make([]byte, 6)
	if _, err := rand.Read(suffixBuf); err != nil {
		return nil, fmt.Errorf("drop_relation_mid_backup: rand: %w", err)
	}
	table := "testkit_drop_" + hex.EncodeToString(suffixBuf)

	// One psql invocation per phase keeps each phase's exit code
	// observable in the shell trace — important when this runs
	// against an OOM-ing cell.  `|| true` after DROP so that a
	// late DROP failure doesn't fail the whole Apply; Recovery
	// will reap any leftover.
	script := fmt.Sprintf(
		`set -e
psql -c "CREATE TABLE %[1]s (id bigserial PRIMARY KEY, data text)"
psql -c "INSERT INTO %[1]s (data) SELECT repeat('x', 1024) FROM generate_series(1, %[2]d)"
psql -c "CHECKPOINT"
psql -c "DROP TABLE %[1]s" || true
`, table, rows)

	for _, t := range tgs {
		if _, err := t.Exec(ctx, "su", "-s", "/bin/sh", "-c", script, user); err != nil {
			return nil, fmt.Errorf("drop_relation_mid_backup on %s: %w", t.Name(), err)
		}
	}
	return func(ctx context.Context) error {
		// Idempotent reap — runs even when Apply succeeded.
		cmd := fmt.Sprintf(`psql -c "DROP TABLE IF EXISTS %s"`, table)
		var firstErr error
		for _, t := range tgs {
			if _, err := t.Exec(ctx, "su", "-s", "/bin/sh", "-c", cmd, user); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("drop_relation_mid_backup recovery on %s: %w", t.Name(), err)
			}
		}
		return firstErr
	}, nil
}

// --- tablespace_unmount ----------------------------------------------

// tablespaceUnmountFault renames a directory the backup tool is
// scanning so subsequent reads of paths under it return ENOENT
// — the same effect, from the agent's point of view, as a
// tablespace mount being yanked out from under PG.
//
// HONEST LIMITATION: real tablespace unmount needs (a) the
// testbed images to declare and mount a tablespace and (b)
// CAP_SYS_ADMIN in the cell to call umount(2).  Neither is
// wired today.  This primitive instead operates on any
// operator-named directory tree, via `mv $path $path.testkit-
// unmounted`.  The blast radius (every subsequent open in the
// subtree returns ENOENT) is the same; the kernel-level
// mechanism is not.  The argument is REQUIRED — there is no
// default that quietly pretends a random path is a tablespace.
//
// Args:
//
//	target — TargetSet selector, REQUIRED
//	path   — absolute path of the directory to unmount-simulate,
//	         REQUIRED.  Must be writable by the target's
//	         entrypoint user (otherwise the mv fails).  Refuses
//	         to operate on $PGDATA root or PG_VERSION-bearing
//	         dirs — taking the data dir away is a different
//	         fault class (disk_full / signal already cover
//	         "PG can't read its own files").
//
// Recovery: mv $path.testkit-unmounted back to $path.
type tablespaceUnmountFault struct{}

// Name returns "tablespace_unmount".
func (tablespaceUnmountFault) Name() string { return "tablespace_unmount" }

// Apply renames the configured path to
// `<path>.testkit-unmounted` on each picked target so further
// reads under it return ENOENT.  Recovery renames it back.
func (tablespaceUnmountFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target, err := args.Require("target")
	if err != nil {
		return nil, err
	}
	path, err := args.Require("path")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("tablespace_unmount: path must be absolute (got %q)", path)
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// Safety: refuse to operate on PGDATA root.  Allowing it
	// would conflate this fault with "disk gone", which is
	// already exercised by disk_full + cgroup_squeeze and would
	// leave the cell wedged in a way this fault's Recovery
	// can't reliably undo.
	hidden := path + ".testkit-unmounted"
	guardScript := fmt.Sprintf(
		`test -d %[1]s || { echo "not a directory: %[1]s" >&2; exit 2; }
if test -e %[1]s/PG_VERSION; then
    echo "refusing to operate on PGDATA-shaped directory: %[1]s" >&2
    exit 3
fi
mv %[1]s %[2]s
`, path, hidden)
	for _, t := range tgs {
		if out, err := t.Exec(ctx, "sh", "-c", guardScript); err != nil {
			return nil, fmt.Errorf("tablespace_unmount on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(out)))
		}
	}
	return func(ctx context.Context) error {
		// Best-effort reverse.  If the source dir has reappeared
		// (e.g. PG recreated a tablespace mount point), do NOT
		// overwrite — surface the conflict instead so the
		// operator inspects the cell's state.
		recoverScript := fmt.Sprintf(
			`if test -e %[1]s && test -e %[2]s; then
    echo "tablespace_unmount recovery: both %[1]s and %[2]s exist; refusing to overwrite" >&2
    exit 4
fi
test -e %[2]s && mv %[2]s %[1]s
exit 0
`, path, hidden)
		var firstErr error
		for _, t := range tgs {
			if _, err := t.Exec(ctx, "sh", "-c", recoverScript); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("tablespace_unmount recovery on %s: %w", t.Name(), err)
			}
		}
		return firstErr
	}, nil
}

// --- inode_exhaustion ------------------------------------------------

// inodeExhaustionFault creates files until the target filesystem
// runs out of inodes — ENOSPC despite df reporting free bytes,
// a real cloud-storage failure mode that disk_full doesn't
// reproduce.
//
// HONEST LIMITATION: the bite of this fault depends entirely on
// how many free inodes the target filesystem has.  Modern overlay
// filesystems on hosts with billions of free inodes will swallow
// this fault as a no-op.  Apply pre-checks `df --output=iavail`
// and refuses to run if the filesystem has more than max_iavail
// free inodes — the operator wires a small-inode loop mount per
// cell when they want this fault to actually fire.
//
// Args:
//
//	target      — TargetSet selector, default "repo"
//	path        — directory the spacer files go into.  Default
//	              "/var/lib/pg_hardstorage/repo/.testkit-inode-fill".
//	max_iavail  — bail-out threshold (inodes).  If `df --output=iavail` shows
//	              more than this many free inodes, Apply errors
//	              with a clear message rather than creating
//	              millions of files no-op-style.  Default 2_000_000.
//	cap         — hard limit on files created, default 1_500_000.
//	              Even when the FS is tight enough, we never
//	              create more than this many — runaway protection.
//
// Recovery: rm -rf the spacer dir.
type inodeExhaustionFault struct{}

// Name returns "inode_exhaustion".
func (inodeExhaustionFault) Name() string { return "inode_exhaustion" }

// Apply pre-checks `df --output=iavail` and refuses if the
// filesystem has more than max_iavail free inodes; otherwise
// creates up to `cap` empty files until ENOSPC.  Recovery
// rm -rf's the spacer dir.
func (inodeExhaustionFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "repo"
	}
	path := args["path"]
	if path == "" {
		path = "/var/lib/pg_hardstorage/repo/.testkit-inode-fill"
	}
	maxIAvail := 2_000_000
	if raw := args["max_iavail"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("inode_exhaustion: max_iavail must be a positive int (got %q)", raw)
		}
		maxIAvail = n
	}
	cap := 1_500_000
	if raw := args["cap"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("inode_exhaustion: cap must be a positive int (got %q)", raw)
		}
		cap = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}

	// Bail-out pre-check + creation loop in one shell call.  We
	// abort if the FS has too many free inodes to be meaningfully
	// exhausted (refusal is honest: the fault is silently a
	// no-op otherwise, and the soak should flag that).
	// `df --output=iavail` already selects the free-inode column;
	// passing `-i` as well makes GNU coreutils df bail out with
	// "options -i and --output are mutually exclusive" (the soak
	// hit this on every cell), so the column request stands alone.
	script := fmt.Sprintf(
		`set -e
mkdir -p %[1]s
iavail=$(df --output=iavail %[1]s | tail -n1 | tr -d ' ')
case "$iavail" in
    ''|*[!0-9]*) echo "inode_exhaustion: cannot read iavail for %[1]s" >&2; exit 2 ;;
esac
if [ "$iavail" -gt %[2]d ]; then
    echo "inode_exhaustion: $iavail free inodes on %[1]s exceeds max_iavail=%[2]d" >&2
    echo "inode_exhaustion: wire a small-inode mount (mkfs.ext4 -N <N>) for this fault to fire" >&2
    exit 3
fi
# Create files in batches; stop the first time write fails (ENOSPC).
target_count=%[3]d
if [ "$iavail" -lt "$target_count" ]; then
    target_count=$iavail
fi
i=0
while [ "$i" -lt "$target_count" ]; do
    : > "%[1]s/f.$i" 2>/dev/null || break
    i=$((i + 1))
done
echo "inode_exhaustion: created $i files in %[1]s"
`, path, maxIAvail, cap)
	for _, t := range tgs {
		if out, err := t.Exec(ctx, "sh", "-c", script); err != nil {
			return nil, fmt.Errorf("inode_exhaustion on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(out)))
		}
	}
	return func(ctx context.Context) error {
		var firstErr error
		for _, t := range tgs {
			if _, err := t.Exec(ctx, "rm", "-rf", path); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("inode_exhaustion recovery on %s: %w", t.Name(), err)
			}
		}
		return firstErr
	}, nil
}

// --- fd_exhaustion ---------------------------------------------------

// fdExhaustionFault lowers RLIMIT_NOFILE for the target's PG
// postmaster (or agent) so any further open(2) /accept(2) hits
// EMFILE.  Surfaces FD-leak bugs and "we cached the open count
// per backup, but reused that count after a fault" classes of
// regression.
//
// HONEST LIMITATION — CAP_SYS_RESOURCE: setting another
// process's rlimits via prlimit(2) requires CAP_SYS_RESOURCE
// in the target's user namespace.  Docker's default container
// capability set does NOT include SYS_RESOURCE; the soak found
// this within 3 minutes of the first 4-slot run with the error
// `prlimit: failed to set the NOFILE resource limit: Operation
// not permitted` from every fd_exhaustion application.  The
// fault therefore SHELLS OUT prlimit and detects EPERM /
// "Operation not permitted" in its output, returning a typed
// `ErrCapSysResource` so the soak driver can classify "the
// container lacks SYS_RESOURCE" cleanly rather than as a
// generic fault failure.  The compose generator now wires
// `cap_add: ["SYS_RESOURCE"]` into every PG cell stanza (see
// writePGService in internal/testkit/compose), so the fault
// fires for real; the typed-error path above remains as a
// clean diagnostic for any cell brought up without it.
//
// Args:
//
//	target — TargetSet selector, default "pg_random"
//	role   — "pg" (default) or "agent": which long-running
//	         process to clamp.  In the testbed cells both run in
//	         the same container, so target picks the container
//	         and role picks the process within it.
//	limit  — new RLIMIT_NOFILE (soft+hard).  Default 64 —
//	         comfortably below the dozens of FDs PG opens on a
//	         live workload, so any new connection / file open
//	         after the fault fires hits the cap.
//
// Recovery: prlimit --nofile=65536:65536 on the same PID (or
// the largest hard limit prlimit allows — 65536 is universally
// supported by util-linux).  No PG restart needed: the limit
// affects subsequent opens but doesn't close existing FDs, so
// PG only sees pressure on new connections / file opens.
type fdExhaustionFault struct{}

// Name returns "fd_exhaustion".
func (fdExhaustionFault) Name() string { return "fd_exhaustion" }

// Apply shells out to prlimit to lower RLIMIT_NOFILE on the
// target PG postmaster (or agent) to `limit`.  Detects EPERM
// and surfaces ErrCapSysResource so the soak driver can tell
// "container lacks SYS_RESOURCE" from a generic fault failure.
// Recovery restores the limit to 65536.
func (fdExhaustionFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	role := args["role"]
	if role == "" {
		role = "pg"
	}
	limit := 64
	if raw := args["limit"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("fd_exhaustion: limit must be a positive int (got %q)", raw)
		}
		limit = n
	}
	var commName string
	switch role {
	case "pg":
		commName = "postgres"
	case "agent":
		commName = "pg_hardstorage"
	default:
		return nil, fmt.Errorf("fd_exhaustion: role must be one of pg|agent (got %q)", role)
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// Find ONE pid for the role (the postmaster, not its
	// children; the agent's main process, not its goroutine
	// shadows).  We pick the oldest matching PID via stat's
	// start_time — `ps -o pid,etime` would do it but isn't
	// available in every minimal image.  /proc/<pid>/stat
	// field 22 is starttime; ascending starttime ⇒ oldest
	// process first.
	applyScript := fmt.Sprintf(
		`set -e
target_comm=%[1]s
oldest_pid=""
oldest_start=""
for d in /proc/[0-9]*; do
    pid=${d#/proc/}
    comm=$(cat $d/comm 2>/dev/null) || continue
    [ "$comm" = "$target_comm" ] || continue
    # field 22 of /proc/<pid>/stat is starttime (jiffies since boot).
    start=$(awk '{print $22}' $d/stat 2>/dev/null) || continue
    if [ -z "$oldest_start" ] || [ "$start" -lt "$oldest_start" ]; then
        oldest_start=$start
        oldest_pid=$pid
    fi
done
if [ -z "$oldest_pid" ]; then
    echo "fd_exhaustion: no process with comm=$target_comm" >&2
    exit 2
fi
prlimit --nofile=%[2]d:%[2]d --pid=$oldest_pid
echo "$oldest_pid"
`, commName, limit)
	type clamped struct {
		name string
		pid  string
	}
	var done []clamped
	for _, t := range tgs {
		out, err := t.Exec(ctx, "sh", "-c", applyScript)
		if err != nil {
			// Detect the missing-capability case: prlimit
			// prints "Operation not permitted" when the
			// caller's user namespace lacks CAP_SYS_RESOURCE.
			// This is the default docker container posture, so
			// the soak hits it on every fd_exhaustion attempt
			// in an out-of-the-box testbed.  Surface a typed
			// sentinel so the orchestrator can classify the
			// outcome as "needs SYS_RESOURCE wiring" rather
			// than a generic fault failure.
			if strings.Contains(string(out), "Operation not permitted") {
				return nil, fmt.Errorf(
					"fd_exhaustion on %s: %w (add cap_add: [\"SYS_RESOURCE\"] to the cell)",
					t.Name(), ErrCapSysResource)
			}
			return nil, fmt.Errorf("fd_exhaustion on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(out)))
		}
		pid := strings.TrimSpace(string(out))
		// The last line is the PID; if the shell printed
		// chatter before it, take the final non-empty line.
		if i := strings.LastIndex(pid, "\n"); i >= 0 {
			pid = strings.TrimSpace(pid[i+1:])
		}
		if pid == "" {
			return nil, fmt.Errorf("fd_exhaustion on %s: prlimit succeeded but no PID echoed", t.Name())
		}
		done = append(done, clamped{name: t.Name(), pid: pid})
	}
	return func(ctx context.Context) error {
		var firstErr error
		for _, c := range done {
			// We don't know the original hard limit; 65536 is
			// the universal sane ceiling and is what every
			// distro ships as the systemd default.
			restoreScript := fmt.Sprintf(`prlimit --nofile=65536:65536 --pid=%s`, c.pid)
			tgs2, err := ts.Pick(c.name)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("fd_exhaustion recovery: re-pick %s: %w", c.name, err)
				}
				continue
			}
			for _, t := range tgs2 {
				if _, err := t.Exec(ctx, "sh", "-c", restoreScript); err != nil && firstErr == nil {
					firstErr = fmt.Errorf("fd_exhaustion recovery on %s: %w", c.name, err)
				}
			}
		}
		return firstErr
	}, nil
}

// --- manifest_targeted_corruption ------------------------------------

// manifestCorruptionFault flips bytes specifically in the
// repository's chunk-graph manifests (`manifests/**/manifest.json`)
// rather than in any random file under the repo.  The product
// invariant under test: restore from a corrupted manifest must
// REFUSE the restore — never "succeed with a torn DB."  The
// existing flip_random_byte fault is uniform across the repo and
// most of the time picks a chunk; this primitive deterministically
// hits the bytes where pg_hardstorage's invariants live.
//
// Args:
//
//	target — TargetSet selector, default "repo"
//	prefix — repo prefix to search, default
//	         "/var/lib/pg_hardstorage/repo/manifests".  Override
//	         when the repo is mounted at a non-default path.
//	count  — number of bytes to flip, default 1.  Increase to
//	         exercise multi-byte tear scenarios.
//
// Apply picks one random manifest.json under prefix, flips
// `count` random bytes, writes it back.  Recovery is a no-op —
// the corruption is now persistent on the repo, exactly as it
// would be on a real corrupted backup.
type manifestCorruptionFault struct{}

// Name returns "manifest_targeted_corruption".
func (manifestCorruptionFault) Name() string { return "manifest_targeted_corruption" }

// Apply picks a random `manifest.json` under prefix and flips
// `count` random bytes via in-container `dd seek=N
// conv=notrunc`.  Skips cleanly when no manifest exists yet
// (early-soak window).  NoRecovery — the corruption is
// persistent.
func (manifestCorruptionFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "repo"
	}
	prefix := args["prefix"]
	if prefix == "" {
		prefix = "/var/lib/pg_hardstorage/repo/manifests"
	}
	count := 1
	if raw := args["count"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("manifest_targeted_corruption: count must be a positive int (got %q)", raw)
		}
		count = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		// Match the same `find … | shuf -n 1` shape
		// flip_random_byte uses so the selection happens in the
		// target's filesystem view (the repo isn't necessarily
		// visible from the test driver host).
		out, err := t.Exec(ctx, "sh", "-c",
			fmt.Sprintf(`find %s -type f -name 'manifest.json' 2>/dev/null | shuf -n 1`, prefix))
		if err != nil {
			return nil, fmt.Errorf("manifest_targeted_corruption: locate on %s: %w", t.Name(), err)
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			// No manifest in the repo yet — typically the
			// early-soak window before the first backup
			// commits its manifest.  Cleanly skip: there is
			// no target to corrupt, so the fault is
			// vacuously satisfied.
			continue
		}
		// In-container byte flip via `dd seek=N conv=notrunc`,
		// NOT CopyOut+CopyIn.  The previous tar-stream shape
		// (read whole file out via `docker cp -`, flip a byte
		// in the host's Go memory, write back via `docker cp -`)
		// failed ~25 % of the time on small files: a small
		// manifest.json tarball has ~512 B of tar header per
		// ~1 KiB of body, so a uniform-random offset lands in
		// the header roughly that often, breaks the tar parse,
		// and `docker cp` exits before reading our stdin —
		// surfacing as "write |1: broken pipe" failures from
		// the soak.  In-container `dd` flips the byte inside
		// the live file directly, no tar in the way.
		//
		// We pick the file offset inside the container so the
		// host doesn't need to learn the file size; `shuf -i`
		// is universally available in the testbed images.  The
		// XOR-with-0x01 mirrors flip_random_byte's shape so
		// reasoning carries over.
		flipScript := fmt.Sprintf(
			`set -e
size=$(stat -c %%s %[1]s)
if [ "$size" -le 0 ]; then echo "manifest_targeted_corruption: %[1]s is empty" >&2; exit 2; fi
for i in $(seq 1 %[2]d); do
    off=$(shuf -i 0-$((size - 1)) -n 1)
    old=$(dd if=%[1]s bs=1 count=1 skip=$off 2>/dev/null | od -An -tu1 | tr -d ' ')
    new=$((old ^ 1))
    printf "\\$(printf %%o $new)" | dd of=%[1]s bs=1 count=1 seek=$off conv=notrunc 2>/dev/null
done
`, path, count)
		if cmdOut, err := t.Exec(ctx, "sh", "-c", flipScript); err != nil {
			return nil, fmt.Errorf("manifest_targeted_corruption: flip on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(cmdOut)))
		}
	}
	return NoRecovery, nil
}

// randIntn returns a cryptographically-random int in [0, n).
// Mirrors flipRandomByteFault's offset selection — read 8
// crypto-rand bytes, build uint64, mod n.  Modding in unsigned
// space and converting after preserves the [0, n) invariant
// even on a 32-bit int target.
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	v := uint64(buf[0])<<56 | uint64(buf[1])<<48 |
		uint64(buf[2])<<40 | uint64(buf[3])<<32 |
		uint64(buf[4])<<24 | uint64(buf[5])<<16 |
		uint64(buf[6])<<8 | uint64(buf[7])
	return int(v % uint64(n))
}

// --- concurrent_repo_writer ------------------------------------------

// concurrentRepoWriterFault launches a SECOND `pg_hardstorage
// backup` against the same repository while a primary backup is
// (typically) in flight from the soak's iteration loop.  Tests
// CAS-dedup correctness, manifest locking, and repository
// concurrency posture under adversarial overlap.
//
// HONEST LIMITATION: the inject layer doesn't carry the cell's
// runtime config (deployment, dsn, repo URL).  The operator
// MUST pass those as fault args — there is no
// "look-them-up-yourself" default.  Examples in faults.yaml
// substitute the orchestrator's $REPO / $DEPLOYMENT / $PG_CONN
// placeholders before the fault is queued.
//
// Args:
//
//	target      — TargetSet selector, default "pg_random"
//	deployment  — REQUIRED: positional arg to `pg_hardstorage backup`
//	pg_conn     — REQUIRED: --pg-connection value
//	repo        — REQUIRED: --repo URL (must match the primary's)
//	agent_bin   — path to the agent binary, default
//	              "/usr/local/bin/pg_hardstorage"
//	timeout_s   — how long the concurrent backup may run before
//	              Recovery kills it.  Default 30.
//	user        — local OS user to run as (must match the agent's
//	              posture — production refuses euid 0).  Default
//	              "pgbackup".
//
// Apply detaches the second backup in the background and stashes
// its PID in /tmp/testkit-concurrent.pid.  Recovery reads the
// PID and best-effort SIGTERM → SIGKILLs it.
type concurrentRepoWriterFault struct{}

// Name returns "concurrent_repo_writer".
func (concurrentRepoWriterFault) Name() string { return "concurrent_repo_writer" }

// Apply launches a second `pg_hardstorage backup` against the
// same repo URL in the background and stashes its PID in
// /tmp/testkit-concurrent.pid.  Recovery best-effort
// SIGTERM → SIGKILLs the PID.
func (concurrentRepoWriterFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	deployment, err := args.Require("deployment")
	if err != nil {
		return nil, err
	}
	pgConn, err := args.Require("pg_conn")
	if err != nil {
		return nil, err
	}
	repo, err := args.Require("repo")
	if err != nil {
		return nil, err
	}
	agentBin := args["agent_bin"]
	if agentBin == "" {
		agentBin = "/usr/local/bin/pg_hardstorage"
	}
	user := args["user"]
	if user == "" {
		user = "pgbackup"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	// We background the agent via setsid + nohup so the docker
	// exec session can return without killing the launched
	// process.  The PID file is per-fault-instance (mktemp -t)
	// so repeated applications don't stomp each other.
	//
	// Why setsid: docker exec's process tree shares the exec
	// session's pgrp; without setsid, the kernel sends SIGHUP
	// to the whole pgrp when the docker exec session ends,
	// killing the "background" backup we just launched.  setsid
	// detaches into a new session/pgrp and is universally
	// available on every base image we ship from.
	applyScript := fmt.Sprintf(
		`set -e
pidf=$(mktemp -t testkit-concurrent.XXXXXX.pid)
setsid su -s /bin/sh -c '%[1]s backup %[2]s --pg-connection %[3]s --repo %[4]s --include-wal --stall-timeout 5m -o json > /tmp/testkit-concurrent.log 2>&1 &
echo $! > '"$pidf"'
disown
' %[5]s
# Echo the pidfile path so Recovery can find it.
echo "$pidf"
`,
		agentBin, shSingleQuote(deployment), shSingleQuote(pgConn), shSingleQuote(repo), user)
	type launched struct {
		name string
		pidf string
	}
	var done []launched
	for _, t := range tgs {
		out, err := t.Exec(ctx, "sh", "-c", applyScript)
		if err != nil {
			return nil, fmt.Errorf("concurrent_repo_writer on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(out)))
		}
		// The pidfile path is the LAST non-empty line of stdout.
		pidf := ""
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				pidf = line
			}
		}
		if pidf == "" {
			return nil, fmt.Errorf("concurrent_repo_writer on %s: no pidfile path returned", t.Name())
		}
		done = append(done, launched{name: t.Name(), pidf: pidf})
	}
	return func(ctx context.Context) error {
		recoverScript := `for pidf in "$@"; do
    [ -r "$pidf" ] || continue
    pid=$(cat "$pidf")
    [ -n "$pid" ] || continue
    # SIGTERM first; if still alive after 3 s, SIGKILL.
    kill -15 "$pid" 2>/dev/null || true
    i=0
    while [ "$i" -lt 3 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 1
        i=$((i + 1))
    done
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$pidf"
done
`
		var firstErr error
		for _, d := range done {
			// We need to re-pick the target to issue the kill —
			// faults don't keep the Target reference across the
			// apply/recover boundary in the inject API.
			tgs2, err := ts.Pick(d.name)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("concurrent_repo_writer recovery: re-pick %s: %w", d.name, err)
				}
				continue
			}
			for _, t := range tgs2 {
				if _, err := t.Exec(ctx, "sh", "-c", recoverScript, "_", d.pidf); err != nil && firstErr == nil {
					firstErr = fmt.Errorf("concurrent_repo_writer recovery on %s: %w", d.name, err)
				}
			}
		}
		return firstErr
	}, nil
}

// --- truncated_wal_segment -------------------------------------------

// truncatedWALSegmentFault truncates a random repo-side WAL
// segment metadata file to a non-record boundary.  Tests
// pg_hardstorage's restore-side detection of mid-stream
// truncation: a torn WAL must fail the restore cleanly, never
// silently produce a torn timeline.
//
// Args:
//
//	target — TargetSet selector, default "repo"
//	prefix — repo prefix containing WAL metadata, default
//	         "/var/lib/pg_hardstorage/repo/wal"
//	bytes  — number of bytes to remove from the tail, default
//	         512 (half a typical sector — guaranteed to land
//	         mid-record for any sane segment format).
//
// Apply: `find $prefix -type f | shuf -n 1` → `truncate -s -N`
// against the chosen file.  Recovery: NoRecovery — the
// truncation is now on disk, exactly as it would be on a real
// corrupted archive.
type truncatedWALSegmentFault struct{}

// Name returns "truncated_wal_segment".
func (truncatedWALSegmentFault) Name() string { return "truncated_wal_segment" }

// Apply picks a random WAL metadata file under prefix and
// `truncate -s -<bytes>`s it.  Skips cleanly when the repo has
// no archived WAL yet.  NoRecovery — the truncation is
// persistent.
func (truncatedWALSegmentFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "repo"
	}
	prefix := args["prefix"]
	if prefix == "" {
		prefix = "/var/lib/pg_hardstorage/repo/wal"
	}
	bytes := 512
	if raw := args["bytes"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("truncated_wal_segment: bytes must be a positive int (got %q)", raw)
		}
		bytes = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		out, err := t.Exec(ctx, "sh", "-c",
			fmt.Sprintf(`find %s -type f 2>/dev/null | shuf -n 1`, prefix))
		if err != nil {
			return nil, fmt.Errorf("truncated_wal_segment: locate on %s: %w", t.Name(), err)
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			// No archived WAL yet — typically the early-soak
			// window before the streamer commits its first
			// segment.  Cleanly skip rather than erroring;
			// the operator who actually wired a wrong prefix
			// sees a per-application zero-effect outcome.
			continue
		}
		if _, err := t.Exec(ctx, "truncate", "-s", fmt.Sprintf("-%d", bytes), path); err != nil {
			return nil, fmt.Errorf("truncated_wal_segment: truncate %s on %s: %w", path, t.Name(), err)
		}
	}
	return NoRecovery, nil
}

// --- missing_wal_segment ---------------------------------------------

// missingWALSegmentFault removes a random repo-side WAL segment
// metadata file from the archive.  Tests the restore-side gap
// detection: PITR through a removed segment must refuse, never
// silently produce a torn timeline.
//
// Args:
//
//	target — TargetSet selector, default "repo"
//	prefix — repo prefix containing WAL metadata, default
//	         "/var/lib/pg_hardstorage/repo/wal"
//
// Apply: `find $prefix -type f | shuf -n 1` → `rm` against the
// chosen file.  Recovery: NoRecovery — the segment is now
// missing, exactly as it would be on a real cleanup-gone-wrong
// archive.
type missingWALSegmentFault struct{}

// Name returns "missing_wal_segment".
func (missingWALSegmentFault) Name() string { return "missing_wal_segment" }

// Apply picks a random WAL metadata file under prefix and rm's
// it.  Skips cleanly when the repo has no archived WAL yet.
// NoRecovery — the gap is persistent.
func (missingWALSegmentFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "repo"
	}
	prefix := args["prefix"]
	if prefix == "" {
		prefix = "/var/lib/pg_hardstorage/repo/wal"
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	for _, t := range tgs {
		out, err := t.Exec(ctx, "sh", "-c",
			fmt.Sprintf(`find %s -type f 2>/dev/null | shuf -n 1`, prefix))
		if err != nil {
			return nil, fmt.Errorf("missing_wal_segment: locate on %s: %w", t.Name(), err)
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			// No archived WAL yet — same early-soak window
			// shape as truncated_wal_segment.  Skip cleanly.
			continue
		}
		if _, err := t.Exec(ctx, "rm", "-f", path); err != nil {
			return nil, fmt.Errorf("missing_wal_segment: rm %s on %s: %w", path, t.Name(), err)
		}
	}
	return NoRecovery, nil
}

// --- torn_page / partial_write ---------------------------------------

// tornPageFault simulates a torn-write (the canonical backup-tool
// nightmare): mid-write power loss leaves a single 8 KiB
// PostgreSQL page with its first 4 KiB sector updated but its
// second 4 KiB sector still holding the previous-generation
// bytes.  PG defends against this with full-page writes; the
// product invariant under test is that pg_hardstorage's restore
// honours those FPWs and that backups taken between the tear
// and the next checkpoint either include the FPW or refuse.
//
// HONEST LIMITATION: a true torn write requires either dm-flakey
// at the block layer or a power-loss simulator — neither
// available inside an unprivileged container.  This primitive
// instead emulates the *visible state* on disk via `dd`:
// rewrites the second 4 KiB sector of a chosen 8 KiB page with
// random bytes, leaving the first sector intact.  The on-disk
// shape is exactly what a torn write produces; the in-memory
// page cache may differ (the kernel's page cache may have the
// pre-tear copy until invalidation).  Operators who want true
// block-layer simulation should pair this with a `signal sig=9`
// against PG to force a cold restart and bypass the page cache.
//
// Args:
//
//	target    — TargetSet selector, default "pg_random"
//	pgdata    — PGDATA root, default "/var/lib/postgresql/data"
//	pages     — number of pages to tear, default 1
//	page_size — PG block size in bytes, default 8192
//	tear_at   — sector offset within the page where the tear
//	            starts, default 4096 (first half intact, second
//	            half torn).  Set to 0 to tear the first half,
//	            leaving the second intact.
//
// Apply scans $PGDATA/base/<oid>/ for user-database relation
// files (any *_fsm / *_vm / *_init / *.1+ segments included),
// excluding the template DBs (OIDs 1 and 4).  For each
// `pages` count, picks a random relation file, picks a random
// page boundary within it, and overwrites
// [page_offset + tear_at, page_offset + page_size) with random
// bytes via dd.  Recovery: NoRecovery — the tear is on disk.
type tornPageFault struct{}

// Name returns "torn_page".
func (tornPageFault) Name() string { return "torn_page" }

// Apply picks a relation file under $PGDATA/base (skipping
// template-DB OIDs 1 and 4), and for each of `pages` count
// overwrites [page_offset + tear_at, page_offset + page_size)
// with random bytes via dd — emulating a torn 8 KiB write at
// the visible on-disk shape.  NoRecovery — the tear is
// persistent.
func (tornPageFault) Apply(ctx context.Context, args Args, ts TargetSet) (Recovery, error) {
	target := args["target"]
	if target == "" {
		target = "pg_random"
	}
	pgdata := args["pgdata"]
	if pgdata == "" {
		pgdata = "/var/lib/postgresql/data"
	}
	pages := 1
	if raw := args["pages"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("torn_page: pages must be a positive int (got %q)", raw)
		}
		pages = n
	}
	pageSize := 8192
	if raw := args["page_size"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n%512 != 0 {
			return nil, fmt.Errorf("torn_page: page_size must be a positive multiple of 512 (got %q)", raw)
		}
		pageSize = n
	}
	tearAt := 4096
	if raw := args["tear_at"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n >= pageSize {
			return nil, fmt.Errorf("torn_page: tear_at must be in [0, page_size) (got %q)", raw)
		}
		tearAt = n
	}
	tgs, err := ts.Pick(target)
	if err != nil {
		return nil, err
	}
	tearLen := pageSize - tearAt
	if tearLen == 0 {
		return nil, fmt.Errorf("torn_page: tear_at=%d page_size=%d leaves zero bytes to tear", tearAt, pageSize)
	}
	// Locate a victim file, compute a random page boundary,
	// overwrite the tail of the page with random bytes.  We
	// exclude PGDATA/base/1 (template1) and PGDATA/base/4
	// (template0) — tearing template DBs has unpredictable
	// blast radius and is not the failure shape the user wants.
	// `dd bs=<sector> seek=<sector_idx> conv=notrunc` is the
	// universal mechanism; `if=/dev/urandom` lands new random
	// bytes (an "all-zeros" tear is less interesting since the
	// page checksum still detects all-zero pages explicitly).
	for _, t := range tgs {
		// Single sh script does both the selection AND the tear
		// so the path doesn't have to round-trip through Go.
		script := fmt.Sprintf(
			`set -e
pgdata=%[1]s
page_size=%[2]d
tear_at=%[3]d
tear_len=%[4]d
pages=%[5]d
# Collect candidate files: every regular file under base/<oid>/
# where oid is not 1 (template1) or 4 (template0).  Exclude
# files smaller than one page — they can't host a torn page.
candidates_file=$(mktemp)
trap 'rm -f "$candidates_file"' EXIT
# Exclude template DBs (OIDs 1, 4) AND every system catalog
# relfilenode (PG's first user OID is 16384, so any basename
# whose numeric prefix < 16384 is a system table — pg_class,
# pg_xact-shadow, pg_largeobject_metadata, etc.).  Tearing a
# system catalog renders the cluster unreadable across ALL
# subsequent iterations on the cell — the 8h soak's run #6
# saw a torn /base/5/2703.1 (pg_largeobject_metadata) make
# every later backup fail with 'could not open file', not
# because the BACKUP tool missed corruption but because PG
# itself can't respond to SHOW or even basic queries.  That
# breaks the iteration model: the soak expects each cell to
# recover between faults.  The corruption-detection invariant
# for system catalogs belongs in a scenario test, not in a
# random soak.
find "$pgdata/base" -mindepth 2 -maxdepth 2 -type f -size +"${page_size}c" 2>/dev/null | \
    grep -vE "/base/(1|4)/" | \
    awk -F/ '{
        # The basename may be "<relfilenode>" or
        # "<relfilenode>.<segno>" or "<relfilenode>_fsm"/_vm/_init.
        # Strip the suffix and check the numeric prefix.
        n = $NF; sub(/[._].*/, "", n)
        # PG_USER_OID_MIN = 16384 (FirstNormalObjectId).
        if (n + 0 >= 16384) print $0
    }' > "$candidates_file" || true
n=$(wc -l < "$candidates_file" | tr -d ' ')
if [ "$n" -eq 0 ]; then
    # Early-soak transient: PG initdb has finished but no user
    # database has been seeded yet (only template1/template0,
    # which we EXCLUDE on purpose).  A fault that produces NO
    # candidates is vacuously satisfied — there is nothing to
    # tear — so exit 0 cleanly rather than counting every
    # early-iteration tick as a fault_apply_failed event.
    echo "torn_page: no candidate relation files yet — skipping"
    exit 0
fi
i=0
while [ "$i" -lt "$pages" ]; do
    file=$(shuf -n 1 "$candidates_file")
    size=$(stat -c %%s "$file" 2>/dev/null || stat -f %%z "$file" 2>/dev/null)
    case "$size" in
        ''|*[!0-9]*) echo "torn_page: cannot stat $file" >&2; exit 3 ;;
    esac
    page_count=$((size / page_size))
    [ "$page_count" -gt 0 ] || { i=$((i + 1)); continue; }
    page_idx=$(shuf -i 0-$((page_count - 1)) -n 1)
    # Compute byte offset for the tear, in 512-byte sectors so
    # dd's bs/seek arithmetic stays integral on any FS.
    sector_size=512
    tear_offset_bytes=$((page_idx * page_size + tear_at))
    if [ $((tear_offset_bytes %% sector_size)) -ne 0 ]; then
        echo "torn_page: misaligned tear offset" >&2
        exit 4
    fi
    tear_offset_sectors=$((tear_offset_bytes / sector_size))
    tear_len_sectors=$((tear_len / sector_size))
    dd if=/dev/urandom of="$file" bs=$sector_size \
        seek=$tear_offset_sectors count=$tear_len_sectors conv=notrunc 2>/dev/null
    echo "torn_page: $file page=$page_idx tear_at=$tear_at len=$tear_len"
    i=$((i + 1))
done
`, pgdata, pageSize, tearAt, tearLen, pages)
		if out, err := t.Exec(ctx, "sh", "-c", script); err != nil {
			return nil, fmt.Errorf("torn_page on %s: %w (output: %s)",
				t.Name(), err, strings.TrimSpace(string(out)))
		}
	}
	return NoRecovery, nil
}
