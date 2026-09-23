package validate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// TestSeedRunsPgbenchAsPGUserWithoutSudo records the docker argv Seed
// produces, through a fake docker binary.
//
// Seed used to run `sudo -u postgres pgbench -i ...` via dockerExec,
// which enters the container as pgbackup. pgbackup has no sudo rights
// on any testbed image, so every seed failed with "sudo: a terminal is
// required to read the password" — all 21 cells of an enterprise_heavy
// soak, before iteration one. That is the only shipped profile that
// seeds, and the one that tests backup under concurrent write load; it
// had never been able to run.
func TestSeedRunsPgbenchAsPGUserWithoutSudo(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	fake := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		// locateContainerPGBin probes with bash -c; answer with a bin dir.
		"case \"$*\" in *bash*) echo /usr/lib/postgresql/17/bin ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &DockerCellRuntime{
		CellName: "c", Container: "cell-c", DockerBin: fake,
		PGUser: "postgres", PGDatabase: "postgres",
		Profile: config.Profile{SeedTargetGB: 1},
	}
	if err := d.Seed(context.Background(), 0); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	raw, _ := os.ReadFile(log)
	var seedLine string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "pgbench") && strings.Contains(l, " -i ") {
			seedLine = l
		}
	}
	if seedLine == "" {
		t.Fatalf("no pgbench -i invocation recorded; argv log:\n%s", raw)
	}
	if strings.Contains(seedLine, "sudo") {
		t.Errorf("seed still goes through sudo, which pgbackup cannot use: %q", seedLine)
	}
	if !strings.HasPrefix(seedLine, "exec -u postgres cell-c ") {
		t.Errorf("seed must enter the container as the PG user via `docker exec -u`, got %q", seedLine)
	}
}
