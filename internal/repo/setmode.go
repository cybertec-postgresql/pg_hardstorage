// setmode.go — Mode (read-write / read-only): repository write-access posture flag.
package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Mode is the repository's write-access posture. An empty Mode is
// equivalent to ModeReadWrite — back-compat for repos created before
// v0.2 where the field didn't exist.
type Mode string

const (
	// ModeReadWrite is the default. All operations permitted.
	ModeReadWrite Mode = "read-write"

	// ModeReadOnly disables every command that mutates the repo:
	// backup, wal stream, wal push, repo gc, kms rotate/shred, manifest
	// commits, retention rotation. Restore + verify + read paths still
	// work.
	//
	// Use case: forensics / incident-response on a repo we don't want
	// any write to touch, even by accident. Flip it back with
	// `repo set-mode <url> read-write`.
	ModeReadOnly Mode = "read-only"
)

// IsValid reports whether m is one of the recognised modes (or empty,
// which is back-compat read-write).
func (m Mode) IsValid() bool {
	switch m {
	case "", ModeReadWrite, ModeReadOnly:
		return true
	}
	return false
}

// Effective returns m when set, else ModeReadWrite. Callers comparing
// "is this writable?" should use this rather than direct equality so
// pre-v0.2 repos behave correctly.
func (m Mode) Effective() Mode {
	if m == "" {
		return ModeReadWrite
	}
	return m
}

// ErrReadOnly is returned by AssertWritable when the repo is in
// read-only mode. CLI mappers translate it to exit code 7 (conflict) —
// the operator's intent ("write to this repo") collides with the
// repo's declared posture ("no writes allowed"), and that's exactly
// what conflict means in our taxonomy.
var ErrReadOnly = errors.New("repo: repository is in read-only mode")

// SetModeOptions controls SetMode. Future fields (audit-event payload,
// approval-token reference for n-of-m) accrete here.
type SetModeOptions struct {
	URL  string
	Mode Mode
}

// SetModeResult is the structured outcome of a successful SetMode.
type SetModeResult struct {
	URL          string `json:"url"`
	PreviousMode Mode   `json:"previous_mode"`
	Mode         Mode   `json:"mode"`
	UpdatedAt    string `json:"updated_at"`
}

// SetMode rewrites HSREPO with Mode set to opts.Mode. The rewrite is
// not atomic against concurrent writers — but `set-mode` is a manual
// operator action, not a hot path, so the simpler read-then-write is
// fine. We keep the original ID and CreatedAt so the repo identity
// doesn't shift across mode flips.
//
// Returns ErrNotARepo when there's no HSREPO at the URL.
func SetMode(ctx context.Context, opts SetModeOptions) (*SetModeResult, error) {
	if !opts.Mode.IsValid() {
		return nil, fmt.Errorf("repo: invalid mode %q (want read-only|read-write)", opts.Mode)
	}
	if opts.Mode == "" {
		// Caller asked us to "clear" the mode. Treat that as
		// read-write and persist explicitly so a later read of
		// HSREPO sees a non-empty mode field — operators reading
		// the file with `cat` shouldn't have to know that empty
		// means read-write.
		opts.Mode = ModeReadWrite
	}

	meta, sp, err := Open(ctx, opts.URL)
	if err != nil {
		return nil, err
	}
	defer sp.Close()

	prev := meta.Mode.Effective()
	now := time.Now().UTC().Format(time.RFC3339)
	meta.Mode = opts.Mode
	meta.UpdatedAt = now

	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("repo: marshal HSREPO: %w", err)
	}
	body = append(body, '\n')

	// Plain Put (no IfNotExists). HSREPO already exists by definition;
	// we're overwriting it with a new mode. The fs and s3 backends
	// both implement plain Put as a tmp+rename / multipart upload, so
	// a crash mid-write doesn't leave a corrupt HSREPO.
	if _, err := sp.Put(ctx, HSREPOFilename, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return nil, fmt.Errorf("repo: rewrite HSREPO: %w", err)
	}

	return &SetModeResult{
		URL:          opts.URL,
		PreviousMode: prev,
		Mode:         opts.Mode,
		UpdatedAt:    now,
	}, nil
}

// AssertWritable returns ErrReadOnly if the repo at sp is in read-only
// mode. Callers (backup orchestrator, wal stream, gc, rotation, kms
// rotate/shred, wal push) call this immediately after Open and before
// any mutating storage call so a read-only flip-then-retry shows up
// at the top of the log instead of mid-operation.
//
// Cheap: one Get of HSREPO, one JSON parse. Cached at the call-site is
// the operator's choice; we don't memoise here because mode flips need
// to take effect on the next operation, not after a cache TTL.
func AssertWritable(ctx context.Context, sp storage.StoragePlugin) error {
	rc, err := sp.Get(ctx, HSREPOFilename)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotARepo
		}
		return fmt.Errorf("repo: read HSREPO for mode check: %w", err)
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("repo: read HSREPO body for mode check: %w", err)
	}
	var meta Metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return fmt.Errorf("repo: parse HSREPO for mode check: %w", err)
	}
	if meta.Mode.Effective() == ModeReadOnly {
		return ErrReadOnly
	}
	return nil
}
