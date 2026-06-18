// Package basebackup issues BASE_BACKUP against a replication-protocol
// connection and drives the resulting message stream through a
// caller-supplied Sink.
//
// Status: scope is bounded to the protocol drive — tablespace tar
// streams are surfaced as opaque byte chunks via Sink. Tar parsing,
// FileEntry iteration, and chunker / CAS / manifest wiring land in
// Slice 6c, where this package's output is the input.
//
// Reliability: every documented failure mode of streaming.Reader
// propagates here (ctx cancel, server error, premature EOF, inactivity
// timeout) plus our own protocol-shape mismatches and Sink errors.
//
// PostgreSQL 15+ wire format for BASE_BACKUP, in order:
//
//	(1) RowDescription, DataRow, CommandComplete  — start LSN result
//	    (2 cols: recptr text, tli int8)
//	(2) RowDescription, DataRow×N                 — tablespace list
//	    (3 cols: spcoid oid, spclocation text, size int8)
//	(3) CommandComplete                           — "SELECT"
//	(4) CopyOutResponse                           — single multiplexed
//	    copy stream covering ALL archives + manifest
//	(5) CopyData×M  — type-byte multiplexed:
//	      'n' new archive  (archive_name + tablespace_path, null-term)
//	      'd' archive/manifest data bytes
//	      'p' progress     (int64 bytes_done, network byte order)
//	      'm' manifest start (signals end of last archive)
//	(6) CopyDone
//	(7) RowDescription, DataRow, CommandComplete  — stop LSN result
//	(8) CommandComplete                           — "BASE_BACKUP"
//	(9) ReadyForQuery
//
// Source: src/backend/backup/basebackup_copy.c (bbsink_copystream_*).
// PG 15 replaced the per-tablespace CopyOut sequence with this
// multiplexed single-stream form.  We commit to PG 15+ (PG 14 and
// earlier are EOL across our test matrix and the legacy non-
// parenthesised BASE_BACKUP shape is already gone in 18 — see
// an earlier change), so the older format is intentionally not handled.
//
// We expect ErrorResponse at any phase: streaming.Reader surfaces it as
// a typed *streaming.ServerError before the next message would arrive.
package basebackup

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// Options drive BASE_BACKUP. Defaults are tuned for the common case:
// MANIFEST on, PROGRESS off, 90 s inactivity timeout (PG can pause
// during the initial checkpoint, especially on a busy server).
//
// FAST checkpoint policy: this package ALWAYS emits FAST on the wire
// (see buildQuery).  An operational backup tool that defers to PG's
// spread checkpoint can sit idle for many minutes waiting for the
// next scheduled checkpoint to arrive — that's never the right
// behaviour when a human or scheduler asked us to take a backup
// NOW.  The Options.Fast field is retained for source-compat with
// callers but is a no-op; FAST is unconditional.
type Options struct {
	// Label is what PG uses for backup_label. Required.
	Label string
	// Deprecated: Fast is now unconditional (we always emit FAST on
	// the wire — see buildQuery).  Setting this field has no effect;
	// it remains for source-compat with existing callers.
	Fast bool
	// Manifest enables PG's MANIFEST option. Defaults to true.
	// Setting to false skips manifest emission and saves a CopyOut.
	Manifest bool
	// IncludeWAL controls whether the WAL files needed to make the
	// backup self-restorable are included in the stream (the PG
	// "WAL" option). Defaults to false because we ingest WAL
	// separately via START_REPLICATION.
	IncludeWAL bool

	// IncrementalManifest, when non-empty, requests an incremental
	// backup against the prior manifest's content. PG 17+ only —
	// the server must have `summarize_wal = on` set, AND the
	// referenced WAL must still be summarised (the prior summary
	// hasn't aged out of `pg_wal/summaries/`).
	//
	// Format: the raw bytes of the prior backup's pg_basebackup-
	// emitted JSON manifest, exactly as PG returned them via the
	// MANIFEST CopyOut. We surface them as a Go []byte rather than
	// a path so callers can fetch from any source (a prior backup's
	// manifest in the repo, a side-band file, etc.) without
	// constraining the storage path.
	//
	// Wire shape: BASE_BACKUP INCREMENTAL '<manifest-bytes>' ...
	// — the manifest is embedded as a single-quoted argument with
	// SQL-style quote escaping (single quotes doubled).
	IncrementalManifest []byte

	// InactivityTimeout overrides streaming.Reader's default. Zero
	// uses the package default (90 s).
	InactivityTimeout time.Duration
}

