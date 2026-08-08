// Package walfetchcmd builds the literal `restore_command` GUC value
// PG executes for each WAL segment during recovery.
//
// Lives in its own leaf package so every site that writes
// postgresql.auto.conf — internal/restore (auto-recovery + PITR),
// internal/restore/postverify (verifier-side temp cluster),
// internal/cli/restore (CLI-driven PITR), internal/standby
// (replica bootstrap), internal/timetravel (time-travel queries) —
// shares a single implementation without forming an import cycle
// (the parent restore package already imports postverify).
package walfetchcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RestoreBinEnv overrides the executable embedded in the generated
// restore_command. The default is the path of the agent that ran the
// restore (os.Executable() at each call site) — correct when PG recovers
// in the SAME environment as that agent. When PG recovers elsewhere — a
// vanilla `postgres:NN` container, a Patroni/k8s pod, a different host —
// that absolute path doesn't exist there and recovery FATALs with
// "could not restore file ... from archive: command not found" (issue
// #107). Set this to the path (or bare name, resolved via PATH) at which
// `pg_hardstorage` is reachable in the recovery environment, e.g.
//
//	PG_HARDSTORAGE_RESTORE_BIN=pg_hardstorage          # on PATH in the PG image
//	PG_HARDSTORAGE_RESTORE_BIN=/usr/local/bin/pg_hardstorage
//
// Read at restore time (when the restore_command is generated), so it
// describes where the binary will live during recovery, not where the
// restore ran.
const RestoreBinEnv = "PG_HARDSTORAGE_RESTORE_BIN"

// Build returns the literal `restore_command` GUC value PG should
// execute for each WAL segment during recovery.  The result chains
// `<agentBin> wal fetch <deployment> %f %p --repo <repoURL>` to a
// short shell-built-in tail that maps the agent's exit code 6
// (ExitNotFound, per the v1 exit-code contract) to PG's expected
// exit code 1.
//
// Why no `sh -c` wrapper here: PG's system() already invokes
// `/bin/sh -c "<value>"`, so the value IS a single shell script —
// adding another `sh -c "..."` layer means the outer shell strips
// our quoting and the inner shell re-tokenizes.  In particular, an
// `&` inside the inner double-quoted URL (`s3://…?…&…&…`) survives
// the OUTER shell's quote-aware tokenizer but lands in the inner
// shell's script unquoted, where it becomes a background-process
// operator and splits `wal fetch …repo …&…&…` into multiple
// commands — turning the wrapper into a no-op that always returns
// exit 0.  Reproduced on 2026-05-12 in the restore sandbox repro:
// `dash -c 'sh -c "echo a&b"'` prints `a` and reports `b: not
// found`.  Dropping the nested `sh -c` keeps the URL inside ONE
// layer of single quotes that survive the only shell that actually
// parses the script.
//
// Quoting model: each of `agentBin`, `deployment`, `repoURL` is
// wrapped in POSIX single quotes inside the script (ShellQuote uses
// the close-escape-reopen `'\”` idiom for embedded single quotes).
// `$?` and `$ec` are written without escapes — they're consumed by
// the only shell that runs, exactly when we want.
//
// Why the wrapping is required (and why every restore_command site
// in the codebase MUST go through this helper):
//
// `pg_hardstorage wal fetch` returns ExitNotFound (6) for missing
// segments — that's the v1 contract documented in
// internal/cli/wal_fetch_extern_test.go.  PG's postmaster reaper
// (src/backend/postmaster/postmaster.c) classifies startup-process
// exits with three macros:
//
//	EXIT_STATUS_0 — recovery completed, transition to operational
//	EXIT_STATUS_1 — `recovery_end_command` requested promotion;
//	                postmaster restarts startup so it can promote
//	anything else — HandleChildCrash: log
//	                "server process (PID …) exited with exit code N"
//	                and "terminating any other active server
//	                processes", then attempt cluster restart
//
// When `restore_command` exits 6 at end-of-archive (the normal "no
// more WAL" case once the cluster is caught up), the startup process
// inherits 6 and postmaster routes it through HandleChildCrash.  PG
// restarts, recovery walks the bundled WAL again, reaches consistency,
// asks for the next segment, gets exit 6, restarts — an infinite
// loop that surfaces as "sandbox did not accept connections within
// 180s" in the testkit's assert_restored_match step.  Reproduced in
// the restore sandbox failure class on 2026-05-12 (5 L3 + 1 L8
// scenarios from the regression sweep).
//
// Mapping 6 → 1 lets PG see the "stop & promote" signal it expects
// at end-of-archive and complete the promote.  Every OTHER non-zero
// code is an infrastructure fault, and passing it through would NOT
// surface as a crash — PG treats every plain nonzero restore_command
// exit as "file not available" (only a signal-death aborts recovery),
// so a passed-through storage.unreachable read as a clean
// end-of-archive and the restore promoted silently behind. The tail
// therefore self-terminates with SIGABRT on those codes — see
// oneShotTail. Standby restore_commands keep the lenient pass-through
// via BuildStandby; a standby's contract is poll-and-retry.
//
// The result is the unquoted GUC value: callers wrapping it in PG's
// SQL single-quote literal (`restore_command = '<this>'`) MUST run
// it through PG's standard SQL string escape first (double the `'`
// chars) — Build's output legitimately contains `'` from the
// argument quoting and would otherwise close the SQL string early.
// `%f` / `%p` are emitted as literal placeholders for PG to
// substitute per the usual restore_command contract.
func Build(agentBin, deployment, repoURL string) string {
	return BuildWithIdentity(agentBin, deployment, repoURL, "")
}

