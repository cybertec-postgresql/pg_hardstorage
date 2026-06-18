// dockercompose_shell_test.go — pins that no docker-compose service
// using the pg_hardstorage image invokes a shell entrypoint.
//
// Why this exists (issue #89): the published pg_hardstorage image
// is built on gcr.io/distroless/static-debian12:nonroot and ships
// no /bin/sh, no busybox, no shell.  Any compose service that
// uses `entrypoint: ["sh", ...]` against that image fails at
// startup with:
//
//   exec "sh": executable file not found in $PATH
//
// The fix in docker-compose.yml splits shell-required work into
// a separate `minio-init` service that runs on minio/mc (which
// has a shell) and keeps the pg_hardstorage container as a
// pure-Go entrypoint.  This test asserts that split holds: any
// future commit that re-introduces `entrypoint: [sh|bash|...]`
// against the distroless image fails here at PR time, not at
// `docker compose up` time.
//
// Issue #89 follow-up — build, don't pull.  After the shell fix,
// `docker compose up` hit a second wall: the two services that
// run the pg_hardstorage binary referenced
// ghcr.io/cybertec-postgresql/pg_hardstorage:latest, which only
// exists after a `v*` release tag and is not anonymously
// pullable.  A fresh `git pull && docker compose up` failed with
// an image-pull `unauthorized` error.  The eval stack now builds
// the image from this checkout, and the second test below pins
// that contract: any pg_hardstorage-binary service must build
// from local source, never depend on an unpullable registry
// image.

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeShellAudit is a relaxed view of docker-compose.yml that
// just looks at every service's image + entrypoint + build.
// Different runtime fields (volumes, environment, etc.) are
// intentionally out of scope — keeping the type narrow keeps the
// test stable against compose-schema additions.
type composeShellAudit struct {
	Services map[string]struct {
		Image      string `yaml:"image"`
		Entrypoint any    `yaml:"entrypoint"` // string OR []string
		Build      any    `yaml:"build"`      // string OR map
	} `yaml:"services"`
}

// pgHardstorageDockerfile is the Dockerfile that builds the
// distroless pg_hardstorage image.  A service whose build points
// here runs the pg_hardstorage binary.
const pgHardstorageDockerfile = "deploy/docker/Dockerfile"

// pgHardstorageImageMarkers is the set of image references we know
// to be the distroless pg_hardstorage image (no shell).  Add to
// this list whenever a new pg_hardstorage variant is published.
var pgHardstorageImageMarkers = []string{
	"ghcr.io/cybertec-postgresql/pg_hardstorage",
	"ghcr.io/cybertec-postgresql/pg-hardstorage",
	"pg_hardstorage:", // local build tag used by the eval stack
}

// shellEntrypoints are the executable names that REQUIRE a shell
// to be present in the image.  Any of these appearing as a
// distroless image's entrypoint[0] is the issue #89 bug.
var shellEntrypoints = []string{
	"sh", "/bin/sh",
	"bash", "/bin/bash",
	"ash", "/bin/ash", // busybox shells
	"dash", "/bin/dash",
	"zsh", "/bin/zsh",
}

func TestDockerComposeYAML_NoShellEntrypointAgainstDistrolessImage(t *testing.T) {
	c := loadComposeShellAudit(t)

	for name, svc := range c.Services {
		if !isPGHardstorageService(svc.Image, svc.Build) {
			continue
		}
		first, ok := firstEntrypointArg(svc.Entrypoint)
		if !ok {
			// No entrypoint override = the image's own ENTRYPOINT
			// runs (`/usr/bin/pg_hardstorage`).  That's fine.
			continue
		}
		for _, shell := range shellEntrypoints {
			if first == shell {
				t.Errorf("service %q (image %q) uses entrypoint[0]=%q against a distroless image — issue #89 regression: there is no shell in the image, the container will fail with `exec %q: executable file not found in $PATH`",
					name, svc.Image, first, first)
				break
			}
		}
	}
}

// TestDockerComposeYAML_PGHardstorageServicesBuildFromSource pins
// the issue #89 follow-up: the eval stack must build the
// pg_hardstorage image from this checkout, not pull the
// release-gated, non-anonymously-pullable GHCR image.  A service
// that runs the pg_hardstorage binary (by image tag) but has no
// `build:` section would reintroduce the `docker compose up`
// image-pull `unauthorized` failure on a fresh clone.
func TestDockerComposeYAML_PGHardstorageServicesBuildFromSource(t *testing.T) {
	c := loadComposeShellAudit(t)

	var found int
	for name, svc := range c.Services {
		if !isPGHardstorageService(svc.Image, svc.Build) {
			continue
		}
		found++
		dockerfile, ok := buildDockerfile(svc.Build)
		if !ok {
			t.Errorf("service %q runs the pg_hardstorage binary (image %q) but has no `build:` section — issue #89 regression: it would pull ghcr.io/cybertec-postgresql/pg_hardstorage, which is release-gated and not anonymously pullable, so a fresh `docker compose up` fails with `unauthorized`. Build from %s instead.",
				name, svc.Image, pgHardstorageDockerfile)
			continue
		}
		if dockerfile != pgHardstorageDockerfile {
			t.Errorf("service %q builds from dockerfile %q, want %q",
				name, dockerfile, pgHardstorageDockerfile)
		}
	}
	if found == 0 {
		t.Fatalf("no pg_hardstorage-binary service found in docker-compose.yml — markers %v out of date?", pgHardstorageImageMarkers)
	}
}

func loadComposeShellAudit(t *testing.T) composeShellAudit {
	t.Helper()
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	var c composeShellAudit
	if err := yaml.Unmarshal(body, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// isPGHardstorageService reports whether a service runs the
// pg_hardstorage binary — either by referencing a known
// pg_hardstorage image tag, or by building from the
// pg_hardstorage Dockerfile.
func isPGHardstorageService(image string, build any) bool {
	for _, marker := range pgHardstorageImageMarkers {
		if strings.HasPrefix(image, marker) {
			return true
		}
	}
	if df, ok := buildDockerfile(build); ok && df == pgHardstorageDockerfile {
		return true
	}
	return false
}

// buildDockerfile returns the dockerfile a service builds from.
// Compose accepts `build:` as either a bare context string (no
// explicit dockerfile → defaults to "Dockerfile") or a map with
// context/dockerfile keys.  Returns ok=false when there is no
// build section.
func buildDockerfile(build any) (string, bool) {
	switch v := build.(type) {
	case nil:
		return "", false
	case string:
		// `build: <context>` → default Dockerfile in that context.
		return "Dockerfile", true
	case map[string]any:
		if df, ok := v["dockerfile"].(string); ok && df != "" {
			return df, true
		}
		return "Dockerfile", true
	}
	return "", false
}

// firstEntrypointArg returns argv[0] of the entrypoint, regardless
// of whether the YAML expressed it as a string or as a list.
// Returns ok=false when the field is absent or empty.
func firstEntrypointArg(ep any) (string, bool) {
	switch v := ep.(type) {
	case nil:
		return "", false
	case string:
		if v == "" {
			return "", false
		}
		// Shell-form: "sh -c '...'"  → first whitespace-token.
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return "", false
		}
		return fields[0], true
	case []any:
		if len(v) == 0 {
			return "", false
		}
		s, ok := v[0].(string)
		return s, ok
	}
	return "", false
}
