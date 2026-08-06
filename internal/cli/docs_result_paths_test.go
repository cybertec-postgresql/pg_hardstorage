package cli

// docs_result_paths_test.go — a documented `.result.<path>` must
// resolve against the result body of the command it is shown with.
//
// docs_jq_paths_test.go closed two thirds of this surface: the phantom
// `body` level beneath the envelope root, and a leaf name that no
// struct in the tree marshals. What it explicitly could not do is
// attribute a path to a command, so a field that is real on `list` but
// absent from `show` passed.
//
// (Spelling that phantom level out literally here would trip the very
// guard this paragraph describes — it scans Go sources too. Wording
// around it beats adding this file to an exemption list that would
// then have to be maintained.)
//
// That gap is not hypothetical. `audit search` was documented as
// emitting `.subject` — a field of the domain audit.Event, but NOT of
// auditSearchRow, which is what the command returns (it flattens the
// subject into deployment / backup_id / tenant). The name exists
// somewhere in the tree, so the weak check waved it through, and an
// operator following the HIPAA breach-window page got null.
//
// This test lives in package cli rather than cli_test so it can name
// the unexported body types directly. The table below is the only
// hand-maintained part, and it is hand-maintained in the way that
// matters least:
//
//   - it names TYPES, not field lists, so a field added to or removed
//     from a body is picked up with no edit here;
//   - renaming a body type breaks compilation;
//   - a documented jq path on a command with no entry FAILS with "add
//     it to the table", so the table cannot silently fall behind the
//     docs.
//
// Coverage is honest rather than total: an expression that computes
// (`map`, `select`, arithmetic) is checked as far as its dotted path
// prefix, and anything below a map[string]any or an interface field is
// unverifiable by construction and stops the walk rather than guessing.

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// documentedResultBodies maps a command path to the body its Result
// carries. Extend it when a new command picks up a documented jq
// example — TestEveryDocumentedResultPathIsAttributable says so by
// name.
var documentedResultBodies = map[string]any{
	"audit search":      auditSearchBody{},
	"capacity report":   capacityReportBody{},
	"compliance report": complianceReportBody{},
	"cost report":       costReportBody{},
	"deployment list":   deploymentListBody{},
	"kms rotate":        kmsRotateBody{},
	"list":              listBody{},
	"repo usage":        repoUsageBody{},
	// `manifest show` is documented as "the same operation as the
	// top-level show" and shares its body type.
	"manifest show": showBody{},
	"show":          showBody{},
	"slo report":    sloReportBody{},
	"wal list":      walListBody{},
}

// dpExemptPrefixes are pages that describe past releases. A changelog
// entry quoting the output of an older version is a historical record,
// not a promise about the current binary; validating it against
// today's structs would be the guard calling a truthful page a liar.
var dpExemptPrefixes = []string{
	"docs/changelog.md",
	"docs/release-notes/",
}

// ---------------------------------------------------------------
// Type walking
// ---------------------------------------------------------------

// dpJSONFields returns the JSON key → field type map for a struct,
// flattening embedded structs the way encoding/json does.
func dpJSONFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	t = dpDeref(t)
	if t.Kind() != reflect.Struct {
		return out
	}
	// Embedded first, so an outer field of the same name overwrites the
	// promoted one — which is the precedence encoding/json applies.
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous {
			continue
		}
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" {
			continue // tagged embedded field is a normal nested object
		}
		for k, v := range dpJSONFields(f.Type) {
			out[k] = v
		}
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || f.PkgPath != "" {
			continue // explicitly skipped, or unexported
		}
		if f.Anonymous && name == "" {
			continue // handled above
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func dpDeref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// dpOpaque reports whether a type's interior cannot be checked: a map
// body or an `any`.
func dpOpaque(t reflect.Type) bool {
	switch dpDeref(t).Kind() {
	case reflect.Map, reflect.Interface:
		return true
	}
	return false
}

// dpResolve walks segments (a dotted path, with "[]" as its own
// segment) from a body type. It returns the type reached, or an
// explanation of where the path left the schema.
func dpResolve(body reflect.Type, segs []string) (reflect.Type, string) {
	cur := body
	for i, seg := range segs {
		if dpOpaque(cur) {
			return nil, "" // unverifiable below here; not a failure
		}
		if seg == "[]" {
			if k := dpDeref(cur).Kind(); k != reflect.Slice && k != reflect.Array {
				return nil, "`" + strings.Join(segs[:i], ".") + "` is " + k.String() +
					", not a list, so `[]` cannot apply"
			}
			cur = dpDeref(cur).Elem()
			continue
		}
		fields := dpJSONFields(cur)
		next, ok := fields[seg]
		if !ok {
			return nil, "`" + seg + "` is not a field of " + dpTypeName(cur) +
				"; it has: " + dpKeyList(fields)
		}
		cur = next
	}
	return cur, ""
}

func dpTypeName(t reflect.Type) string {
	t = dpDeref(t)
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}

func dpKeyList(m map[string]reflect.Type) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) > 12 {
		return strings.Join(ks[:12], ", ") + ", … (" + strconv.Itoa(len(ks)) + " total)"
	}
	return strings.Join(ks, ", ")
}

