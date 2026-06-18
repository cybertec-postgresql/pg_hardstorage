// Package load is the testkit's deterministic load engine. A *.load.yaml
// file declares phases of operations against PG; the engine drives PG
// through them with a chacha20 PRNG so the bit-exact same sequence of
// rows lands on every run with the same seed.
//
// v0.1 ships a small set of operations:
//
//	create_table {name, schema}
//	insert_rows  {table, count, generator}
//	update_rows  {table, fraction, where}
//	delete_rows  {table, fraction, where}
//	create_index {table, columns, unique?}
//	vacuum       {table?, full?}
//	checkpoint   {label}
//
// Heavier ops (copy_in via COPY FROM, alter_table, reindex) land in
// as the verifier subsystem matures.
package load

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SchemaLoad is the schema string in the YAML. Any future format change
// bumps this so old test corpora can still be detected and refused
// (or auto-migrated).
const SchemaLoad = "pg_hardstorage.testload.v1"

// Load is the parsed YAML envelope.
type Load struct {
	Schema      string         `yaml:"schema"`
	Seed        uint64         `yaml:"seed"`
	Locale      string         `yaml:"locale,omitempty"`
	Timezone    string         `yaml:"timezone,omitempty"`
	Phases      []Phase        `yaml:"phases"`
	Checkpoints CheckpointSpec `yaml:"checkpoints,omitempty"`
}

// Phase is one stage of the workload.
type Phase struct {
	Name        string      `yaml:"name"`
	Duration    string      `yaml:"duration,omitempty"`
	TargetQPS   int         `yaml:"target_qps,omitempty"`
	Parallelism int         `yaml:"parallelism,omitempty"`
	Operations  []Operation `yaml:"operations,omitempty"`
	Mix         []MixEntry  `yaml:"mix,omitempty"`
}

// MixEntry is one weighted operation in a phase's mix. Weights are
// relative; they don't need to sum to 100.
type MixEntry struct {
	Op     string `yaml:"op"`
	Weight int    `yaml:"weight"`
}

// Operation is one declarative step. The runner reads the Kind field
// to dispatch and then consults whichever sibling fields the op needs.
//
// YAML shape: each operation in a list is a single-key map whose key
// is the op kind. We translate those to this flat struct via the
// custom UnmarshalYAML below — it keeps the YAML readable while
// staying out of interface dispatch in the runner.
type Operation struct {
	Kind string `yaml:"-"` // populated by UnmarshalYAML

	// create_table
	Name     string `yaml:"name,omitempty"`
	Schema   string `yaml:"schema,omitempty"`
	RefersTo string `yaml:"refers_to,omitempty"`

	// insert_rows / copy_in / update_rows / delete_rows / create_index / vacuum
	Table     string `yaml:"table,omitempty"`
	Count     int    `yaml:"count,omitempty"`
	Generator string `yaml:"generator,omitempty"`

	// insert_rows: starting index for the deterministic generator's
	// row-number axis.  faker_* generators produce content keyed on
	// the row index (e.g. faker_users emits `user-N@example.test`),
	// so a load file that calls `insert_rows` twice into the SAME
	// table with the SAME generator hits a duplicate-key conflict
	// the moment a unique constraint is added.  Set StartOffset on
	// every call after the first so the index axis advances:
	//
	//	- insert_rows: { table: users, count: 1000 }                       # rows 0..999
	//	- create_index: { table: users, columns: [email], unique: true }
	//	- insert_rows: { table: users, count: 1000, start_offset: 1000 }   # rows 1000..1999
	//
	// Default 0 preserves the existing single-call shape.
	StartOffset int `yaml:"start_offset,omitempty"`

	// update_rows / delete_rows
	Fraction float64 `yaml:"fraction,omitempty"`
	Where    string  `yaml:"where,omitempty"`

	// create_index
	Columns []string `yaml:"columns,omitempty"`
	Unique  bool     `yaml:"unique,omitempty"`

	// vacuum
	Full bool `yaml:"full,omitempty"`

	// alter_table
	AddColumn map[string]string `yaml:"add_column,omitempty"`

	// checkpoint
	Label string `yaml:"label,omitempty"`
}

// UnmarshalYAML accepts the single-key-map shape for an operation:
//
//   - create_table:  { name: users, schema: users_v1 }
//   - insert_rows:   { table: users, count: 1000000, generator: faker_users }
//
// The outer map's only key becomes Kind; the value is decoded into
// the rest of the struct.
func (o *Operation) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("operation: want a mapping, got %v", node.Kind)
	}
	if len(node.Content) != 2 {
		return fmt.Errorf("operation: want exactly one key (the op kind), got %d entries", len(node.Content)/2)
	}
	keyNode := node.Content[0]
	valNode := node.Content[1]
	o.Kind = keyNode.Value

	// Empty value (e.g. `- vacuum:`) is acceptable for nullary ops.
	if valNode.Kind == yaml.ScalarNode && valNode.Value == "" {
		return nil
	}
	if valNode.Kind != yaml.MappingNode {
		return fmt.Errorf("operation %s: expected a mapping value, got %v", o.Kind, valNode.Kind)
	}

	// Decode the value into a temporary so we don't recurse infinitely
	// through this same UnmarshalYAML.
	type opPayload Operation
	var p opPayload
	if err := valNode.Decode(&p); err != nil {
		return fmt.Errorf("operation %s: %w", o.Kind, err)
	}
	p.Kind = o.Kind
	*o = Operation(p)
	return nil
}

// CheckpointSpec defines how often + when checkpoints fire.
type CheckpointSpec struct {
	Every                string                `yaml:"every,omitempty"`
	On                   []TriggeredCheckpoint `yaml:"on,omitempty"`
	AssertsPerCheckpoint []map[string]any      `yaml:"asserts_per_checkpoint,omitempty"`
}

// TriggeredCheckpoint is one rule in CheckpointSpec.On. Exactly one
// field is non-empty per entry; the runner inspects which.
type TriggeredCheckpoint struct {
	PhaseEnd   string `yaml:"phase_end,omitempty"`
	LSNAdvance string `yaml:"lsn_advance,omitempty"`
	Wallclock  string `yaml:"wallclock,omitempty"`
}

// LoadFromFile reads + parses a *.load.yaml file. KnownFields=true so
// a typo in a scenario's load reference fails loudly instead of
// silently dropping the field.
func LoadFromFile(path string) (*Load, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load: read %s: %w", path, err)
	}
	return Parse(body)
}

// Parse decodes YAML bytes into a Load. Validates schema / seed.
func Parse(body []byte) (*Load, error) {
	var l Load
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("load: parse: %w", err)
	}
	if l.Schema != SchemaLoad {
		return nil, fmt.Errorf("load: schema %q is not supported; want %q", l.Schema, SchemaLoad)
	}
	if l.Seed == 0 {
		return nil, fmt.Errorf("load: seed is required (use a non-zero uint64; same-seed → same-stream is the determinism guarantee)")
	}
	if len(l.Phases) == 0 {
		return nil, fmt.Errorf("load: at least one phase is required")
	}
	for i, p := range l.Phases {
		if p.Name == "" {
			return nil, fmt.Errorf("load: phase %d has no name", i)
		}
	}
	return &l, nil
}
