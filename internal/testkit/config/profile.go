// profile.go — Profiles YAML schema (testkit workload-shape definitions for the load engine).
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// ProfileSchema is the schema string for profiles.yaml.
const ProfileSchema = "pg_hardstorage.testkit.profile.v1"

// Profiles is the parsed profiles.yaml.
type Profiles struct {
	Schema   string    `yaml:"schema"`
	Version  int       `yaml:"version"`
	Profiles []Profile `yaml:"profiles"`
}

// Profile is a workload shape — what data the load engine
// generates and how it churns over time.
type Profile struct {
	Name          string `yaml:"name"`
	TargetSizeGB  int    `yaml:"target_size_gb"`             // dataset-size hint / label
	ChurnMBPerMin int    `yaml:"churn_mb_per_min,omitempty"` // 0 = no continuous churn
	TableCount    int    `yaml:"table_count,omitempty"`      // hint to the load engine
	Schema        string `yaml:"schema,omitempty"`           // tpcc-lite | fact-tables | bulk-copy | ...
	BackupEvery   string `yaml:"backup_every,omitempty"`     // duration string: "5m", "1h"
	DDLPerMin     int    `yaml:"ddl_per_min,omitempty"`      // schema-churn rate
	NoHostAccess  bool   `yaml:"no_host_access,omitempty"`   // simulates a managed-PG endpoint

	// SeedTargetGB, when ≥1, triggers a one-shot bulk seed of
	// the cell's database to roughly the requested size before
	// the iteration loop starts.  Implementations use pgbench
	// scale = SeedTargetGB * 67 (pgbench scale=1 ≈ 16 MB on
	// disk, including indexes).  0 = skip seeding.  Distinct
	// from TargetSizeGB on purpose: the latter is a label /
	// hint; this is what the runtime acts on.
	SeedTargetGB int `yaml:"seed_target_gb,omitempty"`

	// SustainedClients, when ≥1, runs an UPDATE-heavy pgbench
	// TPC-B writer in the background concurrently with the
	// iteration loop, simulating the enterprise scenario of
	// "backup runs while the database is being modified."
	// 0 = no background writer.  Default per-cell parallelism
	// matches PG's default max_connections headroom.
	SustainedClients int `yaml:"sustained_clients,omitempty"`

	// SustainedRateTPS caps the writer at this many TX/sec
	// (passed to pgbench as -R).  0 = unlimited (saturate
	// what the source PG can handle).
	SustainedRateTPS int `yaml:"sustained_rate_tps,omitempty"`
}

// LoadProfiles reads and validates a profiles YAML file.
func LoadProfiles(path string) (*Profiles, error) {
	if path == "" {
		return emptyProfiles(), nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyProfiles(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("profiles: read %s: %w", path, err)
	}
	var p Profiles
	if err := yaml.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("profiles: parse: %w", err)
	}
	if p.Schema != "" && p.Schema != ProfileSchema {
		return nil, fmt.Errorf("profiles: unexpected schema %q (want %q)", p.Schema, ProfileSchema)
	}
	if p.Schema == "" {
		p.Schema = ProfileSchema
		p.Version = 1
	}
	return &p, nil
}

func emptyProfiles() *Profiles {
	return &Profiles{Schema: ProfileSchema, Version: 1, Profiles: []Profile{}}
}

// SaveProfiles writes the profiles back to disk at mode 0600.
func SaveProfiles(path string, p *Profiles) error {
	if p.Schema == "" {
		p.Schema = ProfileSchema
	}
	if p.Version == 0 {
		p.Version = 1
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("profiles: marshal: %w", err)
	}
	return os.WriteFile(path, body, 0o600)
}

// FindProfile returns the named profile or nil.
func (p *Profiles) FindProfile(name string) *Profile {
	for i, e := range p.Profiles {
		if e.Name == name {
			return &p.Profiles[i]
		}
	}
	return nil
}

// AddProfile appends; errors if the name already exists.
func (p *Profiles) AddProfile(e Profile) error {
	if p.FindProfile(e.Name) != nil {
		return fmt.Errorf("profiles: profile %q already exists", e.Name)
	}
	p.Profiles = append(p.Profiles, e)
	return nil
}

// RemoveProfile deletes by name.
func (p *Profiles) RemoveProfile(name string) error {
	for i, e := range p.Profiles {
		if e.Name == name {
			p.Profiles = append(p.Profiles[:i], p.Profiles[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ReplaceProfile updates by matching on name.
func (p *Profiles) ReplaceProfile(e Profile) error {
	for i, existing := range p.Profiles {
		if existing.Name == e.Name {
			p.Profiles[i] = e
			return nil
		}
	}
	return ErrNotFound
}

// SortByName sorts profiles alphabetically.
func (p *Profiles) SortByName() {
	sort.Slice(p.Profiles, func(i, j int) bool {
		return p.Profiles[i].Name < p.Profiles[j].Name
	})
}

// Validate runs schema-level checks on every profile.  No
// catalog cross-reference here — profiles are independent of
// OS / PG choices; the soak driver picks per-host combinations.
func (p *Profiles) Validate() error {
	seen := map[string]bool{}
	for i, e := range p.Profiles {
		if e.Name == "" {
			return fmt.Errorf("profiles: profile[%d] has empty name", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("profiles: duplicate name %q", e.Name)
		}
		seen[e.Name] = true
		if e.TargetSizeGB < 1 {
			return fmt.Errorf("profiles: profile %q: target_size_gb must be ≥1", e.Name)
		}
		if e.SeedTargetGB < 0 {
			return fmt.Errorf("profiles: profile %q: seed_target_gb must be ≥0 (got %d)", e.Name, e.SeedTargetGB)
		}
		if e.SustainedClients < 0 {
			return fmt.Errorf("profiles: profile %q: sustained_clients must be ≥0 (got %d)", e.Name, e.SustainedClients)
		}
		if e.SustainedRateTPS < 0 {
			return fmt.Errorf("profiles: profile %q: sustained_rate_tps must be ≥0 (got %d)", e.Name, e.SustainedRateTPS)
		}
		// Sustained writes target the seeded pgbench schema —
		// without a seed step the pgbench_accounts table won't
		// exist and the writer will error on first transaction.
		// Catch this at validation time so the operator doesn't
		// burn a multi-minute compose-up to discover the typo.
		if e.SustainedClients > 0 && e.SeedTargetGB == 0 {
			return fmt.Errorf("profiles: profile %q: sustained_clients > 0 requires seed_target_gb ≥ 1 "+
				"(the writer drives pgbench's TPC-B workload, which needs the pgbench schema)", e.Name)
		}
		if e.BackupEvery != "" {
			if _, err := time.ParseDuration(e.BackupEvery); err != nil {
				return fmt.Errorf("profiles: profile %q: backup_every %q: %w",
					e.Name, e.BackupEvery, err)
			}
		}
	}
	return nil
}