// ---------------------------------------------------------------
// Doc scanning
// ---------------------------------------------------------------

var (
	dpCmdRe   = regexp.MustCompile(`pg_hardstorage((?:\s+[a-z][a-z0-9-]*)+)`)
	dpPathRe  = regexp.MustCompile(`(\.\[([0-9]+)\])?\.result((?:\.[a-z][a-z0-9_]*|\[\])+)`)
	dpProjRe  = regexp.MustCompile(`\|\s*\{([^}]*)\}`)
	dpIdentRe = regexp.MustCompile(`[a-z][a-z0-9_]*`)

	// A block may redirect a command's output to a file and slurp
	// several of them back with `jq -s`, in which case `.[0]` is not
	// the nearest preceding command — it is whichever command wrote
	// the first file jq was handed. docs/operations/cost-reporting.md
	// reconciles `repo usage` against `cost report` exactly this way,
	// and attributing both paths to the nearer command reported a
	// correct page as broken. The block says which command wrote which
	// file; use that rather than guessing from proximity.
	dpRedirectRe = regexp.MustCompile(`>\s*([A-Za-z0-9_.-]+\.json)`)
	dpJSONArgRe  = regexp.MustCompile(`\b([A-Za-z0-9_-]+\.json)\b`)
)

// dpDocPath is one documented path with the command it was shown under.
type dpDocPath struct {
	cmd   string
	raw   string
	segs  []string
	proj  []string // {a, b} projection applied to the element, if any
	where string
}

// dpParseSegs turns ".foo.bar[].baz" into [foo bar [] baz].
func dpParseSegs(path string) []string {
	var segs []string
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		n := 0
		for strings.HasSuffix(part, "[]") {
			part = strings.TrimSuffix(part, "[]")
			n++
		}
		if part != "" {
			segs = append(segs, part)
		}
		for i := 0; i < n; i++ {
			segs = append(segs, "[]")
		}
	}
	return segs
}

