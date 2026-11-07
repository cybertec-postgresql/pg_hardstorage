package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// registerFetchConcurrencyFlag adds --fetch-concurrency, the read-side
// twin of backup's --upload-concurrency, to a command that reads a
// backup's chunks in bulk (issue #97).
func registerFetchConcurrencyFlag(c *cobra.Command) {
	c.Flags().Int("fetch-concurrency", 0,
		fmt.Sprintf("chunk reads kept in flight (1..%d; default %d). Reads wait on storage round trips, not CPU, so a high-latency object store benefits from more; 1 is serial",
			repo.MaxFetchConcurrency, repo.DefaultFetchConcurrency))
}

// fetchConcurrencyFlag returns --fetch-concurrency (0 = default) or a
// usage error naming the accepted range.
func fetchConcurrencyFlag(cmd *cobra.Command) (int, error) {
	n, err := cmd.Flags().GetInt("fetch-concurrency")
	if err != nil {
		// The command does not carry the flag: the default.
		return 0, nil
	}
	if n < 0 || n > repo.MaxFetchConcurrency {
		return 0, output.NewError("usage.bad_flag",
			fmt.Sprintf("--fetch-concurrency %d: want 1..%d (0 = default %d)",
				n, repo.MaxFetchConcurrency, repo.DefaultFetchConcurrency)).Wrap(output.ErrUsage)
	}
	return n, nil
}
