// wipe.go — Wipe: tally + delete every object under a repo (chunks/manifests/audit/approvals/WAL).
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// WipeResult records what Wipe deleted, for the operator's audit /
// CLI Result body. Counts cover the four named prefixes (chunks,
// manifests, audit chain, approvals) plus a catch-all for the
// "anything else still in the repo" bucket.
type WipeResult struct {
	Schema           string `json:"schema"`
	Chunks           int    `json:"chunks"`
	Manifests        int    `json:"manifests"`
	Audit            int    `json:"audit"`
	Approvals        int    `json:"approvals"`
	WAL              int    `json:"wal"`
	Other            int    `json:"other"`
	Total            int    `json:"total"`
	HSREPORemoved    bool   `json:"hsrepo_removed"`
	UnreachableKeys  int    `json:"unreachable_keys,omitempty"` // delete failed mid-walk
	FirstUnreachable string `json:"first_unreachable,omitempty"`
}

// WipeSchema is the on-disk version tag for WipeResult bodies.
const WipeSchema = "pg_hardstorage.repo.wipe.v1"

// Wipe permanently deletes every object in the repo, INCLUDING the
// HSREPO marker. This is the most destructive operation the binary
// supports — every backup, every audit event, every approval, every
// chunk: gone.
//
// The CLI gates on a mandatory n-of-m approval + a typed
// confirmation. This function does NOT enforce that — callers are
// responsible. We're just the worker.
//
// Implementation: walk the storage plugin's root prefix and delete
// every key. We don't depend on any specific layout — anything
// under the repo root is fair game (the operator asked for a wipe).
//
// HSREPO is deleted LAST. While it's still there, a concurrent
// operator who tries to Open the URL gets a normal repo (with
// objects steadily disappearing); after HSREPO is gone they see
// ErrNotARepo. This ordering is operator-friendly: if a wipe
// crashes mid-walk, the repo "looks like a real repo" until the
// next wipe finishes the job.
//
// onProgress is called with each key as it's about to be deleted —
// useful for emitting events through the dispatcher. Pass nil for
// silent operation.
func Wipe(ctx context.Context, sp storage.StoragePlugin, onProgress func(key string)) (*WipeResult, error) {
	if sp == nil {
		return nil, errors.New("repo: Wipe requires a non-nil StoragePlugin")
	}
	res := &WipeResult{Schema: WipeSchema}

	// Pass 1: collect every key. Sorting + deferred delete keeps the
	// HSREPO-deleted-last invariant simple.
	var keys []string
	for info, err := range sp.List(ctx, "") {
		if err != nil {
			return res, fmt.Errorf("repo wipe: list: %w", err)
		}
		keys = append(keys, info.Key)
	}

	// Pass 2: delete every non-HSREPO key. HSREPO last.  The
	// `_repo_version.json` marker is treated like HSREPO: we hold
	// it for the very-last step so a partially-wiped repo can
	// still be recognised as one (a marker without HSREPO would
	// leave the repo in a "no HSREPO but format=" limbo).
	for _, k := range keys {
		if k == HSREPOFilename || k == RepoVersionFilename {
			continue
		}
		if onProgress != nil {
			onProgress(k)
		}
		if err := sp.Delete(ctx, k); err != nil {
			res.UnreachableKeys++
			if res.FirstUnreachable == "" {
				res.FirstUnreachable = k
			}
			// Don't bail — finish the walk so the result reflects
			// what we actually deleted vs what we couldn't.
			continue
		}
		switch {
		case strings.HasPrefix(k, "chunks/"):
			res.Chunks++
		case strings.HasPrefix(k, "manifests/"):
			res.Manifests++
		case strings.HasPrefix(k, "audit/"):
			res.Audit++
		case strings.HasPrefix(k, "approvals/"):
			res.Approvals++
		case strings.HasPrefix(k, "wal/"):
			res.WAL++
		default:
			res.Other++
		}
		res.Total++
	}

	// Pass 3: HSREPO. Only attempt if everything else cleared (or
	// only failed for unreachable-key reasons we already counted).
	// Refusing to remove HSREPO when other keys couldn't be deleted
	// is operator-friendly — a half-wiped repo that still says "I'm
	// a real repo" is recoverable; a half-wiped repo without an
	// HSREPO is just a pile of orphan chunks the operator has to
	// puzzle out.
	if res.UnreachableKeys == 0 {
		// _repo_version.json gets removed before HSREPO so that
		// if HSREPO removal then fails, the operator's recovery
		// path is "manually rm HSREPO" — not "puzzle out why a
		// format marker exists without an HSREPO."  Best-effort:
		// missing marker (pre-v0.10 repos, manually deleted) is
		// fine — Delete on a non-existent key is a no-op for
		// every backend, but we tolerate ErrNotFound just in
		// case a backend surfaces it.
		if err := sp.Delete(ctx, RepoVersionFilename); err != nil &&
			!errors.Is(err, storage.ErrNotFound) {
			res.UnreachableKeys++
			if res.FirstUnreachable == "" {
				res.FirstUnreachable = RepoVersionFilename
			}
		} else {
			res.Total++
		}
	}
	if res.UnreachableKeys == 0 {
		if err := sp.Delete(ctx, HSREPOFilename); err != nil {
			res.UnreachableKeys++
			if res.FirstUnreachable == "" {
				res.FirstUnreachable = HSREPOFilename
			}
		} else {
			res.HSREPORemoved = true
			res.Total++
		}
	}
	if res.UnreachableKeys > 0 {
		return res, fmt.Errorf("repo wipe: %d key(s) failed to delete (first: %s); HSREPO preserved",
			res.UnreachableKeys, res.FirstUnreachable)
	}
	return res, nil
}
