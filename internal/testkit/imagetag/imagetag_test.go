package imagetag_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/imagetag"
)

func TestFor_DeterministicAndStableShape(t *testing.T) {
	tag := imagetag.For(
		"ghcr.io/example/repo",
		"ubuntu:24.04", "17", "amd64", "debian", "pgdg-apt")
	// Shape: <repo>:<os-with-':'-replaced>-pg<ver>-<arch>-<short>
	if !strings.HasPrefix(tag, "ghcr.io/example/repo:ubuntu-24.04-pg17-amd64-") {
		t.Errorf("unexpected tag prefix: %s", tag)
	}
	// Same inputs -> same output (the whole point of the
	// fingerprint: CI rebuilds reuse cached layers).
	again := imagetag.For(
		"ghcr.io/example/repo",
		"ubuntu:24.04", "17", "amd64", "debian", "pgdg-apt")
	if tag != again {
		t.Errorf("non-deterministic: %s vs %s", tag, again)
	}
}

func TestForWithRecipe_AppendsDigest(t *testing.T) {
	base := imagetag.For(
		"r", "ubuntu:24.04", "17", "amd64", "debian", "pgdg-apt")
	withRecipe := imagetag.ForWithRecipe(
		"r", "ubuntu:24.04", "17", "amd64", "debian", "pgdg-apt", "abcd1234")
	if withRecipe != base+"-abcd1234" {
		t.Errorf("expected base + '-abcd1234'; got %s vs %s", withRecipe, base)
	}
	// Empty recipe digest collapses to the legacy tag (so
	// tests + stand-alone callers stay byte-compatible).
	collapsed := imagetag.ForWithRecipe(
		"r", "ubuntu:24.04", "17", "amd64", "debian", "pgdg-apt", "")
	if collapsed != base {
		t.Errorf("empty recipe should collapse to legacy tag; got %s vs %s",
			collapsed, base)
	}
}

// TestRecipeDigest_ChangesWithEntrypointContent locks the
// load-bearing property: editing entrypoint-pg.sh changes the
// digest, which changes the tag, which forces a rebuild.
// Without that, a fix to the entrypoint can be silently
// shadowed by a stale local image with the same tag — the
// exact bug the recipe-digest fix exists to prevent.
func TestRecipeDigest_ChangesWithEntrypointContent(t *testing.T) {
	dir := t.TempDir()
	must := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("Dockerfile.debian-family", "FROM ubuntu:24.04\n")
	must("entrypoint-pg.sh", "#!/bin/bash\necho v1\n")
	v1 := imagetag.RecipeDigest("debian", dir)
	if v1 == "" {
		t.Fatal("digest should be non-empty when files exist")
	}
	// Edit the entrypoint — digest must change.
	must("entrypoint-pg.sh", "#!/bin/bash\necho v2 (fix)\n")
	v2 := imagetag.RecipeDigest("debian", dir)
	if v2 == "" || v2 == v1 {
		t.Errorf("digest should change when entrypoint changes; v1=%q v2=%q", v1, v2)
	}
	// Edit the Dockerfile — digest must change again.
	must("Dockerfile.debian-family", "FROM ubuntu:24.04\nENV X=1\n")
	v3 := imagetag.RecipeDigest("debian", dir)
	if v3 == "" || v3 == v2 {
		t.Errorf("digest should change when Dockerfile changes; v2=%q v3=%q", v2, v3)
	}
}

// TestRecipeDigest_FailSoftWhenFilesMissing covers the
// stand-alone-test path: callers that don't have a real
// dockerfileDir should get "" (not an error) so the legacy
// tag scheme keeps working.
func TestRecipeDigest_FailSoftWhenFilesMissing(t *testing.T) {
	if got := imagetag.RecipeDigest("", "/some/dir"); got != "" {
		t.Errorf("empty family should return empty; got %q", got)
	}
	if got := imagetag.RecipeDigest("debian", ""); got != "" {
		t.Errorf("empty dir should return empty; got %q", got)
	}
	if got := imagetag.RecipeDigest("debian", "/no/such/dir/here"); got != "" {
		t.Errorf("missing dir should return empty; got %q", got)
	}
}

// TestRecipeDigest_FamilySpecific covers per-family isolation:
// the debian and rhel families have different Dockerfiles, so
// their digests must not collide even if they share the same
// entrypoint-pg.sh.
func TestRecipeDigest_FamilySpecific(t *testing.T) {
	dir := t.TempDir()
	must := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("Dockerfile.debian-family", "FROM ubuntu:24.04\n")
	must("Dockerfile.rhel-family", "FROM rockylinux:9\n")
	must("entrypoint-pg.sh", "#!/bin/bash\nset -e\n")
	d := imagetag.RecipeDigest("debian", dir)
	r := imagetag.RecipeDigest("rhel", dir)
	if d == "" || r == "" {
		t.Fatal("digests should not be empty")
	}
	if d == r {
		t.Errorf("debian and rhel digests should differ; both = %q", d)
	}
}
