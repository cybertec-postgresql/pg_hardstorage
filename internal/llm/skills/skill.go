// Package skills loads, validates, and serves LLM skill files.
//
// A skill is a versioned YAML file describing one operator-facing
// capability ("ask a question", "run the restore wizard", "draft an
// incident postmortem"). The schema is intentionally small in v0.1
// so the surface stays inspectable; grows it (cosign signature
// verification, golden-test harness, marketplace registry).
//
// On-disk precedence (highest priority first):
//
//	~/.config/pg_hardstorage/skills/   (user-private)
//	/etc/pg_hardstorage/skills/        (operator overrides)
//	/usr/share/pg_hardstorage/skills/  (shipped, read-only)
//
// LoadAll walks each directory in order; later entries with the same
// `name` win. A skill at `/etc` overrides one with the same name at
// `/usr/share`; a user's `~/.config` entry overrides both.
package skills

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*.skill.yaml
var builtinFS embed.FS

// LoadBuiltins parses every skill bundled with the binary.  Used
// by callers (the chat session bootstrap, `llm skill list`) that
// want a guaranteed minimum set even when no /usr/share or /etc
// override directory is present.
//
// Returns a Set in stable name-sorted order.  A parse failure on
// a bundled skill is fatal — the binary ships a known-good corpus
// and a regression should be loud.
func LoadBuiltins() (*Set, error) {
	out := &Set{byName: map[string]*Skill{}}
	err := fs.WalkDir(builtinFS, "builtin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".skill.yaml") {
			return nil
		}
		body, rerr := builtinFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		s, perr := Parse(body)
		if perr != nil {
			return fmt.Errorf("skills: parse builtin %s: %w", p, perr)
		}
		s.Source = "builtin:" + p
		if _, exists := out.byName[s.Name]; !exists {
			out.order = append(out.order, s.Name)
		}
		out.byName[s.Name] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadAllWithBuiltins loads the builtin set first, then layers
// the on-disk precedence chain via LoadAll.  Disk overrides win.
// The typical production-runtime entry point: builtins as the
// floor, /etc + ~/.config as override layers.
func LoadAllWithBuiltins(dirs []string) (*Set, error) {
	out, err := LoadBuiltins()
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return out, nil
	}
	overrides, err := LoadAll(dirs)
	if err != nil {
		return nil, err
	}
	for _, name := range overrides.order {
		s := overrides.byName[name]
		if _, exists := out.byName[s.Name]; !exists {
			out.order = append(out.order, s.Name)
		}
		out.byName[s.Name] = s
	}
	return out, nil
}

// SchemaSkill is the YAML schema string. 24-month back-compat per the
// project-wide commitment.
const SchemaSkill = "pg_hardstorage.skill.v1"

