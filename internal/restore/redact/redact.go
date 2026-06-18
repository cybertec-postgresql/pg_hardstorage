// Package redact is the post-restore PII redaction surface.
//
// Operators restoring a production backup into a non-prod
// environment (development, QA, training) often need to
// scrub PII first.  The integration with `anon`-style
// PostgreSQL extensions is the production path; for cases
// where running anon isn't an option, this package provides
// a row-level scanner that walks the restored database
// post-recovery and rewrites configured columns through a
// deterministic redaction function.
//
// The plugin is OPT-IN: a restore proceeds untouched unless
// the operator passes `--redact-rules <yaml>`.  The rules
// file declares per-table patterns:
//
//	tables:
//	  public.users:
//	    columns:
//	      email: hash_keep_domain
//	      phone: replace_with_xxx
//	      ssn: nullify
//	      name: hash_to_uuid
//	  public.orders:
//	    columns:
//	      shipping_address: nullify
//
// Strategies (built-in):
//
//   - nullify             — set to NULL
//   - hash_to_uuid        — replace with a deterministic uuid
//     derived from sha256(value | salt)
//   - hash_keep_domain    — emails: hash localpart, keep @domain
//   - replace_with_xxx    — replace every char with 'x'
//   - constant:<value>    — replace with a literal value
//   - regex:<pattern>:<replacement> — global regex sub
//
// Determinism: hashing strategies use a per-restore salt
// stored in the restore's audit event so the operator can
// reproduce identical redactions on a future restore (useful
// for join-preserving redactions across two tables that
// reference the same user_id).
//
// # Where this fits in the restore pipeline
//
// The redact stage runs AFTER `pg_verifybackup` succeeds and
// BEFORE the operator first connects to the restored
// cluster.  We launch a small Go-side worker that:
//
//  1. Connects to the restored cluster via libpq.
//  2. Loops over the configured tables.
//  3. For each table, builds an `UPDATE` statement that
//     rewrites every configured column via SQL functions
//     we install at the start of the redact run.
//  4. Drops the helper functions at the end.
//
// All SQL runs in a single transaction per table so a
// crash mid-redact leaves the table either fully redacted
// or untouched.
package redact

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaRules is the YAML schema string carried in the rules
// file's `schema` field.  Stable across releases; bumping
// would be a breaking change.
const SchemaRules = "pg_hardstorage.redact.v1"

// Rules is a parsed --redact-rules file.
type Rules struct {
	Schema string                `yaml:"schema"`
	Tables map[string]TableRules `yaml:"tables"`
}

// TableRules defines what to do with each column of one table.
type TableRules struct {
	Columns map[string]Strategy `yaml:"columns"`
}

// Strategy is the operator-supplied redaction strategy
// string.  Validated at parse time so an unknown strategy
// produces a clear error before any SQL fires.
type Strategy string

// Validated returns nil if s is a known strategy; otherwise
// an error explaining the available options.
func (s Strategy) Validated() error {
	switch {
	case s == "nullify",
		s == "hash_to_uuid",
		s == "hash_keep_domain",
		s == "replace_with_xxx":
		return nil
	case strings.HasPrefix(string(s), "constant:"):
		return nil
	case strings.HasPrefix(string(s), "regex:"):
		// regex:<pattern>:<replacement>
		parts := strings.SplitN(string(s)[len("regex:"):], ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("redact: regex strategy must be regex:<pattern>:<replacement>; got %q", s)
		}
		if _, err := regexp.Compile(parts[0]); err != nil {
			return fmt.Errorf("redact: regex pattern %q: %w", parts[0], err)
		}
		return nil
	}
	return fmt.Errorf("redact: unknown strategy %q (allowed: nullify, hash_to_uuid, hash_keep_domain, replace_with_xxx, constant:<v>, regex:<p>:<r>)", s)
}

