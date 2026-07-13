// Package cli wires the cobra command tree for the pg_hardstorage binary.
//
// All output flows through internal/output. Subcommand bodies that aren't
// implemented yet return a typed *output.Error with a "notimpl.<name>"
// code, so JSON / NDJSON consumers see structured failure (with stable
// exit code 1) and the text renderer prints the same with a hint.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fips"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// Execute parses os.Args, runs the matched command, renders any error
// through the dispatcher attached during PersistentPreRunE, and returns
// the process exit code per the v1 contract.
//
// main() should be `os.Exit(cli.Execute())`.
func Execute() int { return Run(NewRoot()) }

// Run executes the given root command. Tests construct a root, set its
// args / writers, and call Run to observe the same behavior production
// gets from Execute().
func Run(root *cobra.Command) int {
	// Profiling — flags are persistent on the root, so we
	// can read them before any subcommand resolves.
	// startProfiling is a no-op when no flag is set.  We
	// parse args here without executing so the persistent
	// flags are populated even when ExecuteC bails early
	// (e.g. unknown subcommand).  ParseFlags returns an
	// error for unknown flags but accepts unknown
	// positional args, which is what we want.
	_ = root.ParseFlags(os.Args[1:])
	profH, profErr := startProfiling(root)
	if profErr != nil {
		fmt.Fprintln(root.ErrOrStderr(), "error:", profErr)
		return int(output.ExitCodeFor(profErr))
	}
	defer stopProfiling(profH)

	cmd, err := root.ExecuteC()
	if err == nil {
		return int(output.ExitOK)
	}
	// Rewrite cobra's bare "accepts N arg(s), received M"
	// into a message that names the expected positionals
	// and shows a working example.  The original error's
	// exit code is preserved by wrapping output.ErrUsage.
	err = enrichArgsError(cmd, err)
	// Translate cobra's required-flag failure (from MarkFlagRequired)
	// into the structured usage.missing_flag error + ExitMisuse, so the
	// declarative path matches the older hand-written "X is required"
	// checks (same code, same exit).
	err = enrichRequiredFlagError(cmd, err)
	// Audit+ #3 — `--on-error-llm` auto-launch.  If the
	// global flag (or env var) is set AND the failure carries a
	// structured error code AND a loaded skill declares
	// `auto_on_error: [<code>]`, drop into the matching skill
	// before exiting.  The original failure's exit code is
	// preserved — auto-launch is a side car, not a substitute
	// for surfacing the error.
	if cmd != nil && shouldAutoLaunchLLM(cmd, err) {
		// Best-effort.  A failure here doesn't change the exit
		// code; we still return what the original command would.
		_ = launchAutoLLM(cmd, err)
	}
	// Best-effort: render the error through the active dispatcher if
	// PersistentPreRunE got far enough to install one.
	if cmd != nil {
		if d, ok := cmd.Context().Value(dispatcherKey{}).(*output.Dispatcher); ok && d != nil {
			_ = d.Result(output.NewResult(cmd.CommandPath()).WithError(output.ToError(err)))
			return int(output.ExitCodeFor(err))
		}
	}
	// Pre-dispatcher failure (very early). Fall back to a
	// plain stderr line.  When the error is a structured
	// *output.Error we print only its Message — the
	// operator doesn't want to read "usage.bad_args:" in
	// front of every typo'd command line.  JSON / NDJSON
	// consumers still get the structured code via the
	// dispatcher path above.
	if structured, ok := output.AsOutputError(err); ok && structured.Message != "" {
		fmt.Fprintln(root.ErrOrStderr(), "error:", structured.Message)
	} else {
		fmt.Fprintln(root.ErrOrStderr(), "error:", err)
	}
	return int(output.ExitCodeFor(err))
}

