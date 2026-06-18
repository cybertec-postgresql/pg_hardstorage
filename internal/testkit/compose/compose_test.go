package compose_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/compose"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/imagetag"
)

func TestGenerate_HappyPath(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24-pg17", OS: "ubuntu:24.04", PG: "17", Count: 1, StorageGB: 50, Filesystem: "ext4"},
		{Name: "rocky9-pg16", OS: "rockylinux:9", PG: "16", Count: 2, StorageGB: 100, Filesystem: "xfs"},
	}}
	out, err := compose.Generate(f, c, compose.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantSubs := []string{
		"name: pgvalidate",
		"services:",
		"u24-pg17:",
		"rocky9-pg16-c0:",
		"rocky9-pg16-c1:",
		"NET_ADMIN",
		"SYS_RESOURCE",
		"pg_hardstorage_testkit.role: pg",
		"pg_hardstorage_testkit.deployment:",
		"networks:",
		"  default:",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n---\n%s", s, out)
		}
	}
}

func TestGenerate_PortAllocationDeterministic(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "a", OS: "ubuntu:24.04", PG: "17", Count: 1},
		{Name: "b", OS: "debian:12", PG: "16", Count: 2},
	}}
	out, err := compose.Generate(f, c, compose.Options{HostPortBase: 20000})
	if err != nil {
		t.Fatal(err)
	}
	// 1 + 2 = 3 PG containers → ports 20000, 20001, 20002.
	for _, p := range []string{"20000:5432", "20001:5432", "20002:5432"} {
		if !strings.Contains(out, p) {
			t.Errorf("expected port mapping %q in output", p)
		}
	}
}

func TestGenerate_PatroniExpandsToNodes(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "patroni-cluster", OS: "debian:12", PG: "17", Count: 1,
			Role: "patroni-cluster", Nodes: 3},
	}}
	out, err := compose.Generate(f, c, compose.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"patroni-cluster-c0-node0:",
		"patroni-cluster-c0-node1:",
		"patroni-cluster-c0-node2:",
	} {
		if !strings.Contains(out, n) {
			t.Errorf("missing patroni node container %q", n)
		}
	}
}

func TestGenerate_ToxiproxyOptional(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}

	withTox, _ := compose.Generate(f, c, compose.Options{IncludeToxiproxy: true})
	if !strings.Contains(withTox, "u24-tox:") {
		t.Errorf("with IncludeToxiproxy=true expected toxiproxy service")
	}

	withoutTox, _ := compose.Generate(f, c, compose.Options{IncludeToxiproxy: false})
	if strings.Contains(withoutTox, "u24-tox:") {
		t.Errorf("with IncludeToxiproxy=false expected no toxiproxy service")
	}
}

func TestGenerate_ImageTagMatchesImagePackage(t *testing.T) {
	// The whole point of internal/testkit/imagetag — what
	// `compose generate` writes must equal what `image build`
	// would tag.
	c, _ := catalog.Default()
	o, _ := c.FindOS("ubuntu:24.04")
	want := imagetag.For(
		"ghcr.io/cybertec-postgresql/pg-hardstorage-testbed",
		"ubuntu:24.04", "17", "amd64", o.Family, c.EffectivePackages(o))

	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	out, _ := compose.Generate(f, c, compose.Options{})
	if !strings.Contains(out, want) {
		t.Errorf("compose output didn't carry the canonical image tag %q\n---\n%s", want, out)
	}
}

func TestGenerate_RejectsEmptyFleet(t *testing.T) {
	c, _ := catalog.Default()
	if _, err := compose.Generate(&config.Fleet{}, c, compose.Options{}); err == nil {
		t.Errorf("expected error for empty fleet")
	}
}

