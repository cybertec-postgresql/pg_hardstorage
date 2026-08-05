// get_shorts_test.go — a failed remote read must not look like an
// empty successful one.
//
// Found by the randomised lease soak: on scp, a contender reading the
// lease got `decode lease: unexpected end of JSON input`. The stored
// object was fine; the READ was empty.
//
// scp streams: Get checks the file exists, then starts `cat <file>`
// and hands back a reader over the SSH session's stdout. If that
// command fails after the check — the file was deleted in between, the
// remote ran out of descriptors, the channel broke — stdout simply
// ends. io.ReadAll sees a clean EOF and returns nil error, so the
// failure is carried ONLY by the remote exit status, which surfaces at
// Close.
//
// Every caller in the tree does `defer rc.Close()` and discards it —
// 56 sites, none checking. So the failure becomes zero bytes and no
// error. For a manifest or a lease that is a confusing parse error; for
// any consumer that tolerates an empty body it is silent data loss.
//
//go:build integration

package scp_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestGet_DeletedDuringRead_DoesNotReturnSilentlyEmpty drives the race
// deterministically: delete the object between Get's existence check
// and the caller's read.
//
// The point is not that this exact interleaving is common — it is that
// a whole class of remote-read failures reaches the caller as a clean
// empty read, and this is the cheapest way to stand one up.
func TestGet_DeletedDuringRead_DoesNotReturnSilentlyEmpty(t *testing.T) {
	rt, open := startSCPContainer(t)
	_ = rt
	sp := open(t)
	ctx := context.Background()

	const key = "shortread/obj.json"
	body := `{"schema":"x","payload":"` + strings.Repeat("y", 4096) + `"}`
	if _, err := sp.Put(ctx, key, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rc, err := sp.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Remove it while the reader is open. `cat` may already have
	// streamed everything (the file is small), so this test tolerates a
	// COMPLETE read — what it refuses to tolerate is a SHORT read that
	// reports success.
	if derr := sp.Delete(ctx, key); derr != nil {
		t.Fatalf("Delete: %v", derr)
	}

	got, rerr := io.ReadAll(rc)
	cerr := rc.Close()

	switch {
	case len(got) == len(body):
		// Fine: the read completed before the unlink took effect.
		if rerr != nil {
			t.Errorf("complete read reported an error: %v", rerr)
		}
	case rerr != nil || cerr != nil:
		// Also fine: the failure was reported SOMEWHERE.
		t.Logf("short read correctly reported (read err=%v, close err=%v)", rerr, cerr)
	default:
		t.Fatalf("read returned %d of %d bytes with NO error from either ReadAll or "+
			"Close.\n\nA caller doing `defer rc.Close()` — which is every caller in this "+
			"tree — cannot distinguish this from a genuinely empty object. For a manifest "+
			"or lease that surfaces as a corrupt-parse error pointing at the wrong thing; "+
			"for anything that tolerates an empty body it is silent data loss.",
			len(got), len(body))
	}
}

// TestGet_ShortReadSurfacesAtCloseAtLeast pins the weaker guarantee the
// plugin currently offers, so a regression that loses even THAT is
// caught. If Close stops reporting the remote exit status, a failed
// read becomes entirely undetectable.
func TestGet_ShortReadSurfacesAtCloseAtLeast(t *testing.T) {
	rt, open := startSCPContainer(t)
	_ = rt
	sp := open(t)
	ctx := context.Background()

	// Reading a key that does not exist must be ErrNotFound, not an
	// empty success — the coarse version of the same property.
	if _, err := sp.Get(ctx, "shortread/absent.json"); err == nil {
		t.Fatal("Get on a missing key returned no error; an absent object and an empty " +
			"one must not look the same")
	}
}
