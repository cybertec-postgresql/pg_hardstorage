// guard.go — conditional required-flag guards.
//
// cobra's MarkFlagRequired enforces a flag UNCONDITIONALLY, before RunE. Some
// requirements aren't unconditional: a flag may be needed only in local mode
// (not when dispatching to a control plane), or be satisfiable by EITHER a
// --flag OR a positional argument, or be gated by another flag (--pg-connection
// unless --dry-run). Those can't be expressed with MarkFlagRequired.
//
// This file is the runtime sibling: helpers a command calls inside RunE, after
// it has decided which flags it actually needs, that produce the IDENTICAL
// structured usage.missing_flag error + exit code (ExitMisuse) as both
// MarkFlagRequired (translated in Run) and the hand-written checks they
// replace — so the operator- and script-facing contract is unchanged.
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// missingFlagErr builds the structured usage.missing_flag error naming the
// missing item(s), scoped to cmd's path. `items` are human-facing labels —
// usually "--flag", but may be richer ("--repo or a positional <url>").
func missingFlagErr(cmd *cobra.Command, items ...string) error {
	path := cmd.CommandPath()
	if r := cmd.Root(); r != nil {
		path = strings.TrimPrefix(path, r.Name()+" ")
	}
	verb := "is"
	if len(items) > 1 {
		verb = "are"
	}
	return output.NewError("usage.missing_flag",
		fmt.Sprintf("%s: %s %s required", path, strings.Join(items, ", "), verb)).
		Wrap(output.ErrUsage)
}

// requireFlags returns missingFlagErr for any named flag that is empty on cmd,
// or nil when all are set. It is the conditional sibling of cobra's
// MarkFlagRequired: call it inside RunE after deciding the flags are needed —
// e.g. `if controlPlane == "" { return requireFlags(cmd, "pg-connection", "repo") }`.
func requireFlags(cmd *cobra.Command, names ...string) error {
	var missing []string
	for _, n := range names {
		f := cmd.Flags().Lookup(n)
		if f == nil || f.Value.String() == "" {
			missing = append(missing, "--"+n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missingFlagErr(cmd, missing...)
}
