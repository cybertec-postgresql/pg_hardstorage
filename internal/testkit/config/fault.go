// fault.go — Faults YAML schema (testkit fault-injection catalogue with selection weights).
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// FaultSchema is the schema string for faults.yaml.
const FaultSchema = "pg_hardstorage.testkit.fault.v1"

// Faults is the parsed faults.yaml.
type Faults struct {
	Schema  string  `yaml:"schema"`
	Version int     `yaml:"version"`
	Faults  []Fault `yaml:"faults"`
}

// Fault is one named fault-injection primitive.  Weight is the
// drive-loop's selection-probability weight (0 = never).
//
// Action is intentionally a free-form string at this layer:
// the fault catalogue describes WHAT to do; the inject package
// (separate, lands later) interprets the action string.
// Round-tripping the action verbatim lets operators write new
// faults without touching Go code.
type Fault struct {
	Name   string `yaml:"name"`
	Weight int    `yaml:"weight"`
	Action string `yaml:"action"`
}

// knownActionPrefixes shadows the inject package's registered
// fault names for cheap fast-path validation.  Validate() goes
// further and parses every action string through the inject
// parser so unbalanced quotes, duplicate keys, and unknown
// prefixes all surface at config-edit time rather than at
// soak-run time.  Pulling from inject.DefaultRegistry keeps the
// two lists in lock-step automatically — adding a new primitive
// in inject/ makes it valid for `fault add` immediately.
func knownActionPrefixesNow() []string {
	return inject.DefaultRegistry.Names()
}

// LoadFaults reads and validates a faults YAML file.
func LoadFaults(path string) (*Faults, error) {
	if path == "" {
		return emptyFaults(), nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyFaults(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("faults: read %s: %w", path, err)
	}
	var f Faults
	if err := yaml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("faults: parse: %w", err)
	}
	if f.Schema != "" && f.Schema != FaultSchema {
		return nil, fmt.Errorf("faults: unexpected schema %q (want %q)", f.Schema, FaultSchema)
	}
	if f.Schema == "" {
		f.Schema = FaultSchema
		f.Version = 1
	}
	return &f, nil
}

func emptyFaults() *Faults {
	return &Faults{Schema: FaultSchema, Version: 1, Faults: []Fault{}}
}

// SaveFaults writes the faults YAML back to disk at mode 0600.
func SaveFaults(path string, f *Faults) error {
	if f.Schema == "" {
		f.Schema = FaultSchema
	}
	if f.Version == 0 {
		f.Version = 1
	}
	body, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("faults: marshal: %w", err)
	}
	return os.WriteFile(path, body, 0o600)
}

// FindFault returns the named fault or nil.
func (f *Faults) FindFault(name string) *Fault {
	for i, e := range f.Faults {
		if e.Name == name {
			return &f.Faults[i]
		}
	}
	return nil
}

// AddFault appends; errors if the name already exists.
func (f *Faults) AddFault(e Fault) error {
	if f.FindFault(e.Name) != nil {
		return fmt.Errorf("faults: fault %q already exists", e.Name)
	}
	f.Faults = append(f.Faults, e)
	return nil
}

// RemoveFault deletes by name.
func (f *Faults) RemoveFault(name string) error {
	for i, e := range f.Faults {
		if e.Name == name {
			f.Faults = append(f.Faults[:i], f.Faults[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ReplaceFault updates by matching on name.
func (f *Faults) ReplaceFault(e Fault) error {
	for i, existing := range f.Faults {
		if existing.Name == e.Name {
			f.Faults[i] = e
			return nil
		}
	}
	return ErrNotFound
}

// SortByName sorts faults alphabetically.
func (f *Faults) SortByName() {
	sort.Slice(f.Faults, func(i, j int) bool {
		return f.Faults[i].Name < f.Faults[j].Name
	})
}

// Validate runs schema-level checks on every fault and parses
// each action string through the inject package so syntax
// errors surface at config-edit time.
func (f *Faults) Validate() error {
	seen := map[string]bool{}
	for i, e := range f.Faults {
		if e.Name == "" {
			return fmt.Errorf("faults: fault[%d] has empty name", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("faults: duplicate name %q", e.Name)
		}
		seen[e.Name] = true
		if e.Weight < 0 {
			return fmt.Errorf("faults: fault %q: weight must be ≥0", e.Name)
		}
		if e.Action == "" {
			return fmt.Errorf("faults: fault %q: action is required", e.Name)
		}
		// Full parse: catches missing parens, unbalanced
		// quotes, duplicate keys.
		parsed, err := inject.ParseAction(e.Action)
		if err != nil {
			return fmt.Errorf("faults: fault %q: %w", e.Name, err)
		}
		// Lookup: catches unknown prefixes against the
		// inject registry.
		if _, lerr := inject.DefaultRegistry.Lookup(parsed.Prefix); lerr != nil {
			return fmt.Errorf("faults: fault %q: %w", e.Name, lerr)
		}
	}
	return nil
}

// KnownActionPrefixes returns the list of recognised action
// prefixes — exposed for the CLI's help text and the
// interactive add wizard.  Pulled live from the inject
// registry so adding a primitive in inject/ requires no edit
// here.
func KnownActionPrefixes() []string {
	return knownActionPrefixesNow()
}