const defaultInactivityTimeout = 90 * time.Second

// Sink receives the streaming output of a BASE_BACKUP run.
//
// The contract: per-tablespace, OnTablespaceStart fires once, then
// OnTablespaceData fires zero-or-more times with the bytes of that
// tablespace's tar archive in order, then OnTablespaceEnd fires once.
// The same trio runs for the optional manifest blob (idx == -1, info
// fields zero) when Options.Manifest is true.
//
// Returning a non-nil error from any callback aborts the run; Run
// returns the wrapped error after issuing a polite cleanup. Callbacks
// run on Run's goroutine — they must not block indefinitely.
type Sink interface {
	OnTablespaceStart(idx int, info TablespaceInfo) error
	OnTablespaceData(idx int, data []byte) error
	OnTablespaceEnd(idx int) error
}

// TablespaceInfo describes one tablespace as PG reports it in the
// BASE_BACKUP header.  The default tablespace has OID = 0 and an
// empty Location (PG omits both — the data directory is implied).
//
// PG's BASE_BACKUP wire format has shipped exactly three columns
// since at least PG 13: spcoid (OID), spclocation (text), size
// (int8).  No `name` column has ever been part of the protocol;
// our earlier struct invented one.
type TablespaceInfo struct {
	OID      uint32
	Location string
	SizeKiB  int64
}

// Result is the structured outcome of a successful Run.
type Result struct {
	StartLSN      string
	StartTimeline uint32
	StopLSN       string
	StopTimeline  uint32
	Tablespaces   []TablespaceInfo
	ManifestBytes []byte // populated iff Options.Manifest is true
	Stats         streaming.Stats
	StartedAt     time.Time
	StoppedAt     time.Time
}

// ManifestSinkIndex is the Sink-callback "idx" reserved for the
// optional backup-manifest CopyOut sequence. Tablespace indices are
// >= 0; the manifest uses -1 to keep the type uncomplicated.
const ManifestSinkIndex = -1

