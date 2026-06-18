package main

import (
	"strings"
	"testing"
)

// TestDockerBuildArgs_BuildContextIsRepoRoot regression-locks
// the fix from issue-class "Dockerfile COPY path mismatch":
// the testbed Dockerfiles `COPY bin/pg_hardstorage` and
// `COPY dockerfiles/testbed/entrypoint-pg.sh`, both repo-root
// relative.  The build context MUST be "." for those paths to
// resolve.
//
// If a future refactor passes the dockerfile dir as the build
// context, every `image build` against a real Docker daemon
// fails with "file not found in build context".  This test
// catches that without needing Docker.
func TestDockerBuildArgs_BuildContextIsRepoRoot(t *testing.T) {
	c := imageCell{
		OS: "ubuntu:24.04", PG: "17", Arch: "amd64",
		Family: "debian", Packages: "pgdg-apt",
	}
	args := dockerBuildArgs("ghcr.io/example/repo", "dockerfiles/testbed", c)
	if args[len(args)-1] != "." {
		t.Errorf("build context must be \".\" so COPY bin/pg_hardstorage resolves; got %q (full args: %v)",
			args[len(args)-1], args)
	}
}

func TestDockerBuildArgs_FilenameMatchesFamily(t *testing.T) {
	cases := []struct {
		family   string
		wantPath string
	}{
		{"debian", "dockerfiles/testbed/Dockerfile.debian-family"},
		{"rhel", "dockerfiles/testbed/Dockerfile.rhel-family"},
		{"suse", "dockerfiles/testbed/Dockerfile.suse-family"},
	}
	for _, tt := range cases {
		t.Run(tt.family, func(t *testing.T) {
			c := imageCell{OS: "x", PG: "17", Arch: "amd64", Family: tt.family}
			args := dockerBuildArgs("ghcr.io/example/repo",
				"dockerfiles/testbed", c)
			// Find the "-f" arg's value.
			fIdx := -1
			for i, a := range args {
				if a == "-f" {
					fIdx = i + 1
					break
				}
			}
			if fIdx < 0 || fIdx >= len(args) {
				t.Fatalf("no -f flag in args: %v", args)
			}
			if args[fIdx] != tt.wantPath {
				t.Errorf("dockerfile: got %q want %q", args[fIdx], tt.wantPath)
			}
		})
	}
}

// TestDockerBuildArgs_ImageOverrideUsedWhenSet regression-locks
// the fix for the opensuse:leap-15 / rhel:9 catalog bug:
// distros that aren't in Docker Hub's `library/` namespace need
// an `image:` override; OS_IMAGE in the build args must use it.
func TestDockerBuildArgs_ImageOverrideUsedWhenSet(t *testing.T) {
	c := imageCell{
		OS: "opensuse:leap-15", Image: "opensuse/leap:15",
		PG: "16", Arch: "amd64",
		Family: "suse", Packages: "distro",
	}
	args := dockerBuildArgs("ghcr.io/example/repo", "dockerfiles/testbed", c)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--build-arg OS_IMAGE=opensuse/leap:15") {
		t.Errorf("OS_IMAGE should use the override (opensuse/leap:15), not the catalog id: %v", args)
	}
	if strings.Contains(joined, "--build-arg OS_IMAGE=opensuse:leap-15") {
		t.Errorf("OS_IMAGE leaked the catalog id (would cause docker.io/library/opensuse:leap-15 → not-found): %v", args)
	}
}

func TestDockerBuildArgs_FallsBackToOSWhenNoOverride(t *testing.T) {
	// Most distros: catalog id IS the docker pull spec.
	c := imageCell{
		OS: "ubuntu:24.04", // no Image override
		PG: "17", Arch: "amd64",
		Family: "debian", Packages: "pgdg-apt",
	}
	args := dockerBuildArgs("ghcr.io/example/repo", "dockerfiles/testbed", c)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--build-arg OS_IMAGE=ubuntu:24.04") {
		t.Errorf("OS_IMAGE should fall back to OS when Image is empty: %v", args)
	}
}

func TestDockerBuildArgs_BuildArgsArePresent(t *testing.T) {
	c := imageCell{
		OS: "rockylinux:9", PG: "16", Arch: "arm64",
		Family: "rhel", Packages: "pgdg-yum",
	}
	args := dockerBuildArgs("ghcr.io/example/repo", "dockerfiles/testbed", c)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--build-arg OS_IMAGE=rockylinux:9",
		"--build-arg PG_VERSION=16",
		"--build-arg PACKAGES=pgdg-yum",
		"--platform linux/arm64",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("build args missing %q: %v", want, args)
		}
	}
}
