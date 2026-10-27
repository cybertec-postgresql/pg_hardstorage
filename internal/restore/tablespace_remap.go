// tablespace_remap.go — TablespaceRemap: OLDDIR=NEWDIR mapping fed to tablespace_map + pg_combinebackup.
package restore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TablespaceRemap is the operator-supplied mapping from
// source-cluster tablespace paths to target-cluster paths.
// Each entry is one OLDDIR=NEWDIR pair.
//
// Two consumers:
//
//  1. Plain (non-chain) restore: rewrites the manifest's
//     `tablespace_map` content before writing it into the
//     restored data directory. PG reads `tablespace_map` at
//     recovery start and creates symlinks under `pg_tblspc/`
//     pointing at the listed paths; rewriting the listed
//     paths is how we get the restored cluster to use the
//     operator's chosen tablespace locations.
//
//  2. Chain (incremental) restore: passed through to
//     pg_combinebackup via `--tablespace-mapping=OLD=NEW`.
//     PG's tool already handles the path rewrites + symlink
//     creation under its own logic; we just translate our
//     mapping into its flag shape.
//
// The mapping is absolute-path-strict: both sides must be
// absolute. Empty / relative paths are refused at parse time
// so a typo never silently no-ops.
type TablespaceRemap []TablespaceRemapEntry

// TablespaceRemapEntry is one entry in the remap. Old is the
// source-cluster path as recorded in the manifest's
// `tablespace_map`; New is the target-cluster path the
// operator wants the restored data to land at.
type TablespaceRemapEntry struct {
	Old string
	New string
}

// ParseTablespaceRemap parses a slice of "OLD=NEW" strings
// into a structured TablespaceRemap. Empty input yields a
// nil result (no remap requested). Validation:
//
//   - Each entry MUST contain exactly one '=' (the separator).
//   - Both Old and New MUST be non-empty.
//   - Both Old and New MUST be absolute paths.
//   - Duplicate Old entries refuse — last-wins would silently
//     mask a typo'd config.
//
// Returned errors are operator-friendly: include the offending
// entry so the CLI's usage-error layer can quote it back.
func ParseTablespaceRemap(entries []string) (TablespaceRemap, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(TablespaceRemap, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, fmt.Errorf("restore: tablespace mapping entry %d is empty", i)
		}
		// A stricter "exactly one =" check: an `=` in either
		// path is rare but not impossible (paths with `=`
		// are syntactically valid on Linux). PG's own
		// pg_combinebackup uses the same simple split-on-
		// first-equals; we mirror that.
		idx := strings.Index(raw, "=")
		if idx <= 0 || idx == len(raw)-1 {
			return nil, fmt.Errorf("restore: tablespace mapping %q must be OLD=NEW with both sides non-empty", raw)
		}
		old := raw[:idx]
		neu := raw[idx+1:]
		if !filepath.IsAbs(old) {
			return nil, fmt.Errorf("restore: tablespace mapping %q: OLD path %q must be absolute", raw, old)
		}
		if !filepath.IsAbs(neu) {
			return nil, fmt.Errorf("restore: tablespace mapping %q: NEW path %q must be absolute", raw, neu)
		}
		// Reject embedded newline / CR / NUL in either path. These can
		// never appear in a legitimate tablespace path (NUL is not a
		// valid path byte at all), and a newline in NEW would split the
		// "<oid> <path>" line when Apply rewrites tablespace_map —
		// forging an extra OID→path entry that PG turns into a symlink
		// (e.g. "/new\n99999 /attacker" injects tablespace 99999). The
		// IsAbs check above passes such a value because it only inspects
		// the leading "/". Gate it here, at the single parser both the
		// CLI and the control-plane agent route through.
		if strings.ContainsAny(old, "\n\r\x00") {
			return nil, fmt.Errorf("restore: tablespace mapping %q: OLD path contains an illegal control character", raw)
		}
		if strings.ContainsAny(neu, "\n\r\x00") {
			return nil, fmt.Errorf("restore: tablespace mapping %q: NEW path contains an illegal control character", raw)
		}
		if _, dup := seen[old]; dup {
			return nil, fmt.Errorf("restore: tablespace mapping has duplicate OLD path %q", old)
		}
		seen[old] = struct{}{}
		out = append(out, TablespaceRemapEntry{Old: old, New: neu})
	}
	return out, nil
}

// CleanTablespaceMapAll validates a --tablespace-mapping-all parent
// directory and returns it cleaned. It must be an absolute path without
// control characters, for the same reasons ParseTablespaceRemap's NEW side
// must (a newline would forge a tablespace_map line). "" means "not
// requested".
func CleanTablespaceMapAll(parent string) (string, error) {
	if strings.TrimSpace(parent) == "" {
		return "", nil
	}
	if !filepath.IsAbs(parent) {
		return "", fmt.Errorf("restore: tablespace-mapping-all %q: parent directory must be an absolute path", parent)
	}
	if strings.ContainsAny(parent, "\n\r\x00") {
		return "", fmt.Errorf("restore: tablespace-mapping-all %q: path contains an illegal control character", parent)
	}
	return filepath.Clean(parent), nil
}

