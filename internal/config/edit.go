// edit.go — layered config editing: write back only the target file's own content.
package config

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// EditView is the config as an editing command needs it. Readers see
// the merged view (Merged); write-back must persist only what the
// target file itself says plus the edit, never the merge — otherwise
// every drop-in deployment and the PG_HARDSTORAGE_CONFIG env YAML
// (credentials included) get copied into the main file on the first
// `deployment add`, and removing a drop-in deployment "succeeds" while
// the drop-in keeps defining it.
type EditView struct {
	// Merged is the effective config, exactly as Load returns it.
	Merged Config
	// Path is the file write-back targets: the -c file, else the main
	// pg_hardstorage.yaml.
	Path string

	own      Config // the target file's own content (a private copy)
	baseline Config // Merged as loaded (a private copy), to diff edits against
	// elsewhere names, per deployment / sink name, the OTHER layers
	// that define it.
	deploymentsElsewhere map[string][]string
	otherSinks           []sourcedSink
}

type sourcedSink struct {
	spec   output.SinkSpec
	name   string
	source string
}

// DefinedElsewhereError refuses an edit to something another layer (a
// conf.d drop-in, or the PG_HARDSTORAGE_CONFIG env YAML) defines: the
// target file cannot express the change, and writing it there would be
// shadowed by — or duplicate — the other layer.
type DefinedElsewhereError struct {
	Kind    string // "deployment" | "sink"
	Name    string
	Sources []string
}

func (e *DefinedElsewhereError) Error() string {
	return fmt.Sprintf("config: %s %q is defined in %s, not in the file this command edits; change it there",
		e.Kind, e.Name, strings.Join(e.Sources, ", "))
}

// sourceLabel is how a layer is named to the operator.
func sourceLabel(sf SourceFile) string {
	if sf.Kind == "env" {
		return "the PG_HARDSTORAGE_CONFIG environment variable"
	}
	return sf.Path
}

// LoadForEdit reads the config for an editing command. Unlike Load, an
// explicit -c file that does not exist yet is fine: the edit creates it.
func LoadForEdit(p *paths.Paths) (*EditView, error) {
	ls, err := readLayers(p)
	if err != nil {
		return nil, err
	}
	v := &EditView{
		Path:                 ls.layers[ls.own].sf.Path,
		deploymentsElsewhere: map[string][]string{},
	}
	for i, l := range ls.layers {
		if !l.sf.ReadOK {
			continue
		}
		v.Merged = mergeConfig(v.Merged, l.cfg)
		if i == ls.own {
			continue
		}
		for name := range l.cfg.Deployments {
			v.deploymentsElsewhere[name] = append(v.deploymentsElsewhere[name], sourceLabel(l.sf))
		}
		for _, s := range l.cfg.Sinks {
			v.otherSinks = append(v.otherSinks, sourcedSink{spec: s, name: s.Name, source: sourceLabel(l.sf)})
		}
	}
	if err := validate(v.Merged); err != nil {
		return nil, err
	}
	if ls.layers[ls.own].sf.ReadOK {
		if v.own, err = clone(ls.layers[ls.own].cfg); err != nil {
			return nil, err
		}
	}
	if v.baseline, err = clone(v.Merged); err != nil {
		return nil, err
	}
	// Hand the caller its own copy so in-place mutation of nested maps
	// cannot reach the baseline or the target's content.
	if v.Merged, err = clone(v.Merged); err != nil {
		return nil, err
	}
	return v, nil
}

// Apply computes the content to write to Path for an edited copy of
// Merged: the target file's own content with exactly the caller's
// changes applied. Changing or removing a deployment or sink that
// another layer defines returns *DefinedElsewhereError.
func (v *EditView) Apply(updated *Config) (*Config, error) {
	out, err := clone(v.own)
	if err != nil {
		return nil, err
	}

	// Top-level fields other than the two keyed collections: take the
	// caller's value only where it differs from what was loaded, so a
	// value inherited from another layer is not copied into the file.
	ov, uv, bv := reflect.ValueOf(&out).Elem(), reflect.ValueOf(*updated), reflect.ValueOf(v.baseline)
	for i := 0; i < ov.NumField(); i++ {
		switch ov.Type().Field(i).Name {
		case "Deployments", "Sinks":
			continue
		}
		if !yamlEqual(uv.Field(i).Interface(), bv.Field(i).Interface()) {
			ov.Field(i).Set(uv.Field(i))
		}
	}

	// Deployments: per-name three-way diff against the loaded view.
	names := map[string]bool{}
	for n := range v.baseline.Deployments {
		names[n] = true
	}
	for n := range updated.Deployments {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		before, wasThere := v.baseline.Deployments[name]
		after, isThere := updated.Deployments[name]
		if wasThere && isThere && yamlEqual(before, after) {
			continue // untouched
		}
		if srcs := v.deploymentsElsewhere[name]; len(srcs) > 0 {
			return nil, &DefinedElsewhereError{Kind: "deployment", Name: name, Sources: srcs}
		}
		if isThere {
			if out.Deployments == nil {
				out.Deployments = map[string]DeploymentConfig{}
			}
			out.Deployments[name] = after
		} else {
			delete(out.Deployments, name)
		}
	}

	// Sinks append across layers, so the loaded list interleaves them.
	// Remove every other layer's sink from the edited list (each must
	// still be there, unchanged); what remains is the target's own list.
	if !yamlEqual(v.baseline.Sinks, updated.Sinks) {
		remaining := append(updated.Sinks[:0:0], updated.Sinks...)
		for _, other := range v.otherSinks {
			found := -1
			for i, s := range remaining {
				if yamlEqual(s, other.spec) {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, &DefinedElsewhereError{Kind: "sink", Name: other.name, Sources: []string{other.source}}
			}
			remaining = append(remaining[:found], remaining[found+1:]...)
		}
		out.Sinks = remaining
	}

	if out.Schema == "" {
		out.Schema = Schema
	}
	return &out, nil
}

// clone deep-copies a Config through its YAML form — the same form it is
// read from and written to, so nothing the file can carry is lost.
func clone(c Config) (Config, error) {
	b, err := yaml.Marshal(&c)
	if err != nil {
		return Config{}, fmt.Errorf("config: copy: %w", err)
	}
	var out Config
	if err := yaml.Unmarshal(b, &out); err != nil {
		return Config{}, fmt.Errorf("config: copy: %w", err)
	}
	return out, nil
}

// yamlEqual compares two values by their YAML encoding, which is what
// lands in the file: nil and empty collections, both omitted, compare
// equal, where reflect.DeepEqual would call them different.
func yamlEqual(a, b any) bool {
	ab, err1 := yaml.Marshal(a)
	bb, err2 := yaml.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}
