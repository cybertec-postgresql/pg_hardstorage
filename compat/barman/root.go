// root.go — Barman shim root: top-level `barman` cobra tree wiring the seven supported verbs.
package barman

import (
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// NewRoot returns the top-level `barman` command tree.  Operators
// symlink bin/pg-hardstorage-barman -> /usr/bin/barman; existing
// scripts hit this root when they invoke `barman <verb>`.
//
// Eight verbs are implemented (backup, recover, list-backup,
// show-backup, check, delete, status, plus the wal-archive companion
// at barman-wal-archive).  Everything else refuses cleanly with a
// remediation pointing at the native equivalent.
func NewRoot(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "barman",
		Short: "Backup and Recovery Manager (pg_hardstorage compat shim)",
		Long: `barman is the legacy Barman CLI surface, served by the
pg_hardstorage v1.1+ compatibility shim.

This binary accepts the most-cited Barman verbs and translates
them to native pg_hardstorage commands.  Existing cron jobs,
archive_command settings, and monitoring scripts run unchanged
but produce native pg_hardstorage backups.

Verbs not in the v1.1 surface refuse with a remediation pointing
at the native equivalent.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Barman's global flags. Real barman 3.20 accepts all of these
	// (barman/cli.py); the shim registered none, so `barman -f json
	// list-backup db1` died on "unknown shorthand flag: 'f'" — and the
	// short bool forms were worse: `barman -q backup db2` reported
	// `unknown command "db2"`, blaming the operator's server name for
	// an unregistered flag. They are presentation concerns the native
	// structured output already covers, so they parse and are ignored.
	registerBarmanGlobalFlags(root.PersistentFlags())

	root.AddCommand(
		newBackupCmd(stdout, stderr),
		newRecoverCmd(stdout, stderr),
		newListBackupCmd(stdout, stderr),
		newShowBackupCmd(stdout, stderr),
		newCheckCmd(stdout, stderr),
		newDeleteCmd(stdout, stderr),
		newStatusCmd(stdout, stderr),
	)

	// Barman spells several verbs in both singular and plural. Real
	// 3.20 accepts `list-backups` and `show-backups` as first-class
	// verbs; the shim answered "unknown command" with no pointer at
	// the singular form it does implement. Cobra aliases cost nothing
	// and remove a whole class of "the shim is broken" reports.
	addVerbAliases(root)

	// Refusal-only verbs.  Each gets a tiny shell command that prints
	// the canonical refusal line and exits non-zero.  We register
	// them explicitly (rather than catch-all the unknown command)
	// because it's nicer to surface remediation at flag-parse time
	// than to fall through Cobra's "unknown command" generic error.
	root.AddCommand(
		newRefusalCmd(stderr, "cron", "use systemd timers or native `pg_hardstorage agent`"),
		newRefusalCmd(stderr, "archive-wal", "no equivalent — native uses `pg_hardstorage wal stream` (continuous)"),
		newRefusalCmd(stderr, "switch-wal", "not implemented in v1.1; native commits trigger pg_switch_wal automatically"),
		newRefusalCmd(stderr, "rebuild-xlogdb", "not implemented in v1.1; native repository is self-describing"),
		newRefusalCmd(stderr, "diagnose", "use `pg_hardstorage doctor` (more thorough)"),
		newRefusalCmd(stderr, "get-wal", "not implemented in v1.1; PG invokes `pg_hardstorage wal fetch` via restore_command"),
		newRefusalCmd(stderr, "put-wal", "not implemented in v1.1; PG invokes `barman-wal-archive` (this shim) via archive_command"),
		newRefusalCmd(stderr, "replication-status", "use `pg_hardstorage doctor <deployment>` for slot health"),
		newRefusalCmd(stderr, "show-server", "use `pg_hardstorage deployment show <deployment>`"),
		newRefusalCmd(stderr, "list-server", "use `pg_hardstorage deployment list`"),
		newRefusalCmd(stderr, "lock-directory-cleanup", "not implemented in v1.1; native uses lockless atomic commits"),
		newRefusalCmd(stderr, "verify-backup", "use `pg_hardstorage verify <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "verify", "use `pg_hardstorage verify <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "keep", "use `pg_hardstorage hold add <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "receive-wal", "use `pg_hardstorage wal stream` (continuous, slot-based)"),

		// Real Barman 3.20 verbs that had no shim entry at all and so
		// fell through to cobra's generic "unknown command" with no
		// remediation. Each now names the native equivalent.
		newRefusalCmd(stderr, "check-backup", "use `pg_hardstorage verify <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "check-wal-archive", "use `pg_hardstorage wal list <deployment>` and `pg_hardstorage doctor <deployment>` (WAL-gap check)"),
		newRefusalCmd(stderr, "check-archived-wal-range", "use `pg_hardstorage wal list <deployment>`"),
		newRefusalCmd(stderr, "config-switch", "not implemented; pg_hardstorage has no per-server config models"),
		newRefusalCmd(stderr, "config-update", "edit pg_hardstorage.yaml directly, or use `pg_hardstorage deployment edit <deployment>`"),
		newRefusalCmd(stderr, "export-backup", "use `pg_hardstorage repo bundle export` for an air-gapped copy"),
		newRefusalCmd(stderr, "import-backup", "use `pg_hardstorage repo bundle import`"),
		newRefusalCmd(stderr, "generate-manifest", "manifests are written and signed at backup time; inspect with `pg_hardstorage manifest show <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "list-files", "use `pg_hardstorage manifest files <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "list-servers", "use `pg_hardstorage deployment list`"),
		newRefusalCmd(stderr, "show-servers", "use `pg_hardstorage deployment show <deployment>`"),
		newRefusalCmd(stderr, "list-processes", "not implemented; the native agent reports work via `pg_hardstorage status` and its metrics endpoint"),
		newRefusalCmd(stderr, "list-processlist", "not implemented; the native agent reports work via `pg_hardstorage status` and its metrics endpoint"),
		newRefusalCmd(stderr, "terminate-process", "not implemented; stop the agent or the specific command instead"),
		newRefusalCmd(stderr, "switch-xlog", "not implemented in v1.1; native commits trigger pg_switch_wal automatically"),
		newRefusalCmd(stderr, "sync-backup", "use `pg_hardstorage repo replicate` to mirror a repository"),
		newRefusalCmd(stderr, "sync-wals", "use `pg_hardstorage repo replicate` to mirror a repository"),
		newRefusalCmd(stderr, "sync-info", "use `pg_hardstorage repo replicate --preview`"),
	)

	// Cobra's default unknown-command error names no alternative.
	// Enabling suggestions turns `barman lst-backup` into a
	// "Did you mean this?" pointing at a verb the shim implements.
	root.SuggestionsMinimumDistance = 2

	return root
}

// registerBarmanGlobalFlags declares the global flags real Barman
// accepts. They are presentation / logging concerns (output format,
// colour, verbosity, alternate config file) that the native
// structured-output stack replaces, so they parse and are ignored
// rather than refused: the point is that an existing command line
// keeps running.
func registerBarmanGlobalFlags(fs *pflag.FlagSet) {
	type globalFlag struct {
		name      string
		shorthand string
		isBool    bool
	}
	for _, g := range []globalFlag{
		{name: "format", shorthand: "f"},
		{name: "config", shorthand: "c"},
		{name: "color"},
		{name: "colour"},
		{name: "log-level"},
		{name: "quiet", shorthand: "q", isBool: true},
		{name: "debug", shorthand: "d", isBool: true},
		{name: "verbose", shorthand: "v", isBool: true},
	} {
		if g.isBool {
			fs.BoolP(g.name, g.shorthand, false, "")
		} else {
			fs.StringP(g.name, g.shorthand, "", "")
		}
		_ = fs.MarkHidden(g.name)
	}
}

// addVerbAliases attaches Barman's alternate spellings to the verbs
// the shim implements.
func addVerbAliases(root *cobra.Command) {
	aliases := map[string][]string{
		"list-backup": {"list-backups"},
		"show-backup": {"show-backups"},
	}
	for _, c := range root.Commands() {
		if alts, ok := aliases[c.Name()]; ok {
			c.Aliases = append(c.Aliases, alts...)
		}
	}
}

// newRefusalCmd registers a verb that always refuses with a clear
// remediation.  Used for Barman commands that deliberately have no
// pg_hardstorage equivalent (different architecture) or that the
// v1.1 surface has not yet covered.
func newRefusalCmd(stderr io.Writer, name, suggestion string) *cobra.Command {
	c := &cobra.Command{
		Use:   name,
		Short: "Not implemented in v1.1 (Barman compat refusal)",
		// Accept any args so the user gets the remediation regardless
		// of what they typed after the verb.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return refuseUnimplemented(stderr, name, suggestion)
		},
	}
	c.SilenceUsage = true
	c.FParseErrWhitelist.UnknownFlags = true
	return c
}
