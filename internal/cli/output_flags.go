// output_flags.go — --format/--output renderer dispatch (text/json/markdown/csv/junit/tap/etc).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/tracing"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
	renderercsv "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/csv"
	rendererhtml "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/html"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
	rendererjunit "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/junit"
	renderermd "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/markdown"
	rendererndjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/ndjson"
	rendererpdf "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/pdf"
	renderertap "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/tap"
	renderertemplate "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/template"
	renderertext "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/text"
	rendereryaml "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/yaml"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"

	// Self-registering sink plugins. Importing for side effects only:
	// each package's init() puts itself in output.DefaultSinkRegistry.
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/cef"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/datadog"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/discord"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/email"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/jira"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/opsgenie"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/otelevents"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/pagerduty"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/servicenow"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/slack"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/splunkhec"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/syslog"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/teams"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/webhook"
)

// EnvOutput is the env var that overrides --output when the flag is unset.
const EnvOutput = "PG_HARDSTORAGE_OUTPUT"

// dispatcherKey is the context key under which the dispatcher is stashed
// for subcommands to retrieve.
type dispatcherKey struct{}

// WithDispatcher returns a context carrying d.
func WithDispatcher(ctx context.Context, d *output.Dispatcher) context.Context {
	return context.WithValue(ctx, dispatcherKey{}, d)
}

// DispatcherFrom returns the dispatcher previously installed by the
// PersistentPreRunE on the command's context. It panics if missing —
// that's a programmer error (a subcommand running without going through
// the root) rather than a runtime condition.
func DispatcherFrom(cmd *cobra.Command) *output.Dispatcher {
	d, ok := cmd.Context().Value(dispatcherKey{}).(*output.Dispatcher)
	if !ok || d == nil {
		panic("cli: no dispatcher in context (root PersistentPreRunE not run?)")
	}
	return d
}

// resolveRenderer picks the active Renderer based on the precedence chain:
//  1. --output flag (if non-empty)
//  2. PG_HARDSTORAGE_OUTPUT env var (if set)
//  3. text if stdout is a TTY, json otherwise
//
// noColor honors --no-color and the de-facto NO_COLOR env (https://no-color.org/).
// It only affects renderers that produce ANSI; for now only `text`.
func resolveRenderer(flagOutput string, stdout io.Writer, noColorFlag bool, templateText string) (output.Renderer, error) {
	mode := strings.ToLower(strings.TrimSpace(flagOutput))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv(EnvOutput)))
	}
	if mode == "" {
		// --template implies template mode without the operator
		// having to set --output template explicitly.  Saves
		// typing in the common case "I just want jq-style
		// extraction".
		if templateText != "" {
			mode = "template"
		} else if isTerminal(stdout) {
			mode = "text"
		} else {
			mode = "json"
		}
	}
	switch mode {
	case "text":
		r := renderertext.New()
		r.NoColor = noColorFlag || os.Getenv("NO_COLOR") != ""
		return r, nil
	case "json":
		return rendererjson.New(), nil
	case "ndjson":
		return rendererndjson.New(), nil
	case "yaml", "yml":
		return rendereryaml.New(), nil
	case "template", "tmpl", "go-template":
		return renderertemplate.New(templateText)
	case "csv":
		return renderercsv.New(), nil
	case "markdown", "md":
		return renderermd.New(), nil
	case "html":
		return rendererhtml.New(), nil
	case "tap":
		return renderertap.New(), nil
	case "junit", "junit-xml":
		return rendererjunit.New(), nil
	case "pdf", "pdf-report":
		return rendererpdf.New(), nil
	default:
		// Surface the offending value (which may have come from --output,
		// PG_HARDSTORAGE_OUTPUT, or auto-detect, in that order) without
		// guessing the source.
		return nil, output.NewError("usage.unknown_output_format",
			fmt.Sprintf("unknown output format %q (supported: text|json|ndjson|yaml|template|csv|markdown|html|tap|junit|pdf)", mode)).
			Wrap(output.ErrUsage)
	}
}

