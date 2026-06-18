// dockercompose_pg_test.go — pins docker-compose.yml's `pg` service
// against the PG-18-alpine PGDATA gotcha (issue #88).
//
// PG 18 alpine images moved the default PGDATA from
// `/var/lib/postgresql/data` to `/var/lib/postgresql/18/docker` for
// pg_ctlcluster compatibility.  A volume mounted at the OLD path
// without a PGDATA override leaves the container exiting at
// startup with:
//
//   Error: in 18+, these Docker images are configured to store
//   database data in a format which is compatible with
//   pg_ctlcluster.  Counter to that, there appears to be PostgreSQL
//   data in /var/lib/postgresql/data (unused mount/volume)
//
// This test parses the shipped docker-compose.yml and asserts the
// configuration is self-consistent: either PGDATA is pinned to the
// volume's mount path, or the volume is mounted at the image's
// default PGDATA for that major version.  A future change that
// drops PGDATA without also fixing the mount path fails here at
// PR time instead of at `docker compose up` time.

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// imageDefaultPGDATA maps a PG image tag suffix to the default
// PGDATA path that image uses.  Older PG / non-alpine variants
// keep the legacy path; PG-18-alpine and newer alpine images use
// the pg_ctlcluster-style versioned path.
var imageDefaultPGDATA = map[string]string{
	// Legacy default — PG 13..17 alpine, all -debian / Debian-base
	// variants, and PG-18 -bookworm.
	"":          "/var/lib/postgresql/data",
	"17-alpine": "/var/lib/postgresql/data",
	"16-alpine": "/var/lib/postgresql/data",
	// PG-18-alpine pg_ctlcluster-style default — issue #88.
	"18-alpine": "/var/lib/postgresql/18/docker",
}

// composeFile is the minimal slice of docker-compose.yml we parse
// here: just the `pg` service's image / environment / volumes.
type composeFile struct {
	Services struct {
		PG struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"pg"`
	} `yaml:"services"`
}

// TestDockerComposeYAML_PGServiceMountIsConsistent asserts the
// `pg` service mounts the data volume at the SAME path as PGDATA
// resolves to under that image.  Either:
//   - PGDATA env is set, AND a volume is mounted at PGDATA's value;
//   - OR PGDATA env is unset, AND a volume is mounted at the
//     image's default PGDATA for its major version.
//
// Either way, the data directory is on a persistent volume — the
// reporter's failure mode (volume mounted at an unused location
// while real PGDATA lives in an emptyDir) can't recur.
func TestDockerComposeYAML_PGServiceMountIsConsistent(t *testing.T) {
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}

	var c composeFile
	if err := yaml.Unmarshal(body, &c); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}

	image := c.Services.PG.Image
	if image == "" {
		t.Fatal("docker-compose.yml has no `pg.image` set")
	}
	// Extract the tag suffix (everything after the colon).
	tagSuffix := ""
	if i := strings.LastIndexByte(image, ':'); i >= 0 {
		tagSuffix = image[i+1:]
	}
	defaultPath, known := imageDefaultPGDATA[tagSuffix]
	if !known {
		t.Skipf("docker-compose.yml uses pg image %q whose default PGDATA isn't in this test's lookup table; add it to imageDefaultPGDATA when bumping",
			image)
	}

	// Resolve the effective PGDATA: the env override beats the
	// image default.
	effective := c.Services.PG.Environment["PGDATA"]
	if effective == "" {
		effective = defaultPath
	}

	// Volumes are listed as "src:dst[:mode]" strings.  Look for
	// a mount whose dst equals the effective PGDATA.
	found := false
	for _, vol := range c.Services.PG.Volumes {
		parts := strings.Split(vol, ":")
		if len(parts) < 2 {
			continue
		}
		dst := parts[1]
		if dst == effective {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("docker-compose.yml `pg` service mounts no volume at PGDATA (%q); data dir won't persist and PG 18-alpine will exit on startup (issue #88).\n  image:   %s\n  PGDATA:  %s\n  volumes: %v",
			effective, image, effective, c.Services.PG.Volumes)
	}
}

// Defensive: if a future bump replaces the `pg` service entirely
// (different orchestrator pattern, custom Dockerfile), the test's
// scope must remain meaningful.  An empty image string would
// otherwise short-circuit silently.
func TestDockerComposeYAML_PGServiceImageIsDeclared(t *testing.T) {
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	var c composeFile
	if err := yaml.Unmarshal(body, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Services.PG.Image == "" {
		t.Error("docker-compose.yml `pg` service has no image declared; downstream tests can't reason about its defaults")
	}
}
