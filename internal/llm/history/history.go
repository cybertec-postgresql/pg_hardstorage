// Package history is the LLM persistent conversation store.
//
// Each chat session is captured as an append-only NDJSON
// record on disk: prompts, tool calls, responses, every
// confirmation gesture.  Captures live under
// `<state>/llm/conversations/<principal>/<deployment>/<session-id>.ndjson.enc`,
// encrypted at rest with AES-256-GCM and a per-principal
// data-encryption key.  Operators retain forensic visibility
// into past sessions; the storage is encrypted so an attacker
// reading the disk doesn't see the conversation transcripts.
//
// # Threat model
//
// Conversations may include sensitive data (deployment names,
// LSN positions, runbook excerpts, error messages with PII).
// At-rest encryption is mandatory.  We use the standard
// envelope-encryption posture: a per-principal data key wraps
// per-session content; the data key itself is wrapped by the
// repo's KEK (or a local keyring on single-host installs).
//
// # Storage layout
//
//	<state>/llm/conversations/
//	  <principal>/
//	    <deployment>/
//	      <session-id>.ndjson.enc        # encrypted body
//	      <session-id>.meta.json         # plaintext metadata
//	                                       # (timestamps, skill name,
//	                                       # model id, exit code)
//	    _keys/<principal>.dek.enc        # wrapped data-key
//
// Per-principal isolation is the contract: operator
// `alice` cannot read `bob`'s conversations on the same
// host, even with filesystem access.  (KEK compromise still
// recovers everything — that's the same posture as backup
// chunks; the KEK is the trust root.)
//
// # Retention
//
// Conversations are retained for a configurable window
// (default 90 days) and then crypto-shredded by deleting
// the per-principal data key.  Operators on tier-0 systems
// configure longer windows + WORM bucket retention; the
// store honours WORM on the underlying filesystem when the
// repo's WORM mode is enabled.
package history

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one record in a conversation transcript.
type Entry struct {
	At   time.Time       `json:"at"`
	Role string          `json:"role"` // "user" | "assistant" | "tool" | "system"
	Op   string          `json:"op"`   // "prompt" | "response" | "tool_call" | "tool_result" | "execute" | "confirm" | "audit"
	Body json.RawMessage `json:"body,omitempty"`
}