// isTerminal returns true when w is a *os.File backed by a character
// device (i.e. a real terminal). When the destination is a pipe / file /
// in-memory buffer, it returns false. Stdlib only — no external deps.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// installDispatcher is the PersistentPreRunE used by NewRoot. It reads
// the persistent --output / --no-color flags, builds a Dispatcher with
// stdout/stderr borrowed from cobra, attaches every Sink declared in
// the loaded config (best-effort), and stashes it on the context.
//
// Sink-build failures are NEVER fatal here. A typo'd webhook URL or a
// missing slack plugin should not prevent `pg_hardstorage version`
// from running. Failures are emitted as warning Events through the
// dispatcher itself so JSON consumers see them in the stream while
// the foreground command continues.
func installDispatcher(cmd *cobra.Command, _ []string) error {
	flagOutput, _ := cmd.Flags().GetString("output")
	noColor, _ := cmd.Flags().GetBool("no-color")
	tmpl, _ := cmd.Flags().GetString("template")

	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	r, err := resolveRenderer(flagOutput, stdout, noColor, tmpl)
	if err != nil {
		return err
	}
	d := output.NewDispatcher(r, stdout, stderr)

	// Resolve the air-gap policy from flag > env > config and seed
	// the process-wide default BEFORE we build any sink (sink
	// builders consult airgap.Default()).  Failures here are
	// non-fatal — we surface a warning event and proceed with the
	// gate disabled, matching the rest of installDispatcher.
	loaded := loadConfigBestEffort(cmd.Context(), d)
	resolveAndSetAirgap(cmd, loaded, d)

	// Discover Tier-2 plugins on $HSPLUGIN_PATH.  Discovery is
	// best-effort: a misbehaving plugin emits a warning event
	// but doesn't block startup.  The discovered plugins are
	// stashed on the context for kind-specific dispatchers
	// (storage / sink / kms) to consult.
	plugins := external.Discover(cmd.Context(), func(format string, args ...any) {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "plugin.tier2", "probe_failed").
			WithBody(map[string]any{"detail": fmt.Sprintf(format, args...)}))
	})
	if len(plugins) > 0 {
		// Surface the discovered plugins as a single notice
		// event — operators see the inventory in their normal
		// event stream without having to query the registry.
		names := make([]string, 0, len(plugins))
		for _, p := range plugins {
			names = append(names, fmt.Sprintf("%s/%s", p.Kind, p.Name))
		}
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityNotice, "plugin.tier2", "discovered").
			WithBody(map[string]any{"plugins": names}))
	}
	cmd.SetContext(withTier2Plugins(cmd.Context(), plugins))

	// Attach configured sinks. We don't fail PreRun on sink errors —
	// see comment above. Loading config may also fail (e.g. a
	// completely missing config file is fine; a malformed one is
	// not). Either way: surface the failure as a warning event but
	// keep the dispatcher and the foreground command running.
	attachLoadedSinks(cmd.Context(), d, loaded)

	// Wire OpenTelemetry tracing if --otel-endpoint or --otel-stdout
	// is set. Failures here are non-fatal: a misconfigured collector
	// shouldn't kill the foreground command. The shutdown hook lives
	// on the cobra command's persistent post-run via an indirection
	// through the context — `pg_hardstorage version` doesn't pay the
	// cost of a tracer it never used.
	otelEndpoint, _ := cmd.Flags().GetString("otel-endpoint")
	otelStdout, _ := cmd.Flags().GetBool("otel-stdout")
	if otelEndpoint != "" || otelStdout {
		if shutdown, err := tracing.Init(cmd.Context(), tracing.Options{
			ServiceName:    "pg_hardstorage",
			ServiceVersion: version.Version,
			OTLPEndpoint:   otelEndpoint,
			Stdout:         otelStdout,
		}); err != nil {
			_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "tracing", "init_failed").
				WithBody(map[string]any{"error": err.Error()}))
		} else {
			cmd.SetContext(withTracingShutdown(cmd.Context(), shutdown))
			cobra.OnFinalize(func() {
				shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = shutdown(shCtx)
			})
		}
	}

	cmd.SetContext(WithDispatcher(cmd.Context(), d))
	return nil
}

// tracingShutdownKey is the context key for the tracer's shutdown
// hook. Tests use it to flush spans manually before asserting.
type tracingShutdownKey struct{}

func withTracingShutdown(ctx context.Context, shutdown func(context.Context) error) context.Context {
	return context.WithValue(ctx, tracingShutdownKey{}, shutdown)
}

// tier2PluginsKey is the context key under which discovered
// Tier-2 plugins are stashed by installDispatcher.  Kind-
// specific dispatchers (storage / sink / kms) consult it via
// Tier2Plugins.
type tier2PluginsKey struct{}

func withTier2Plugins(ctx context.Context, plugins []external.Plugin) context.Context {
	return context.WithValue(ctx, tier2PluginsKey{}, plugins)
}

// Tier2Plugins returns the discovered Tier-2 plugin list from
// the context (empty when discovery was skipped, e.g. in
// tests).
func Tier2Plugins(ctx context.Context) []external.Plugin {
	v, _ := ctx.Value(tier2PluginsKey{}).([]external.Plugin)
	return v
}

