// cli_helpers.go — small shared helpers (overwrite guard, fleet YAML writer) for the testkit subcommands.
package main

import (
	"errors"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// existsOnDisk returns true when path exists.  Used by the
// generators (fleet random, compose generate) to refuse
// overwrite without --force.
func existsOnDisk(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// saveFleetYAML writes a fleet through the config package's
// canonical writer so the on-disk YAML round-trips cleanly
// against `fleet list` / `fleet validate`.
func saveFleetYAML(path string, f *config.Fleet) error {
	if path == "" {
		return errors.New("output path is empty")
	}
	return config.SaveFleet(path, f)
}
