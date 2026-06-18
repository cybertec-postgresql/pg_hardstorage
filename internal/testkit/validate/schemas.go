// schemas.go — workload-shape generators the orchestrator
// drives via the Schema interface (tpcc-lite, bulk-copy,
// schema-churn).
package validate

import (
	"fmt"
	"math/rand"
	"strings"
)

// Schema names a workload-shape generator the cell runtime
// applies on every iteration.  The schema decides what tables
// exist, how rows are inserted / updated, and how schema-churn
// (DDL) operations are interleaved.
//
// The set is deliberately small in v1: tpcc-lite (mixed
// read/write OLTP), bulk-copy (sequential heavy writes,
// streams a single growing fact table), schema-churn (frequent
// ALTER TABLE on top of a tpcc-lite shape).  Operators
// pick by name in profiles.yaml; the runtime calls
// SchemaSetup once + SchemaIteration each loop.
type Schema interface {
	// Name matches the profiles.yaml `schema:` value.
	Name() string

	// Setup is called once per soak run on first contact.
	// Idempotent: must tolerate re-runs against an already-
	// populated DB.  Returns nil even if the tables already
	// exist (CREATE TABLE IF NOT EXISTS shape).
	Setup(exec ExecFn) error

	// Iteration runs one batch of work.  Returns the
	// approximate bytes of churn it generated for the
	// orchestrator's accounting.
	Iteration(exec ExecFn, rng *rand.Rand, churnMBPerMin int) (int64, error)
}

// ExecFn is what schemas call to issue SQL.  Wraps pgx's
// `Exec` so unit tests can pass a fake.
type ExecFn func(sql string, args ...any) error

// LookupSchema returns the named schema or an error.
func LookupSchema(name string) (Schema, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "tpcc-lite":
		return &tpccLite{}, nil
	case "bulk-copy":
		return &bulkCopy{}, nil
	case "schema-churn":
		return &schemaChurn{}, nil
	}
	return nil, fmt.Errorf("schemas: unknown schema %q (known: tpcc-lite, bulk-copy, schema-churn)", name)
}

// tpccLite — small OLTP shape: customers, orders, order_line.
// Inserts new orders, updates a customer's balance, selects
// a recent order detail.  Mixed read/write; representative of
// most production workloads.
type tpccLite struct{}

// Name returns "tpcc-lite".
func (tpccLite) Name() string { return "tpcc-lite" }