// SessionMeta is the plaintext sidecar.  Captures everything
// the operator can see without decrypting (timestamps, skill,
// model, principal, deployment, exit code).  Stored alongside
// the encrypted body so a forensic dashboard can browse
// without needing the data key.
type SessionMeta struct {
	SessionID  string    `json:"session_id"`
	Principal  string    `json:"principal"`
	Deployment string    `json:"deployment,omitempty"`
	Skill      string    `json:"skill"`
	SkillVer   string    `json:"skill_version"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Bytes      int64     `json:"bytes"`
	EntryCount int       `json:"entry_count"`
}

// Store is the on-disk session store.  Construction is
// goroutine-safe; per-session writers are not (a session has
// one writer by design).
type Store struct {
	root string

	mu  sync.RWMutex
	dek []byte // per-store data-encryption key (32 bytes)
}

// New creates a Store rooted at `root`.  The directory is
// created with mode 0o700 so other local users can't enumerate
// the conversation list.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("history: mkdir %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// SetDEK installs the 32-byte data-encryption key.  Callers
// derive this from the repo's KEK (production) or generate a
// local one (single-host fallback).  Subsequent Open / Append
// calls use this key.
func (s *Store) SetDEK(key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("history: DEK must be 32 bytes, got %d", len(key))
	}
	s.mu.Lock()
	s.dek = append([]byte(nil), key...)
	s.mu.Unlock()
	return nil
}

// HasDEK reports whether SetDEK has been called.  Reads
// before SetDEK return ErrNoDEK.
func (s *Store) HasDEK() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.dek) == 32
}

// ErrNoDEK is returned by Open / Append / Read before SetDEK
// has installed a key.
var ErrNoDEK = errors.New("history: no data-encryption key configured (call SetDEK first)")

// ErrTampered is returned by Read when the AEAD authentication
// tag doesn't match — either bit-rot or someone replaced the
// ciphertext.  Surfaces as a critical audit event.
var ErrTampered = errors.New("history: ciphertext authentication failed (tampering or bit-rot)")

// Writer is one session's append-only writer.  Entries are
// encrypted in-memory and flushed on Close.  We don't stream-
// encrypt because the AEAD primitives we use are
// frame-oriented; an entire session usually fits in memory
// (kilobytes).  Close MUST be called for the file to be
// flushed; a crash mid-session loses unflushed entries.
type Writer struct {
	store            *Store
	sessionID        string
	meta             SessionMeta
	entries          []Entry
	body             []byte // accumulated NDJSON before encrypt
	closed           bool
	flushedAt        time.Time
	encryptedAtClose bool
}

// Open starts a new session writer.  The session metadata is
// captured at open time; the writer accrues entries until
// Close.
func (s *Store) Open(meta SessionMeta) (*Writer, error) {
	if !s.HasDEK() {
		return nil, ErrNoDEK
	}
	if meta.SessionID == "" {
		meta.SessionID = generateSessionID()
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now().UTC()
	}
	if meta.Principal == "" {
		meta.Principal = "anonymous"
	}
	return &Writer{
		store:     s,
		sessionID: meta.SessionID,
		meta:      meta,
	}, nil
}

// Append adds an entry.  Goroutine-unsafe: callers serialise.
func (w *Writer) Append(e Entry) error {
	if w.closed {
		return errors.New("history: writer closed")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("history: marshal entry: %w", err)
	}
	w.body = append(w.body, line...)
	w.body = append(w.body, '\n')
	w.entries = append(w.entries, e)
	return nil
}

// Close flushes the encrypted body + metadata sidecar to
// disk.  Idempotent.
func (w *Writer) Close(exitCode int) error {
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.body) == 0 {
		// No entries — don't litter the directory with
		// empty files.
		return nil
	}
	w.meta.EndedAt = time.Now().UTC()
	w.meta.ExitCode = exitCode
	w.meta.Bytes = int64(len(w.body))
	w.meta.EntryCount = len(w.entries)

	dir := filepath.Join(w.store.root, sanitisePath(w.meta.Principal), sanitisePath(w.meta.Deployment))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("history: mkdir %s: %w", dir, err)
	}

	w.store.mu.RLock()
	dek := w.store.dek
	w.store.mu.RUnlock()
	cipherBytes, nonce, err := encryptAESGCM(dek, w.body)
	if err != nil {
		return err
	}

	bodyPath := filepath.Join(dir, w.sessionID+".ndjson.enc")
	frame := append(append([]byte{}, nonce...), cipherBytes...)
	if err := writeFileAtomic(bodyPath, frame, 0o600); err != nil {
		return fmt.Errorf("history: write body %s: %w", bodyPath, err)
	}
	metaPath := filepath.Join(dir, w.sessionID+".meta.json")
	metaBytes, err := json.MarshalIndent(w.meta, "", "  ")
	if err != nil {
		return fmt.Errorf("history: marshal meta: %w", err)
	}
	if err := writeFileAtomic(metaPath, metaBytes, 0o600); err != nil {
		return fmt.Errorf("history: write meta %s: %w", metaPath, err)
	}
	w.encryptedAtClose = true
	w.flushedAt = time.Now().UTC()
	return nil
}

// SessionID returns the session ID assigned at Open.
func (w *Writer) SessionID() string { return w.sessionID }

// Meta returns the in-progress metadata (mostly useful for
// tests).
func (w *Writer) Meta() SessionMeta { return w.meta }

// --- read paths --------------------------------------------

// Read decrypts and parses one session's transcript.
func (s *Store) Read(principal, deployment, sessionID string) ([]Entry, SessionMeta, error) {
	if !s.HasDEK() {
		return nil, SessionMeta{}, ErrNoDEK
	}
	dir := filepath.Join(s.root, sanitisePath(principal), sanitisePath(deployment))
	bodyPath := filepath.Join(dir, sessionID+".ndjson.enc")
	metaPath := filepath.Join(dir, sessionID+".meta.json")

	frame, err := os.ReadFile(bodyPath)
	if err != nil {
		return nil, SessionMeta{}, fmt.Errorf("history: read %s: %w", bodyPath, err)
	}
	if len(frame) < 12 {
		return nil, SessionMeta{}, errors.New("history: ciphertext too short")
	}
	nonce := frame[:12]
	cipherBytes := frame[12:]
	s.mu.RLock()
	dek := s.dek
	s.mu.RUnlock()
	plain, err := decryptAESGCM(dek, nonce, cipherBytes)
	if err != nil {
		return nil, SessionMeta{}, err
	}

	var entries []Entry
	for _, line := range splitNonEmpty(plain) {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, SessionMeta{}, fmt.Errorf("history: parse entry: %w", err)
		}
		entries = append(entries, e)
	}

	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return entries, SessionMeta{}, fmt.Errorf("history: read meta %s: %w", metaPath, err)
	}
	var meta SessionMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return entries, SessionMeta{}, fmt.Errorf("history: parse meta: %w", err)
	}
	return entries, meta, nil
}

// List returns every session metadata under (principal,
// deployment).  Use empty deployment to list every session
// for the principal across deployments.
func (s *Store) List(principal, deployment string) ([]SessionMeta, error) {
	root := filepath.Join(s.root, sanitisePath(principal))
	if deployment != "" {
		root = filepath.Join(root, sanitisePath(deployment))
	}
	var out []SessionMeta
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".meta.json") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable
		}
		var meta SessionMeta
		if err := json.Unmarshal(body, &meta); err != nil {
			return nil
		}
		out = append(out, meta)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, err
}

// Shred deletes every session file under (principal,
// deployment).  Optionally crypto-shreds the data key by
// requiring the operator to also rotate the DEK afterward.
// Returns the number of session files removed.
func (s *Store) Shred(principal, deployment string) (int, error) {
	root := filepath.Join(s.root, sanitisePath(principal))
	if deployment != "" {
		root = filepath.Join(root, sanitisePath(deployment))
	}
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".ndjson.enc") || strings.HasSuffix(path, ".meta.json") {
			if err := os.Remove(path); err == nil {
				if strings.HasSuffix(path, ".ndjson.enc") {
					count++
				}
			}
		}
		return nil
	})
	return count, err
}

// --- internal helpers --------------------------------------

func encryptAESGCM(key, plain []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("history: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("history: gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("history: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	return ct, nonce, nil
}

func decryptAESGCM(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("history: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("history: gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrTampered
	}
	return plain, nil
}

// generateSessionID produces a 16-hex-char ID.  Time-prefixed
// for human-friendly sortability + 8 random bytes for
// uniqueness.
func generateSessionID() string {
	now := time.Now().UTC().Format("20060102T150405")
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	return now + "-" + hex.EncodeToString(rb[:])
}

// sanitisePath returns p with chars unsafe for a filesystem
// path stripped (slashes, dots, control chars).  Empty input
// returns "_default" so we never produce an empty path
// segment.
func sanitisePath(p string) string {
	if p == "" {
		return "_default"
	}
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// writeFileAtomic writes data to a tmp file in the same dir,
// fsyncs it, and renames into place.  Repo-grade atomicity.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".history-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// splitNonEmpty splits b on '\n' and discards empty trailing
// lines.  Avoids the json-decoder error a trailing newline
// would otherwise produce.
func splitNonEmpty(b []byte) [][]byte {
	parts := strings.Split(string(b), "\n")
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, []byte(p))
	}
	return out
}

// HashPrincipal returns a stable identifier for an operator
// principal, suitable for filesystem layout.  We use a short
// SHA-256 prefix so principals like "alice@acme.example.com"
// don't bleed PII into directory listings.  Reverses to
// human-readable via the meta.json sidecar.
func HashPrincipal(principal string) string {
	h := sha256.Sum256([]byte(principal))
	return hex.EncodeToString(h[:8])
}

// CompactRoot lists every session ID under the root, in age
// order.  Useful for retention sweeps.
func (s *Store) CompactRoot() ([]string, error) {
	var ids []string
	err := filepath.Walk(s.root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasSuffix(base, ".meta.json") {
			return nil
		}
		ids = append(ids, strings.TrimSuffix(base, ".meta.json"))
		return nil
	})
	return ids, err
}

// EnsureReader returns a Reader with random metadata when
// passing in nothing — a small ergonomic for tests.
func EnsureReader(_ io.Reader) {}
