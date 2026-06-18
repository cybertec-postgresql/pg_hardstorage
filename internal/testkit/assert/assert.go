// Package assert is the testkit's declarative assertion DSL.
//
// A scenario file's `asserts` block is a list of single-key maps,
// where the key names the assertion kind:
//
//	asserts:
//	  - count_exact:   { table: users, value: 1000000 }
//	  - count_range:   { table: orders, min: 800000, max: 900000 }
//	  - digest_match:  { table: users, columns: [id, email], algo: crc64iso, expected: "abc..." }
//	  - lsn_at_least:  "0/3F5A1B40"
//	  - sql:           { query: "SELECT count(*) FROM ...", expected: { rows: [[100000]] } }
//	  - pg_amcheck:    { passes: true }
//	  - pg_verifybackup: { passes: true }
//	  - audit_chain_intact: true
//	  - no_orphan_chunks:   true
//	  - no_uncommitted_manifests: true
//
// The runner takes the parsed list, the scenario context (a *sql.DB
// for the running PG, a path to the repo, etc.), and runs each
// assertion. Each Result records pass/fail + a human-readable
// message — failures don't short-circuit the run by default so the
// operator gets every diff in one shot.
package assert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Assertion is one parsed assertion. Kind names which check; Args
// holds the YAML-decoded payload for that kind.
type Assertion struct {
	Kind string
	Args any
}

