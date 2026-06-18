package catalog_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
)

func TestDefault_Embedded(t *testing.T) {
	c, err := catalog.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if c.Schema != "pg_hardstorage.testkit.catalog.v1" {
		t.Errorf("schema: %q", c.Schema)
	}
	if len(c.OSes) < 5 {
		t.Errorf("expected ≥5 OSes; got %d", len(c.OSes))
	}
	if !contains(c.OSIDs(), "ubuntu:24.04") {
		t.Errorf("ubuntu:24.04 missing from catalog: %v", c.OSIDs())
	}
}

func TestFindOS_Known(t *testing.T) {
	c, _ := catalog.Default()
	o, err := c.FindOS("rockylinux:9")
	if err != nil {
		t.Fatal(err)
	}
	if o.Family != "rhel" {
		t.Errorf("family: %s", o.Family)
	}
}

func TestFindOS_Unknown(t *testing.T) {
	c, _ := catalog.Default()
	_, err := c.FindOS("ubunt:24.04") // typo
	if err == nil || !strings.Contains(err.Error(), "unknown OS") {
		t.Errorf("expected unknown-OS error; got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "ubuntu:24.04") {
		t.Errorf("error should list valid IDs; got %v", err)
	}
}

func TestValidateCombination_Happy(t *testing.T) {
	c, _ := catalog.Default()
	if err := c.ValidateCombination("ubuntu:24.04", "17", "amd64"); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
}

func TestValidateCombination_BadPG(t *testing.T) {
	c, _ := catalog.Default()
	err := c.ValidateCombination("rockylinux:9", "12", "amd64")
	if err == nil || !strings.Contains(err.Error(), "does not support PG 12") {
		t.Errorf("expected PG-not-supported error; got %v", err)
	}
}

func TestValidateCombination_BadArch(t *testing.T) {
	c, _ := catalog.Default()
	err := c.ValidateCombination("rhel:9", "17", "arm64") // RHEL 9: amd64 only
	if err == nil || !strings.Contains(err.Error(), "does not support arch arm64") {
		t.Errorf("expected arch-not-supported error; got %v", err)
	}
}

func TestEffectivePackages_FamilyDefault(t *testing.T) {
	c, _ := catalog.Default()
	o, _ := c.FindOS("ubuntu:24.04")
	if got := c.EffectivePackages(o); got != "pgdg-apt" {
		t.Errorf("expected pgdg-apt from family default; got %q", got)
	}
}

func TestEffectivePackages_OverrideWins(t *testing.T) {
	c, _ := catalog.Default()
	o, _ := c.FindOS("debian:13")
	if got := c.EffectivePackages(o); got != "distro" {
		t.Errorf("expected override 'distro'; got %q", got)
	}
}

func TestHasFilesystem(t *testing.T) {
	c, _ := catalog.Default()
	for _, fs := range []string{"ext4", "xfs", "zfs", "btrfs"} {
		if !c.HasFilesystem(fs) {
			t.Errorf("expected catalog to list %s", fs)
		}
	}
	if c.HasFilesystem("ntfs") {
		t.Errorf("ntfs should not be listed")
	}
}

func TestHasRole(t *testing.T) {
	c, _ := catalog.Default()
	for _, r := range []string{"standalone", "primary", "replica", "patroni-cluster"} {
		if !c.HasRole(r) {
			t.Errorf("expected catalog to list %s", r)
		}
	}
}

func TestLoad_RejectsBadSchema(t *testing.T) {
	body := []byte("schema: wrong.schema\nversion: 1\nfamilies: {}\noses: []\n")
	_, err := catalog.Load(body)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema rejection; got %v", err)
	}
}

func TestLoad_RejectsUnknownFamily(t *testing.T) {
	body := []byte(`schema: pg_hardstorage.testkit.catalog.v1
version: 1
families:
  debian: { packages: pgdg-apt, init: systemd }
oses:
  - id: ubuntu:22.04
    family: not-a-real-family
    pg_versions: [17]
    arches: [amd64]
`)
	_, err := catalog.Load(body)
	if err == nil || !strings.Contains(err.Error(), "unknown family") {
		t.Errorf("expected unknown-family rejection; got %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
