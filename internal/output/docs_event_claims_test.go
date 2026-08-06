package output_test

// docs_event_claims_test.go — the events and severities the docs
// promise must be the ones the binary emits.
//
// Third surface in the same family as docs_error_claims_test.go. The
// severity half is the sharpest, because severity is an ORDER, not a
// label: RFC 5424 counts DOWN from emergency (0) to debug (7), so
// "more severe" means a SMALLER number and every threshold comparison
// reads backwards from the English. That inversion is easy to get
// wrong — a `<` written where `<=` belongs, or a filter that keeps the
// chatter and drops the errors — and the mistake is invisible in
// review because both versions look reasonable.
//
// What this pins:
//
//   - the severity table in docs/reference/output-event-schema.md
//     against severityNames, both directions and by value;
//   - the ordering invariant itself, so a renumbering that made
//     emergency the largest value fails here rather than silently
//     inverting every sink's floor;
//   - documented event names, against the (component, op) pairs
//     production code actually emits.
//
// The event-name half is scoped to lines that claim the BINARY emits
// something. That distinction is load-bearing:
// R3-cold-start-from-backups.md says "Append an audit event:
// `restore.cold_start_completed`", which is an instruction to the
// operator to append a custom action with `audit append` — not a claim
// about what we emit. Flagging it would be the guard failing a correct
// page, so lines that tell the reader to append are excluded by rule.

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// eventSchemaDoc is the page whose tables define the wire contract.
const eventSchemaDoc = "docs/reference/output-event-schema.md"

// allSeverities is every severity the package defines, in declaration
// order. Kept as a slice so the ordering test has something to walk.
var allSeverities = []output.Severity{
	output.SeverityEmergency,
	output.SeverityAlert,
	output.SeverityCritical,
	output.SeverityError,
	output.SeverityWarning,
	output.SeverityNotice,
	output.SeverityInfo,
	output.SeverityDebug,
}

// TestSeverityOrderingIsSyslogNotEnglish pins the direction of the
// scale. Nothing in the type system stops someone renumbering these,
// and every severity floor in the codebase and every documented
// "warning+" threshold depends on smaller meaning more severe.
func TestSeverityOrderingIsSyslogNotEnglish(t *testing.T) {
	for i := 1; i < len(allSeverities); i++ {
		if allSeverities[i] <= allSeverities[i-1] {
			t.Fatalf("severity %q (%d) does not sort after %q (%d): the scale must ascend "+
				"from emergency to debug, because every threshold comparison in the tree "+
				"reads `event.Severity <= floor` and would invert if this changed",
				allSeverities[i].String(), int(allSeverities[i]),
				allSeverities[i-1].String(), int(allSeverities[i-1]))
		}
	}
	if output.SeverityEmergency != 0 {
		t.Errorf("emergency is %d, not 0 — RFC 5424 numbering is part of the documented "+
			"wire contract and syslog/CEF consumers depend on it", int(output.SeverityEmergency))
	}
	if output.SeverityError >= output.SeverityWarning {
		t.Errorf("error (%d) must be MORE severe — i.e. numerically smaller — than warning (%d)",
			int(output.SeverityError), int(output.SeverityWarning))
	}
	if output.SeverityDebug != 7 {
		t.Errorf("debug is %d, not 7", int(output.SeverityDebug))
	}
}

// docSeverityRows parses the "| 4 | `warning` | … |" table.
var docSeverityRowRe = regexp.MustCompile("\\|\\s*(\\d)\\s*\\|\\s*`([a-z]+)`\\s*\\|")

// TestSeverityTableMatchesCode runs both directions over the
// documented severity table.
func TestSeverityTableMatchesCode(t *testing.T) {
	root := repoRootFromOutput(t)
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(eventSchemaDoc)))
	if err != nil {
		t.Fatalf("read %s: %v", eventSchemaDoc, err)
	}

	documented := map[int]string{}
	for _, m := range docSeverityRowRe.FindAllStringSubmatch(string(body), -1) {
		n, cerr := strconv.Atoi(m[1])
		if cerr != nil {
			continue
		}
		documented[n] = m[2]
	}
	if len(documented) == 0 {
		t.Fatalf("parsed zero severity rows from %s — the table's shape changed and this "+
			"test is no longer reading it", eventSchemaDoc)
	}

	// code → docs
	for _, s := range allSeverities {
		name, ok := documented[int(s)]
		if !ok {
			t.Errorf("severity %d (%q) has no row in %s", int(s), s.String(), eventSchemaDoc)
			continue
		}
		if name != s.String() {
			t.Errorf("severity %d: docs call it %q, the binary emits %q in `severity_name`",
				int(s), name, s.String())
		}
	}
	// docs → code
	known := map[int]bool{}
	for _, s := range allSeverities {
		known[int(s)] = true
	}
	for n, name := range documented {
		if !known[n] {
			t.Errorf("%s documents severity %d (%q), which the binary has no constant for",
				eventSchemaDoc, n, name)
		}
	}
}

