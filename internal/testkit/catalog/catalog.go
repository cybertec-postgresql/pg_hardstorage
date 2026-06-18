// Package catalog is the testkit's source-of-truth for supported
// (OS × PG version × architecture) combinations.
//
// The on-disk YAML at oses.yaml is embedded into the binary via
// embed.FS so a checked-out copy of pg_hardstorage_testkit always
// carries the catalog the binary was built with.  Tests load the
// embedded catalog directly; operators wanting to override (e.g.
// for an internal distro fork) point PG_HARDSTORAGE_TESTKIT_CATALOG
// at an alternative path.
//
// The catalog is purposely small: ~10 distros × ~4 PG versions ×
// 2 arches = ~70 cells.  Wider matrices land here only after
// the image-build template grows to support them.
package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed oses.yaml
var embeddedCatalog []byte

// Catalog is the parsed YAML.
type Catalog struct {
	Schema      string                  `yaml:"schema"`
	Version     int                     `yaml:"version"`
	Families    map[string]FamilyConfig `yaml:"families"`
	OSes        []OS                    `yaml:"oses"`
	Filesystems []string                `yaml:"filesystems"`
	Roles       []string                `yaml:"roles"`
}

// FamilyConfig is per-family default behaviour every member OS
// inherits unless it sets the same field directly.
type FamilyConfig struct {
	Packages string `yaml:"packages"` // pgdg-apt | pgdg-yum | distro
	Init     string `yaml:"init"`     // systemd
}

// OS is one (id, family, pg, arch) cell.  PG versions are stored
// as strings so "18-dev" round-trips.
//
// Image is the optional override that maps the operator-friendly
// `id` to a real Docker pull spec.  Most distros need no
// override (their Docker Hub library image matches the id):
// `ubuntu:22.04`, `debian:12`, `rockylinux:9`, etc.  Distros
// with custom namespaces or non-Docker-Hub registries set
// Image explicitly:
//
//	id:    opensuse:leap-15
//	image: opensuse/leap:15
//
//	id:    rhel:9
//	image: registry.access.redhat.com/ubi9/ubi:latest  (UBI 9)
type OS struct {
	ID         string   `yaml:"id"`
	Image      string   `yaml:"image,omitempty"`
	Family     string   `yaml:"family"`
	Packages   string   `yaml:"packages,omitempty"` // overrides family default
	PGVersions []string `yaml:"pg_versions"`
	Arches     []string `yaml:"arches"`
}

// EffectiveImage returns the docker pull spec for o — the
// `image` override when set, falling back to `id` for distros
// whose Docker Hub library image already matches the id.
func (o *OS) EffectiveImage() string {
	if o.Image != "" {
		return o.Image
	}
	return o.ID
}

// Load parses the catalog from the supplied YAML bytes.
func Load(data []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	if c.Schema != "pg_hardstorage.testkit.catalog.v1" {
		return nil, fmt.Errorf("catalog: unexpected schema %q", c.Schema)
	}
	if len(c.OSes) == 0 {
		return nil, fmt.Errorf("catalog: no OSes defined")
	}
	for i, o := range c.OSes {
		if o.ID == "" {
			return nil, fmt.Errorf("catalog: OS[%d] has empty id", i)
		}
		if _, ok := c.Families[o.Family]; !ok {
			return nil, fmt.Errorf("catalog: OS %q references unknown family %q", o.ID, o.Family)
		}
		if len(o.PGVersions) == 0 {
			return nil, fmt.Errorf("catalog: OS %q has no PG versions", o.ID)
		}
		if len(o.Arches) == 0 {
			return nil, fmt.Errorf("catalog: OS %q has no arches", o.ID)
		}
	}
	return &c, nil
}

// Default loads the embedded catalog, with optional override via
// the PG_HARDSTORAGE_TESTKIT_CATALOG environment variable for
// operators carrying internal-distro forks.
func Default() (*Catalog, error) {
	if path := os.Getenv("PG_HARDSTORAGE_TESTKIT_CATALOG"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("catalog: read %s: %w", path, err)
		}
		return Load(body)
	}
	return Load(embeddedCatalog)
}

// FindOS returns the OS entry for the given ID, or an error
// listing the closest valid IDs when the ID is unknown.
func (c *Catalog) FindOS(id string) (*OS, error) {
	for i, o := range c.OSes {
		if o.ID == id {
			return &c.OSes[i], nil
		}
	}
	return nil, fmt.Errorf("catalog: unknown OS %q (valid: %s)",
		id, strings.Join(c.OSIDs(), ", "))
}

// OSIDs returns every OS ID, sorted for deterministic display.
func (c *Catalog) OSIDs() []string {
	ids := make([]string, 0, len(c.OSes))
	for _, o := range c.OSes {
		ids = append(ids, o.ID)
	}
	sort.Strings(ids)
	return ids
}

// EffectivePackages returns the packages strategy for o, falling
// back to the family default when the OS doesn't override.
func (c *Catalog) EffectivePackages(o *OS) string {
	if o.Packages != "" {
		return o.Packages
	}
	return c.Families[o.Family].Packages
}

// SupportsPG returns true if the OS supports the named PG version.
func (o *OS) SupportsPG(pg string) bool {
	for _, v := range o.PGVersions {
		if v == pg {
			return true
		}
	}
	return false
}

// SupportsArch returns true if the OS supports the named arch.
func (o *OS) SupportsArch(arch string) bool {
	for _, a := range o.Arches {
		if a == arch {
			return true
		}
	}
	return false
}

// ValidateCombination asserts (os, pg, arch) is a real catalog
// entry.  Used by `fleet validate` and by every command that
// accepts an OS/PG/arch tuple.
func (c *Catalog) ValidateCombination(osID, pg, arch string) error {
	o, err := c.FindOS(osID)
	if err != nil {
		return err
	}
	if !o.SupportsPG(pg) {
		return fmt.Errorf("catalog: %s does not support PG %s (supported: %s)",
			osID, pg, strings.Join(o.PGVersions, ", "))
	}
	if !o.SupportsArch(arch) {
		return fmt.Errorf("catalog: %s does not support arch %s (supported: %s)",
			osID, arch, strings.Join(o.Arches, ", "))
	}
	return nil
}

// HasFilesystem returns true if the catalog lists the named FS as
// supported by the testbed images.
func (c *Catalog) HasFilesystem(fs string) bool {
	for _, f := range c.Filesystems {
		if f == fs {
			return true
		}
	}
	return false
}

// HasRole returns true if the catalog lists the named role.
func (c *Catalog) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}