// BuildWithIdentity is Build carrying the seed backup's
// system_identifier: the generated command passes it to `wal fetch
// --expect-system-identifier`, which refuses the FIRST segment
// archived by a different cluster — typed and named — instead of
// letting PostgreSQL discover the mix mid-replay with a FATAL that
// names neither the deployment nor the repair. Empty sysID emits
// exactly Build's command (older call sites and pre-schema manifests
// keep their behaviour).
func BuildWithIdentity(agentBin, deployment, repoURL, sysID string) string {
	// `-o text -q`: restore_command runs off-TTY, where the global
	// output default is JSON — every routine "segment not in repo"
	// probe (normal at end-of-WAL) would otherwise dump a 10-line
	// pretty-printed JSON document into the PostgreSQL server log,
	// twice per poll cycle, and restore error reports quoting the log
	// tail became walls of nested escaped JSON. Text mode keeps it to
	// a single line.
	expect := ""
	if sysID != "" {
		expect = " --expect-system-identifier " + ShellQuote(sysID)
	}
	return fmt.Sprintf(`%s wal fetch %s %%f %%p --repo %s%s -o text -q; %s`,
		ShellQuote(resolveRestoreBin(agentBin)),
		ShellQuote(deployment),
		ShellQuote(repoURL),
		expect,
		oneShotTail())
}

// BuildStandby is Build for a STANDBY's restore_command, where the
// exit-code discipline must stay lenient: a standby polls
// restore_command forever, and "segment not available yet" — the
// answer it gets all day, every day, while caught up — is exit
// nonzero by contract. Aborting on infrastructure faults there would
// turn every transient repo blip into a crashed replica.
//
// The trade this keeps (deliberately): a PERSISTENT infrastructure
// fault leaves the standby up and silently frozen at its last
// replayed position, indistinguishable to PG from "no new WAL yet".
// That is the standby's inherent failure shape — surface it with lag
// monitoring, which every HA deployment already has. The one-shot
// paths (Build) refuse to make that trade because a one-shot recovery
// that stops early PROMOTES, and a promoted-behind cluster is data
// loss, not lag.
func BuildStandby(agentBin, deployment, repoURL string) string {
	return BuildStandbyWithIdentity(agentBin, deployment, repoURL, "")
}

// BuildStandbyWithIdentity is BuildStandby carrying the seed backup's
// system_identifier (see BuildWithIdentity). The identity REFUSAL
// composes correctly with the lenient tail: a mismatch exits nonzero,
// which a standby treats as "not archived yet" — so a foreign-lineage
// standby freezes at the boundary and logs the typed mismatch every
// poll, rather than replaying foreign WAL. Frozen-and-explaining is
// the standby posture; the one-shot paths abort instead.
func BuildStandbyWithIdentity(agentBin, deployment, repoURL, sysID string) string {
	expect := ""
	if sysID != "" {
		expect = " --expect-system-identifier " + ShellQuote(sysID)
	}
	return fmt.Sprintf(`%s wal fetch %s %%f %%p --repo %s%s -o text -q; ec=$?; [ $ec = 6 ] && exit 1 || exit $ec`,
		ShellQuote(resolveRestoreBin(agentBin)),
		ShellQuote(deployment),
		ShellQuote(repoURL),
		expect)
}

// resolveRestoreBin picks the executable to embed in the restore_command:
// the RestoreBinEnv override when set (the recovery environment may differ
// from the restore host — issue #107), otherwise the companion-normalised
// agentBin the caller resolved via os.Executable().
func resolveRestoreBin(agentBin string) string {
	if override := strings.TrimSpace(os.Getenv(RestoreBinEnv)); override != "" {
		return override
	}
	return normalizeAgentBin(agentBin)
}

// knownCompanionBinaries are executables built from this repo that drive a
// restore but do NOT implement `wal fetch`.  When one of them is the running
// process, os.Executable() — which every restore_command site resolves the
// agent path from — yields the companion's path, and PG's recovery then runs
// e.g. `pg_hardstorage_simple wal fetch …`, which fails with
//
//	pg_hardstorage_simple: unknown argument "wal"
//
// so the restored cluster sits forever waiting for WAL that never arrives
// (issue #105).  pg_hardstorage_simple is the interactive kind-interface
// companion; only the full `pg_hardstorage` agent implements `wal fetch`.
var knownCompanionBinaries = map[string]bool{
	"pg_hardstorage_simple": true,
}

// normalizeAgentBin rewrites a companion-binary path to the full
// `pg_hardstorage` agent that sits beside it (same directory), so the
// restore_command PG executes during recovery names a binary that actually
// implements `wal fetch`.  The companion and the agent are built and
// installed together (…/bin/pg_hardstorage_simple alongside …/bin/
// pg_hardstorage), so a basename swap is the correct, filesystem-free
// resolution.  Any unrecognised name — the agent itself, a custom install,
// or a test binary — is returned unchanged.
func normalizeAgentBin(bin string) string {
	dir, base := filepath.Split(bin)
	if knownCompanionBinaries[base] {
		return dir + "pg_hardstorage"
	}
	return bin
}

// ShellQuote wraps s in POSIX single quotes, using the standard
// `'\”` close-escape-reopen idiom for any embedded single quote.
// The result is one POSIX shell token regardless of whitespace,
// `&`, `$`, backticks, or any other metacharacter.
//
// Exported so tests can pin the contract independently of Build's
// full restore_command shape.  Same shape as the in-tree shellQuote
// helpers in scp / testkit-runner.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