// Skill is one parsed YAML file.
//
// JSON tags mirror the YAML tags so `llm skill show -o json`
// surfaces operator-friendly snake_case keys (matches the v1
// stable-schema convention used everywhere else).
type Skill struct {
	Schema      string `yaml:"schema"      json:"schema"`
	Name        string `yaml:"name"        json:"name"`
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Version     string `yaml:"version"     json:"version"`
	Description string `yaml:"description,omitempty"  json:"description,omitempty"`

	Trigger     Trigger     `yaml:"trigger,omitempty"     json:"trigger,omitempty"`
	Permissions Permissions `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Context     ContextSpec `yaml:"context,omitempty"     json:"context,omitempty"`
	Guardrails  []Guardrail `yaml:"guardrails,omitempty"  json:"guardrails,omitempty"`

	PromptTemplate string `yaml:"prompt_template" json:"prompt_template"`

	// Locales overrides DisplayName / Description / PromptTemplate
	// for a specific operator locale.  Keyed by ISO 639-1 (or
	// language-region) code: "de", "fr", "ja", "de-AT", "fr-CA".
	// Lookup uses prefix matching: a request for "de-AT" first
	// tries "de-AT", then falls back to "de", then to the default
	// English fields.  Empty when the skill ships English-only.
	Locales map[string]LocaleOverride `yaml:"locales,omitempty" json:"locales,omitempty"`

	// Source is set by LoadAll to the file the skill was read from.
	// Useful for `llm skill show` and for error messages that point
	// at a specific override path.
	Source string `yaml:"-" json:"source,omitempty"`
}

// LocaleOverride is one language's translation for a Skill's
// operator-facing strings.  Any field left empty falls back to
// the Skill's default (English) value.
type LocaleOverride struct {
	DisplayName    string `yaml:"display_name,omitempty"    json:"display_name,omitempty"`
	Description    string `yaml:"description,omitempty"     json:"description,omitempty"`
	PromptTemplate string `yaml:"prompt_template,omitempty" json:"prompt_template,omitempty"`
}

// Localized returns a copy of the Skill with DisplayName /
// Description / PromptTemplate substituted from the matching
// LocaleOverride.  An empty locale (or one with no entry)
// returns the Skill unchanged.
//
// Lookup precedence: exact-match → language-prefix-match →
// default.  E.g. "de-AT" tries "de-AT" then "de"; "ja-JP"
// tries "ja-JP" then "ja"; "klingon" falls back to default.
func (s *Skill) Localized(locale string) *Skill {
	locale = strings.ToLower(strings.TrimSpace(locale))
	if locale == "" || len(s.Locales) == 0 {
		return s
	}
	candidates := []string{locale}
	if i := strings.IndexByte(locale, '-'); i > 0 {
		candidates = append(candidates, locale[:i])
	}
	for _, c := range candidates {
		if ov, ok := s.Locales[c]; ok {
			out := *s
			if ov.DisplayName != "" {
				out.DisplayName = ov.DisplayName
			}
			if ov.Description != "" {
				out.Description = ov.Description
			}
			if ov.PromptTemplate != "" {
				out.PromptTemplate = ov.PromptTemplate
			}
			return &out
		}
	}
	return s
}

// Trigger describes when the skill fires.
type Trigger struct {
	Manual      []string `yaml:"manual,omitempty"`
	AutoOnError []string `yaml:"auto_on_error,omitempty"`
}

// Permissions gates skill invocation against operator RBAC.
type Permissions struct {
	ReadOnly     bool     `yaml:"read_only"`
	RequiredRBAC []string `yaml:"required_rbac,omitempty"`
}

// ContextSpec declares what the skill needs preloaded and which tools
// it may invoke.
type ContextSpec struct {
	PreloadTools   []ToolPreload `yaml:"preload_tools,omitempty" json:"preload_tools,omitempty"`
	AvailableTools []string      `yaml:"available_tools,omitempty" json:"available_tools,omitempty"`

	// AllowedExecutes is the prefix-allowlist for the
	// execute_command tool (advise+execute mode).  Each entry
	// is a command-prefix string; execute_command refuses any
	// invocation that doesn't START WITH one of them.
	//
	// Required when AvailableTools includes "execute_command".
	// Empty refuses every execution.
	//
	// Example:
	//
	//   allowed_executes:
	//     - "pg_hardstorage doctor"
	//     - "pg_hardstorage list"
	//     - "pg_hardstorage status"
	//     - "pg_hardstorage show"
	AllowedExecutes []string `yaml:"allowed_executes,omitempty" json:"allowed_executes,omitempty"`
}

// ToolPreload is one entry in preload_tools. Entries can be either a
// bare string (run the tool with default args) or a map shape with
// args. The custom UnmarshalYAML accepts both.
type ToolPreload struct {
	Name string         `yaml:"-"`
	Args map[string]any `yaml:"-"`
}

// UnmarshalYAML accepts:
//
//   - read_doctor                              # bare string
//   - read_runbook: { id: R5 }                 # mapping
//
// Both decode to a ToolPreload with Name set and Args populated when
// present.
func (p *ToolPreload) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Name = node.Value
		return nil
	case yaml.MappingNode:
		if len(node.Content) != 2 {
			return fmt.Errorf("preload_tools: want single-key map, got %d entries", len(node.Content)/2)
		}
		p.Name = node.Content[0].Value
		valNode := node.Content[1]
		if valNode.Kind == yaml.ScalarNode && valNode.Value == "" {
			return nil
		}
		var args map[string]any
		if err := valNode.Decode(&args); err != nil {
			return fmt.Errorf("preload_tools[%s]: %w", p.Name, err)
		}
		p.Args = args
		return nil
	}
	return fmt.Errorf("preload_tools: unsupported node kind %v", node.Kind)
}

// Guardrail is one entry in the skill's guardrails list. Entries are
// single-key maps; we lift the key as Kind and decode the rest into
// a typed body.
type Guardrail struct {
	Kind  string
	Value any
}

// UnmarshalYAML accepts the single-key-map shape used in the SPEC.
func (g *Guardrail) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("guardrails: want single-key map, got %d entries", len(node.Content)/2)
	}
	g.Kind = node.Content[0].Value
	var v any
	if err := node.Content[1].Decode(&v); err != nil {
		return err
	}
	g.Value = v
	return nil
}

// LoadFile reads + parses a single skill file. Validates schema and
// required fields.
func LoadFile(path string) (*Skill, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skills: read %s: %w", path, err)
	}
	s, err := Parse(body)
	if err != nil {
		return nil, fmt.Errorf("skills: parse %s: %w", path, err)
	}
	s.Source = path
	return s, nil
}

// Parse decodes YAML bytes. KnownFields=true so a typo lands as a
// loud error rather than a silent dropped field.
func Parse(body []byte) (*Skill, error) {
	var s Skill
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if s.Schema != SchemaSkill {
		return nil, fmt.Errorf("schema %q is not supported; want %q", s.Schema, SchemaSkill)
	}
	if s.Name == "" {
		return nil, errors.New("name is required")
	}
	if s.Version == "" {
		return nil, errors.New("version is required")
	}
	if s.PromptTemplate == "" {
		return nil, errors.New("prompt_template is required")
	}
	return &s, nil
}

// Set is the resolved set of skills after walking the precedence
// chain. Skills are addressable by name; iterating in deterministic
// order helps `llm skill list`.
type Set struct {
	byName map[string]*Skill
	order  []string
}

// LoadAll walks the precedence chain and returns the merged Set.
// Each directory's `*.skill.yaml` is loaded; later directories'
// skills with the same name override earlier ones.
//
// Returns the empty Set + nil error when no directory contains a
// skill file. A directory that exists but contains a parse error is
// fatal — silent partial loads are worse than a loud refusal.
func LoadAll(dirs []string) (*Set, error) {
	out := &Set{byName: map[string]*Skill{}}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("skills: read dir %s: %w", dir, err)
		}
		// Sort entries by name so the load order within a directory
		// is deterministic (last-wins is conditioned on the dir
		// order; within a dir, alphabetical is the documented rule).
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() && (strings.HasSuffix(n, ".skill.yaml") || strings.HasSuffix(n, ".skill.yml")) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			path := filepath.Join(dir, n)
			s, err := LoadFile(path)
			if err != nil {
				return nil, err
			}
			if _, exists := out.byName[s.Name]; !exists {
				out.order = append(out.order, s.Name)
			}
			out.byName[s.Name] = s
		}
	}
	return out, nil
}

// Get returns the skill with the given name, or ErrNotFound.
func (s *Set) Get(name string) (*Skill, error) {
	got, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return got, nil
}

// All returns every loaded skill, sorted by name.
func (s *Set) All() []*Skill {
	out := make([]*Skill, 0, len(s.byName))
	for _, n := range s.order {
		out = append(out, s.byName[n])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every loaded skill name, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.byName))
	for n := range s.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ErrNotFound is returned by Get when the skill name isn't loaded.
var ErrNotFound = errors.New("skills: not found")

// DefaultDirs returns the precedence chain the production agent uses.
// Tests may override entries; the production caller passes this
// through to LoadAll.
//
// homeDir is the operator's $HOME (empty for tests / non-interactive
// use); we skip the user-private directory when it's empty rather
// than appending a misleading "/skills" suffix.
func DefaultDirs(homeDir string) []string {
	out := []string{
		"/usr/share/pg_hardstorage/skills",
		"/etc/pg_hardstorage/skills",
	}
	if homeDir != "" {
		out = append(out, filepath.Join(homeDir, ".config/pg_hardstorage/skills"))
	}
	return out
}

// Lint inspects a Skill for common authoring mistakes. Returns a
// slice of human-readable issues — empty if the skill is clean.
// Used by `llm skill lint`. Distinct from the schema-level checks
// in Parse: those reject malformed YAML; lint warns about smells
// (missing description, unbounded token budget, banned tool in the
// available_tools list).
func (s *Skill) Lint() []string {
	var issues []string
	if s.Description == "" {
		issues = append(issues, "no description set; operator-facing skill should describe what it does")
	}
	if !s.Permissions.ReadOnly {
		issues = append(issues, "permissions.read_only is false; v0.1 only supports read-only skills (advise+execute lands)")
	}
	for _, t := range s.Context.AvailableTools {
		if t == "execute_command" {
			issues = append(issues, "available_tools includes execute_command; v0.1 doesn't ship that tool — strip it or guard behind a release")
		}
	}
	hasBudget := false
	for _, g := range s.Guardrails {
		if strings.HasPrefix(g.Kind, "max_token_budget") {
			hasBudget = true
		}
	}
	if !hasBudget {
		issues = append(issues, "no max_token_budget guardrail; a runaway skill can burn budget — set one")
	}
	return issues
}
