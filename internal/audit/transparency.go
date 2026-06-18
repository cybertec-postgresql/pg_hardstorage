// transparency.go — TransparencyLog interface + storage-backed implementation for anchoring chain heads.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stdio "io"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TransparencyLog is the abstraction over a Sigstore-Rekor-shaped
// append-only public ledger. The audit chain is hash-linked LOCALLY;
// anchoring the chain head into a TransparencyLog gives us an
// external witness so even a fully-compromised repo operator can't
// silently rewrite history without simultaneously corrupting the
// log entry.
//
// ships exactly two implementations:
//
//   - StorageBackedLog: persists anchors as objects in the repo's
//     storage at audit/anchors/<seq>.json. Same dumb-but-correct
//     posture as the rest of the audit subsystem; gives operators
//     who can't reach a public log a tamper-evident-but-self-hosted
//     trust story.
//
//   - rekor.Log: real Rekor + cosign attestation, behind the
//     same interface so the chain anchor flow doesn't change shape
//     as the trust model grows.
//
// This is deliberately a small interface — Put / Get / Latest. The
// Sigstore-Rekor public API is much larger; we cover only what the
// audit chain anchor actually needs.
type TransparencyLog interface {
	// PutAnchor publishes anchor and returns its log-side ID. The
	// log MUST guarantee that PutAnchor for the same anchor body is
	// deterministic OR returns a stable ID (so re-anchoring after
	// a network blip doesn't double-record).
	PutAnchor(ctx context.Context, anchor Anchor) (logID string, err error)

	// GetAnchor reads back a previously-stored anchor. logID is
	// whatever PutAnchor returned. Used by VerifyAnchor to detect
	// tampering between the local chain and the public log.
	GetAnchor(ctx context.Context, logID string) (*Anchor, error)

	// LatestAnchor returns the highest-Sequence anchor seen by the
	// log, or nil when none exist. Used by `audit anchor` to refuse
	// re-anchoring an already-current head.
	LatestAnchor(ctx context.Context) (*Anchor, error)
}

// Anchor is one chain-head witness record. Schema-versioned
// independently from Event so the anchor format can grow without
// migrating every event.
//
// The anchor binds:
//   - ChainHeadHash: the SHA-256 of the head event's canonical bytes
//     (= what audit.HeadPointer also stores)
//   - HeadSequence: the head event's Sequence; cross-references the
//     audit chain for forensic walks.
//   - AnchoredAt: when this anchor was written. Chosen by the
//     publisher; receivers should sanity-check against the
//     log-side timestamp when both exist.
//   - PublisherID: opaque label for who wrote the anchor (e.g.
//     control-plane node ID, automation principal). Useful for
//     post-incident "who anchored the chain?" forensic walks.
type Anchor struct {
	Schema string `json:"schema"`
	// Shard names the audit chain this anchor witnesses. Empty is the
	// global chain — and pre-sharding anchors decode to "", so they
	// keep verifying against the global chain unchanged.
	Shard         string    `json:"shard,omitempty"`
	ChainHeadHash string    `json:"chain_head_hash"`
	HeadSequence  int64     `json:"head_sequence"`
	AnchoredAt    time.Time `json:"anchored_at"`
	PublisherID   string    `json:"publisher_id,omitempty"`
	LogID         string    `json:"log_id,omitempty"`
}

// AnchorSchema is the on-disk version tag for Anchor records.
const AnchorSchema = "pg_hardstorage.audit.anchor.v1"

// AnchorPrefix is the storage prefix used by StorageBackedLog. Lives
// alongside chain events under the audit/ namespace so a single
// repo-wide read covers the trust artifacts.
const AnchorPrefix = "audit/anchors/"

// StorageBackedLog implements TransparencyLog over a StoragePlugin.
// Anchor records live at AnchorPrefix + <hex-sequence>.json. Lex
// ordering matches sequence ordering, so LatestAnchor is the lex-
// tail of the listing.
//
// This is a self-hosted transparency log: it doesn't give external-
// witness security (an operator with write access to the repo could
// still rewrite history), but it gives:
//   - tamper-evident chain ordering across multiple operators (each
//     anchor's sequence + hash is captured separately from the chain
//     itself, so a forger has to corrupt both),
//   - an audit trail of who anchored what, when,
//   - a foundation that drops in a real Rekor backend without
//     reshaping the chain.
type StorageBackedLog struct {
	sp   storage.StoragePlugin
	worm *repo.WORMPolicy
}

// NewStorageBackedLog wraps sp without WORM threading.
func NewStorageBackedLog(sp storage.StoragePlugin) *StorageBackedLog {
	if sp == nil {
		panic("audit: NewStorageBackedLog requires a non-nil StoragePlugin")
	}
	return &StorageBackedLog{sp: sp}
}

