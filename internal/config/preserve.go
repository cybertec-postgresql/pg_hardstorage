// preserve.go — write a Config back over an existing file without
// destroying the operator's comments.
package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarshalPreserving serialises cfg as the next content of a config file
// whose current content is existing. Comments, key order and scalar
// quoting of everything the edit did not touch survive: the new values
// are merged into the existing document's node tree instead of
// replacing it. Blank lines and flow/block layout are still normalised
// by the YAML encoder.
//
// With no existing content (a new file), or content that is not a YAML
// mapping, it is Marshal. It also falls back to Marshal whenever the
// merged document would not decode to exactly cfg (anchors and aliases
// are the case that can do that): a config file that says something
// other than what the command reported would be worse than lost
// comments.
func MarshalPreserving(cfg *Config, existing []byte) ([]byte, error) {
	plain, err := Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		return plain, nil
	}
	var oldDoc, newDoc yaml.Node
	if err := yaml.Unmarshal(existing, &oldDoc); err != nil {
		return plain, nil
	}
	if err := yaml.Unmarshal(plain, &newDoc); err != nil {
		return nil, fmt.Errorf("config: marshal: %w", err)
	}
	if !isMappingDoc(&oldDoc) || !isMappingDoc(&newDoc) {
		return plain, nil
	}
	oldDoc.Content[0] = mergeNode(oldDoc.Content[0], newDoc.Content[0])

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(detectIndent(existing))
	if err := enc.Encode(&oldDoc); err != nil {
		return plain, nil
	}
	if err := enc.Close(); err != nil {
		return plain, nil
	}
	if !sameConfig(buf.Bytes(), plain) {
		return plain, nil
	}
	return buf.Bytes(), nil
}

func isMappingDoc(doc *yaml.Node) bool {
	return doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 &&
		doc.Content[0].Kind == yaml.MappingNode
}

// sameConfig reports whether two YAML documents decode to the same
// Config.
func sameConfig(a, b []byte) bool {
	var ca, cb Config
	if yaml.Unmarshal(a, &ca) != nil || yaml.Unmarshal(b, &cb) != nil {
		return false
	}
	return yamlEqual(ca, cb)
}

// mergeNode returns the node to write where the file has old and the
// config now says want. Whatever is unchanged is the old node itself,
// comments and style included.
func mergeNode(old, want *yaml.Node) *yaml.Node {
	if old == nil {
		return want
	}
	if old.Kind == yaml.AliasNode {
		if old.Alias != nil && nodeEqual(old.Alias, want) {
			return old
		}
		return withComments(want, old)
	}
	if old.Kind != want.Kind {
		return withComments(want, old)
	}
	switch want.Kind {
	case yaml.MappingNode:
		return mergeMapping(old, want)
	case yaml.SequenceNode:
		return mergeSequence(old, want)
	case yaml.ScalarNode:
		if old.ShortTag() == want.ShortTag() && old.Value == want.Value {
			return old
		}
		return withComments(want, old)
	}
	return want
}

// mergeMapping keeps the file's key order for keys that remain, drops
// the ones the config no longer has, and appends new keys in the order
// the encoder gave them.
func mergeMapping(old, want *yaml.Node) *yaml.Node {
	wantVal := make(map[string]*yaml.Node, len(want.Content)/2)
	for i := 0; i+1 < len(want.Content); i += 2 {
		wantVal[want.Content[i].Value] = want.Content[i+1]
	}
	out := *old
	out.Content = make([]*yaml.Node, 0, len(want.Content))
	kept := make(map[string]bool, len(wantVal))
	for i := 0; i+1 < len(old.Content); i += 2 {
		k := old.Content[i]
		wv, ok := wantVal[k.Value]
		if !ok || kept[k.Value] {
			continue
		}
		kept[k.Value] = true
		out.Content = append(out.Content, k, mergeNode(old.Content[i+1], wv))
	}
	for i := 0; i+1 < len(want.Content); i += 2 {
		if !kept[want.Content[i].Value] {
			out.Content = append(out.Content, want.Content[i], want.Content[i+1])
		}
	}
	if len(old.Content) == 0 {
		// `{}` in the file: let the new entries render as a block.
		out.Style = want.Style
	}
	return &out
}

// mergeSequence reuses an old element for every new element that equals
// it, so removing or inserting an item does not shift the neighbours'
// comments. A new element with no equal is merged into the old element
// at its own position when that one is otherwise unclaimed (an item
// edited in place).
func mergeSequence(old, want *yaml.Node) *yaml.Node {
	out := *old
	out.Content = make([]*yaml.Node, len(want.Content))
	used := make([]bool, len(old.Content))
	for i, w := range want.Content {
		for j, o := range old.Content {
			if !used[j] && nodeEqual(o, w) {
				used[j] = true
				out.Content[i] = o
				break
			}
		}
	}
	for i, w := range want.Content {
		if out.Content[i] != nil {
			continue
		}
		if i < len(old.Content) && !used[i] {
			used[i] = true
			out.Content[i] = mergeNode(old.Content[i], w)
			continue
		}
		out.Content[i] = w
	}
	if len(old.Content) == 0 {
		out.Style = want.Style
	}
	return &out
}

// withComments returns want carrying the comments that were attached to
// the node it replaces.
func withComments(want, old *yaml.Node) *yaml.Node {
	out := *want
	out.HeadComment, out.LineComment, out.FootComment = old.HeadComment, old.LineComment, old.FootComment
	return &out
}

// nodeEqual compares two nodes by value, ignoring comments, style and
// mapping key order.
func nodeEqual(a, b *yaml.Node) bool {
	if a.Kind == yaml.AliasNode && a.Alias != nil {
		a = a.Alias
	}
	if b.Kind == yaml.AliasNode && b.Alias != nil {
		b = b.Alias
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.ShortTag() == b.ShortTag() && a.Value == b.Value
	case yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !nodeEqual(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	case yaml.MappingNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		bv := make(map[string]*yaml.Node, len(b.Content)/2)
		for i := 0; i+1 < len(b.Content); i += 2 {
			bv[b.Content[i].Value] = b.Content[i+1]
		}
		for i := 0; i+1 < len(a.Content); i += 2 {
			v, ok := bv[a.Content[i].Value]
			if !ok || !nodeEqual(a.Content[i+1], v) {
				return false
			}
		}
		return true
	}
	return false
}

// detectIndent returns the indentation step the file uses, so an edit
// does not reflow a 2-space file to the encoder's 4.
func detectIndent(src []byte) int {
	const def = 4
	prevOpens := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		n := len(line) - len(trimmed)
		if prevOpens && n > 0 && !strings.HasPrefix(trimmed, "- ") {
			if n < 2 || n > 8 {
				return def
			}
			return n
		}
		prevOpens = n == 0 && strings.HasSuffix(strings.TrimRight(trimmed, " \t\r"), ":")
	}
	return def
}