// attachConfiguredSinks loads the merged config (best-effort) and
// adds every successfully-built sink to d. Build failures emit a
// warning event each. Config-load failures emit a single warning.
//
// The function is intentionally tolerant: every error path falls
// through to "we have a dispatcher with possibly fewer sinks than
// configured." Operators see the diagnostic in their event stream
// without their CLI invocation getting stuck.
// loadConfigBestEffort returns the merged config or nil. Errors
// are surfaced as warning events; never fatal.  Hoisted out of
// attachConfiguredSinks so installDispatcher can read the
// air-gap policy from the same loaded config without paying for
// a second YAML round-trip.
func loadConfigBestEffort(ctx context.Context, d *output.Dispatcher) *config.LoadResult {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "config", "paths.resolve_failed").
			WithBody(map[string]any{"error": err.Error()}))
		return nil
	}
	loaded, err := config.Load(p)
	if err != nil {
		// Permission-denied on the config path is functionally
		// equivalent to "no config present" — the rest of the
		// process falls back to defaults regardless.  Suppress
		// the warning event in this case because the canonical
		// trigger is PG's restore_command forking
		// `pg_hardstorage wal fetch` as the `postgres` user,
		// which can't read /root/.config/pg_hardstorage/... that
		// root previously created.  The warning would otherwise
		// land in pg_ctl start's stderr once per fetched WAL
		// segment (hundreds during a long PITR replay), and
		// drown actionable output.  Surfaced as GH issue #20.
		//
		// Malformed-YAML / drop-in-dir errors (which carry their
		// own non-EACCES error kinds) still emit the warning —
		// those ARE actionable and the operator needs to see
		// them.
		if !errors.Is(err, fs.ErrPermission) {
			_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "config", "load_failed").
				WithBody(map[string]any{"error": err.Error()}))
		}
		return nil
	}
	return loaded
}

// resolveAndSetAirgap merges the flag > env > config inputs and
// installs the process-wide policy.  Resolution rules:
//
//   - --airgapped flag: any truthy value forces strict.  An
//     unset flag defers to env / config.
//   - PG_HARDSTORAGE_AIRGAPPED env: any truthy value forces strict.
//   - config.airgapped: parsed via airgap.ParseMode.  Invalid
//     value emits a warning event and leaves the gate off.
//
// The Allowlist comes from the config's `airgap.allowlist` slice
// only — flags / env vars are not the right place for a list.
func resolveAndSetAirgap(cmd *cobra.Command, loaded *config.LoadResult, d *output.Dispatcher) {
	mode := airgap.ModeOff
	var allowlist []string
	if loaded != nil {
		if loaded.Config.Airgapped != "" {
			m, err := airgap.ParseMode(loaded.Config.Airgapped)
			if err != nil {
				_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityWarning, "config", "airgapped.parse_failed").
					WithBody(map[string]any{"value": loaded.Config.Airgapped, "error": err.Error()}))
			} else {
				mode = m
			}
		}
		allowlist = loaded.Config.Airgap.Allowlist
	}
	// Env (truthy) wins over config.
	if airgap.ParseModeOrOff(os.Getenv("PG_HARDSTORAGE_AIRGAPPED")) == airgap.ModeStrict {
		mode = airgap.ModeStrict
	}
	// Flag wins over env.  Flag is bool: only "set to true" means
	// strict.  An explicit `--airgapped=false` flips the gate off
	// regardless of what env / config say.
	if f := cmd.Flags().Lookup("airgapped"); f != nil && f.Changed {
		if v, _ := cmd.Flags().GetBool("airgapped"); v {
			mode = airgap.ModeStrict
		} else {
			mode = airgap.ModeOff
		}
	}
	airgap.SetDefault(airgap.Policy{Mode: mode, Allowlist: allowlist})
}

func attachLoadedSinks(ctx context.Context, d *output.Dispatcher, loaded *config.LoadResult) {
	if loaded == nil || len(loaded.Config.Sinks) == 0 {
		return
	}

	sinks, errs := output.DefaultSinkRegistry.BuildAll(loaded.Config.Sinks)
	for _, sink := range sinks {
		// Open is best-effort (e.g. syslog dial may fail if the
		// remote is down). Same pattern: warn but proceed.
		if err := sink.Open(ctx, nil); err != nil {
			_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "config", "sink.open_failed").
				WithBody(map[string]any{
					"sink":  sink.Name(),
					"error": err.Error(),
				}))
			// Still attach: subsequent Emits may re-dial successfully.
		}
		d.AddSink(sink)
	}
	for _, e := range errs {
		_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "config", "sink.build_failed").
			WithBody(map[string]any{
				"sink":   e.Spec.Name,
				"plugin": e.Spec.Plugin,
				"error":  e.Err.Error(),
			}))
	}
}