// TestGenerate_ContainerNameProjectPrefixed regression-locks
// the fix for the cross-run conflict bug: two soak runs
// sharing the same Docker daemon collided on the global
// container_name namespace ("Conflict. The container name
// '/debian-12-pg15-tox' is already in use by container ...").
//
// container_name MUST carry the project prefix so each run
// gets its own globally-unique names; the `services:` map key
// stays unprefixed because compose namespaces services
// per-project on its own.
func TestGenerate_ContainerNameProjectPrefixed(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	out, err := compose.Generate(f, c, compose.Options{
		ProjectName:      "myproj",
		IncludeToxiproxy: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefixed := []string{
		"container_name: myproj-u24",
		"container_name: myproj-u24-tox",
	}
	for _, want := range wantPrefixed {
		if !strings.Contains(out, want) {
			t.Errorf("compose output missing prefixed container_name %q\n---\n%s",
				want, out)
		}
	}
	// The unprefixed forms must NOT appear as bare
	// container_name lines (the service-map key entries are
	// fine — they end with ":" not " ").
	badLines := []string{
		"container_name: u24\n",
		"container_name: u24-tox\n",
	}
	for _, bad := range badLines {
		if strings.Contains(out, bad) {
			t.Errorf("compose output kept the unprefixed container_name %q\n---\n%s",
				bad, out)
		}
	}
}

// TestGenerate_ContainerNameProjectPrefixed_PatroniNodes covers
// the multi-node case so the fix doesn't only protect the
// non-Patroni shape.
func TestGenerate_ContainerNameProjectPrefixed_PatroniNodes(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "pat", OS: "debian:12", PG: "17", Count: 1,
			Role: "patroni-cluster", Nodes: 3},
	}}
	out, err := compose.Generate(f, c, compose.Options{
		ProjectName:      "soak-2",
		IncludeToxiproxy: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"container_name: soak-2-pat-c0-node0",
		"container_name: soak-2-pat-c0-node1",
		"container_name: soak-2-pat-c0-node2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing prefixed Patroni container_name %q", want)
		}
	}
}