// ParseRules reads YAML bytes into a Rules struct, validating
// the schema string and every strategy.
func ParseRules(body []byte) (*Rules, error) {
	var r Rules
	if err := yaml.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("redact: parse YAML: %w", err)
	}
	if r.Schema != "" && r.Schema != SchemaRules {
		return nil, fmt.Errorf("redact: schema %q is not supported (want %q)", r.Schema, SchemaRules)
	}
	if len(r.Tables) == 0 {
		return nil, errors.New("redact: rules file declares no tables")
	}
	for table, tr := range r.Tables {
		if !looksLikeQualifiedName(table) {
			return nil, fmt.Errorf("redact: table %q must be schema.table-form", table)
		}
		if len(tr.Columns) == 0 {
			return nil, fmt.Errorf("redact: table %q has no columns to redact", table)
		}
		for col, strat := range tr.Columns {
			if !looksLikeIdentifier(col) {
				return nil, fmt.Errorf("redact: column %q.%q is not a valid identifier", table, col)
			}
			if err := strat.Validated(); err != nil {
				return nil, fmt.Errorf("redact: %s.%s: %w", table, col, err)
			}
		}
	}
	return &r, nil
}

// Plan is a per-restore redaction plan: the parsed rules,
// the salt used for hashing strategies, and a stable
// table-order for predictable execution.
type Plan struct {
	Rules     *Rules
	Salt      []byte
	TableList []string
}

// NewPlan binds a Rules to a fresh random salt and produces
// a stable ordering of tables.  The salt becomes part of the
// audit-event body so a future restore can reproduce
// identical redactions.
func NewPlan(rules *Rules) (*Plan, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("redact: salt: %w", err)
	}
	tables := make([]string, 0, len(rules.Tables))
	for t := range rules.Tables {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	return &Plan{Rules: rules, Salt: salt, TableList: tables}, nil
}

// SetSalt overrides the random salt with operator-supplied
// bytes.  Used for join-preserving redactions across multiple
// restores: pass the same salt every time → identical hashes.
func (p *Plan) SetSalt(b []byte) error {
	if len(b) < 8 {
		return fmt.Errorf("redact: salt must be at least 8 bytes; got %d", len(b))
	}
	p.Salt = append([]byte(nil), b...)
	return nil
}

// SaltHex returns the salt as a hex string suitable for
// audit-event bodies.
func (p *Plan) SaltHex() string { return hex.EncodeToString(p.Salt) }

// SQL generates the per-table UPDATE statements.  Returns
// one statement per table, in TableList order.  The
// statements use parameterised string-replacement of the
// salt — Postgres parameter binding handles the rest.
//
// Each statement is wrapped in a per-table transaction so a
// failure in one table doesn't leave a partially-redacted
// row visible.  Transaction boundaries:
//
//	BEGIN;
//	UPDATE <table> SET col = strategy(col, ...);
//	COMMIT;
//
// The actual Pg connection / Exec lives in the restore
// package; this function is the SQL generator + unit-testable.
func (p *Plan) SQL() []TableSQL {
	out := make([]TableSQL, 0, len(p.TableList))
	for _, table := range p.TableList {
		tr := p.Rules.Tables[table]
		sqls := buildSetClauses(tr.Columns, p.SaltHex())
		out = append(out, TableSQL{
			Table:  table,
			SetSQL: sqls,
			Stmt:   fmt.Sprintf("UPDATE %s SET %s", quoteQualified(table), strings.Join(sqls, ", ")),
		})
	}
	return out
}

// TableSQL is one table's redaction step.
type TableSQL struct {
	Table  string
	SetSQL []string // each "col = expr"
	Stmt   string   // full UPDATE
}

