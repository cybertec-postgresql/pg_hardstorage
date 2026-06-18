// pg-hardstorage-compat — multi-call dispatcher for every
// pg_hardstorage compatibility shim.
//
// Each shim (pg-hardstorage-pgbackrest, pg-hardstorage-barman,
// pg-hardstorage-barman-wal-archive, pg-hardstorage-walg) is
// a 62 MiB Go binary that statically links the entire native
// pg_hardstorage CLI surface (Cobra command tree + every
// storage plugin + every KMS + audit + ...) on top of a tiny
// translation layer.  Building four separate binaries
// quadruples the disk + container-image footprint for
// translation deltas measured in tens of kilobytes.
//
// The multi-call pattern (BusyBox / coreutils / git all use
// it): one binary, an argv[0] switch, four install-time
// symlinks.  Disk footprint drops from 4 × 62 MiB to
// 1 × 62 MiB plus four symlinks.  The Linux page cache
// already shares text segments across same-binary
// invocations, so warm-cache latency is unchanged; cold-cache
// latency improves because there are fewer megabytes to fault
// in across all four shim names.
//
// Operators install with:
//
//	install -m 0755 pg-hardstorage-compat /usr/local/bin/
//	for n in pg-hardstorage-pgbackrest \
//	         pg-hardstorage-barman \
//	         pg-hardstorage-barman-wal-archive \
//	         pg-hardstorage-walg; do
//	    ln -sf pg-hardstorage-compat /usr/local/bin/$n
//	done
//
// (the Makefile's `make build-compat` target does this
// automatically into ./bin).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/barman"
	"github.com/cybertec-postgresql/pg_hardstorage/compat/barmancloud"
	"github.com/cybertec-postgresql/pg_hardstorage/compat/pgbackrest"
	"github.com/cybertec-postgresql/pg_hardstorage/compat/walg"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "pg-hardstorage-pgbackrest", "pgbackrest":
		// pgbackrest's CLI surface — invoked as the named
		// binary or via the operator's `ln -sf
		// pg-hardstorage-compat /usr/bin/pgbackrest` drop-in.
		os.Exit(pgbackrest.Execute())

	case "pg-hardstorage-barman", "barman":
		// Barman's main CLI (backup / recover / list-backup /
		// show-backup / check / delete).  Operators using
		// barman-wal-archive in archive_command should symlink
		// the dedicated companion name below.
		root := barman.NewRoot(os.Stdout, os.Stderr)
		root.SetArgs(os.Args[1:])
		if _, err := root.ExecuteC(); err != nil {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(barman.ExitCode(err))
		}

	case "pg-hardstorage-barman-wal-archive", "barman-wal-archive":
		// Barman's separate archive_command companion binary;
		// PG invokes it directly per WAL segment.
		root := barman.NewWALArchiveRoot(os.Stdout, os.Stderr)
		root.SetArgs(os.Args[1:])
		if _, err := root.ExecuteC(); err != nil {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(barman.ExitCode(err))
		}

	case "pg-hardstorage-walg", "wal-g":
		// WAL-G's env-var-driven CLI.  Same multi-call entry
		// point regardless of whether the operator symlinked
		// as the upstream `wal-g` name or kept the prefixed
		// pg-hardstorage- form.
		os.Exit(walg.Execute())

	case "pg-hardstorage-barman-cloud-backup", "barman-cloud-backup":
		// CNPG's `Backup` CRD invokes this directly.
		os.Exit(barmancloud.ExecuteBackup(os.Args[1:]))

	case "pg-hardstorage-barman-cloud-restore", "barman-cloud-restore":
		// CNPG's `Cluster.spec.bootstrap.recovery` invokes this.
		os.Exit(barmancloud.ExecuteRestore(os.Args[1:]))

	case "pg-hardstorage-barman-cloud-wal-archive", "barman-cloud-wal-archive":
		// CNPG's archive_command target — fires every WAL
		// segment.
		os.Exit(barmancloud.ExecuteWalArchive(os.Args[1:]))

	case "pg-hardstorage-barman-cloud-wal-restore", "barman-cloud-wal-restore":
		// CNPG replicas' restore_command target — fires when a
		// replica falls out of streaming and needs a WAL fetch.
		os.Exit(barmancloud.ExecuteWalRestore(os.Args[1:]))

	default:
		// Invoked under an unrecognised name.  Print the
		// install hint and exit non-zero — better than
		// silently picking a default and confusing the
		// operator about which shim they're actually running.
		fmt.Fprintf(os.Stderr,
			"pg-hardstorage-compat: invoked as %q which is not a known shim name.\n"+
				"Symlink this binary to one of:\n"+
				"  pg-hardstorage-pgbackrest                (or pgbackrest)\n"+
				"  pg-hardstorage-barman                    (or barman)\n"+
				"  pg-hardstorage-barman-wal-archive        (or barman-wal-archive)\n"+
				"  pg-hardstorage-walg                      (or wal-g)\n"+
				"  pg-hardstorage-barman-cloud-backup       (or barman-cloud-backup)\n"+
				"  pg-hardstorage-barman-cloud-restore      (or barman-cloud-restore)\n"+
				"  pg-hardstorage-barman-cloud-wal-archive  (or barman-cloud-wal-archive)\n"+
				"  pg-hardstorage-barman-cloud-wal-restore  (or barman-cloud-wal-restore)\n",
			os.Args[0])
		os.Exit(2)
	}
}
