// manifest.go — SegmentManifest schema for the per-WAL-segment v1 metadata committed alongside chunks.
package walsink

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Schema is the on-disk identifier for SegmentManifest. Versioned with
// the same v-prefix scheme as the backup manifest, repo, and config
// schemas. We commit to 24-month backward-read compatibility on this
// value: any agent built against schema v1 must keep reading v1
// manifests for at least 24 months after a successor schema lands.
const Schema = "pg_hardstorage.wal_segment.v1"

// SegmentManifest is the per-WAL-segment metadata committed to the
// repo alongside the chunked segment body. One file per 16 MiB WAL
// segment.
//
// Field choices:
//
//   - SystemIdentifier and Timeline pin the manifest to a specific PG
//     cluster + branch. A misconfigured agent pointed at the wrong PG
//     would otherwise silently mix segments from two clusters in the
//     same repo path.
//
//   - SegmentNumber is redundant with SegmentName but cheap, and lets
//     readers compute LSN ranges without parsing the hex name.
//
//   - SegmentSize is recorded on every manifest so a future PG cluster
//     with a non-default --with-wal-segsize is restorable end-to-end.
//
//   - Chunks lists the CAS chunk references in stream order. Concatenated
//     they reproduce the segment's bytes byte-for-byte.
type SegmentManifest struct {
	Schema           string     `json:"schema"`
	Deployment       string     `json:"deployment"`
	SystemIdentifier string     `json:"system_identifier"`
	Timeline         uint32     `json:"timeline"`
	SegmentNumber    uint64     `json:"segment_number"`
	SegmentName      string     `json:"segment_name"`
	StartLSN         string     `json:"start_lsn"`
	EndLSN           string     `json:"end_lsn"`
	SegmentSize      int64      `json:"segment_size"`
	Chunks           []ChunkRef `json:"chunks"`
	CreatedAt        time.Time  `json:"created_at"`

	// Encryption, when non-nil, records the envelope under which THIS
	// segment's chunks were written, so restore can resolve the shared DEK
	// from the segment manifest alone — no base-backup manifest required
	// (issue #106). omitempty + trailing position means a plaintext segment
	// (Encryption nil) serialises byte-for-byte as before, preserving both
	// the ChunkRefsEqual idempotency check and 24-month forward-read
	// compatibility: an old plaintext v1 manifest parses with Encryption nil.
	Encryption *EncryptionInfo `json:"encryption,omitempty"`
}

// EncryptionInfo is the per-segment envelope describing how this segment's
// chunks were encrypted and how to recover the DEK. Field names and shape
// match backup.EncryptionInfo intentionally — the shared-DEK resolver
// (internal/repo/sharedkey) reads both via the same JSON shape — but the
// type is duplicated rather than imported so the wal-segment manifest schema
// evolves independently of the backup manifest schema (same rationale as
// ChunkRef).
type EncryptionInfo struct {
	Scheme          string `json:"scheme"`
	KEKRef          string `json:"kek_ref"`
	WrappedDEK      string `json:"wrapped_dek"`
	EnvelopeVersion int    `json:"envelope_version"`
}

// ChunkRef points at one chunk inside a WAL segment's byte stream.
// Same shape as backup.ChunkRef intentionally — the resolution path
// (CAS lookup by hash) is identical; the type is duplicated rather
// than imported so the wal-segment manifest schema can evolve
// independently of the backup manifest schema.
type ChunkRef struct {
	Hash   repo.Hash `json:"hash"`
	Offset int64     `json:"offset"`
	Len    int64     `json:"len"`
}

// MarshalToBytes returns the canonical on-disk encoding of m. JSON,
// HTML-escaping disabled, no trailing newline. Determinism is
// guaranteed by the same constraints the backup manifest holds:
// struct fields emit in declaration order; no maps anywhere; no
// pretty-printing whitespace.
func (m *SegmentManifest) MarshalToBytes() ([]byte, error) {
	if m == nil {
		return nil, errors.New("walsink: marshal nil manifest")
	}
	var buf canonicalBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("walsink: marshal: %w", err)
	}
	return buf.TrimTrailingNewline(), nil
}

// ParseSegmentManifest is the symmetric reader. It rejects manifests
// whose Schema doesn't match — that's the 24-month-window enforcement
// point.
func ParseSegmentManifest(raw []byte) (*SegmentManifest, error) {
	var m SegmentManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("walsink: parse: %w", err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("walsink: schema %q not supported (want %q)",
			m.Schema, Schema)
	}
	return &m, nil
}

// ChunkRefsEqual reports whether two ChunkRef slices reference
// the same content in the same layout.  Used by the
// commit-manifest path to distinguish a true idempotent
// re-push (same chunks, same offsets, same lengths — every
// byte agrees) from a split-brain collision (same segment
// name, same system_identifier, but DIFFERENT body bytes
// produced by a doppelgänger cluster).  Without this check
// the second push silently treats the loser as success.
//
// Comparison is order-sensitive: the chunker emits chunks in
// stream order, so two runs over identical bytes produce
// identical (offset, length, hash) sequences.  Different bytes
// at any offset diverge the FastCDC cut points and therefore
// the entire downstream slice — there's no realistic
// false-positive path here.
func ChunkRefsEqual(a, b []ChunkRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Hash != b[i].Hash || a[i].Offset != b[i].Offset || a[i].Len != b[i].Len {
			return false
		}
	}
	return true
}

// SegmentFileName returns the canonical 24-character hex file name
// PG uses for a WAL segment given (timeline, contiguous segment_number,
// segment size).
//
// Layout:  TTTTTTTT LLLLLLLL SSSSSSSS
//
//	timeline    log_id seg_in_log
//
// where log_id = segNum / segmentsPerLog and seg_in_log = segNum %
// segmentsPerLog, and segmentsPerLog = 4 GiB / segmentSize (256 for the
// default 16 MiB, 64 for 64 MiB, 4096 for 1 MiB). PG's own XLogFileName
// helper computes this identically. segmentSize 0 resolves to 16 MiB.
func SegmentFileName(timeline uint32, segNum uint64, segmentSize int64) string {
	perLog := SegmentsPerLog(segmentSize)
	logID := uint32(segNum / perLog)
	segLo := uint32(segNum % perLog)
	return fmt.Sprintf("%08X%08X%08X", timeline, logID, segLo)
}

// SegmentPath returns the repo key for a WAL segment manifest. Layout
// matches the SPEC's `wal/<deployment>/<timeline>/<segment>.json`.
// Caller passes the bare 24-char segment name; we add the suffix.
func SegmentPath(deployment string, timeline uint32, segmentName string) string {
	// Defensive: strip any caller-applied .json so callers and tests
	// that pass either form Just Work.
	segmentName = strings.TrimSuffix(segmentName, ".json")
	return fmt.Sprintf("wal/%s/%08X/%s.json", deployment, timeline, segmentName)
}

// canonicalBuffer is a tiny []byte wrapper that lets us strip the
// trailing newline json.Encoder.Encode appends. Mirrors backup.canonicalBuffer
// but lives here so the wal-segment manifest schema is self-contained.
type canonicalBuffer []byte

// Write appends p to the buffer and reports full success.
func (b *canonicalBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// TrimTrailingNewline returns the buffer bytes with at most one
// trailing '\n' removed — the newline json.Encoder.Encode appends.
func (b *canonicalBuffer) TrimTrailingNewline() []byte {
	out := []byte(*b)
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}