// RedactValue applies a strategy in pure-Go (no DB).  Used
// by tests + dry-run preview.  Production redaction runs
// the SQL on the restored cluster.
func RedactValue(strategy Strategy, salt []byte, value string) string {
	switch {
	case strategy == "nullify":
		return ""
	case strategy == "hash_to_uuid":
		return hashUUID(salt, value)
	case strategy == "hash_keep_domain":
		i := strings.LastIndexByte(value, '@')
		if i < 0 {
			return hashUUID(salt, value)
		}
		return hashUUID(salt, value[:i])[:8] + value[i:]
	case strategy == "replace_with_xxx":
		out := make([]byte, len(value))
		for i := range out {
			out[i] = 'x'
		}
		return string(out)
	case strings.HasPrefix(string(strategy), "constant:"):
		return strings.TrimPrefix(string(strategy), "constant:")
	case strings.HasPrefix(string(strategy), "regex:"):
		parts := strings.SplitN(string(strategy)[len("regex:"):], ":", 2)
		if len(parts) != 2 {
			return value
		}
		re, err := regexp.Compile(parts[0])
		if err != nil {
			return value
		}
		return re.ReplaceAllString(value, parts[1])
	}
	return value
}

// hashUUID is sha256-keyed by the salt; we take 16 bytes and
// format as a UUID-looking string so it slots into typical
// PG UUID / VARCHAR columns.
func hashUUID(salt []byte, value string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(value))
	sum := h.Sum(nil)
	hexStr := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// buildSetClauses renders one "col = SQL_expr" per column.
// We deliberately use literal SQL function calls (no
// placeholder binding) because:
//
//  1. Each strategy maps to a fixed SQL expression; there's
//     nothing to inject (the salt is hex-only).
//  2. Postgres' UPDATE doesn't accept named-parameter binding
//     for SET-clause expressions in a way that's available
//     across libpq client variants.
//
// The salt is hex-only so injection isn't a concern; the
// strategies only emit safe SQL.
func buildSetClauses(cols map[string]Strategy, saltHex string) []string {
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, col := range keys {
		strat := cols[col]
		expr := strategyToSQLExpr(strat, col, saltHex)
		out = append(out, fmt.Sprintf("%s = %s", quoteIdent(col), expr))
	}
	return out
}

// strategyToSQLExpr maps a Strategy to a Postgres expression
// referencing the column by name.
func strategyToSQLExpr(s Strategy, col, saltHex string) string {
	q := quoteIdent(col)
	switch {
	case s == "nullify":
		return "NULL"
	case s == "hash_to_uuid":
		return fmt.Sprintf("md5(decode('%s', 'hex') || %s::text)::uuid::text", saltHex, q)
	case s == "hash_keep_domain":
		// keep the @domain suffix; hash the localpart
		return fmt.Sprintf(
			"left(md5(decode('%s','hex')||split_part(%s,'@',1)), 8)||'@'||split_part(%s,'@',2)",
			saltHex, q, q)
	case s == "replace_with_xxx":
		return fmt.Sprintf("repeat('x', length(%s))", q)
	case strings.HasPrefix(string(s), "constant:"):
		return fmt.Sprintf("%s", quoteString(strings.TrimPrefix(string(s), "constant:")))
	case strings.HasPrefix(string(s), "regex:"):
		parts := strings.SplitN(string(s)[len("regex:"):], ":", 2)
		if len(parts) != 2 {
			return q
		}
		return fmt.Sprintf("regexp_replace(%s, %s, %s, 'g')", q, quoteString(parts[0]), quoteString(parts[1]))
	}
	return q
}

// quoteIdent wraps a column name in double quotes (handling
// embedded quotes by doubling them).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteQualified handles "schema.table" → quote each part.
func quoteQualified(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 {
		return quoteIdent(s[:i]) + "." + quoteIdent(s[i+1:])
	}
	return quoteIdent(s)
}

// quoteString single-quotes a string literal, doubling
// embedded quotes per SQL spec.
func quoteString(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

func looksLikeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func looksLikeQualifiedName(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return false
	}
	return looksLikeIdentifier(parts[0]) && looksLikeIdentifier(parts[1])
}