// TestEventFieldWireTypesMatchTheDoc pins the TYPE column of the
// `Event` table against a real marshalled event.
//
// TestSeverityTableMatchesCode above checks the severity VALUE table
// and passed happily while the field table said `severity` was an
// `int8` carrying "RFC 5424 numeric severity (0 = emergency, 7 =
// debug)". It is not: Severity implements MarshalText, so encoding/json
// writes `{"severity": "warning"}` and never `{"severity": 4}`. The
// page also described `severity_name` as mirroring it "for human
// consumers", which only reads sensibly if `severity` were the number —
// on the wire the two fields are byte-identical.
//
// A consumer written against that table and comparing `severity <= 4`
// gets a type error in a strict language and a silently false
// comparison in a loose one. Checking value tables is not enough; the
// types have to be pinned to what marshals.
func TestEventFieldWireTypesMatchTheDoc(t *testing.T) {
	root := repoRootFromOutput(t)
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(eventSchemaDoc)))
	if err != nil {
		t.Fatalf("read %s: %v", eventSchemaDoc, err)
	}

	// The page documents several structs, each under its own `## `
	// heading, and `severity` appears in BOTH the Event and Error
	// tables. A flat field→type map silently lets one table's row
	// overwrite the other's — the first version of this test read the
	// Error row and reported it against the Event fixture. Sections are
	// kept separate.
	sections := map[string]string{}
	var cur string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "## ") {
			cur = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "## ")), "`")
			continue
		}
		if cur != "" {
			sections[cur] += line + "\n"
		}
	}

	rowRe := regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|\\s*([^|]+?)\\s*\\|")
	fieldsUnder := func(heading string) map[string]string {
		out := map[string]string{}
		for _, m := range rowRe.FindAllStringSubmatch(sections[heading], -1) {
			out[m[1]] = strings.ToLower(strings.TrimSpace(m[2]))
		}
		return out
	}

	// Each documented struct, with a fixture populated enough that
	// omitempty does not hide the fields under test.
	fixtures := []struct {
		heading string
		value   any
	}{
		{"Event", output.NewEvent(output.SeverityWarning, "backup", "started").
			WithSubject(output.Subject{Deployment: "db1"}).
			WithBody(map[string]any{"k": "v"}).
			WithSuggestion(&output.Suggestion{Human: "do the thing"})},
		{"Error", output.NewError("notfound.repo", "no repository there").
			WithSeverity(output.SeverityError).
			WithSuggestion(&output.Suggestion{Human: "create it"})},
		{"Subject", output.Subject{Tenant: "t", Deployment: "db1", BackupID: "b1",
			Timeline: 3, LSN: "0/1000000"}},
		{"Suggestion", output.Suggestion{Human: "h", Command: "c", DocURL: "u"}},
	}

	// documented type → the JSON kinds that satisfy it.
	kindOf := func(v stdjson.RawMessage) string {
		s := strings.TrimSpace(string(v))
		switch {
		case s == "" || s == "null":
			return "null"
		case strings.HasPrefix(s, `"`):
			return "string"
		case strings.HasPrefix(s, "{"):
			return "object"
		case strings.HasPrefix(s, "["):
			return "array"
		case s == "true" || s == "false":
			return "bool"
		default:
			return "number"
		}
	}
	satisfies := func(doc, got string) bool {
		switch {
		case strings.Contains(doc, "any"):
			return true
		case strings.Contains(doc, "timestamp"), strings.Contains(doc, "string"):
			return got == "string"
		case strings.Contains(doc, "object"):
			return got == "object"
		case strings.Contains(doc, "int"), strings.Contains(doc, "uint"),
			strings.Contains(doc, "float"), strings.Contains(doc, "number"):
			return got == "number"
		case strings.Contains(doc, "bool"):
			return got == "bool"
		}
		return true // a type this test does not model; do not guess
	}

	checked := 0
	for _, f := range fixtures {
		documented := fieldsUnder(f.heading)
		if len(documented) == 0 {
			t.Errorf("no field rows parsed under the `%s` heading in %s — either the "+
				"section was renamed or its table shape changed, and it is now unchecked",
				f.heading, eventSchemaDoc)
			continue
		}
		raw, merr := stdjson.Marshal(f.value)
		if merr != nil {
			t.Errorf("marshal %s fixture: %v", f.heading, merr)
			continue
		}
		var wire map[string]stdjson.RawMessage
		if uerr := stdjson.Unmarshal(raw, &wire); uerr != nil {
			t.Errorf("%s does not marshal to a JSON object: %v", f.heading, uerr)
			continue
		}
		for field, docType := range documented {
			v, present := wire[field]
			if !present {
				continue // omitempty and not set in this fixture
			}
			checked++
			if got := kindOf(v); !satisfies(docType, got) {
				t.Errorf("%s.`%s` is documented as %q but marshals as a JSON %s (%s).\n"+
					"A consumer written against this table gets the wrong type — for "+
					"`severity` specifically, a numeric comparison against a name never "+
					"matches and never errors either.",
					f.heading, field, docType, got, strings.TrimSpace(string(v)))
			}
		}
	}
	if checked < 10 {
		t.Fatalf("compared only %d fields against the wire — the fixtures stopped populating "+
			"their structs, so this test asserts almost nothing", checked)
	}
	t.Logf("compared %d documented field type(s) across %d structs against the wire",
		checked, len(fixtures))
}