// Run issues BASE_BACKUP on c (which must already be in replication
// mode) and drives the protocol response through sink. The Result
// captures start/stop LSN, tablespace metadata, and (if requested) the
// PG-emitted backup manifest bytes.
//
// Run takes ownership of c via streaming.Reader's Hijack — the *pg.Conn
// must not be used afterwards. Run closes the reader (and the
// underlying TCP) before returning, regardless of outcome.
func Run(ctx context.Context, c *pg.Conn, opts Options, sink Sink) (*Result, error) {
	if c == nil {
		return nil, errors.New("basebackup: nil connection")
	}
	if c.Mode() != pg.ModeReplication {
		return nil, fmt.Errorf("basebackup: connection must be in replication mode; got %s", c.Mode())
	}
	if strings.TrimSpace(opts.Label) == "" {
		return nil, errors.New("basebackup: Options.Label is required")
	}
	if sink == nil {
		return nil, errors.New("basebackup: nil Sink")
	}

	timeout := opts.InactivityTimeout
	if timeout == 0 {
		timeout = defaultInactivityTimeout
	}
	reader, err := streaming.New(ctx, c.PgConn(), streaming.Options{
		InactivityTimeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("basebackup: streaming.New: %w", err)
	}
	defer reader.Close()

	// PG 17+ incremental backup is a TWO-stage wire dance.
	//
	// Stage 1 (when IncrementalManifest is set):
	//   1. Client sends `UPLOAD_MANIFEST` query.
	//   2. Server replies `CopyInResponse`.
	//   3. Client writes the manifest bytes as one or more
	//      `CopyData` messages, then `CopyDone`.
	//   4. Server replies `CommandComplete("UPLOAD_MANIFEST")`
	//      then `ReadyForQuery`.
	//
	// Stage 2: BASE_BACKUP with the INCREMENTAL option (a
	// BOOLEAN flag in PG 17's grammar — NOT a place to inline
	// the manifest body).  The server uses the manifest that
	// was uploaded in stage 1.
	//
	// History: an earlier version of this code tried to inline
	// the manifest as the INCREMENTAL option's argument
	// (`INCREMENTAL '<manifest-bytes>'`), which PG 17 rejects
	// with `42601 incremental requires a Boolean value` — the
	// options parser treats INCREMENTAL as a boolean and won't
	// accept a multi-KB JSON blob as the value.  Reference:
	// https://www.postgresql.org/docs/17/protocol-replication.html#PROTOCOL-REPLICATION-UPLOAD-MANIFEST
	if len(opts.IncrementalManifest) > 0 {
		if err := uploadIncrementalManifest(ctx, reader, opts.IncrementalManifest); err != nil {
			return nil, fmt.Errorf("basebackup: UPLOAD_MANIFEST: %w", err)
		}
	}

	// Stage 2: send the BASE_BACKUP command.  Replication-protocol
	// commands are carried in standard Query messages; the server
	// dispatches based on the per-connection replication=database
	// flag.
	queryStr := buildQuery(opts)
	if err := reader.Send(&pgproto3.Query{String: queryStr}); err != nil {
		return nil, fmt.Errorf("basebackup: send command: %w", err)
	}

	res := &Result{StartedAt: time.Now().UTC()}
	if err := drive(ctx, reader, opts, sink, res); err != nil {
		return res, err
	}
	res.StoppedAt = time.Now().UTC()
	res.Stats = reader.Stats()
	return res, nil
}

// drive runs the BASE_BACKUP protocol state machine over reader.
// Documented states map directly to the wire-format phases above.
func drive(ctx context.Context, reader *streaming.Reader, opts Options, sink Sink, res *Result) error {
	// Phase 1: start LSN result set.
	startLSN, startTLI, err := readLSNResult(ctx, reader, "start LSN")
	if err != nil {
		return err
	}
	res.StartLSN = startLSN
	res.StartTimeline = startTLI

	// Phase 2 + 3: tablespace list (RowDescription + DataRow×N) followed
	// by an explicit CommandComplete from bbsink_copystream_begin_backup.
	if err := expectMessage[*pgproto3.RowDescription](ctx, reader, "tablespace list schema"); err != nil {
		return err
	}
	for {
		msg, err := reader.Receive(ctx)
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.DataRow:
			info, parseErr := parseTablespaceRow(m)
			if parseErr != nil {
				return fmt.Errorf("basebackup: parse tablespace row: %w", parseErr)
			}
			res.Tablespaces = append(res.Tablespaces, info)
		case *pgproto3.CommandComplete:
			// End of tablespace list.  Server now flips into the
			// multiplexed CopyOut.
			goto copyPhase
		default:
			return fmt.Errorf("basebackup: %w in tablespace list: %T",
				streaming.ErrUnexpectedMessage, msg)
		}
	}
copyPhase:
	// Phase 4 + 5 + 6: a single CopyOutResponse followed by multiplexed
	// CopyData (archive content + manifest content + progress reports)
	// and finally CopyDone.
	if err := expectMessage[*pgproto3.CopyOutResponse](ctx, reader, "multiplexed CopyOut"); err != nil {
		return err
	}
	if err := drainMultiplexed(ctx, reader, opts, sink, res); err != nil {
		return err
	}

	// Phase 7: stop LSN result set.
	stopLSN, stopTLI, err := readLSNResult(ctx, reader, "stop LSN")
	if err != nil {
		return err
	}
	res.StopLSN = stopLSN
	res.StopTimeline = stopTLI

	// Phase 8: BASE_BACKUP-tagged CommandComplete from
	// EndReplicationCommand("BASE_BACKUP").
	if err := expectMessage[*pgproto3.CommandComplete](ctx, reader, "BASE_BACKUP CommandComplete"); err != nil {
		return err
	}
	// Phase 9: ReadyForQuery closes the simple-query exchange.
	return expectMessage[*pgproto3.ReadyForQuery](ctx, reader, "ReadyForQuery")
}

