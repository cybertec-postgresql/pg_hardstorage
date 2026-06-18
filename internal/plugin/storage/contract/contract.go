// Package contract defines the StoragePlugin behavioural
// contract as a runnable test suite.  Every backend
// implementation (fs, s3, gcs, azblob, sftp, scp) has a
// glue file that calls RunContract with a freshly-opened
// plugin; if the plugin honours every invariant, all
// assertions pass.
//
// Why this exists
// ---------------
// Backends drift.  S3, Azure, and GCS each have subtle
// differences from the documented StoragePlugin contract —
// eventual consistency, error envelope shapes, multipart
// vs single-PUT thresholds, idempotent-delete semantics.
// Without a single suite that exercises every documented
// invariant against every backend, an "I tested S3 against
// MinIO and it worked" claim doesn't extend to GCS or
// Azure, and a regression in one plugin slips through.
//
// This harness is that single source of truth.  It exercises:
//
//   - Put + Get round-trip — exact bytes back
//   - Put + Stat — Size, Key
//   - Get on missing key → ErrNotFound
//   - Stat on missing key → ErrNotFound
//   - Delete missing key → idempotent (no error)
//   - Delete then Get → ErrNotFound
//   - List empty prefix → empty stream
//   - List with prefix → only matching keys
//   - IfNotExists Put: first wins, subsequent → ErrAlreadyExists
//   - RenameIfNotExists src→dst happy path
//   - RenameIfNotExists with dst present → ErrAlreadyExists
//
// What it deliberately does NOT exercise
// --------------------------------------
//   - Performance / throughput characteristics — separate suite
//   - SetRetention / WORM — each backend has its own semantics
//   - Cross-region / replication invariants — out of contract scope
//   - Backend-specific surface (S3 storage class, Azure
//     immutability, ...) — covered by plugin-specific tests
//
// Usage:
//
//	func TestS3_Contract(t *testing.T) {
//	    contract.Run(t, func(t *testing.T) storage.StoragePlugin {
//	        // bring up MinIO, open fresh plugin, return it
//	    })
//	}
//
// The opener gets a *testing.T so it can register cleanup
// hooks (t.Cleanup) for emulator teardown.  A nil opener
// or one that returns a nil plugin fails the suite
// immediately.
package contract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// PluginOpener constructs a fresh plugin instance for one
// test case.  Called once per Run sub-test so cases don't
// pollute each other.  The opener owns lifecycle: register
// cleanup hooks via t.Cleanup as needed.
type PluginOpener func(t *testing.T) storage.StoragePlugin

// Run drives every contract case against a freshly-opened
// plugin.  Any failure surfaces with the contract clause
// name in the test output, so an operator reading CI logs
// can pinpoint which invariant the backend broke.
func Run(t *testing.T, open PluginOpener) {
	t.Helper()
	if open == nil {
		t.Fatal("contract.Run: opener is nil")
	}

	type tc struct {
		name string
		fn   func(t *testing.T, p storage.StoragePlugin)
	}
	cases := []tc{
		{"PutGet_RoundTrip", caseRoundTrip},
		{"Stat_AfterPut", caseStat},
		{"Get_MissingKey_ErrNotFound", caseGetMissing},
		{"Stat_MissingKey_ErrNotFound", caseStatMissing},
		{"Delete_MissingKey_Idempotent", caseDeleteIdempotent},
		{"Delete_ThenGet_ErrNotFound", caseDeleteThenGet},
		{"List_EmptyPrefix_OnFreshStore", caseListEmpty},
		{"List_WithPrefix_OnlyMatching", caseListPrefix},
		{"IfNotExists_FirstWins_OthersErr", caseIfNotExists},
		{"RenameIfNotExists_HappyPath", caseRenameHappy},
		{"RenameIfNotExists_DstPresent_ErrAlreadyExists", caseRenameDstPresent},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := open(t)
			if p == nil {
				t.Fatalf("contract.Run/%s: opener returned nil plugin", c.name)
			}
			c.fn(t, p)
		})
	}
}

