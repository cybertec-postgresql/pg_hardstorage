// notify.go — CLI surface for managing notification sinks.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newNotifyCmd implements the `notify` command tree. The
// subcommands mutate the `sinks:` block in pg_hardstorage.yaml.
// Each is a thin wrapper over loadEditableConfig + the registry's
// validation — we intentionally don't have a separate "sink config"
// model.
func newNotifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "notify",
		Short: "Configure event sinks (slack, webhook, syslog, ...)",
		Long: `Manage the event sinks the dispatcher fans out to.

Each sink is identified by an operator-chosen name; subcommands
operate on that name. Adding a sink validates the configuration via
the same SinkRegistry the agent uses at start-up, so a typo'd
webhook URL or a missing key is rejected before it lands on disk.`,
	}
	c.AddCommand(newNotifyAddCmd())
	c.AddCommand(newNotifyListCmd())
	c.AddCommand(newNotifyRemoveCmd())
	return c
}

func newNotifyAddCmd() *cobra.Command {
	var (
		name   string
		plugin string
		setKVs []string
		minSev string
		yes    bool
	)
	c := &cobra.Command{
		Use:   "add <plugin> [--name <id>] [--set key=value ...]",
		Short: "Add a sink to pg_hardstorage.yaml",
		Long: `Append a sink to the configured list. The plugin argument is
required; --name defaults to the plugin name.

Common configurations:

  notify add slack    --set webhook_url=https://hooks.slack.com/services/T/B/X
  notify add webhook  --name ops-pager --set url=https://ops.example.com/hook
  notify add syslog   --set protocol=tcp --set address=siem.example.com:6514

--set values are passed verbatim into the sink's config map; the
plugin's builder validates them.

Re-adding a sink with the same name replaces the existing entry —
unless --no-replace is set, in which case a duplicate name is
rejected.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			plugin = args[0]
			if name == "" {
				name = plugin
			}
			return runNotifyAdd(cmd, name, plugin, setKVs, minSev, yes)
		},
	}
	c.Flags().StringVar(&name, "name", "", "operator-chosen sink name (default: plugin name)")
	c.Flags().StringSliceVar(&setKVs, "set", nil,
		"key=value pairs to merge into the sink's config (repeatable)")
	c.Flags().StringVar(&minSev, "min-severity", "",
		"convenience for --set min_severity=<level>")
	c.Flags().BoolVar(&yes, "yes", false,
		"replace an existing sink with the same name without confirmation")
	return c
}

func runNotifyAdd(cmd *cobra.Command, name, plugin string, setKVs []string, minSev string, yes bool) error {
	d := DispatcherFrom(cmd)

	if name == "" {
		return output.NewError("usage.empty_name",
			"notify add: --name must be a non-empty string").Wrap(output.ErrUsage)
	}
	if plugin == "" {
		return output.NewError("usage.empty_plugin",
			"notify add: plugin name is required").Wrap(output.ErrUsage)
	}

	cfgMap := map[string]any{}
	for _, kv := range setKVs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return output.NewError("usage.bad_set",
				fmt.Sprintf("notify add: --set %q must be key=value", kv)).Wrap(output.ErrUsage)
		}
		cfgMap[k] = v
	}
	if minSev != "" {
		cfgMap["min_severity"] = minSev
	}

	spec := output.SinkSpec{Name: name, Plugin: plugin, Config: cfgMap}

	// Validate by attempting to build via the default registry.
	// The builder verifies required keys and value types; we throw
	// the result away — the actual sink will be built at agent
	// start-up against the persisted config.
	if _, err := output.DefaultSinkRegistry.Build(spec); err != nil {
		return output.NewError("notify.add.invalid_spec",
			fmt.Sprintf("notify add: %v", err)).Wrap(err)
	}

	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}

	// Replace-by-name. Locate the existing entry (if any), refuse
	// without --yes when overwriting.
	for i, existing := range cfg.Sinks {
		if existing.Name == name {
			if !yes {
				return output.NewError("conflict.sink_exists",
					fmt.Sprintf("notify add: sink %q already exists; pass --yes to replace", name)).
					WithSuggestion(&output.Suggestion{
						Human:   "review the existing sink first",
						Command: "pg_hardstorage notify list",
					})
			}
			cfg.Sinks[i] = spec
			if err := write(cfg); err != nil {
				return err
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(notifyAddedBody{
				Name: name, Plugin: plugin, Replaced: true,
			}))
		}
	}
	cfg.Sinks = append(cfg.Sinks, spec)
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(notifyAddedBody{
		Name: name, Plugin: plugin, Replaced: false,
	}))
}

func newNotifyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List configured sinks",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNotifyList(cmd)
		},
	}
}

func runNotifyList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}

	out := make([]notifyListEntry, 0, len(cfg.Sinks))
	for _, s := range cfg.Sinks {
		entry := notifyListEntry{Name: s.Name, Plugin: s.Plugin}
		// Surface a couple of common keys for the at-a-glance view;
		// the operator always has the YAML for the full picture.
		if s.Config != nil {
			if v, ok := s.Config["min_severity"].(string); ok && v != "" {
				entry.MinSeverity = v
			}
			if v, ok := s.Config["webhook_url"].(string); ok && v != "" {
				entry.Endpoint = redactURL(v)
			} else if v, ok := s.Config["url"].(string); ok && v != "" {
				entry.Endpoint = redactURL(v)
			} else if v, ok := s.Config["address"].(string); ok && v != "" {
				entry.Endpoint = v
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(notifyListBody{Sinks: out}))
}

// redactURL keeps the host visible but hides any embedded secret-
// shaped path. Slack webhooks have the form
// https://hooks.slack.com/services/T*/B*/X* — the path IS the
// secret. We replace anything past the third `/` with "****" so
// `notify list` is safe to paste into a ticket.
func redactURL(u string) string {
	// Find the host part: scheme://host/...
	idx := strings.Index(u, "://")
	if idx < 0 {
		return "****"
	}
	rest := u[idx+3:]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return u
	}
	return u[:idx+3+slash] + "/****"
}

func newNotifyRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "remove <name>",
		Short:        "Remove a sink by name",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNotifyRemove(cmd, args[0])
		},
	}
	return c
}

func runNotifyRemove(cmd *cobra.Command, name string) error {
	d := DispatcherFrom(cmd)
	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	idx := -1
	for i, s := range cfg.Sinks {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return output.NewError("notfound.sink",
			fmt.Sprintf("notify remove: no such sink %q", name))
	}
	cfg.Sinks = append(cfg.Sinks[:idx], cfg.Sinks[idx+1:]...)
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(notifyRemovedBody{Name: name}))
}

// Result body shapes — stable per the v1 schema commitment.

type notifyAddedBody struct {
	Name     string `json:"name"`
	Plugin   string `json:"plugin"`
	Replaced bool   `json:"replaced"`
}

// WriteText renders the add/replace confirmation as a single-line summary to w.
func (b notifyAddedBody) WriteText(w io.Writer) error {
	verb := "added"
	if b.Replaced {
		verb = "replaced"
	}
	_, err := fmt.Fprintf(w, "✓ Sink %s — %s (plugin %s)", verb, b.Name, b.Plugin)
	return err
}

type notifyListEntry struct {
	Name        string `json:"name"`
	Plugin      string `json:"plugin"`
	Endpoint    string `json:"endpoint,omitempty"`
	MinSeverity string `json:"min_severity,omitempty"`
}

type notifyListBody struct {
	Sinks []notifyListEntry `json:"sinks"`
}

// WriteText renders the configured sinks as human-readable text to w.
func (b notifyListBody) WriteText(w io.Writer) error {
	if len(b.Sinks) == 0 {
		_, err := fmt.Fprintln(w, "no sinks configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d sink(s) configured\n", len(b.Sinks))
	for _, s := range b.Sinks {
		fmt.Fprintf(bw, "  %s\n", s.Name)
		fmt.Fprintf(bw, "    plugin:       %s\n", s.Plugin)
		if s.Endpoint != "" {
			fmt.Fprintf(bw, "    endpoint:     %s\n", s.Endpoint)
		}
		if s.MinSeverity != "" {
			fmt.Fprintf(bw, "    min_severity: %s\n", s.MinSeverity)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type notifyRemovedBody struct {
	Name string `json:"name"`
}

// WriteText renders the sink-removed confirmation as a single line to w.
func (b notifyRemovedBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ Sink %q removed", b.Name)
	return err
}