// readLSNResult consumes the three-message result set that bbsink
// emits via SendXlogRecPtrResult: RowDescription (2 cols recptr+tli),
// DataRow with the values, CommandComplete "SELECT".  Returned LSN is
// the recptr text; tli is parsed as a uint32 (PG ships it as int8 on
// the wire to leave room for the unsigned range).
func readLSNResult(ctx context.Context, reader *streaming.Reader, label string) (string, uint32, error) {
	if err := expectMessage[*pgproto3.RowDescription](ctx, reader, label+" schema"); err != nil {
		return "", 0, err
	}
	msg, err := reader.Receive(ctx)
	if err != nil {
		return "", 0, err
	}
	dr, ok := msg.(*pgproto3.DataRow)
	if !ok {
		return "", 0, fmt.Errorf("basebackup: %w expecting %s DataRow; got %T",
			streaming.ErrUnexpectedMessage, label, msg)
	}
	lsn, tli, perr := parseLSNRow(dr)
	if perr != nil {
		return "", 0, fmt.Errorf("basebackup: parse %s row: %w", label, perr)
	}
	if err := expectMessage[*pgproto3.CommandComplete](ctx, reader, label+" CommandComplete"); err != nil {
		return "", 0, err
	}
	return lsn, tli, nil
}

// MaxManifestBytes bounds res.ManifestBytes in drainMultiplexed. A
// real PG `pg_verifybackup` manifest is sized by the number of
// files in PGDATA — for a 100 TB cluster this is on the order of
// hundreds of thousands of files × ~100 bytes per entry, so a
// few hundred MiB at the extreme. 256 MiB is comfortably above the
// realistic ceiling and well below an OOM-risk threshold for the
// tiny supervisor process. A misbehaving / hostile server that
// streams CopyData past this bound is refused with a clean error
// rather than dragging the agent down.
const MaxManifestBytes = 256 << 20

// Multiplex type bytes — first byte of every CopyData payload in PG
// 15+ BASE_BACKUP.  Values from src/backend/backup/basebackup_copy.c.
const (
	mplexNewArchive    = 'n' // archive_name + tablespace_path (cstrings)
	mplexData          = 'd' // archive or manifest content bytes
	mplexProgress      = 'p' // int64 bytes_done (network byte order)
	mplexManifestStart = 'm' // empty payload; ends previous archive
)

// drainMultiplexed reads the single multiplexed CopyOut stream and
// dispatches type-byte-prefixed CopyData frames to the Sink.  The
// stream covers every tablespace archive in declared order plus the
// optional backup manifest; CopyDone closes the stream.
//
// State machine:
//
//	current = -2 (uninitialised)  →  must see 'n' first
//	current >= 0 (in archive)     →  'd' = archive bytes,
//	                                  'n' = next archive (closes prev),
//	                                  'm' = manifest start (closes prev),
//	                                  'p' = progress (ignored)
//	current == ManifestSinkIndex  →  'd' = manifest bytes,
//	                                  'p' = progress (ignored),
//	                                  any other type byte = error
//
// PG always emits archives in order; the index we surface to Sink is
// the position in res.Tablespaces.  Surplus archives or a manifest
// arriving when Manifest=false produce ErrUnexpectedMessage.
func drainMultiplexed(ctx context.Context, reader *streaming.Reader, opts Options, sink Sink, res *Result) error {
	const stateInitial = -2
	current := stateInitial
	archivesSeen := 0

	closeCurrent := func() error {
		if current == stateInitial {
			return nil
		}
		if err := sink.OnTablespaceEnd(current); err != nil {
			return fmt.Errorf("basebackup: sink rejected idx=%d end: %w", current, err)
		}
		return nil
	}

	for {
		msg, err := reader.Receive(ctx)
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.CopyData:
			if len(m.Data) == 0 {
				return fmt.Errorf("basebackup: %w empty CopyData (no type byte)",
					streaming.ErrUnexpectedMessage)
			}
			typeByte := m.Data[0]
			payload := m.Data[1:]
			switch typeByte {
			case mplexNewArchive:
				if err := closeCurrent(); err != nil {
					return err
				}
				if archivesSeen >= len(res.Tablespaces) {
					return fmt.Errorf("basebackup: %w (extra archive 'n' frame; header announced %d)",
						streaming.ErrUnexpectedMessage, len(res.Tablespaces))
				}
				current = archivesSeen
				archivesSeen++
				if err := sink.OnTablespaceStart(current, res.Tablespaces[current]); err != nil {
					return fmt.Errorf("basebackup: sink rejected idx=%d start: %w", current, err)
				}
				// Payload is archive_name\0tablespace_path\0; we
				// don't surface it to the Sink today (the caller
				// already has TablespaceInfo).  Discarding is
				// safe — it's metadata, not file content.
			case mplexData:
				if current == stateInitial {
					return fmt.Errorf("basebackup: %w 'd' frame before any archive started",
						streaming.ErrUnexpectedMessage)
				}
				if current == ManifestSinkIndex {
					if int64(len(res.ManifestBytes))+int64(len(payload)) > MaxManifestBytes {
						return fmt.Errorf("basebackup: manifest exceeds %d bytes (DoS guard)",
							MaxManifestBytes)
					}
					res.ManifestBytes = append(res.ManifestBytes, payload...)
				}
				if err := sink.OnTablespaceData(current, payload); err != nil {
					return fmt.Errorf("basebackup: sink rejected idx=%d data: %w", current, err)
				}
			case mplexProgress:
				// 8-byte int64 bytes_done.  We don't surface
				// progress today — discard.  Validate length
				// to catch protocol drift early.
				if len(payload) != 8 {
					return fmt.Errorf("basebackup: %w progress frame payload = %d bytes; want 8",
						streaming.ErrUnexpectedMessage, len(payload))
				}
				_ = binary.BigEndian.Uint64(payload)
			case mplexManifestStart:
				if !opts.Manifest {
					return fmt.Errorf("basebackup: %w (got manifest 'm' frame with Manifest=false)",
						streaming.ErrUnexpectedMessage)
				}
				if err := closeCurrent(); err != nil {
					return err
				}
				current = ManifestSinkIndex
				if err := sink.OnTablespaceStart(ManifestSinkIndex, TablespaceInfo{}); err != nil {
					return fmt.Errorf("basebackup: sink rejected manifest start: %w", err)
				}
			default:
				return fmt.Errorf("basebackup: %w unknown multiplex type byte 0x%02x",
					streaming.ErrUnexpectedMessage, typeByte)
			}
		case *pgproto3.CopyDone:
			if err := closeCurrent(); err != nil {
				return err
			}
			if archivesSeen != len(res.Tablespaces) {
				return fmt.Errorf("basebackup: %w (saw %d archives; header announced %d)",
					streaming.ErrUnexpectedMessage, archivesSeen, len(res.Tablespaces))
			}
			if opts.Manifest && len(res.ManifestBytes) == 0 {
				return fmt.Errorf("basebackup: %w (Manifest=true but server emitted no manifest 'm' frame)",
					streaming.ErrUnexpectedMessage)
			}
			return nil
		default:
			return fmt.Errorf("basebackup: %w inside multiplexed CopyOut: %T",
				streaming.ErrUnexpectedMessage, msg)
		}
	}
}