// putString is a tiny helper — ~80% of contract cases do
// "put a literal string at a key" and we don't want the
// io.NopCloser wrapping to clutter every case body.
func putString(t *testing.T, p storage.StoragePlugin, key, body string) storage.PutResult {
	t.Helper()
	res, err := p.Put(context.Background(), key,
		bytes.NewReader([]byte(body)), storage.PutOptions{})
	if err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
	if res.Size != int64(len(body)) {
		t.Errorf("Put(%s): Size=%d, want %d", key, res.Size, len(body))
	}
	return res
}

// getString returns the byte body at key, or t.Fatal'd if
// the Get fails for any reason.
func getString(t *testing.T, p storage.StoragePlugin, key string) string {
	t.Helper()
	rc, err := p.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Get(%s) read: %v", key, err)
	}
	return string(body)
}

func caseRoundTrip(t *testing.T, p storage.StoragePlugin) {
	const body = "hello round-trip\n"
	putString(t, p, "rt/file", body)
	if got := getString(t, p, "rt/file"); got != body {
		t.Errorf("round-trip body mismatch: got %q, want %q", got, body)
	}
}

func caseStat(t *testing.T, p storage.StoragePlugin) {
	const body = "size-12-byte"
	putString(t, p, "stat/k", body)
	info, err := p.Stat(context.Background(), "stat/k")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != "stat/k" {
		t.Errorf("Stat.Key = %q, want stat/k", info.Key)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Stat.Size = %d, want %d", info.Size, len(body))
	}
}

func caseGetMissing(t *testing.T, p storage.StoragePlugin) {
	_, err := p.Get(context.Background(), "this/key/does/not/exist")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get on missing key: err = %v, want ErrNotFound", err)
	}
}

func caseStatMissing(t *testing.T, p storage.StoragePlugin) {
	_, err := p.Stat(context.Background(), "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat on missing key: err = %v, want ErrNotFound", err)
	}
}

func caseDeleteIdempotent(t *testing.T, p storage.StoragePlugin) {
	if err := p.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("Delete on missing key should be no-op (got %v)", err)
	}
}

func caseDeleteThenGet(t *testing.T, p storage.StoragePlugin) {
	putString(t, p, "del/k", "x")
	if err := p.Delete(context.Background(), "del/k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := p.Get(context.Background(), "del/k")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func caseListEmpty(t *testing.T, p storage.StoragePlugin) {
	count := 0
	for info, err := range p.List(context.Background(), "no-such-prefix/") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		_ = info
		count++
	}
	if count != 0 {
		t.Errorf("List on missing prefix returned %d objects, want 0", count)
	}
}

func caseListPrefix(t *testing.T, p storage.StoragePlugin) {
	// Populate two prefixes; assert List(prefix1) returns
	// only the prefix1 keys regardless of insertion order.
	putString(t, p, "a/1", "one")
	putString(t, p, "a/2", "two")
	putString(t, p, "b/3", "three")

	want := map[string]bool{"a/1": true, "a/2": true}
	got := map[string]bool{}
	for info, err := range p.List(context.Background(), "a/") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got[info.Key] = true
	}
	if len(got) != len(want) {
		t.Errorf("List(a/) returned %d objects (%v), want 2 (%v)", len(got), got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("List(a/) missing %q (got: %v)", k, got)
		}
	}
}