// Setup creates customers / orders / order_line with
// CREATE TABLE IF NOT EXISTS and seeds 100 customers when the
// table is empty.  Idempotent.
func (tpccLite) Setup(exec ExecFn) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS customers (
			c_id BIGSERIAL PRIMARY KEY,
			c_name TEXT NOT NULL,
			c_balance NUMERIC(12,2) NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS orders (
			o_id BIGSERIAL PRIMARY KEY,
			o_customer BIGINT NOT NULL REFERENCES customers(c_id),
			o_amount NUMERIC(12,2) NOT NULL,
			o_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS order_line (
			ol_order BIGINT NOT NULL REFERENCES orders(o_id) ON DELETE CASCADE,
			ol_seq INT NOT NULL,
			ol_item TEXT NOT NULL,
			ol_qty INT NOT NULL,
			PRIMARY KEY (ol_order, ol_seq))`,
		// Seed some customers if empty so the iteration loop
		// has something to refer to.
		`INSERT INTO customers (c_name)
		 SELECT 'cust-' || n FROM generate_series(1, 100) AS n
		 WHERE NOT EXISTS (SELECT 1 FROM customers LIMIT 1)`,
	}
	for _, s := range stmts {
		if err := exec(s); err != nil {
			return fmt.Errorf("tpcc-lite setup: %w", err)
		}
	}
	return nil
}

// Iteration inserts a churn-scaled batch of orders and
// updates a quarter as many customer balances.  Returns the
// approximate byte count for the orchestrator's accounting.
func (tpccLite) Iteration(exec ExecFn, rng *rand.Rand, churnMBPerMin int) (int64, error) {
	// Scale the per-iteration row count by churn rate.  The
	// orchestrator calls Iteration once per loop tick; we
	// don't know the loop frequency here, so this is an
	// approximation: more churn → more rows per iteration.
	rows := 50
	if churnMBPerMin > 0 {
		rows = 25 + churnMBPerMin/4
	}
	if rows > 5000 {
		rows = 5000
	}

	// Insert N orders, each with 1-3 line items.
	for i := 0; i < rows; i++ {
		custID := rng.Intn(100) + 1
		if err := exec(
			`INSERT INTO orders (o_customer, o_amount) VALUES ($1, $2)`,
			custID, rng.Float64()*1000); err != nil {
			return 0, err
		}
	}
	// Update some customer balances.
	for i := 0; i < rows/4; i++ {
		custID := rng.Intn(100) + 1
		if err := exec(
			`UPDATE customers SET c_balance = c_balance + $1 WHERE c_id = $2`,
			rng.Float64()*100, custID); err != nil {
			return 0, err
		}
	}
	// Approximate bytes: ~80 bytes per inserted order +
	// a small constant for the updates.
	return int64(rows*80 + (rows/4)*40), nil
}

// bulkCopy — single growing fact table, 1k row inserts at a
// time.  Models a metrics / log ingestion shape: high write
// volume, no updates, occasional partition rollover.
type bulkCopy struct{}

// Name returns "bulk-copy".
func (bulkCopy) Name() string { return "bulk-copy" }

// Setup creates the facts table with CREATE TABLE IF NOT
// EXISTS.  Idempotent.
func (bulkCopy) Setup(exec ExecFn) error {
	return exec(`CREATE TABLE IF NOT EXISTS facts (
		f_id BIGSERIAL PRIMARY KEY,
		f_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		f_metric TEXT NOT NULL,
		f_value DOUBLE PRECISION NOT NULL,
		f_payload JSONB NOT NULL)`)
}

// Iteration runs a single
// `INSERT ... SELECT generate_series(1, $1)` of churn-scaled
// size — server-side row generation avoids per-row round
// trips.
func (bulkCopy) Iteration(exec ExecFn, rng *rand.Rand, churnMBPerMin int) (int64, error) {
	rows := 1000
	if churnMBPerMin > 0 {
		rows = 500 + churnMBPerMin*4
	}
	if rows > 50000 {
		rows = 50000
	}
	// One INSERT ... SELECT generate_series — server-side row
	// generation is far cheaper than round-tripping.
	if err := exec(
		`INSERT INTO facts (f_metric, f_value, f_payload)
		 SELECT 'metric-' || (n % 10),
		        random() * 1000,
		        jsonb_build_object('seq', n, 'tag', md5(n::text))
		 FROM generate_series(1, $1) AS n`, rows); err != nil {
		return 0, err
	}
	// Approximate bytes: ~150 per row (id + ts + jsonb).
	return int64(rows * 150), nil
}

// schemaChurn — tpcc-lite as the base, plus a periodic
// ALTER TABLE on every iteration.  Catches WAL handling of
// schema changes mid-flight, which is a known correctness
// hazard for backup tools.
type schemaChurn struct {
	tpccLite
	iter int
}

// Name returns "schema-churn".
func (s *schemaChurn) Name() string { return "schema-churn" }

// Iteration runs the tpcc-lite batch and, every fifth call,
// adds a new churn_col_* column on customers (dropping the
// one added 10 iterations ago to keep the table bounded).
func (s *schemaChurn) Iteration(exec ExecFn, rng *rand.Rand, churnMBPerMin int) (int64, error) {
	bytes, err := s.tpccLite.Iteration(exec, rng, churnMBPerMin)
	if err != nil {
		return bytes, err
	}
	s.iter++
	// Every Nth iteration, flip a column on / off.  CREATE +
	// DROP keeps the schema-history nontrivial.
	if s.iter%5 == 0 {
		colName := fmt.Sprintf("churn_col_%d", rng.Intn(1000))
		if err := exec(
			fmt.Sprintf("ALTER TABLE customers ADD COLUMN IF NOT EXISTS %s TEXT",
				colName)); err != nil {
			return bytes, err
		}
		// Drop the column we added two iterations ago to
		// keep the table bounded.
		if s.iter > 10 {
			oldCol := fmt.Sprintf("churn_col_%d", s.iter-10)
			_ = exec(fmt.Sprintf("ALTER TABLE customers DROP COLUMN IF EXISTS %s", oldCol))
		}
	}
	return bytes, nil
}