// NewStorageBackedLogWithRetention wraps sp + applies the WORM
// retention policy on every PutAnchor. When policy is nil/zero
// this is identical to NewStorageBackedLog(sp).
//
// Anchors are write-once-read-many at the value level (one log ID
// per chain head; PutAnchor is idempotent on race). Retention
// fits naturally — each anchor commits at chain-head time and
// keeps its retention rolling against that timestamp. The
// transparency log thereby satisfies SEC-17a-4(f)-class
// "non-rewritable, non-erasable" requirements.
func NewStorageBackedLogWithRetention(sp storage.StoragePlugin, policy *repo.WORMPolicy) *StorageBackedLog {
	if sp == nil {
		panic("audit: NewStorageBackedLogWithRetention requires a non-nil StoragePlugin")
	}
	return &StorageBackedLog{sp: sp, worm: policy}
}

// PutAnchor implements TransparencyLog. The log ID is derived from
// the chain-head hash so re-anchoring the same head is idempotent
// (different anchored-at timestamps still produce the same log key).
//
// We use IfNotExists=true so a concurrent publisher race doesn't
// create two anchors for the same head. The first writer wins; the
// loser's PutAnchor returns the existing log ID.
func (l *StorageBackedLog) PutAnchor(ctx context.Context, anchor Anchor) (string, error) {
	if anchor.Schema == "" {
		anchor.Schema = AnchorSchema
	}
	if anchor.ChainHeadHash == "" {
		return "", errAuditField("audit: ChainHeadHash is required for anchor")
	}
	logID := deriveAnchorID(anchor)
	anchor.LogID = logID

	body, err := json.MarshalIndent(&anchor, "", "  ")
	if err != nil {
		return "", err
	}
	key := AnchorPrefix + logID + ".json"
	putOpts := storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	}
	if !l.worm.IsZero() {
		now := time.Now().UTC()
		putOpts.RetainUntil = l.worm.RetainUntil(now)
		putOpts.RetentionMode = storage.WORMMode(l.worm.Mode)
	}
	_, putErr := l.sp.Put(ctx, key, bytes.NewReader(body), putOpts)
	if putErr == nil {
		return logID, nil
	}
	// IfNotExists=true means a duplicate key is a precondition
	// failure; the storage layer surfaces this as ErrPreconditionFailed.
	// Re-read the existing anchor — same head, possibly different
	// timestamp — and return its log ID. Idempotent.
	if existsErr := l.exists(ctx, key); existsErr == nil {
		return logID, nil
	}
	return "", putErr
}

// GetAnchor implements TransparencyLog.
func (l *StorageBackedLog) GetAnchor(ctx context.Context, logID string) (*Anchor, error) {
	rc, err := l.sp.Get(ctx, AnchorPrefix+logID+".json")
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var a Anchor
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// LatestAnchor implements TransparencyLog. Walks the prefix and
// returns the highest-Sequence anchor (which == lex-tail of the
// hex-padded seq filename, modulo digests; we read each candidate
// to find the actual max).
func (l *StorageBackedLog) LatestAnchor(ctx context.Context) (*Anchor, error) {
	var keys []string
	for info, err := range l.sp.List(ctx, AnchorPrefix) {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		keys = append(keys, info.Key)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	var best *Anchor
	for _, k := range keys {
		// Trim prefix + suffix so we get the bare logID for GetAnchor.
		id := strings.TrimSuffix(strings.TrimPrefix(k, AnchorPrefix), ".json")
		a, err := l.GetAnchor(ctx, id)
		if err != nil {
			continue
		}
		if best == nil || a.HeadSequence > best.HeadSequence {
			best = a
		}
	}
	return best, nil
}

// exists is the lightest "does this key exist?" probe — used by
// PutAnchor's idempotent race-loser path.
func (l *StorageBackedLog) exists(ctx context.Context, key string) error {
	rc, err := l.sp.Get(ctx, key)
	if err != nil {
		return err
	}
	rc.Close()
	return nil
}

// deriveAnchorID returns a deterministic ID for an anchor: SHA-256
// of (schema + chain_head_hash + head_sequence). The resulting ID
// is the same for two anchors over the same chain head, regardless
// of who wrote them or when — so re-anchoring is a no-op.
func deriveAnchorID(a Anchor) string {
	var h [32]byte
	hh := sha256.New()
	hh.Write([]byte(a.Schema))
	hh.Write([]byte("\x00"))
	hh.Write([]byte(a.ChainHeadHash))
	hh.Write([]byte("\x00"))
	// Sequence as a 16-hex string so two adjacent sequences produce
	// distinguishably-different bytes feeding the hash (avoids any
	// "looks like a length" collision worry).
	hh.Write([]byte(hexSeq(a.HeadSequence)))
	// Shard distinguishes per-shard anchors. Appended ONLY when
	// non-empty so a global anchor's ID is byte-identical to the
	// pre-sharding derivation (existing anchors keep their IDs).
	if a.Shard != "" {
		hh.Write([]byte("\x00"))
		hh.Write([]byte(a.Shard))
	}
	copy(h[:], hh.Sum(nil))
	return hex.EncodeToString(h[:16]) // 32 hex chars; plenty of entropy
}