// expectMessage is a small helper that asserts the next message has the
// expected concrete type. Used at phase boundaries.
func expectMessage[T pgproto3.BackendMessage](ctx context.Context, r *streaming.Reader, label string) error {
	msg, err := r.Receive(ctx)
	if err != nil {
		return err
	}
	if _, ok := msg.(T); !ok {
		return fmt.Errorf("basebackup: %w expecting %s; got %T",
			streaming.ErrUnexpectedMessage, label, msg)
	}
	return nil
}

// buildQuery assembles the BASE_BACKUP command string from opts.
//
// Wire format: PG 13 introduced the parenthesised
//
//	BASE_BACKUP ( option [, ...] )
//
// shape; PG 17 deprecated the legacy space-separated keyword
// form; PG 18 REMOVED it.  Marcelo Diaz reported issue #6 on
// PG 18.3 with a clean reproducer:
//
//	pg_hardstorage backup db1 ...   →  pg ERROR [42601]: syntax error
//
// the same syntax error also bit our soak driver against PG
// 15 because the legacy form was always going to be a ticking
// upgrade clock.  We commit to PG 15+, all of which support
// the parenthesised form, so emit it unconditionally.
//
// Quoting rules: LABEL + INCREMENTAL embed user/manifest-
// supplied strings as `Sconst`; both escape single quotes so
// the command stays well-formed.  Boolean keywords (FAST, WAL)
// stand alone — PG accepts either bare keyword (legacy /
// "true") or `KEYWORD <bool>`; we keep it bare for symmetry
// with how operators typically read these in audit logs.
//
// CHECKPOINT 'fast' is ALWAYS emitted.  Without it, PG's BASE_BACKUP
// waits for the next scheduled checkpoint before any tablespace
// bytes flow — on a quiet primary with checkpoint_timeout=5min
// that can sit idle for the full 5 minutes before our backup even
// starts streaming.  An operator-initiated tool should never
// surprise the operator with a multi-minute idle gap; an immediate
// checkpoint trades a brief I/O burst on the primary for
// predictable startup latency, and that's the right trade for
// every caller we have.  The Options.Fast field is retained for
// source-compat but is intentionally ignored — see Options doc.
//
// Wire shape note: in the parenthesised PG 15+ form, the legacy
// bare `FAST` keyword does NOT exist — it was renamed to
// `CHECKPOINT 'fast' | 'spread'` (see PG 17 source
// src/backend/backup/basebackup.c — parse_basebackup_options
// recognizes only "checkpoint" with a 'fast'/'spread' Sconst).
// Emitting bare `FAST` against PG 15+ in the parenthesised form
// fails with `unrecognized base backup option: "fast"`.
//
// Order matters only for human readability of wire-protocol
// dumps; PG accepts options in any order.  We emit
// (LABEL, CHECKPOINT, WAL, MANIFEST, INCREMENTAL).
func buildQuery(opts Options) string {
	parts := []string{
		"LABEL '" + escapeSingleQuotes(opts.Label) + "'",
		"CHECKPOINT 'fast'",
	}
	if opts.IncludeWAL {
		parts = append(parts, "WAL")
	}
	if opts.Manifest {
		parts = append(parts, "MANIFEST 'yes'")
	}
	// PG 17+: incremental against the manifest uploaded in the
	// prior UPLOAD_MANIFEST stage.  INCREMENTAL is a BOOLEAN
	// option here; the actual manifest content was already sent
	// as CopyData by uploadIncrementalManifest.  The value is
	// REQUIRED — PG's defGetBoolean rejects a value-less option
	// with `incremental requires a Boolean value` (a bare
	// `INCREMENTAL` fails against a real server, even though the
	// grammar parses it). Send an explicit 'true', mirroring the
	// `MANIFEST 'yes'` form above.
	if len(opts.IncrementalManifest) > 0 {
		parts = append(parts, "INCREMENTAL 'true'")
	}
	return "BASE_BACKUP (" + strings.Join(parts, ", ") + ")"
}

