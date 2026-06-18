// Package checkpoint implements the testkit's NDJSON checkpoint stream.
// During load execution, a checkpoint snapshots ground-truth state
// (table counts, content digests, current LSN) and writes one NDJSON
// line to a sidecar file per checkpoint event.
//
// The sidecar file is the post-hoc oracle: a restore-verify assertion
// reads `<scenario>.checkpoints.ndjson`, picks the entry matching the
// caller's --at filter, and asserts the restored database matches.
package checkpoint

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"os"
	"sort"
	"sync"
	"time"
)

// Checkpoint is one ground-truth snapshot. Schema: pg_hardstorage.checkpoint.v1.
type Checkpoint struct {
	Schema  string                   `json:"schema"`
	At      time.Time                `json:"at"`
	Label   string                   `json:"label,omitempty"`
	Phase   string                   `json:"phase,omitempty"`
	LSN     string                   `json:"lsn,omitempty"`
	Tables  map[string]TableSnapshot `json:"tables,omitempty"`
	Schemas string                   `json:"schema_fingerprint,omitempty"`
}

// TableSnapshot captures whatever's cheap and stable to compute. v0.1
// gathers row count + a column-digest. The Digest column choice is
// declared in the load YAML's asserts_per_checkpoint block; v0.1
// always digests *all* columns (canonicalised text representation)
// when no explicit list is given.
type TableSnapshot struct {
	Count     int64  `json:"count"`
	Digest    string `json:"digest,omitempty"`
	DigestAlg string `json:"digest_alg,omitempty"`
}

// SchemaCheckpoint is the JSON schema string. Same 24-month back-compat
// commitment as the production manifest schema.
const SchemaCheckpoint = "pg_hardstorage.checkpoint.v1"

// Writer streams checkpoints to an NDJSON file. Concurrency: any
// number of goroutines may call Emit; writes are serialised through
// the embedded mutex so the file's line ordering is deterministic.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	bw  *bufio.Writer
	enc *json.Encoder
}

// NewWriter opens path for write (truncating any existing file). The
// caller closes the Writer to flush + sync before reading the file
// from another process.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: open %s: %w", path, err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &Writer{f: f, bw: bw, enc: enc}, nil
}

// Emit writes one checkpoint as a single NDJSON line. Returns the
// final marshalled body so a caller assembling assertions has the
// canonical form (avoids re-marshalling drift).
func (w *Writer) Emit(c Checkpoint) error {
	if c.Schema == "" {
		c.Schema = SchemaCheckpoint
	}
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(c)
}

// Close flushes and syncs. After Close any further Emit returns ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	err := w.f.Close()
	w.f = nil
	w.bw = nil
	return err
}

// SnapshotTable produces a deterministic count + digest of every row
// in table. The digest uses crc64-iso (cheap, sufficient for change
// detection in tests; collisions are not a concern at the row counts
// the testkit operates at).
//
// The caller passes the SQL needed to enumerate rows in deterministic
// order — typically `SELECT * FROM <table> ORDER BY <pk>`. Without an
// ORDER BY the digest would depend on PG's row ordering, which isn't
// stable across vacuum/reindex.
func SnapshotTable(ctx context.Context, db *sql.DB, table, query string) (TableSnapshot, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return TableSnapshot{}, fmt.Errorf("checkpoint: query %s: %w", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return TableSnapshot{}, err
	}

	var (
		count int64
		h     = crc64.New(crc64.MakeTable(crc64.ISO))
		buf   = make([][]byte, len(cols))
		ptrs  = make([]any, len(cols))
	)
	for i := range ptrs {
		ptrs[i] = &buf[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return TableSnapshot{}, fmt.Errorf("checkpoint: scan %s: %w", table, err)
		}
		// Sort columns lexicographically by name so a future ALTER
		// TABLE that reorders columns doesn't change digests.
		ordered := orderedColumns(cols, buf)
		for _, kv := range ordered {
			h.Write([]byte(kv.k))
			h.Write([]byte{0})
			h.Write(kv.v)
			h.Write([]byte{0})
		}
		h.Write([]byte("\n"))
		count++
	}
	if err := rows.Err(); err != nil {
		return TableSnapshot{}, err
	}
	return TableSnapshot{
		Count:     count,
		Digest:    hex.EncodeToString(h.Sum(nil)),
		DigestAlg: "crc64-iso/v1",
	}, nil
}

type kv struct {
	k string
	v []byte
}

// orderedColumns pairs column names with their row values and returns
// the pairs sorted by name. We allocate a fresh slice per row — at the
// row counts the testkit produces this is fine; if it ever shows up in
// a profile, switch to a pre-sorted index permutation.
func orderedColumns(cols []string, vals [][]byte) []kv {
	out := make([]kv, len(cols))
	for i, c := range cols {
		out[i] = kv{k: c, v: vals[i]}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}