// ---------------------------------------------------------------
// Event names
// ---------------------------------------------------------------

var (
	// NewEvent(SeverityX, "component", "op") — tolerant of wrapping.
	newEventRe = regexp.MustCompile(
		`NewEvent\(\s*(?:output\.)?Severity([A-Za-z]+)\s*,\s*"([a-z0-9_.]+)"\s*,\s*"([a-z0-9_.]+)"`)
	// audit.Event{Action: "…"}
	auditActionRe = regexp.MustCompile(`Action:\s*"([a-z0-9_.]+)"`)

	// A line claiming the binary emits something.
	emitCueRe = regexp.MustCompile(`(?i)\b(emits?|emitted|logs include)\b`)
	// …unless it is telling the operator to write one themselves.
	appendCueRe = regexp.MustCompile(`(?i)\b(append|appends|appending)\b`)

	docEventTokenRe = regexp.MustCompile("`([a-z_][a-z0-9_]*\\.[a-z0-9_.]+)`")
	nonEventSuffix  = regexp.MustCompile(`\.(md|go|yaml|yml|json|bin|sh|txt|v1|conf|log)$`)
)

// emittedEvents returns "component.op" → severity name, plus the raw
// production source text for existence fallbacks.
func emittedEvents(t *testing.T, root string) (map[string]string, map[string]bool, string) {
	t.Helper()
	events := map[string]string{}
	actions := map[string]bool{}
	var all strings.Builder

	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top),
			func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
					strings.HasSuffix(path, "_test.go") {
					return nil
				}
				src, rerr := os.ReadFile(path)
				if rerr != nil {
					return nil
				}
				text := string(src)
				all.WriteString(text)
				all.WriteByte('\n')
				for _, m := range newEventRe.FindAllStringSubmatch(text, -1) {
					events[m[2]+"."+m[3]] = strings.ToLower(m[1])
				}
				for _, m := range auditActionRe.FindAllStringSubmatch(text, -1) {
					actions[m[1]] = true
				}
				return nil
			})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return events, actions, all.String()
}