// dpSlurpArgs returns the .json file arguments handed to a jq
// invocation starting at line idx, following backslash continuations.
// Their order is jq's input order, so args[N] is what `.[N]` selects.
func dpSlurpArgs(all []string, idx int) []string {
	var files []string
	for j := idx; j < len(all); j++ {
		for _, m := range dpJSONArgRe.FindAllStringSubmatch(all[j], -1) {
			files = append(files, m[1])
		}
		if !strings.HasSuffix(strings.TrimSpace(all[j]), `\`) {
			break
		}
	}
	return files
}

func dpRepoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

// dpCollect walks the docs and pairs each `.result…` with the nearest
// preceding pg_hardstorage invocation inside the same fenced block.
func dpCollect(t *testing.T, resolve func(string) string) []dpDocPath {
	t.Helper()
	root := dpRepoRoot(t)
	var out []dpDocPath
	files := 0
	err := filepath.Walk(filepath.Join(root, "docs"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, pfx := range dpExemptPrefixes {
			if strings.HasPrefix(rel, pfx) {
				return nil
			}
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		files++
		srcLines := strings.Split(string(src), "\n")
		inBlock, cur := false, ""
		wroteFile := map[string]string{} // output file → command that wrote it
		for i, line := range srcLines {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inBlock = !inBlock
				// Reset on BOTH transitions. Carrying a command across a
				// block boundary would attribute a path mentioned in
				// prose to whatever invocation happened to appear last,
				// which is how a guard invents a failure on a page that
				// is fine.
				cur = ""
				wroteFile = map[string]string{}
				continue
			}
			if !inBlock {
				continue
			}
			if m := dpCmdRe.FindStringSubmatch(line); m != nil {
				cur = resolve(strings.Join(strings.Fields(m[1]), " "))
			}
			if m := dpRedirectRe.FindStringSubmatch(line); m != nil && cur != "" {
				wroteFile[m[1]] = cur
			}
			for _, pm := range dpPathRe.FindAllStringSubmatch(line, -1) {
				owner := cur
				if pm[2] != "" {
					// `.[N].result…` — the Nth slurped input.
					owner = ""
					if n, err := strconv.Atoi(pm[2]); err == nil {
						if args := dpSlurpArgs(srcLines, i); n < len(args) {
							owner = wroteFile[args[n]]
						}
					}
				}
				dp := dpDocPath{
					cmd:   owner,
					raw:   pm[0],
					segs:  dpParseSegs(pm[3]),
					where: rel + ":" + strconv.Itoa(i+1),
				}
				if proj := dpProjRe.FindStringSubmatch(line); proj != nil {
					dp.proj = dpIdentRe.FindAllString(proj[1], -1)
				}
				out = append(out, dp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if files == 0 {
		t.Fatal("read no markdown files — every assertion below would hold vacuously")
	}
	return out
}

// dpCommandPaths enumerates every command path under the real root,
// relative to it ("audit search", not "pg_hardstorage audit search").
func dpCommandPaths(c *cobra.Command, prefix string, out map[string]bool) {
	for _, sub := range c.Commands() {
		if sub.Hidden {
			continue
		}
		path := strings.TrimSpace(prefix + " " + sub.Name())
		out[path] = true
		dpCommandPaths(sub, path, out)
	}
}

// dpResolver maps a documented word sequence to the longest real
// command path it starts with.
func dpResolver(t *testing.T) func(string) string {
	t.Helper()
	paths := map[string]bool{}
	dpCommandPaths(NewRoot(), "", paths)
	if len(paths) == 0 {
		t.Fatal("built no command paths from NewRoot() — attribution would be vacuous")
	}
	return func(words string) string {
		f := strings.Fields(words)
		for i := len(f); i > 0; i-- {
			if cand := strings.Join(f[:i], " "); paths[cand] {
				return cand
			}
		}
		return ""
	}
}

// ---------------------------------------------------------------
// The guards
// ---------------------------------------------------------------

// TestDocumentedResultPathsResolve is the guard this file exists for.
func TestDocumentedResultPathsResolve(t *testing.T) {
	paths := dpCollect(t, dpResolver(t))

	checked := 0
	var bad []string
	for _, dp := range paths {
		body, ok := documentedResultBodies[dp.cmd]
		if dp.cmd == "" || !ok {
			continue // unattributed, or reported by the table test below
		}
		checked++
		typ, why := dpResolve(reflect.TypeOf(body), dp.segs)
		if why != "" {
			bad = append(bad, dp.where+"\n      `"+dp.raw+"` on `"+dp.cmd+"` — "+why)
			continue
		}
		if typ == nil || len(dp.proj) == 0 || dpOpaque(typ) {
			continue
		}
		// `.result.events[] | {a, b}` — the projection names fields of
		// the element type.
		fields := dpJSONFields(typ)
		if len(fields) == 0 {
			continue
		}
		for _, name := range dp.proj {
			if _, ok := fields[name]; !ok {
				bad = append(bad, dp.where+"\n      projection `"+name+"` on `"+dp.cmd+
					"` — not a field of "+dpTypeName(typ)+"; it has: "+dpKeyList(fields))
			}
		}
	}

	if checked == 0 {
		t.Fatal("resolved no documented paths against a known body — the guard asserts nothing")
	}
	t.Logf("checked %d documented result path(s) against %d command bodies",
		checked, len(documentedResultBodies))

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d documented output path(s) do not resolve against the result body of the "+
			"command they are shown with:\n  %s\n\nThe leaf-existence check in "+
			"docs_jq_paths_test.go cannot catch these — the field name is real somewhere in "+
			"the tree, just not on this command. An operator running the page gets null, and "+
			"for `jq -e` gates and `--template` that failure is silent.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestEveryDocumentedResultPathIsAttributable keeps the table honest:
// a jq example on a command with no entry is a gap in coverage, and a
// silent gap is how a guard stops guarding.
func TestEveryDocumentedResultPathIsAttributable(t *testing.T) {
	paths := dpCollect(t, dpResolver(t))

	missing := map[string][]string{}
	for _, dp := range paths {
		if dp.cmd == "" {
			continue
		}
		if _, ok := documentedResultBodies[dp.cmd]; !ok {
			missing[dp.cmd] = append(missing[dp.cmd], dp.where)
		}
	}
	if len(missing) == 0 {
		return
	}
	cmds := make([]string, 0, len(missing))
	for c := range missing {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	var lines []string
	for _, c := range cmds {
		sort.Strings(missing[c])
		lines = append(lines, "`"+c+"`  (first at "+missing[c][0]+")")
	}
	t.Errorf("%d command(s) have a documented `.result.<path>` but no entry in "+
		"documentedResultBodies:\n  %s\n\nAdd the command's body type to the table in "+
		"docs_result_paths_test.go. Without an entry the path goes unchecked, and an "+
		"unchecked path is exactly what let `audit search` be documented as emitting "+
		"`.subject` when auditSearchRow has no such field.",
		len(missing), strings.Join(lines, "\n  "))
}

// TestResultBodyReflectionMatchesMarshal defends the premise the whole
// file rests on: that walking a body's struct tags describes what the
// command actually emits.
//
// Three of the tabled types — complianceReportBody, costReportBody,
// capacityReportBody — define MarshalJSON. Each currently forwards to
// an embedded Report, so the emitted keys match the promoted ones and
// reflection is right. Nothing enforced that. A marshaller that
// renamed, wrapped or dropped a key would leave the guard checking
// documented paths against a shape no operator ever sees, and it would
// still be green.
//
// So: fill a body with non-zero values, marshal it for real, and
// require the top-level keys to be exactly what reflection predicted.
func TestResultBodyReflectionMatchesMarshal(t *testing.T) {
	for _, cmd := range dpSortedKeys(documentedResultBodies) {
		body := documentedResultBodies[cmd]
		t.Run(cmd, func(t *testing.T) {
			typ := reflect.TypeOf(body)
			filled := dpFill(reflect.New(typ).Elem(), 0)
			raw, err := stdjson.Marshal(filled.Interface())
			if err != nil {
				t.Fatalf("marshal %s: %v", dpTypeName(typ), err)
			}
			var got map[string]stdjson.RawMessage
			if err := stdjson.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s marshals to something that is not a JSON object: %v",
					dpTypeName(typ), err)
			}
			want := dpJSONFields(typ)

			var missing, extra []string
			for k := range want {
				if _, ok := got[k]; !ok {
					missing = append(missing, k)
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					extra = append(extra, k)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			if len(missing) > 0 || len(extra) > 0 {
				t.Errorf("reflection over %s disagrees with what it marshals to.\n"+
					"  predicted but not emitted: %s\n  emitted but not predicted: %s\n\n"+
					"Documented paths are checked against the predicted shape, so while these "+
					"disagree the guard is validating against a body no operator receives.",
					dpTypeName(typ), dpOrNone(missing), dpOrNone(extra))
			}
		})
	}
}

func dpSortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func dpOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

// dpFill sets every exported field to a non-zero value so omitempty
// cannot hide it from the marshalled output. Depth-limited: the shape
// only needs to be populated far enough for the TOP-LEVEL keys to
// appear.
func dpFill(v reflect.Value, depth int) reflect.Value {
	if depth > 6 || !v.CanSet() {
		return v
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		dpFill(v.Elem(), depth+1)
	case reflect.Struct:
		// time.Time and friends marshal from unexported state; setting
		// their fields is neither possible nor needed.
		if v.NumField() > 0 && v.Type().Field(0).PkgPath != "" {
			return v
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported
			}
			dpFill(v.Field(i), depth+1)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		dpFill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		dpFill(key, depth+1)
		val := reflect.New(v.Type().Elem()).Elem()
		dpFill(val, depth+1)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
	return v
}

// TestDocumentedResultBodiesAreRealCommands stops the table itself
// from rotting: an entry naming a command that no longer exists would
// silently stop checking anything.
func TestDocumentedResultBodiesAreRealCommands(t *testing.T) {
	paths := map[string]bool{}
	dpCommandPaths(NewRoot(), "", paths)
	var gone []string
	for cmd := range documentedResultBodies {
		if !paths[cmd] {
			gone = append(gone, cmd)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		t.Errorf("documentedResultBodies names %d command(s) that do not exist: %s\n\n"+
			"The entry checks nothing — no documented path can attribute to it.",
			len(gone), strings.Join(gone, ", "))
	}
}