// NewRoot returns the top-level cobra command. Exposed mainly for tests
// that want to invoke commands without going through Execute().
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pg_hardstorage",
		Short: "PostgreSQL backup, done right.",
		Long: `pg_hardstorage is an enterprise-grade PostgreSQL backup tool.

Resilience, compliance, simplicity, and scale-spanning (10 GB to 100+ TB)
are the design north stars. WAL streaming over the replication protocol is
the central data plane. Apache 2.0.

Every command is a real implementation (no scaffolding stubs).
The advanced surfaces — gRPC, OIDC + RBAC, advise+execute LLM mode,
sandbox-PG runtime — extend per docs/SPEC.md.`,
		// We render errors ourselves through the dispatcher.
		SilenceErrors: true,
		// Don't auto-print usage on every RunE error (only on argument errors).
		SilenceUsage: true,
		// Two-stage gate: refuse euid 0 first (the agent must run
		// as a non-root system user; see refuse_root.go for the
		// allow-list and rationale), then build the dispatcher.
		// Order matters — running the dispatcher as root would
		// open the keyring + state dirs with root-owned files that
		// a later legitimate run as `pgbackup` couldn't read.
		PersistentPreRunE: chainPreRunE(refuseRoot, installDispatcher, resolveDeploymentDefaultsPreRun),
	}

	// Flag-parse failures (unknown flag, bad flag value) are usage
	// errors. cobra's default FlagErrorFunc returns a bare error that
	// ExitCodeFor can't classify, so it leaked out as the generic
	// exit 1 — while a missing positional arg (handled by our own
	// Args validators) correctly exits 2. exitcode.go's contract
	// explicitly lists "unknown flag" as an ErrUsage case; wrap flag
	// errors so they map to ExitMisuse uniformly. Inherited by every
	// subcommand that doesn't set its own FlagErrorFunc.
	root.SetFlagErrorFunc(func(_ *cobra.Command, ferr error) error {
		return output.NewError("usage.flag", ferr.Error()).Wrap(output.ErrUsage)
	})

	root.PersistentFlags().StringP("config", "c", "", "path to config file (default: XDG/FHS lookup)")
	root.PersistentFlags().StringP("output", "o", "", "output format: text|json|ndjson|yaml|template|csv|markdown|html|tap|junit|pdf (default: text on TTY, json off-TTY)")
	root.PersistentFlags().String("template", "", "Go text/template applied when --output template (or implied if --template is set without --output)")
	root.PersistentFlags().BoolP("quiet", "q", false, "suppress non-essential output")
	root.PersistentFlags().Bool("no-color", false, "disable ANSI color in text output")
	root.PersistentFlags().String("otel-endpoint", "",
		"OpenTelemetry OTLP/HTTP endpoint (e.g. http://otel-collector:4318); empty disables tracing")
	root.PersistentFlags().Bool("otel-stdout", false,
		"also export OpenTelemetry traces to stderr (useful for dev)")
	root.PersistentFlags().Bool("on-error-llm", false,
		"on a structured-error failure, drop into the matching LLM helper skill (auto_on_error trigger). Also enabled by PG_HARDSTORAGE_ON_ERROR_LLM=1.")
	root.PersistentFlags().Bool("airgapped", false,
		"refuse outbound endpoints (LLM providers, sinks, OTLP collectors) outside loopback / RFC1918 / explicit airgap.allowlist. Also enabled by PG_HARDSTORAGE_AIRGAPPED=1 or `airgapped: strict` in the config file.")

	// Profiling — wired here as persistent flags so the
	// long-running commands (wal stream, backup runner) can
	// be profiled without a separate build.  Off by default;
	// no overhead when unset.  See profile.go for the
	// start/stop hooks the Run wrapper invokes.
	root.PersistentFlags().String("cpu-profile", "",
		"write a pprof CPU profile to this path for the duration of the command (`go tool pprof <path>` to analyse). Off when empty.")
	root.PersistentFlags().String("mem-profile", "",
		"write a pprof heap profile to this path at command exit. Off when empty.")
	root.PersistentFlags().Int("profile-port", 0,
		"if non-zero, expose net/http/pprof on 127.0.0.1:<port> for live profiling of long-running commands (e.g. `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30`). Off when zero.")

	root.AddCommand(
		newVersionCmd(),
		newInitCmd(),
		newRealBackupCmd(),
		newRealRestoreCmd(),
		newRealStatusCmd(),
		newRealListCmd(),
		newRealShowCmd(),
		newManifestCmd(),
		newLogsCmd(),
		newRealDoctorCmd(),
		newVerifyCmd(),
		newDemoCmd(),
		newLintCmd(),
		newExplainCmd(),
		newChangelogCmd(),
		newGlossaryCmd(),
		newDeploymentCmd(),
		newScheduleCmd(),
		newTimetableCmd(),
		newPatroniCmd(),
		newNotifyCmd(),
		newRotateCmd(),
		newRealRepoCmd(),
		newRepairCmd(),
		newGameDayCmd(),
		newRunbookCmd(),
		newWalCmd(),
		newLogicalCmd(),
		newTimeTravelCmd(),
		newStandbyCmd(),
		newPartialCmd(),
		newHoldCmd(),
		newClassifyCmd(),
		newResidencyCmd(),
		newSloCmd(),
		newCostCmd(),
		newCapacityCmd(),
		newKmsCmd(),
		newAnomalyCmd(),
		newAuditCmd(),
		newApprovalCmd(),
		newComplianceCmd(),
		newForecastCmd(),
		newRecoveryCmd(),
		newJitCmd(),
		newThresholdCmd(),
		newIntegrityCmd(),
		newInsiderCmd(),
		newDsaCmd(),
		newFleetCmd(),
		newServerCmd(),
		newAgentCmd(),
		newLlmCmd(),
		newDbCmd(),
		newRedactCmd(),
		newCompatCmd(),
		newPluginCmd(),
		newCompletionCmd(root),
		newDumpCmdTreeCmd(),
	)
	return root
}

