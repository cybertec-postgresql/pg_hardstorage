//go:build !mutation_fetch_sysid_unchecked

package cli

// wal_fetch_sysid.go — the restore-side half of the cluster-identity
// guard (the write side lives on `wal stream` and `wal push`, which
// refuse a system-identifier change at archive time).
//
// The scenario this closes: a deployment name reused across clusters —
// retention wiped the old world, an operator re-initialised under the
// same name, or `--allow-system-identifier-change` was used and mixed
// lineages share a prefix. PostgreSQL does validate xlp_sysid itself,
// but only mid-replay, after wallclock has been spent, with a FATAL
// whose wording ("WAL file is from different database system") names
// neither the deployment, the offending segment's origin, nor the
// repair. With the seed backup's identifier threaded through
// restore_command, the FIRST foreign segment is refused here — typed,
// named, and (through the strict one-shot tail) an immediate loud
// recovery abort.
//
// Empty expectations are a no-op by design: older restore_commands
// predate the flag, and a manifest without a recorded identifier
// (pre-schema archives) must not start refusing on upgrade.

import (
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func checkFetchSystemIdentifier(expect, got, segmentName string) error {
	if expect == "" || got == "" || expect == got {
		return nil
	}
	return output.NewError("wal.fetch.system_identifier_mismatch",
		fmt.Sprintf("wal fetch: segment %s was archived by a DIFFERENT cluster "+
			"(system_identifier %s, the restoring backup's is %s) — refusing to hand "+
			"foreign WAL to recovery", segmentName, got, expect)).
		WithSuggestion(&output.Suggestion{
			Human: "this deployment name carries WAL from more than one cluster lineage (a re-initialised deployment under an old name, or an --allow-system-identifier-change archive). Recovery from this backup cannot use those segments. Inspect the mix with `pg_hardstorage wal audit`, and restore with a repository/deployment that holds only the seed cluster's WAL.",
		})
}