// escapeSingleQuotes prepares s for safe embedding inside a single-
// quoted argument of a replication-protocol command (BASE_BACKUP
// LABEL '...'). The protocol's argument parser stops at the closing
// `'` byte; the only character that can break the literal is `'`,
// which we double per SQL convention.
//
// Backslashes are NOT escaped here: PG 9.1+ default
// `standard_conforming_strings=on` makes `\` literal in `'...'`
// strings, and the project commits to PG 15+. Doubling backslashes
// would corrupt labels that legitimately contain them.
//
// We DO strip embedded newlines, carriage returns, and NULs — these
// would be ambiguous to the protocol's framing or render the
// resulting label uninterpretable in tooling that re-parses it.
// Defensive belt-and-braces on operator-supplied labels.
func escapeSingleQuotes(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "'", "''")
}

// parseTablespaceRow extracts (oid, location, size_kib) from one
// PG-emitted DataRow.  PG documents three columns:
//
//	col 0  spcoid       OID   tablespace oid (NULL → main data dir)
//	col 1  spclocation  text  filesystem path (NULL → main data dir)
//	col 2  size         int8  KiB (NULL when progress not requested)
//
// We accept >= 3 to be tolerant of any future PG version that
// adds optional trailing fields, OR of distro patches that
// extend the schema (e.g., a tablespace_oid_text variant).
// Strict equality bit us once already — this round it bit us
// AGAIN with a different mismatch, so the lesson is clear:
// future-proof the protocol parsing, fail-fast only on the
// load-bearing leading three columns being absent.
func parseTablespaceRow(m *pgproto3.DataRow) (TablespaceInfo, error) {
	if len(m.Values) < 3 {
		return TablespaceInfo{}, fmt.Errorf(
			"expected at least 3 columns (spcoid, spclocation, size); got %d",
			len(m.Values))
	}
	// PG emits NULL for spcoid AND spclocation for the main data
	// directory tablespace ("ti->path == NULL" in
	// src/backend/backup/basebackup.c).  Treat NULL oid as 0 and
	// NULL location as empty — the runner already encodes this
	// convention as "main data dir is implied".  An empty []byte
	// from pgproto3 covers both SQL NULL (nil slice) and the
	// degenerate empty-string case.
	info := TablespaceInfo{
		Location: stringOrEmpty(m.Values[1]),
	}
	if len(m.Values[0]) > 0 {
		oid, err := parseUint32Bytes(m.Values[0])
		if err != nil {
			return TablespaceInfo{}, fmt.Errorf("oid: %w", err)
		}
		info.OID = oid
	}
	if len(m.Values[2]) > 0 {
		size, err := strconv.ParseInt(string(m.Values[2]), 10, 64)
		if err != nil {
			return info, fmt.Errorf("size: %w", err)
		}
		info.SizeKiB = size
	}
	return info, nil
}

