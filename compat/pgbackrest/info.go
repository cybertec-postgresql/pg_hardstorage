// info.go — pgBackRest shim verb: `pgbackrest info [--output=text|json]` → native `pg_hardstorage list`.
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newInfoCmd implements `pgbackrest --stanza=<n> info`.
//
// Native dispatch: `pg_hardstorage list <stanza> --output text|json`.
// pgBackRest's info has its own text + JSON output shapes; we
// honour --output=json by passing through the native JSON
// payload (the native list output uses a stable JSON schema
// that monitoring scripts can pivot on; see
// docs/reference/output-schema.md).  For plain text the native
// list output is already grep-friendly enough that we forward
// it verbatim.
func newInfoCmd() *cobra.Command {
	var outputFormat string
	c := &cobra.Command{
		Use:           "info",
		Short:         "Show repository state for the stanza",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInfo(globalArgs, outputFormat)
		},
	}
	c.Flags().StringVar(&outputFormat, "output", "text",
		"output format: text | json")
	return c
}

func runInfo(a pgbackrestArgs, outputFormat string) error {
	native, warnings, err := mapToNativeArgs("list", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	out := []string{native[0], a.stanza}
	out = append(out, native[1:]...)
	switch outputFormat {
	case "", "text":
		// Native default.
	case "json":
		out = append(out, "--output", "json")
	default:
		return fmt.Errorf(
			"pg-hardstorage-pgbackrest: info: unsupported --output %q (text or json)",
			outputFormat)
	}

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: info: native CLI exited %d", rc)
	}
	return nil
}
