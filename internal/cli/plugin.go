// plugin.go — CLI surface for Tier-2 plugin discovery.
//
// The Tier-2 plugin protocol is documented in
// docs/reference/plugins/tier2-go-plugin-protocol.md.  The doc
// has long advertised `pg_hardstorage plugin list` as the
// discovery diagnostic; before this file there was no such
// subcommand — the binary would refuse with "unknown command".
// Doc-vs-binary drift caught by the docs/CLI reachability
// meta-test (TestDocsCLIReachability_AllSubcommandsExist).
//
// The command surface is intentionally minimal: list (always)
// + later: info.  Discovery itself happens in
// internal/plugin/external/protocol.go's Discover.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

func newPluginCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "plugin",
		Short: "Discover and inspect Tier-2 plugins",
		Long: `Tier-2 plugins are separately-shipped executables placed on
HSPLUGIN_PATH (or under /usr/local/lib/pg_hardstorage/plugins or
/usr/lib/pg_hardstorage/plugins by default).  This command surface
exposes the host-side discovery + probe path so operators can confirm
what the agent will see before relying on a third-party plugin.

See docs/reference/plugins/tier2-go-plugin-protocol.md for the
wire contract.`,
	}
	c.AddCommand(newPluginListCmd())
	return c
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List discovered Tier-2 plugins",
		Long: `Walks $HSPLUGIN_PATH (or the default plugin dirs), probes each
discovered binary, and reports name / kind / version / path for those
that respond with a valid handshake.  Probe failures are surfaced as
warning events but do not fail the command — a single bad plugin
won't block listing the rest.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)

			// Discovery has already happened in installDispatcher
			// (PersistentPreRunE).  Re-reading from the context
			// means `plugin list` shows the same inventory every
			// other command operates against — no risk of two
			// commands disagreeing about what's installed.
			plugins := Tier2Plugins(cmd.Context())

			body := pluginListBody{Plugins: make([]pluginRow, 0, len(plugins))}
			for _, p := range plugins {
				body.Plugins = append(body.Plugins, pluginRow{
					Name:     p.Name,
					Kind:     p.Kind,
					Version:  p.Version,
					Protocol: p.Protocol,
					Schemes:  p.Schemes,
					Path:     p.Path,
				})
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
}

// pluginRow is one row in the plugin-list output.
type pluginRow struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Version  string   `json:"version,omitempty"`
	Protocol string   `json:"protocol"`
	Schemes  []string `json:"schemes,omitempty"`
	Path     string   `json:"path"`
}

// pluginListBody is the typed body for `plugin list`.  Implements
// text.TextWriter so the human renderer shows a sorted table.
type pluginListBody struct {
	Plugins []pluginRow `json:"plugins"`
}

// WriteText prints a fixed-width table.  Matches the format the
// Tier-2 docs advertise:
//
//	NAME             KIND        VERSION    PATH
//	my-storage       storage     1.2.3      /usr/local/lib/pg_hardstorage/plugins/...
func (b pluginListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Plugins) == 0 {
		_, err := io.WriteString(w, "no Tier-2 plugins discovered ($HSPLUGIN_PATH or default plugin dirs)\n")
		return err
	}
	fmt.Fprintf(bw, "%-20s %-12s %-10s %s\n", "NAME", "KIND", "VERSION", "PATH")
	for _, p := range b.Plugins {
		ver := p.Version
		if ver == "" {
			ver = "-"
		}
		fmt.Fprintf(bw, "%-20s %-12s %-10s %s\n", p.Name, p.Kind, ver, p.Path)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// Discoverer is the hook tests use to inject a fake plugin list
// without HSPLUGIN_PATH gymnastics.  Production code uses
// external.Discover via the dispatcher's PreRun hook.
var Discoverer = func(ctx context.Context) []external.Plugin {
	return external.Discover(ctx, nil)
}