// versionBody is the typed payload for `pg_hardstorage version`.
// It implements text.TextWriter so the text renderer prints the same
// one-liner the binary did before, while JSON consumers get fields.
type versionBody struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Variant string `json:"variant"` // "default" | "fips"
	FIPS    bool   `json:"fips"`
}

// WriteText satisfies the text.TextWriter contract. We don't import the
// text-renderer package here to avoid a cycle; the interface is a
// duck-typed io.Writer-taker.
func (v versionBody) WriteText(w io.Writer) error {
	suffix := ""
	if v.FIPS {
		suffix = " [FIPS]"
	}
	_, err := fmt.Fprintf(w, "pg_hardstorage %s%s (%s, built %s)", v.Version, suffix, v.Commit, v.Date)
	return err
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(versionBody{
				Version: version.Version,
				Commit:  version.Commit,
				Date:    version.Date,
				Variant: fips.Variant(),
				FIPS:    fips.Enabled(),
			}))
		},
	}
}

// stub returns a cobra command whose body emits a structured *output.Error
// with code "notimpl.<command>" so consumers can detect the scaffold state
// uniformly. Exit code: ExitError (1). Persistent flags (e.g. --output)
// remain parsed so users can request JSON form even from a stub.
func stub(use, short, long string) *cobra.Command {
	c := &cobra.Command{
		Use:          use,
		Short:        short,
		Long:         long,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return output.NewError(
				"notimpl."+cmd.Name(),
				fmt.Sprintf("`%s` is not yet implemented; this is a scaffold tracking the design plan", cmd.CommandPath()),
			).WithSuggestion(&output.Suggestion{
				Human:  "see the design specification for what this command will do",
				DocURL: "docs/SPEC.md",
			})
		},
	}
	// Accept any positional args so stubs don't choke on planned args.
	c.Args = cobra.ArbitraryArgs
	// Allow unknown flags to pass through silently (the planned flags
	// don't exist yet); cobra otherwise rejects them.
	c.FParseErrWhitelist.UnknownFlags = true
	return c
}

// newLogsCmd is implemented in logs.go.

// newVerifyCmd is implemented in verify.go.

func newGameDayCmd() *cobra.Command {
	return newRealGameDayCmd()
}

// newRunbookCmd is implemented in runbook.go.

func newLogicalCmd() *cobra.Command {
	return newRealLogicalCmd()
}

func newTimeTravelCmd() *cobra.Command {
	return newRealTimeTravelCmd()
}

func newStandbyCmd() *cobra.Command {
	return newRealStandbyCmd()
}

func newPartialCmd() *cobra.Command {
	return newRealPartialCmd()
}

// newHoldCmd is implemented in hold.go.

// newClassifyCmd is implemented in classify.go.

// newSloCmd is implemented in slo.go.

func newCostCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cost <report>",
		Short: "Per-deployment / per-tenant repository cost",
	}
	c.AddCommand(newRealCostCmd())
	return c
}

func newCapacityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "capacity <report|preflight>",
		Short: "Projected repository size, WAL volume, and pre-flight free-space checks",
	}
	c.AddCommand(newRealCapacityCmd())
	c.AddCommand(newCapacityPreflightCmd())
	return c
}

// newKmsCmd is implemented in kms.go.

// newAuditCmd is implemented in audit.go.

func newFleetCmd() *cobra.Command {
	return newRealFleetCmd()
}

func newServerCmd() *cobra.Command {
	return newRealServerCmd()
}

func newLlmCmd() *cobra.Command {
	return newRealLlmCmd()
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                   "completion <bash|zsh|fish|powershell>",
		Short:                 "Generate shell completion scripts",
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return output.NewError("usage.unknown_shell", fmt.Sprintf("unknown shell %q", args[0])).Wrap(output.ErrUsage)
			}
		},
	}
}

func newLintCmd() *cobra.Command      { return newLintCmdImpl() }
func newDemoCmd() *cobra.Command      { return newDemoCmdImpl() }
func newExplainCmd() *cobra.Command   { return newExplainCmdImpl() }
func newChangelogCmd() *cobra.Command { return newChangelogCmdImpl() }
func newGlossaryCmd() *cobra.Command  { return newGlossaryCmdImpl() }