// WithMapAll extends the remap so EVERY non-default tablespace of the
// backup lands under one parent directory, the way pgBackRest's
// --tablespace-map-all does: tablespace <oid> goes to <parent>/<oid>. The
// expansion is computed from the backup's own tablespace list at restore
// time, so a tablespace added after the mapping was configured is covered
// without touching it, and the result is deterministic (the OID is the
// only input).
//
// An explicit entry for a tablespace's recorded location always wins over
// the parent mapping. parent == "" returns r unchanged. parent must
// already have passed CleanTablespaceMapAll.
func (r TablespaceRemap) WithMapAll(parent string, tablespaces []backup.Tablespace) TablespaceRemap {
	if parent == "" {
		return r
	}
	have := make(map[string]struct{}, len(r))
	for _, e := range r {
		have[e.Old] = struct{}{}
	}
	out := append(TablespaceRemap(nil), r...)
	for _, ts := range tablespaces {
		// Same discriminator as tablespaceDestRoots: only a real external
		// tablespace (non-zero OID, absolute location) has anything to move.
		if ts.OID == 0 || !filepath.IsAbs(ts.Location) {
			continue
		}
		if _, explicit := have[ts.Location]; explicit {
			continue
		}
		have[ts.Location] = struct{}{}
		out = append(out, TablespaceRemapEntry{
			Old: ts.Location,
			New: filepath.Join(parent, strconv.FormatUint(uint64(ts.OID), 10)),
		})
	}
	return out
}

// Empty reports whether the remap has no entries.
func (r TablespaceRemap) Empty() bool {
	return len(r) == 0
}

// Apply rewrites a manifest's tablespace_map body, replacing
// every Old path with its New mapping. The manifest's body
// shape is one entry per line:
//
//	<oid> <path>
//
// The OID is left untouched; only the path is replaced. Lines
// without a matching Old path stay verbatim — operators can
// remap a subset of tablespaces without listing every entry.
//
// Empty input or empty receiver returns the input unchanged
// (the caller can use Apply unconditionally).
//
// Whitespace and trailing newlines are preserved so the
// rewritten file is byte-equivalent to the original where no
// path was changed. Unrecognised line shapes (blank lines,
// comments, malformed entries) are passed through untouched
// rather than refused — PG itself won't accept a malformed
// tablespace_map at recovery time, so any garbage we see was
// already there in the manifest.
func (r TablespaceRemap) Apply(body string) string {
	if r.Empty() || body == "" {
		return body
	}
	// Build an old→new lookup once. Order doesn't matter
	// because we forbid duplicate Old paths at parse time.
	lookup := make(map[string]string, len(r))
	for _, e := range r {
		lookup[e.Old] = e.New
	}

	// Process line-by-line so the per-line rewrite preserves
	// the original line endings. We deliberately keep the
	// trailing newline if present; PG's reader is sensitive
	// to file-end shape (it scans line-by-line).
	var out strings.Builder
	out.Grow(len(body))
	lines := strings.SplitAfter(body, "\n")
	for _, line := range lines {
		// Strip the trailing newline for parsing; we re-add
		// it from the original.
		trimmed := strings.TrimRight(line, "\n")
		newline := line[len(trimmed):] // "\n" or "" (last line)

		// Tablespace_map line shape: "<OID> <PATH>".
		// PG splits on the FIRST space; the path may itself
		// contain spaces and is read to end-of-line.
		idx := strings.Index(trimmed, " ")
		if idx <= 0 || idx == len(trimmed)-1 {
			// No space, OID-only, or path-only line —
			// pass through. PG would reject it at recovery
			// either way; not our job to rewrite garbage.
			out.WriteString(line)
			continue
		}
		oid := trimmed[:idx]
		path := trimmed[idx+1:]
		if newPath, ok := lookup[path]; ok {
			out.WriteString(oid)
			out.WriteString(" ")
			out.WriteString(newPath)
			out.WriteString(newline)
			continue
		}
		out.WriteString(line)
	}
	return out.String()
}

// ToCombineArgs converts the remap into pg_combinebackup
// `--tablespace-mapping=OLD=NEW` flags. Returns an empty
// slice for an empty receiver — the caller can append the
// result unconditionally.
//
// Output order matches the receiver's order (which matches
// the operator's argv input). pg_combinebackup processes
// flags left-to-right; first match wins.
func (r TablespaceRemap) ToCombineArgs() []string {
	if r.Empty() {
		return nil
	}
	out := make([]string, 0, len(r))
	for _, e := range r {
		out = append(out, fmt.Sprintf("--tablespace-mapping=%s=%s", e.Old, e.New))
	}
	return out
}

// AppliedPaths returns the New paths the remap touched in
// the order they appear. Used by the result body so the
// operator sees exactly which directories were redirected.
func (r TablespaceRemap) AppliedPaths() []string {
	if r.Empty() {
		return nil
	}
	out := make([]string, 0, len(r))
	for _, e := range r {
		out = append(out, e.New)
	}
	return out
}

// ErrEmptyTablespaceRemap is the sentinel returned when an
// operator passes an empty entry. Tests + the CLI gate on
// it via errors.Is.
var ErrEmptyTablespaceRemap = errors.New("restore: empty tablespace remap entry")
