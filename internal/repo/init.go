// init.go — repo bootstrap (HSREPO write) and Open (HSREPO read +
// format-version gating).

package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"

	// Side-effect imports: register URL schemes. Every CLI command
	// that opens a repo gets every linked backend automatically.
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

// InitOptions controls what Init writes. Future fields (encryption ref,
// initial tenants, default retention) accrete here.
type InitOptions struct {
	URL string // canonical repo URL (file://..., s3://..., ...)

	// WORM, when non-nil + non-zero, is recorded in HSREPO and
	// propagates to every committed object's retention. Set at
	// init time only — there's no flag to flip WORM on later
	// (would create a mixed-fleet situation). Operators wanting
	// WORM on an existing repo migrate by initialising a new repo
	// + replicating into it.
	WORM *WORMPolicy

	// Compression picks the zstd level for new chunks; empty
	// means CompressionBalanced (v0.1..default).  See
	// the field doc on Metadata.Compression for the
	// trade-off table.
	Compression CompressionLevel
}

// InitResult is the structured outcome of a successful Init.
type InitResult struct {
	URL      string   `json:"url"`
	ID       string   `json:"id"`
	Schema   string   `json:"schema"`
	Metadata Metadata `json:"metadata"`
}

// Init creates a new repository at opts.URL. The operation is atomic and
// race-safe via Put(IfNotExists=true) on the HSREPO key — if another
// process raced us, exactly one of us wins.
//
// Returns a typed error chain whose error code maps to:
//
//	conflict.repo_exists      — HSREPO is already there
//	storage.unreachable       — backend Open failed
//	internal                  — anything unexpected
//
// Callers that want a specific error code use errors.Is on the returned error.
func Init(ctx context.Context, opts InitOptions) (*InitResult, error) {
	sp, err := storage.Open(ctx, opts.URL)
	if err != nil {
		return nil, fmt.Errorf("repo: open backend %q: %w", opts.URL, err)
	}
	defer sp.Close()

	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("repo: generate id: %w", err)
	}

	if opts.WORM != nil {
		if err := opts.WORM.Validate(); err != nil {
			return nil, fmt.Errorf("repo: invalid WORM policy: %w", err)
		}
	}
	if err := opts.Compression.Validate(); err != nil {
		return nil, fmt.Errorf("repo: invalid compression level: %w", err)
	}
	meta := Metadata{
		Schema:      SchemaRepo,
		ID:          id,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		ToolVersion: version.Version,
		WORM:        opts.WORM,
		Compression: opts.Compression,
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("repo: marshal metadata: %w", err)
	}
	body = append(body, '\n')

	_, err = sp.Put(ctx, HSREPOFilename, bytes.NewReader(body), storage.PutOptions{
		IfNotExists:   true,
		ContentLength: int64(len(body)),
	})
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("repo: write HSREPO: %w", err)
	}

	// Stamp the on-disk-format marker.  Best-effort: a failure here
	// doesn't unwind the HSREPO write — the marker is the canary for
	// FUTURE forward-format checks, and a missing marker is treated
	// by Open() as "v1.0" (the only format this binary writes today).
	// Operators concerned about the gap can re-stamp via a future
	// `repo set-version` command; the L4 test scenario for forward-
	// format gating exercises Open's refusal path, not Init's.
	rvBody, mErr := json.MarshalIndent(RepoVersion{
		Format:    RepoFormatV1_0,
		WrittenBy: "pg_hardstorage " + version.Version,
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if mErr == nil {
		rvBody = append(rvBody, '\n')
		_, _ = sp.Put(ctx, RepoVersionFilename, bytes.NewReader(rvBody), storage.PutOptions{
			ContentLength: int64(len(rvBody)),
		})
	}

	return &InitResult{
		URL:      opts.URL,
		ID:       id,
		Schema:   SchemaRepo,
		Metadata: meta,
	}, nil
}

// ErrAlreadyExists is returned by Init when the URL already hosts a repo.
// It's a typed sentinel so the CLI can map it to exit code 7 (conflict).
var ErrAlreadyExists = errors.New("repo: HSREPO already exists at this URL")

// Open opens an existing repository at url and returns its parsed
// Metadata. Use this from any later operation that needs to verify "yes,
// this URL is a real pg_hardstorage repo and we agree on the schema".
func Open(ctx context.Context, url string) (*Metadata, storage.StoragePlugin, error) {
	sp, err := storage.Open(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: open backend %q: %w", url, err)
	}
	rc, err := sp.Get(ctx, HSREPOFilename)
	if err != nil {
		_ = sp.Close()
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, ErrNotARepo
		}
		return nil, nil, fmt.Errorf("repo: read HSREPO: %w", err)
	}
	defer rc.Close()

	body, err := stdio.ReadAll(rc)
	if err != nil {
		_ = sp.Close()
		return nil, nil, fmt.Errorf("repo: read HSREPO body: %w", err)
	}
	var meta Metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		_ = sp.Close()
		return nil, nil, fmt.Errorf("repo: parse HSREPO: %w", err)
	}
	if meta.Schema != SchemaRepo {
		_ = sp.Close()
		return nil, nil, fmt.Errorf("repo: schema %q is not supported; expected %q", meta.Schema, SchemaRepo)
	}

	// Forward-format gate.  Read _repo_version.json (if present) and
	// refuse if the marker advertises a format this binary doesn't
	// know.  Pre-v0.10 repos don't have the marker — treat absence
	// as RepoFormatV1_0 (the only format this binary writes today).
	// The check is the load-bearing part of the test scenario at
	// test/scenarios/L4_repo_format_forward_check.scenario.yaml.
	if rvc, rvErr := sp.Get(ctx, RepoVersionFilename); rvErr == nil {
		rvBody, _ := stdio.ReadAll(rvc)
		_ = rvc.Close()
		var rv RepoVersion
		if err := json.Unmarshal(rvBody, &rv); err != nil {
			_ = sp.Close()
			return nil, nil, fmt.Errorf("repo: parse %s: %w", RepoVersionFilename, err)
		}
		known := false
		for _, f := range SupportedRepoFormats {
			if rv.Format == f {
				known = true
				break
			}
		}
		if !known {
			_ = sp.Close()
			return nil, nil, &ErrRepoFormatUnsupported{Format: rv.Format, Supported: SupportedRepoFormats}
		}
	}

	return &meta, sp, nil
}

// ErrRepoFormatUnsupported is returned by Open when the repo's
// _repo_version.json advertises a format this binary doesn't
// know.  The error message names the format and points at the
// runbook so operators don't grep source for a fix.
type ErrRepoFormatUnsupported struct {
	Format    string
	Supported []string
}

// Error formats the unsupported-format diagnostic with the
// advertised format, the binary's known set, and a runbook
// pointer so operators don't have to grep source for a fix.
func (e *ErrRepoFormatUnsupported) Error() string {
	return fmt.Sprintf(
		"repo.format.future: repo advertises on-disk format %q which this binary does not support "+
			"(known: %v); upgrade pg_hardstorage to a release that lists %q in SupportedRepoFormats. "+
			"See docs/runbooks/repo-format-mismatch.md.",
		e.Format, e.Supported, e.Format)
}

// ErrNotARepo is returned by Open when the URL has no HSREPO.
var ErrNotARepo = errors.New("repo: no HSREPO at this URL")

// generateID returns a 32-character hex string from 16 random bytes.
func generateID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