// TestDocumentedEventNamesExist checks the names the prose says we
// emit.
//
// Existence is satisfied three ways, because one event legitimately
// has three spellings in the tree: a full "component.op" name (which
// never appears as a single literal — the two halves are separate
// arguments), an audit action, or a bare op naming its own emission
// site. Anything matching none of those is a name the binary cannot
// produce.
func TestDocumentedEventNamesExist(t *testing.T) {
	root := repoRootFromOutput(t)
	events, actions, sources := emittedEvents(t, root)
	if len(events) < 50 {
		t.Fatalf("extracted only %d events from NewEvent call sites — the scan stopped "+
			"matching, so this test asserts nothing", len(events))
	}

	checked := 0
	var bad []string
	seen := map[string]bool{}
	walkDocFiles(t, root, func(rel string, lines []string) {
		for _, pfx := range docsClaimExemptPrefixes {
			if strings.HasPrefix(rel, pfx) {
				return
			}
		}
		for i, line := range lines {
			if !emitCueRe.MatchString(line) || appendCueRe.MatchString(line) {
				continue
			}
			for _, m := range docEventTokenRe.FindAllStringSubmatch(line, -1) {
				tok := m[1]
				if nonEventSuffix.MatchString(tok) {
					continue
				}
				checked++
				if _, ok := events[tok]; ok {
					continue
				}
				if actions[tok] || strings.Contains(sources, tok) || seen[tok] {
					continue
				}
				seen[tok] = true
				bad = append(bad, rel+":"+strconv.Itoa(i+1)+"\n      `"+tok+
					"` is not an event the binary emits, an audit action, or any string in "+
					"the production sources")
			}
		}
	})

	if checked == 0 {
		t.Fatal("matched no event names in the docs — the cue or token regex stopped " +
			"matching, so this test asserts nothing")
	}
	t.Logf("checked %d documented event name(s) against %d emitted events and %d audit actions",
		checked, len(events), len(actions))

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d documented event name(s) are never emitted:\n  %s\n\n"+
			"Operators build alerts and log filters on these strings. A name that cannot "+
			"occur is a filter that never matches — which reads exactly like a healthy "+
			"system. Name the event that actually fires, or say the surface is not shipped "+
			"yet rather than describing it in the present tense.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestDocumentedEventSeveritiesMatch checks a severity named alongside
// an event. Getting this wrong points an operator at the wrong alarm:
// R6-slot-dropped-gap.md described the WAL-gap signal as a `notice`
// when the event that carries a non-zero gap is `critical` — the
// no-gap reconcile is the notice.
func TestDocumentedEventSeveritiesMatch(t *testing.T) {
	root := repoRootFromOutput(t)
	events, _, _ := emittedEvents(t, root)

	// The severity must be marked up as one — in backticks, or named
	// by an explicit "at <sev> severity" phrase.
	//
	// A bare word will not do. Every severity name is also an ordinary
	// English word, and the docs use them as such constantly: the
	// patroni tutorial writes "`patroni.poll_error` on a transient REST
	// error", where the trailing "error" describes the cause, not the
	// level. Matching that reported a correct page as wrong. Requiring
	// the markup costs coverage on sloppily-written claims and buys the
	// guarantee that a flag is always a real claim.
	const sevAlt = "emergency|alert|critical|error|warning|notice|info|debug"
	sevWord := regexp.MustCompile(
		"(?i)(?:`(" + sevAlt + ")`" +
			"|\\bat (" + sevAlt + ") severity\\b" +
			// Bare word, but only as the first thing after the event
			// name — "`wal.follower.slot_reconciled` notice", the shape
			// R6 used. Anything further away is prose about the cause.
			"|\\A[\\s,]*(?:is |was |an? |at )?(" + sevAlt + ")\\b)")

	checked := 0
	var bad []string
	walkDocFiles(t, root, func(rel string, lines []string) {
		for _, pfx := range docsClaimExemptPrefixes {
			if strings.HasPrefix(rel, pfx) {
				return
			}
		}
		for i, line := range lines {
			// Prose wraps, and the severity routinely lands on the next
			// line: R6 reads "…`wal.follower.wal_gap_detected` event\n
			// at `critical`". A line-at-a-time scan silently skips those
			// — it did not even cover the fix that prompted this test —
			// so the window runs one line past the match.
			window := line
			if i+1 < len(lines) {
				window += " " + strings.TrimSpace(lines[i+1])
			}
			for _, loc := range docEventTokenRe.FindAllStringSubmatchIndex(line, -1) {
				tok := line[loc[2]:loc[3]]
				want, ok := events[tok]
				if !ok {
					continue
				}
				// Only judge a severity word close enough to be
				// describing this event.
				tail := window[loc[1]:]
				if len(tail) > 64 {
					tail = tail[:64]
				}
				m := sevWord.FindStringSubmatch(tail)
				if m == nil {
					continue
				}
				claimed := ""
				for _, g := range m[1:] {
					if g != "" {
						claimed = strings.ToLower(g)
						break
					}
				}
				if claimed == "" {
					continue
				}
				checked++
				if claimed == want {
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(i+1)+"\n      `"+tok+
					"` is described as `"+claimed+"` but is emitted at `"+want+"`")
			}
		}
	})

	t.Logf("checked %d documented event-severity claim(s)", checked)
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d documented event severity/severities disagree with the emitter:\n  %s\n\n"+
			"Sinks filter on severity, so this decides whether the event reaches a pager at "+
			"all. Pointing an operator at a `notice` for a condition we raise as `critical` "+
			"tells them to look where nothing will be.",
			len(bad), strings.Join(bad, "\n  "))
	}
}
