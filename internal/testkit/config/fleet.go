// Package config carries the YAML models for the testkit's
// fleet, profile, and fault catalogues.
//
// Three on-disk files are managed:
//
//	fleet.yaml     — list of named test cells (OS × PG × arch)
//	profiles.yaml  — workload shapes (size, churn, schema)
//	faults.yaml    — fault-injection vocabulary (weighted)
//
// Every file is round-tripped through gopkg.in/yaml.v3 with
// preserved comments (where present in the source).  The CLI
// list / add / edit / remove commands operate on these
// structures; load and save go through the same helpers so
// a hand-edit and a CLI-mediated edit produce identical YAML.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
)

// FleetSchema is the schema string every fleet.yaml carries
// in its top-level `schema:` field.  Bumped on incompatible
// changes; readers refuse anything else.
const FleetSchema = "pg_hardstorage.testkit.fleet.v1"

// Fleet is the parsed fleet.yaml.
type Fleet struct {
	Schema  string       `yaml:"schema"`
	Version int          `yaml:"version"`
	Entries []FleetEntry `yaml:"fleet"`
}

// FleetEntry is one named cell in the fleet.  An entry can
// expand to multiple identical cells via Count (and to
// multiple PG nodes per cell via Nodes for Patroni clusters).
type FleetEntry struct {
	Name        string `yaml:"name"`
	OS          string `yaml:"os"`             // e.g. ubuntu:24.04
	PG          string `yaml:"pg"`             // e.g. 17 or 18-dev
	Arch        string `yaml:"arch,omitempty"` // amd64 (default) | arm64
	Count       int    `yaml:"count"`
	Role        string `yaml:"role,omitempty"`        // standalone (default) | primary | replica | patroni-cluster
	Nodes       int    `yaml:"nodes,omitempty"`       // PG nodes per cluster (only for role: patroni-cluster)
	Filesystem  string `yaml:"filesystem,omitempty"`  // ext4 (default) | xfs | zfs | btrfs
	StorageGB   int    `yaml:"storage_gb,omitempty"`  // loopback disk size, default 10
	Compression string `yaml:"compression,omitempty"` // optional override (zstd default)

	// Sink is the per-cell storage backend kind.  Empty
	// inherits the scenario / soak's repo (file:// by
	// default).  Setting different Sink values across cells
	// in the same fleet exercises concurrent multi-sink
	// coverage — every cell brings up its own emulator and
	// dedup / repo-state isolation is observable.  Must
	// match a key in internal/testkit/sink.SinkImages.
	Sink string `yaml:"sink,omitempty"`
}

// LoadFleet reads and validates a fleet YAML file.  Path "" is a
// no-such-file shortcut returning an empty fleet with the
// canonical schema header set, suitable for first-time `add`.
func LoadFleet(path string) (*Fleet, error) {
	if path == "" {
		return emptyFleet(), nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyFleet(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("fleet: read %s: %w", path, err)
	}
	return parseFleet(body)
}

func parseFleet(body []byte) (*Fleet, error) {
	var f Fleet
	if err := yaml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("fleet: parse: %w", err)
	}
	if f.Schema != "" && f.Schema != FleetSchema {
		return nil, fmt.Errorf("fleet: unexpected schema %q (want %q)", f.Schema, FleetSchema)
	}
	if f.Schema == "" {
		// Fresh file written by another tool — accept and
		// stamp the schema on save.
		f.Schema = FleetSchema
		f.Version = 1
	}
	return &f, nil
}

func emptyFleet() *Fleet {
	return &Fleet{Schema: FleetSchema, Version: 1, Entries: []FleetEntry{}}
}

// SaveFleet writes the fleet back to disk at path with mode 0600.
// We pick 0600 because the YAML may carry credentials or
// internal hostnames if operators get creative with tags; let
// owner-only be the default.
func SaveFleet(path string, f *Fleet) error {
	if f.Schema == "" {
		f.Schema = FleetSchema
	}
	if f.Version == 0 {
		f.Version = 1
	}
	body, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("fleet: marshal: %w", err)
	}
	return os.WriteFile(path, body, 0o600)
}

// FindEntry returns the entry with the given name, or nil if
// absent.
func (f *Fleet) FindEntry(name string) *FleetEntry {
	for i, e := range f.Entries {
		if e.Name == name {
			return &f.Entries[i]
		}
	}
	return nil
}

