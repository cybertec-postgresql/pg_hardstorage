// Package docs is the LLM helper's bundled documentation corpus.
//
// What's bundled:
//
//   - The disaster-recovery runbooks R1-R7 and the control-plane
//     setup guide (every markdown file under docs/runbooks/).
//   - The repository-root README.md (operator-facing intro).
//   - The CHANGELOG.md (so the assistant knows what's recent — a
//     surprising amount of useful context lives here, especially
//     for "why does this command exist?" questions).
//
// What's NOT bundled:
//
//   - The full architectural plan (~30 KLOC of internal design
//     notes).  Including the plan would dominate the LLM's
//     context and bias the assistant toward over-explaining
//     internal architecture instead of helping the operator.
//     A future version can opt-in via a privacy-aware lookup
//     when the operator explicitly asks "explain the
//     architecture".
//   - License files / build configs / etc.
//
// The corpus is loaded once at startup via go:embed.  search_docs
// and list_docs are the tool surfaces the assistant uses to find
// relevant content; the on-startup bootstrap surfaces the
// runbook index in the system prompt so the LLM knows what's
// available without having to call list first.
package docs

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed runbooks/*.md root/CHANGELOG.md root/README.md
var corpus embed.FS

// Doc is one entry in the bundled corpus.
type Doc struct {
	// ID is the stable lookup key.  For runbooks this is the
	// canonical "R1", "R2", ...; for the CHANGELOG it's
	// "CHANGELOG"; for README it's "README".
	ID string

	// Path is the embedded file's path within the corpus FS.
	// Surfaced for traceability — operators inspecting
	// search_docs results see exactly which file the snippet
	// came from.
	Path string

	// Title is the operator-facing label.  For runbooks this is
	// the H1 of the markdown body; for the CHANGELOG / README
	// it's the document name.
	Title string

	// Body is the full markdown content.
	Body string
}

// All returns every loaded doc in stable (ID-sorted) order.
func All() ([]Doc, error) {
	var out []Doc
	err := fs.WalkDir(corpus, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, rerr := corpus.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, Doc{
			ID:    deriveID(p),
			Path:  p,
			Title: deriveTitle(p, body),
			Body:  string(body),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("docs: walk corpus: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the doc with the given ID.  Lookup is exact-match;
// list IDs via All when an operator-friendly ID set is needed.
func Get(id string) (Doc, error) {
	all, err := All()
	if err != nil {
		return Doc{}, err
	}
	want := strings.ToUpper(strings.TrimSpace(id))
	for _, d := range all {
		if strings.EqualFold(d.ID, want) {
			return d, nil
		}
	}
	return Doc{}, fmt.Errorf("docs: %q not found (try one of: %s)", id, strings.Join(allIDs(all), ", "))
}

// Search returns Match entries for every doc whose body contains
// query (case-insensitive).  Each Match carries up to three
// excerpts (each ~240 chars) around the first three hit positions
// — enough for the LLM to decide whether to drill into the full
// body without flooding the context.
func Search(query string) ([]Match, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("docs: query is required")
	}
	all, err := All()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var matches []Match
	for _, d := range all {
		body := strings.ToLower(d.Body)
		if !strings.Contains(body, q) {
			continue
		}
		matches = append(matches, Match{
			Doc:      d,
			Excerpts: extractExcerpts(d.Body, body, q, maxExcerptsPerDoc, excerptRadius),
		})
	}
	return matches, nil
}

// Match is one search hit.
type Match struct {
	Doc      Doc      `json:"doc"`
	Excerpts []string `json:"excerpts"`
}

const (
	maxExcerptsPerDoc = 3
	excerptRadius     = 120 // chars around each hit position
)

// extractExcerpts pulls up to maxN excerpts of width 2*radius+len(q)
// chars centred on the first maxN occurrences of q within
// lowerBody.  Hits are taken from the lower-cased body but
// excerpts are clipped from the original (case-preserving).
func extractExcerpts(orig, lowerBody, q string, maxN, radius int) []string {
	var out []string
	from := 0
	for len(out) < maxN {
		idx := strings.Index(lowerBody[from:], q)
		if idx < 0 {
			break
		}
		hit := from + idx
		start := hit - radius
		if start < 0 {
			start = 0
		}
		end := hit + len(q) + radius
		if end > len(orig) {
			end = len(orig)
		}
		excerpt := strings.TrimSpace(orig[start:end])
		// Mark with leading/trailing ellipsis when we clipped.
		if start > 0 {
			excerpt = "…" + excerpt
		}
		if end < len(orig) {
			excerpt = excerpt + "…"
		}
		out = append(out, excerpt)
		from = hit + len(q)
	}
	return out
}

// deriveID maps a corpus path to the operator-facing ID.
//
//	runbooks/R3-cold-start-from-backups.md -> R3
//	root/CHANGELOG.md                       -> CHANGELOG
//	root/README.md                          -> README
func deriveID(p string) string {
	base := path.Base(p)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if strings.HasPrefix(p, "runbooks/") {
		// Runbooks: take the leading token before the first dash.
		if dash := strings.Index(stem, "-"); dash > 0 {
			return strings.ToUpper(stem[:dash])
		}
		return strings.ToUpper(stem)
	}
	return strings.ToUpper(stem)
}

// deriveTitle returns a human-readable label.  For markdown bodies
// we look for the first H1 (`# ...`); fall back to the file's
// stem if none found.
func deriveTitle(p string, body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

func allIDs(docs []Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out
}

// IndexEntry is one row of the runbook index: a stable ID plus a
// human-readable title, returned in the order the underlying docs
// store lists them.
type IndexEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// RunbookIndex returns a compact (id, title) listing the LLM helper
// includes in every system prompt. Letting the assistant know
// "R6 is for slot-dropped + gap detected" up front means it does
// not have to call list_docs / read_runbook on simple questions.
func RunbookIndex() ([]IndexEntry, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	var out []IndexEntry
	for _, d := range all {
		if strings.HasPrefix(d.ID, "R") && len(d.ID) >= 2 && d.ID[1] >= '0' && d.ID[1] <= '9' {
			out = append(out, IndexEntry{ID: d.ID, Title: d.Title})
		}
	}
	return out, nil
}