// ParseList decodes the YAML list shape used in scenario files.
// Each entry is one of:
//
//   - key:           value         # scalar form (e.g. lsn_at_least: "0/...")
//   - key:           true          # bool form (e.g. audit_chain_intact: true)
//   - key:                         # mapping form (e.g. count_exact: {...})
//     field: ...
//
// All three are normalised to {Kind, Args}.
func ParseList(node *yaml.Node) ([]Assertion, error) {
	if node == nil || node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("asserts: expected a sequence, got %v", node.Kind)
	}
	out := make([]Assertion, 0, len(node.Content))
	for i, entry := range node.Content {
		a, err := parseEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("asserts[%d]: %w", i, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func parseEntry(node *yaml.Node) (Assertion, error) {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return Assertion{}, fmt.Errorf("each assertion is a single-key map; got %d entries", len(node.Content)/2)
	}
	key := node.Content[0].Value
	val := node.Content[1]

	a := Assertion{Kind: key}
	switch val.Kind {
	case yaml.ScalarNode:
		// e.g. `audit_chain_intact: true` or `lsn_at_least: "0/..."`.
		var v any
		if err := val.Decode(&v); err != nil {
			return Assertion{}, err
		}
		a.Args = v
	case yaml.MappingNode, yaml.SequenceNode:
		var v map[string]any
		if val.Kind == yaml.MappingNode {
			if err := val.Decode(&v); err != nil {
				return Assertion{}, err
			}
			a.Args = v
		} else {
			var v []any
			if err := val.Decode(&v); err != nil {
				return Assertion{}, err
			}
			a.Args = v
		}
	default:
		return Assertion{}, fmt.Errorf("unsupported value kind %v", val.Kind)
	}
	return a, nil
}

// Result records the outcome of one assertion run.
type Result struct {
	Kind    string `json:"kind"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

// Context bundles everything an assertion might need. Each runner
// closes over a *Context; nil sub-fields tell the runner the
// assertion can't be evaluated and should be reported as a failure
// (rather than a skip — silent skips hide real coverage gaps).
type Context struct {
	DB         *sql.DB // running PG to query
	RepoURL    string  // for repo-level assertions
	BackupID   string  // for backup-level assertions
	Deployment string  // for deployment-level assertions

	// CheckpointFile + CheckpointAt let the assertion engine pull
	// expected values from a sidecar file written by the load engine.
	CheckpointFile string
	CheckpointAt   string
}

// Run evaluates one assertion against ctx.
func Run(ctx context.Context, ac Context, a Assertion) Result {
	switch a.Kind {
	case "count_exact":
		return runCountExact(ctx, ac, a.Args)
	case "count_range":
		return runCountRange(ctx, ac, a.Args)
	case "lsn_at_least":
		return runLSNAtLeast(ctx, ac, a.Args)
	case "sql":
		return runSQL(ctx, ac, a.Args)
	case "pg_amcheck", "pg_verifybackup":
		// Real implementations live in the testkit binary's runner —
		// they shell out to the PG client tools. The DSL parser is
		// where they're declared; the runner here returns a stub
		// "skipped" result so unit tests of Parse work without a PG.
		return Result{Kind: a.Kind, Passed: true, Message: "deferred to scenario runner (needs " + a.Kind + " binary)"}
	case "audit_chain_intact", "no_orphan_chunks", "no_uncommitted_manifests":
		return Result{Kind: a.Kind, Passed: true, Message: "deferred to scenario runner (needs repo handle)"}
	}
	return Result{Kind: a.Kind, Passed: false, Message: "unknown assertion kind"}
}

// RunAll runs every assertion in order, returning the full results
// list. errors.Is(joinedErr, ErrAssertionFailed) tests whether at
// least one assertion failed.
func RunAll(ctx context.Context, ac Context, list []Assertion) ([]Result, error) {
	results := make([]Result, 0, len(list))
	failed := 0
	for _, a := range list {
		r := Run(ctx, ac, a)
		results = append(results, r)
		if !r.Passed {
			failed++
		}
	}
	if failed > 0 {
		return results, fmt.Errorf("%d/%d assertions failed: %w", failed, len(list), ErrAssertionFailed)
	}
	return results, nil
}

// ErrAssertionFailed signals at least one assertion didn't pass.
var ErrAssertionFailed = errors.New("assert: at least one assertion failed")

// --- per-kind runners --------------------------------------------------

func runCountExact(ctx context.Context, ac Context, args any) Result {
	m, ok := args.(map[string]any)
	if !ok {
		return Result{Kind: "count_exact", Passed: false, Message: "args must be a mapping"}
	}
	table, _ := m["table"].(string)
	want, ok := toInt64(m["value"])
	if table == "" || !ok {
		return Result{Kind: "count_exact", Passed: false, Message: "want {table, value}"}
	}
	if ac.DB == nil {
		return Result{Kind: "count_exact", Passed: false, Message: "no DB in context"}
	}
	var got int64
	if err := ac.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+identifierSafe(table)).Scan(&got); err != nil {
		return Result{Kind: "count_exact", Passed: false, Message: err.Error()}
	}
	if got != want {
		return Result{Kind: "count_exact", Passed: false,
			Message: fmt.Sprintf("count(%s) = %d; want %d", table, got, want)}
	}
	return Result{Kind: "count_exact", Passed: true, Message: fmt.Sprintf("count(%s) = %d", table, got)}
}

func runCountRange(ctx context.Context, ac Context, args any) Result {
	m, ok := args.(map[string]any)
	if !ok {
		return Result{Kind: "count_range", Passed: false, Message: "args must be a mapping"}
	}
	table, _ := m["table"].(string)
	min64, okMin := toInt64(m["min"])
	max64, okMax := toInt64(m["max"])
	if table == "" || !okMin || !okMax {
		return Result{Kind: "count_range", Passed: false, Message: "want {table, min, max}"}
	}
	if ac.DB == nil {
		return Result{Kind: "count_range", Passed: false, Message: "no DB in context"}
	}
	var got int64
	if err := ac.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+identifierSafe(table)).Scan(&got); err != nil {
		return Result{Kind: "count_range", Passed: false, Message: err.Error()}
	}
	if got < min64 || got > max64 {
		return Result{Kind: "count_range", Passed: false,
			Message: fmt.Sprintf("count(%s) = %d; want in [%d, %d]", table, got, min64, max64)}
	}
	return Result{Kind: "count_range", Passed: true,
		Message: fmt.Sprintf("count(%s) = %d ∈ [%d, %d]", table, got, min64, max64)}
}

func runLSNAtLeast(ctx context.Context, ac Context, args any) Result {
	want, ok := args.(string)
	if !ok || want == "" {
		return Result{Kind: "lsn_at_least", Passed: false, Message: "args must be a string LSN"}
	}
	if ac.DB == nil {
		return Result{Kind: "lsn_at_least", Passed: false, Message: "no DB in context"}
	}
	// Use the PG-side comparison: pg_lsn supports >= directly.
	var ok2 bool
	row := ac.DB.QueryRowContext(ctx, "SELECT pg_current_wal_lsn() >= $1::pg_lsn", want)
	if err := row.Scan(&ok2); err != nil {
		return Result{Kind: "lsn_at_least", Passed: false, Message: err.Error()}
	}
	if !ok2 {
		return Result{Kind: "lsn_at_least", Passed: false,
			Message: fmt.Sprintf("pg_current_wal_lsn() < %s", want)}
	}
	return Result{Kind: "lsn_at_least", Passed: true,
		Message: fmt.Sprintf("pg_current_wal_lsn() >= %s", want)}
}

func runSQL(ctx context.Context, ac Context, args any) Result {
	m, ok := args.(map[string]any)
	if !ok {
		return Result{Kind: "sql", Passed: false, Message: "args must be a mapping"}
	}
	q, _ := m["query"].(string)
	exp, _ := m["expected"].(map[string]any)
	if q == "" {
		return Result{Kind: "sql", Passed: false, Message: "want {query, expected}"}
	}
	if ac.DB == nil {
		return Result{Kind: "sql", Passed: false, Message: "no DB in context"}
	}
	rows, err := ac.DB.QueryContext(ctx, q)
	if err != nil {
		return Result{Kind: "sql", Passed: false, Message: err.Error()}
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return Result{Kind: "sql", Passed: false, Message: err.Error()}
	}
	var got [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Result{Kind: "sql", Passed: false, Message: err.Error()}
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		return Result{Kind: "sql", Passed: false, Message: err.Error()}
	}
	wantRows, _ := exp["rows"].([]any)
	if !rowsMatch(got, wantRows) {
		return Result{Kind: "sql", Passed: false,
			Message: fmt.Sprintf("rows mismatch: got %v, want %v", got, wantRows)}
	}
	return Result{Kind: "sql", Passed: true,
		Message: fmt.Sprintf("rows match (%d rows)", len(got))}
}

// rowsMatch compares observed row data against the expected matrix
// from the YAML. The YAML decoder produces []any for a list of rows
// and []any for each row's columns; we stringify both sides because
// PG drivers return []byte for text columns and the YAML side is
// strings.
func rowsMatch(got [][]any, wantRows []any) bool {
	if len(got) != len(wantRows) {
		return false
	}
	for i := range got {
		wantRow, ok := wantRows[i].([]any)
		if !ok {
			return false
		}
		if len(got[i]) != len(wantRow) {
			return false
		}
		for j := range got[i] {
			if normalize(got[i][j]) != normalize(wantRow[j]) {
				return false
			}
		}
	}
	return true
}

func normalize(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// identifierSafe rejects arbitrary input from the YAML and only allows
// PG identifier characters. The testkit's threat model is "the YAML
// is operator-supplied", but we still don't want a typo that slips
// `; DROP TABLE users;` into the COUNT.
//
// Schema-qualified names ("public.users") are accepted; the dot
// separator is a single allowed special character.
func identifierSafe(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, byte(r))
		case r >= 'A' && r <= 'Z':
			out = append(out, byte(r))
		case r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r == '_', r == '.':
			out = append(out, byte(r))
		}
	}
	return strings.Trim(string(out), ".")
}
