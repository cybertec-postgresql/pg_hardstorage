// Package commitlsn extracts the transaction end LSN from pgoutput
// commit messages — the only LSN a logical sink may safely confirm
// back to PostgreSQL.
//
// Why not WALStart+len(Data)? That is a synthetic value: WALStart is
// the LSN of the decoded change, but Data is the pgoutput encoding of
// it, whose length has nothing to do with WAL layout. A big row or a
// wide Relation message pushes the sum past the end of the transaction
// it belongs to — and past commits the sink has not received yet.
// Confirming it moves the slot's confirmed_flush_lsn over those
// commits and PostgreSQL never re-sends them after a restart: the
// transactions are skipped. A commit message's end LSN is exact — it
// is what pg_recvlogical and built-in subscribers confirm — and
// confirming it re-sends, on restart, precisely the transactions whose
// commit the sink did not yet hold.
package commitlsn

import (
	"encoding/binary"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
)

// pgoutput message layouts (PostgreSQL "Logical Replication Message
// Formats"). Offsets are of the transaction end LSN field.
const (
	// 'C' Commit: Byte1 type, Int8 flags, Int64 commit LSN, Int64 end
	// LSN, Int64 timestamp.
	commitEndOff = 1 + 1 + 8
	// 'c' Stream Commit: Byte1 type, Int32 xid, Int8 flags, Int64
	// commit LSN, Int64 end LSN, Int64 timestamp.
	streamCommitEndOff = 1 + 4 + 1 + 8
	// 'K' Commit Prepared: Byte1 type, Int8 flags, Int64 commit LSN,
	// Int64 end LSN, Int64 timestamp, Int32 xid, String gid.
	commitPreparedEndOff = 1 + 1 + 8
)

// FromMessage returns the transaction end LSN carried by a pgoutput
// commit-type message, and ok=false for every other message (or a
// truncated one). Only commit-type messages mark a point up to which
// every transaction has been received in full.
func FromMessage(data []byte) (pglogrepl.LSN, bool) {
	if len(data) == 0 {
		return 0, false
	}
	var off int
	switch data[0] {
	case 'C':
		off = commitEndOff
	case 'c':
		off = streamCommitEndOff
	case 'K':
		off = commitPreparedEndOff
	default:
		return 0, false
	}
	if len(data) < off+8 {
		return 0, false
	}
	return pglogrepl.LSN(binary.BigEndian.Uint64(data[off : off+8])), true
}

// Highest returns the highest transaction end LSN among the commit
// messages in recs, or 0 when the batch holds no commit (it ends
// mid-transaction, so nothing in it may be confirmed yet).
func Highest(recs []logicalreceiver.Record) pglogrepl.LSN {
	var hi pglogrepl.LSN
	for _, r := range recs {
		if end, ok := FromMessage(r.Data); ok && end > hi {
			hi = end
		}
	}
	return hi
}

// Advance raises *synced to commit when commit is higher — the
// monotonic confirm every sink performs after a durable write. Never
// moves backwards: a batch without a commit (commit == 0) or a batch
// that raced an earlier-finishing later one leaves it alone.
func Advance(synced interface {
	Load() uint64
	CompareAndSwap(old, new uint64) bool
}, commit pglogrepl.LSN) {
	for {
		cur := synced.Load()
		if uint64(commit) <= cur || synced.CompareAndSwap(cur, uint64(commit)) {
			return
		}
	}
}

// Message builds a minimal pgoutput 'C' Commit message whose
// transaction end LSN is end. For tests that drive sinks with
// synthetic streams; production never builds commits.
func Message(end pglogrepl.LSN) []byte {
	b := make([]byte, 1+1+8+8+8)
	b[0] = 'C'
	binary.BigEndian.PutUint64(b[2:], uint64(end))
	binary.BigEndian.PutUint64(b[commitEndOff:], uint64(end))
	return b
}