// AddEntry appends a new entry, returning an error if an entry
// with the same name already exists.
func (f *Fleet) AddEntry(e FleetEntry) error {
	if f.FindEntry(e.Name) != nil {
		return fmt.Errorf("fleet: entry %q already exists", e.Name)
	}
	f.Entries = append(f.Entries, e)
	return nil
}

// RemoveEntry removes the entry with the given name; no-op if
// absent (returns ErrNotFound so callers can distinguish from
// "actually removed").
var ErrNotFound = errors.New("fleet: entry not found")

// RemoveEntry deletes by name.
func (f *Fleet) RemoveEntry(name string) error {
	for i, e := range f.Entries {
		if e.Name == name {
			f.Entries = append(f.Entries[:i], f.Entries[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ReplaceEntry updates an existing entry by matching on name.
// Returns ErrNotFound if no entry matches.
func (f *Fleet) ReplaceEntry(e FleetEntry) error {
	for i, existing := range f.Entries {
		if existing.Name == e.Name {
			f.Entries[i] = e
			return nil
		}
	}
	return ErrNotFound
}

// SortByName sorts entries alphabetically.  Display helpers call
// this before printing so listings are deterministic.
func (f *Fleet) SortByName() {
	sort.Slice(f.Entries, func(i, j int) bool {
		return f.Entries[i].Name < f.Entries[j].Name
	})
}

// Validate checks every entry against the catalog.  Returns the
// first error encountered or nil when the whole fleet is sound.
func (f *Fleet) Validate(c *catalog.Catalog) error {
	if len(f.Entries) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for i, e := range f.Entries {
		if e.Name == "" {
			return fmt.Errorf("fleet: entry[%d] has empty name", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("fleet: duplicate entry name %q", e.Name)
		}
		seen[e.Name] = true
		if err := validateEntry(e, c); err != nil {
			return fmt.Errorf("fleet: entry %q: %w", e.Name, err)
		}
	}
	return nil
}

func validateEntry(e FleetEntry, c *catalog.Catalog) error {
	arch := e.Arch
	if arch == "" {
		arch = "amd64"
	}
	if err := c.ValidateCombination(e.OS, e.PG, arch); err != nil {
		return err
	}
	if e.Count < 1 {
		return fmt.Errorf("count must be ≥1 (got %d)", e.Count)
	}
	role := e.Role
	if role == "" {
		role = "standalone"
	}
	if !c.HasRole(role) {
		return fmt.Errorf("unknown role %q (valid: %s)", role, strings.Join(c.Roles, ", "))
	}
	if role == "patroni-cluster" {
		if e.Nodes < 2 {
			return fmt.Errorf("role=patroni-cluster requires nodes ≥2 (got %d)", e.Nodes)
		}
	} else if e.Nodes > 0 {
		return fmt.Errorf("nodes is only meaningful for role=patroni-cluster")
	}
	if e.Filesystem != "" && !c.HasFilesystem(e.Filesystem) {
		return fmt.Errorf("unknown filesystem %q (valid: %s)",
			e.Filesystem, strings.Join(c.Filesystems, ", "))
	}
	if e.StorageGB < 0 {
		return fmt.Errorf("storage_gb must be ≥0 (got %d)", e.StorageGB)
	}
	return nil
}

// EffectiveArch returns the arch field with "amd64" default
// applied — used by display + image-build code.
func (e FleetEntry) EffectiveArch() string {
	if e.Arch == "" {
		return "amd64"
	}
	return e.Arch
}

// EffectiveRole returns the role with "standalone" default applied.
func (e FleetEntry) EffectiveRole() string {
	if e.Role == "" {
		return "standalone"
	}
	return e.Role
}

// EffectiveFilesystem returns the FS with "ext4" default applied.
func (e FleetEntry) EffectiveFilesystem() string {
	if e.Filesystem == "" {
		return "ext4"
	}
	return e.Filesystem
}

// EffectiveStorageGB returns storage_gb with 10 default applied.
func (e FleetEntry) EffectiveStorageGB() int {
	if e.StorageGB == 0 {
		return 10
	}
	return e.StorageGB
}

// EffectiveContainerCount expands Count + Nodes into the total
// number of containers this entry produces.  patroni-cluster
// emits Count × Nodes containers; other roles emit Count.
func (e FleetEntry) EffectiveContainerCount() int {
	if e.EffectiveRole() == "patroni-cluster" {
		return e.Count * e.Nodes
	}
	return e.Count
}
