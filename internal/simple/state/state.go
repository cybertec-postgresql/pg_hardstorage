// Package state is the on-disk "what did you pick last time" cache
// for pg_hardstorage_simple.  Lives at <Config>/simple.yaml alongside
// the operator's authoritative deployment configs; flows read it at
// startup to pre-populate defaults and write it back on commit.
//
// This is *not* a source of truth — the authoritative deployment
// list still lives in pg_hardstorage's normal config dirs.  Losing
// simple.yaml just means the next prompt asks the question again
// instead of defaulting to last time's answer.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Schema is the wire-format identifier carried on every committed
// State.  Bumped on backward-incompatible field changes; v1 is
// "just the last-picked defaults".
const Schema = "pg_hardstorage.simple.v1"

// FileName is the on-disk filename (under <Config>/).
const FileName = "simple.yaml"

// State is the persisted cache.  All fields are optional; an empty
// State (the first-run case) just means every prompt asks fresh.
type State struct {
	Schema           string `yaml:"schema"`
	LastDeployment   string `yaml:"last_deployment,omitempty"`
	LastRepoURL      string `yaml:"last_repo_url,omitempty"`
	LastPGConnection string `yaml:"last_pg_connection,omitempty"`
	LastTargetDir    string `yaml:"last_target_dir,omitempty"`
}

// Load reads simple.yaml from configDir.  Missing file → empty State
// with the current Schema stamped, no error (first-run case).  Parse
// errors propagate so a corrupted cache surfaces loudly instead of
// silently dropping defaults.
func Load(configDir string) (*State, error) {
	path := filepath.Join(configDir, FileName)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{Schema: Schema}, nil
		}
		return nil, fmt.Errorf("simple state: read %s: %w", path, err)
	}
	var s State
	if err := yaml.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("simple state: parse %s: %w", path, err)
	}
	if s.Schema == "" {
		s.Schema = Schema
	}
	return &s, nil
}

// Save writes s to <configDir>/simple.yaml.  Creates the directory if
// needed (mode 0700; same posture as the keyring sibling).  Atomic
// via write-then-rename so a crashed write doesn't leave a partial
// file that fails parse on the next Load.
func Save(configDir string, s *State) error {
	if s == nil {
		return errors.New("simple state: nil state")
	}
	if s.Schema == "" {
		s.Schema = Schema
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("simple state: mkdir %s: %w", configDir, err)
	}
	body, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("simple state: marshal: %w", err)
	}
	dst := filepath.Join(configDir, FileName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("simple state: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("simple state: rename %s → %s: %w", tmp, dst, err)
	}
	return nil
}