// parseLSNRow extracts (lsn, timeline) from PG's SendXlogRecPtrResult
// emission: 2 columns, recptr (text "X/X") + tli (int8 rendered as
// text by DestRemoteSimple).  Used for both the start and stop LSN
// result sets.
func parseLSNRow(m *pgproto3.DataRow) (string, uint32, error) {
	if len(m.Values) != 2 {
		return "", 0, fmt.Errorf("expected 2 columns (recptr, tli); got %d", len(m.Values))
	}
	lsn := string(m.Values[0])
	tli, err := parseUint32Bytes(m.Values[1])
	if err != nil {
		return "", 0, fmt.Errorf("timeline: %w", err)
	}
	return lsn, tli, nil
}

func parseUint32Bytes(b []byte) (uint32, error) {
	if len(b) == 0 {
		return 0, errors.New("empty")
	}
	v, err := strconv.ParseUint(string(b), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

func stringOrEmpty(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// uploadIncrementalManifest drives the PG 17+ UPLOAD_MANIFEST wire
// protocol on the supplied reader.  Sequence:
//
//	→ Query{"UPLOAD_MANIFEST"}
//	← CopyInResponse                 (server: "OK, start sending")
//	→ CopyData(manifest bytes)       (one or more; we send in one)
//	→ CopyDone
//	← CommandComplete("UPLOAD_MANIFEST")
//	← ReadyForQuery
//
// Notes:
//   - Manifests are typically a few KB — well below the 1 GiB
//     CopyData frame cap — so we send them in a single CopyData.
//   - We accept both CommandComplete-then-ReadyForQuery and
//     ReadyForQuery alone as the terminal signal: pgproto3 server
//     implementations differ in whether they elide the empty
//     CommandComplete tag.  We tolerate either ordering.
//   - On server ErrorResponse the reader's typed *ServerError
//     propagates up — same posture as every other phase here.
func uploadIncrementalManifest(ctx context.Context, reader *streaming.Reader, manifest []byte) error {
	if err := reader.Send(&pgproto3.Query{String: "UPLOAD_MANIFEST"}); err != nil {
		return fmt.Errorf("send UPLOAD_MANIFEST query: %w", err)
	}
	// 1. Wait for CopyInResponse — the server's "begin upload" ack.
	if err := expectMessage[*pgproto3.CopyInResponse](ctx, reader, "UPLOAD_MANIFEST CopyInResponse"); err != nil {
		return err
	}
	// 2. Stream the manifest bytes.  Single CopyData frame; the
	//    typical manifest is a few KB, well under the 1 GiB
	//    CopyData cap.  Defensive copy because pgproto3 keeps
	//    the slice alive past the Send call.
	body := append([]byte(nil), manifest...)
	if err := reader.Send(&pgproto3.CopyData{Data: body}); err != nil {
		return fmt.Errorf("send manifest CopyData: %w", err)
	}
	if err := reader.Send(&pgproto3.CopyDone{}); err != nil {
		return fmt.Errorf("send CopyDone: %w", err)
	}
	// 3. Drain the CommandComplete + ReadyForQuery that bracket
	//    a successful UPLOAD_MANIFEST.  ReadyForQuery is required
	//    so that the subsequent BASE_BACKUP Query message lands
	//    in a clean state.
	for {
		msg, err := reader.Receive(ctx)
		if err != nil {
			return fmt.Errorf("await UPLOAD_MANIFEST terminator: %w", err)
		}
		switch msg.(type) {
		case *pgproto3.CommandComplete:
			// Continue; ReadyForQuery follows.
		case *pgproto3.ReadyForQuery:
			return nil
		default:
			return fmt.Errorf("%w awaiting UPLOAD_MANIFEST terminator: got %T",
				streaming.ErrUnexpectedMessage, msg)
		}
	}
}