func caseIfNotExists(t *testing.T, p storage.StoragePlugin) {
	const key = "ifnotexists/k"
	const winner = "first-write-wins"
	const loser = "this-must-NOT-overwrite"

	// First Put with IfNotExists — wins.
	if _, err := p.Put(context.Background(), key,
		strings.NewReader(winner),
		storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("Put(IfNotExists) #1: %v", err)
	}
	// Second Put with IfNotExists — must error with ErrAlreadyExists.
	_, err := p.Put(context.Background(), key,
		strings.NewReader(loser),
		storage.PutOptions{IfNotExists: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("Put(IfNotExists) #2: err = %v, want ErrAlreadyExists", err)
	}
	// And the original body must still be there.
	if got := getString(t, p, key); got != winner {
		t.Errorf("body after losing IfNotExists: got %q, want %q", got, winner)
	}
}

func caseRenameHappy(t *testing.T, p storage.StoragePlugin) {
	const body = "to-be-renamed"
	putString(t, p, "ren/src", body)
	if err := p.RenameIfNotExists(context.Background(), "ren/src", "ren/dst"); err != nil {
		t.Fatalf("RenameIfNotExists: %v", err)
	}
	// dst now has the body, src is gone.
	if got := getString(t, p, "ren/dst"); got != body {
		t.Errorf("body at dst: got %q, want %q", got, body)
	}
	if _, err := p.Stat(context.Background(), "ren/src"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(src) after rename: err = %v, want ErrNotFound", err)
	}
}

func caseRenameDstPresent(t *testing.T, p storage.StoragePlugin) {
	putString(t, p, "ren2/src", "src-body")
	putString(t, p, "ren2/dst", "dst-body-existing")
	err := p.RenameIfNotExists(context.Background(), "ren2/src", "ren2/dst")
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("RenameIfNotExists with dst present: err = %v, want ErrAlreadyExists", err)
	}
	// And dst is unchanged — the rename must NOT have
	// silently overwritten.  This is the manifest-commit
	// safety property the agent relies on.
	if got := getString(t, p, "ren2/dst"); got != "dst-body-existing" {
		t.Errorf("dst body after refused rename: got %q, want unchanged", got)
	}
}

// ParallelPuts is an extra-stress contract case kept
// separate so plugins that don't yet meet it don't fail
// the core suite.  Exercises N concurrent IfNotExists
// Puts to the same key — exactly ONE must win.  Backends
// that emulate IfNotExists by check-then-write under
// concurrent access often fail this.
func ParallelPuts(t *testing.T, open PluginOpener, n int) {
	t.Helper()
	if n < 2 {
		n = 8
	}
	p := open(t)
	const key = "parallel/k"
	var (
		wg       sync.WaitGroup
		winnerCt int
		loserCt  int
		mu       sync.Mutex
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("body-%d", i)
			_, err := p.Put(context.Background(), key,
				strings.NewReader(body),
				storage.PutOptions{IfNotExists: true})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winnerCt++
			case errors.Is(err, storage.ErrAlreadyExists):
				loserCt++
			default:
				t.Errorf("ParallelPuts: unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()
	if winnerCt != 1 {
		t.Errorf("ParallelPuts: expected exactly 1 winner, got %d (losers=%d)", winnerCt, loserCt)
	}
	if winnerCt+loserCt != n {
		t.Errorf("ParallelPuts: outcomes don't sum: w=%d l=%d n=%d", winnerCt, loserCt, n)
	}
}

// ParallelOverwrites is an extra-stress contract case (like ParallelPuts).
// It hammers N concurrent OVERWRITE Puts (IfNotExists unset) at the SAME
// key, each writing a distinct full-length body, then asserts the final
// object equals exactly ONE writer's body — every byte, full length —
// never a torn/interleaved mix. A backend that stages an overwrite at a
// FIXED tmp path shared across concurrent writers (the fs putOverwrite
// bug) publishes torn content and fails here.
func ParallelOverwrites(t *testing.T, open PluginOpener, n int) {
	t.Helper()
	if n < 2 {
		n = 8
	}
	p := open(t)
	const key = "parallel-ow/k"
	const size = 8192
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte('A' + i)}, size)
			if _, err := p.Put(context.Background(), key, bytes.NewReader(body),
				storage.PutOptions{ContentLength: size}); err != nil {
				t.Errorf("ParallelOverwrites: put #%d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	rc, err := p.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("ParallelOverwrites: Get: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ParallelOverwrites: read: %v", rerr)
	}
	if len(got) != size {
		t.Errorf("ParallelOverwrites: final body len=%d, want %d — TORN write (concurrent overwrites shared a tmp)", len(got), size)
		return
	}
	first := got[0]
	for j, b := range got {
		if b != first {
			t.Errorf("ParallelOverwrites: TORN body — byte[%d]=%q but byte[0]=%q (mixed concurrent overwrites)", j, b, first)
			return
		}
	}
	if first < 'A' || first >= byte('A'+n) {
		t.Errorf("ParallelOverwrites: final byte %q is not from any writer", first)
	}
}