// TestGenerate_BindMountAbsolute regression-locks the fix for
// the bind-mount failure on Fedora: the Docker daemon resolves
// bind paths against its OWN cwd (typically `/`), not the
// caller's, so a relative HostRepoDir lands at the wrong place
// at the daemon and the mount fails with "no such file or
// directory".  The generator MUST resolve to absolute.
//
// History: this used to be enforced on the `device:` line of a
// top-level named-volume `driver_opts: o=bind` indirection.
// That indirection has been replaced with a long-form
// `type: bind` mount per service (which eliminated the
// "mount of type `volume` should not define `bind` option"
// Compose warning); the absolute-path invariant moved with it
// from `device:` to `source:`.
func TestGenerate_BindMountAbsolute(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	out, err := compose.Generate(f, c, compose.Options{
		IncludeRepoVolume: true,
		HostRepoDir:       "./relative/path",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Catch any line of the form "        source: ./..." (or
	// indeed any non-absolute source:) — the fix must always
	// emit an absolute path.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		const src = "source:"
		if !strings.HasPrefix(trimmed, src) {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, src))
		if !strings.HasPrefix(val, "/") {
			t.Errorf("source path must be absolute; got %q", val)
		}
		if val == "./relative/path" {
			t.Errorf("source path looks unresolved: %q", val)
		}
	}
}

// TestGenerate_RepoVolumeUsesZRelabel regression-locks the
// fix for SELinux denial on Fedora / RHEL hosts: without the
// `selinux: z` long-form bind option (formerly the short-form
// `:z` flag), container_t can't write through to the host-side
// bind mount and `repo init` fails with
// `HSREPO: permission denied` even though the caller is uid 0
// inside the container.  Lowercase z (shared label) so all
// cells writing to the same repo concurrently can see each
// other's commits.
func TestGenerate_RepoVolumeUsesZRelabel(t *testing.T) {
	c, _ := catalog.Default()
	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	out, err := compose.Generate(f, c, compose.Options{
		IncludeRepoVolume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Long-form bind block must include `selinux: z`.
	if !strings.Contains(out, "selinux: z\n") {
		t.Errorf("compose bind mount missing `selinux: z` relabel\n  in output:\n%s", out)
	}
	// Mount must be a bind type targeting the canonical repo path.
	if !strings.Contains(out, "type: bind\n") {
		t.Errorf("compose mount must be `type: bind` (the named-volume indirection is gone)")
	}
	if !strings.Contains(out, "target: /var/lib/pg_hardstorage/repo\n") {
		t.Errorf("compose mount target wrong; expected /var/lib/pg_hardstorage/repo")
	}
	// Capital Z would isolate the label per-container,
	// breaking concurrent shared-repo writes.  Make sure
	// that's not what we emit.
	if strings.Contains(out, "selinux: Z") {
		t.Errorf("compose used :Z (private label); cells need shared :z so concurrent writes work")
	}
	// The named-volume indirection MUST be gone — emitting the
	// short-form alongside the long-form would re-trigger the
	// `mount of type volume should not define bind option`
	// Compose warning.
	if strings.Contains(out, "- repo-data:/var/lib/pg_hardstorage/repo:z") {
		t.Errorf("legacy short-form `repo-data:...:z` mount re-appeared; this brings back the Compose warning")
	}
	if strings.Contains(out, "\nvolumes:\n  repo-data:\n") {
		t.Errorf("legacy top-level `volumes: repo-data:` block re-appeared; this brings back the Compose warning")
	}
}

// TestPrefixedContainerName_EmptyProjectIsLegacy covers the
// stand-alone (non-compose) path where there is no project
// name to prefix with — leaves the service unchanged.
func TestPrefixedContainerName_EmptyProjectIsLegacy(t *testing.T) {
	if got := compose.PrefixedContainerName("", "lead"); got != "lead" {
		t.Errorf("empty project should be a passthrough; got %q", got)
	}
	if got := compose.PrefixedContainerName("p", "lead"); got != "p-lead" {
		t.Errorf("project + service: got %q want %q", got, "p-lead")
	}
}

// TestGenerate_TagIncludesRecipeDigest regression-locks the
// fix for the "stale local image satisfies the new tag" class
// of bug.  Once compose runs against a real DockerfileDir the
// emitted image tag MUST include the per-family recipe-content
// digest from imagetag.RecipeDigest — without that, an
// entrypoint fix produces an image with the same tag as the
// pre-fix image and `pull_policy: missing` reuses the stale
// one indefinitely.
func TestGenerate_TagIncludesRecipeDigest(t *testing.T) {
	c, _ := catalog.Default()
	o, _ := c.FindOS("ubuntu:24.04")

	// Build a synthetic recipe dir so the test doesn't depend
	// on the real `dockerfiles/testbed/` (which differs by
	// where `go test` is invoked).
	dir := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("Dockerfile.debian-family", "FROM ubuntu:24.04\n")
	mustWrite("entrypoint-pg.sh", "#!/bin/bash\necho original\n")

	f := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{
		{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 1},
	}}
	out1, err := compose.Generate(f, c, compose.Options{
		DockerfileDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	want1 := imagetag.ForWithRecipe(
		"ghcr.io/cybertec-postgresql/pg-hardstorage-testbed",
		"ubuntu:24.04", "17", "amd64", o.Family, c.EffectivePackages(o),
		imagetag.RecipeDigest(o.Family, dir))
	if !strings.Contains(out1, want1) {
		t.Errorf("compose tag missing recipe digest:\n  want substring %q\n  in output\n%s", want1, out1)
	}

	// Edit the entrypoint — the digest, and therefore the
	// emitted image tag, must change.
	mustWrite("entrypoint-pg.sh", "#!/bin/bash\necho fixed v2\n")
	out2, err := compose.Generate(f, c, compose.Options{
		DockerfileDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	want2 := imagetag.ForWithRecipe(
		"ghcr.io/cybertec-postgresql/pg-hardstorage-testbed",
		"ubuntu:24.04", "17", "amd64", o.Family, c.EffectivePackages(o),
		imagetag.RecipeDigest(o.Family, dir))
	if want1 == want2 {
		t.Fatal("recipe digest must differ after entrypoint edit")
	}
	if !strings.Contains(out2, want2) {
		t.Errorf("post-edit compose tag missing new recipe digest:\n  want substring %q\n  in output\n%s",
			want2, out2)
	}
	if strings.Contains(out2, want1) {
		t.Errorf("post-edit compose still contained PRE-edit tag %q (would let docker re-use the stale image)",
			want1)
	}
}
