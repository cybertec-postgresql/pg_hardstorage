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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

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
		{"RenameIfNotExists_AcrossPrefix", caseRenameAcrossPrefix},
		{"ContentSHA256_MatchesAdvertisedCapability", caseContentSHA256Honesty},
		{"WORM_MatchesAdvertisedCapability", caseWORMHonesty},
		{"StorageClass_MatchesAdvertisedCapability", caseStorageClassHonesty},
		{"DurabilityBarrier_MatchesAdvertisedCapability", caseDurabilityBarrierHonesty},
		{"Multipart_HandlesLargeObject", caseMultipartLargeObject},
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

	// Concurrency cases are part of the CORE suite: a backend that
	// advertises ConditionalPut but loses the single-winner race is a
	// data-loss bug (the scp `mv -T` emulation silently forked the
	// shared DEK and destroyed committed audit events — concurrency
	// audit / issue #31 class). No backend gets to "not yet meet"
	// these; the only accepted out is HONESTY (ConditionalPut=false),
	// which ParallelPuts skips-with-reason on.
	t.Run("ParallelPuts_SingleWinner", func(t *testing.T) {
		ParallelPuts(t, open, 8)
	})
	t.Run("ParallelOverwrites_NoTornContent", func(t *testing.T) {
		ParallelOverwrites(t, open, 8)
	})
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

// caseRenameAcrossPrefix renames into a prefix that does not exist
// yet.  Every other rename case keeps src and dst under the SAME
// prefix, which on a filesystem-backed backend means the destination
// directory already exists — so the suite could not distinguish a
// backend that creates the parent from one that requires it.
//
// It could not: fs:// created it (os.MkdirAll) and s3:// has no
// directories at all, while scp:// and sftp:// failed with a bare
// "No such file or directory".  Same documented operation, four
// backends, two answers.  Callers today happen to rename within one
// directory (a manifest's staging file sits beside its final key), so
// nothing broke — but a caller that did not would work on a local repo
// and fail on an SSH one.
func caseRenameAcrossPrefix(t *testing.T, p storage.StoragePlugin) {
	const body = "moved-across-prefixes"
	putString(t, p, "renx/staging/obj", body)
	// "renx/committed/" has never been written to.
	if err := p.RenameIfNotExists(context.Background(),
		"renx/staging/obj", "renx/committed/obj"); err != nil {
		t.Fatalf("RenameIfNotExists into a fresh prefix: %v", err)
	}
	if got := getString(t, p, "renx/committed/obj"); got != body {
		t.Errorf("body at dst: got %q, want %q", got, body)
	}
	if _, err := p.Stat(context.Background(), "renx/staging/obj"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(src) after rename: err = %v, want ErrNotFound", err)
	}
}

// caseContentSHA256Honesty pins Capabilities.VerifiesContentSHA256 to
// actual behaviour.
//
// PutOptions.ContentSHA256 is optional-by-capability: only a backend
// that advertises VerifiesContentSHA256 promises to verify the
// post-write hash and return ErrChecksumMismatch. Today only fs does;
// S3 / Azure / GCS / SFTP / SCP deliberately ignore the field and lean
// on transport-layer integrity, and internal/repo.CAS skips computing
// the hash for them (it costs a second full SHA-256 pass per chunk).
//
// The risk is a plugin that advertises the capability but silently
// drops the field. Callers gate on the capability, so they would
// believe they had post-write verification while nothing checked
// anything. This asserts the two agree, in whichever direction the
// backend claims.
func caseContentSHA256Honesty(t *testing.T, p storage.StoragePlugin) {
	if !p.Capabilities().VerifiesContentSHA256 {
		t.Skip("backend does not advertise VerifiesContentSHA256; the field is optional for it")
	}
	body := []byte("content-sha256-honesty")
	var wrong [32]byte
	wrong[0] = 0x01 // cannot be the SHA-256 of anything we just wrote

	_, err := p.Put(context.Background(), "sha/mismatch", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body)), ContentSHA256: wrong})
	if err == nil {
		t.Fatal("backend advertises VerifiesContentSHA256 but accepted a mismatched ContentSHA256")
	}
	if !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Errorf("mismatched ContentSHA256: err = %v, want ErrChecksumMismatch", err)
	}
	// A rejected write must not leave a readable object behind.
	if rc, gerr := p.Get(context.Background(), "sha/mismatch"); gerr == nil {
		_ = rc.Close()
		t.Error("rejected Put left a readable object at the key")
	}

	// The matching case must still succeed, so the check is not simply
	// refusing every ContentSHA256.
	good := sha256.Sum256(body)
	if _, err := p.Put(context.Background(), "sha/match", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body)), ContentSHA256: good}); err != nil {
		t.Errorf("Put with a CORRECT ContentSHA256 failed: %v", err)
	}
}

// --- Capability honesty -------------------------------------------------
//
// Capabilities is a set of PROMISES the rest of the codebase acts on:
// cas.go refuses to write a WORM-required repo to a backend without
// Capabilities().WORM, replicate.go gates cross-region copies the same
// way, and runner.go warns when a backend can neither fsync on demand
// nor guarantee inline durability. A backend that advertises a
// capability it does not deliver turns each of those gates into a
// silent lie — the caller checks, sees true, and proceeds.
//
// Until this block, only two of the nine flags were verified
// (ConditionalPut and VerifiesContentSHA256). The cases below add the
// ones that are OBSERVABLE through the plugin interface.
//
// Three flags are deliberately NOT asserted, because nothing a client
// can call distinguishes them:
//
//   - ServerSideEncryption — whether the server encrypts at rest is
//     invisible to a client that only Puts and Gets.
//   - CrossRegionReplicate — asserting it would need a second region
//     and a replication-lag wait; it belongs in the replicate suite,
//     not the per-object contract.
//   - InlineDurable — "durable the moment Put returns" can only be
//     falsified by cutting power mid-write.
//
// Writing assertions for those would be coverage theatre. They are
// listed here so the omission is a recorded decision rather than an
// oversight.

// caseWORMHonesty pins Capabilities().WORM to SetRetention's behaviour.
// A backend claiming WORM must not answer ErrUnsupported: cas.go and
// replicate.go both refuse to proceed without the flag, so a false
// positive here means a regulated repo silently writes unlocked
// objects while reporting worm.active.
func caseWORMHonesty(t *testing.T, p storage.StoragePlugin) {
	putString(t, p, "worm/obj", "retained")
	until := time.Now().Add(24 * time.Hour)
	err := p.SetRetention(context.Background(), "worm/obj", until, storage.WORMGovernance)

	if !p.Capabilities().WORM {
		// Not advertised: ErrUnsupported is the correct answer. Anything
		// else is fine too (a backend may support retention without
		// claiming the flag), so only the inverse is a failure.
		return
	}
	if errors.Is(err, storage.ErrUnsupported) {
		t.Fatal("backend advertises Capabilities().WORM but SetRetention returned ErrUnsupported")
	}
	// Any OTHER error is an environment condition, not a broken promise.
	// Capabilities().WORM says "this plugin implements retention", not
	// "this bucket has it switched on" — S3 needs the bucket created with
	// ObjectLockConfiguration, which the shared contract fixture is not.
	// Asserting err == nil here would make the case fail on bucket setup
	// and say "capability lie", which is the wrong diagnosis. Enforcement
	// against a real Object-Lock bucket lives in
	// internal/backup/worm_objectlock_integration_test.go.
	if err != nil {
		t.Logf("SetRetention returned %v — accepted: the capability is implemented, "+
			"the fixture bucket simply has no Object Lock configured", err)
	}
}

// caseStorageClassHonesty pins Capabilities().StorageClassSelectable to
// PutOptions.StorageClass actually being accepted. When the backend
// also reports a class back through Stat, the round-trip is checked —
// a backend that silently drops the requested class would otherwise
// look identical to one that honoured it.
func caseStorageClassHonesty(t *testing.T, p storage.StoragePlugin) {
	if !p.Capabilities().StorageClassSelectable {
		t.Skip("backend does not advertise StorageClassSelectable")
	}
	const key = "sclass/obj"
	body := "stored-with-a-class"
	if _, err := p.Put(context.Background(), key, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body)), StorageClass: "STANDARD"}); err != nil {
		t.Fatalf("Put with StorageClass on a backend advertising selectability: %v", err)
	}
	if got := getString(t, p, key); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	oi, err := p.Stat(context.Background(), key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Reporting the class back is optional; contradicting it is not.
	if oi.StorageClass != "" && oi.StorageClass != "STANDARD" {
		t.Errorf("Stat reports StorageClass %q after writing STANDARD", oi.StorageClass)
	}
}

// caseDurabilityBarrierHonesty pins Capabilities().DurabilityBarrier to
// Barrier actually being implemented. Callers batch DurabilityDeferred
// writes and pay one barrier for all of them before committing a
// manifest that references those chunks; a backend that advertises the
// barrier but inherits the no-op NopBarrier would let that manifest
// commit over bytes still sitting in a page cache.
//
// Barrier itself cannot be proven to fsync from user space, so what is
// checked is the observable half: it must succeed, and the deferred
// bytes must be readable afterwards.
func caseDurabilityBarrierHonesty(t *testing.T, p storage.StoragePlugin) {
	const key = "durable/deferred"
	body := "written-deferred-then-barriered"
	if _, err := p.Put(context.Background(), key, strings.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		Durability:    storage.DurabilityDeferred,
	}); err != nil {
		t.Fatalf("deferred Put: %v", err)
	}

	err := p.Barrier(context.Background())
	if p.Capabilities().DurabilityBarrier {
		if err != nil {
			t.Fatalf("backend advertises DurabilityBarrier but Barrier failed: %v", err)
		}
	} else if err != nil {
		// A backend without the capability is expected to no-op, not error.
		t.Errorf("Barrier on a non-barrier backend returned %v, want nil (no-op)", err)
	}

	if got := getString(t, p, key); got != body {
		t.Errorf("after Barrier: body = %q, want %q", got, body)
	}
}

// caseMultipartLargeObject exercises the functional consequence of
// Capabilities().Multipart: an object past the point where a backend
// would switch to a chunked upload strategy must round-trip byte-for-
// byte. The strategy itself is internal, so this asserts the outcome
// rather than the mechanism — and it runs everywhere, because a
// backend that does NOT advertise multipart still has to store a large
// object correctly, just by another route.
func caseMultipartLargeObject(t *testing.T, p storage.StoragePlugin) {
	const size = 6 << 20 // past the common 5 MiB multipart threshold
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251) // 251 is prime: no alignment with any block size
	}
	sum := sha256.Sum256(body)

	if _, err := p.Put(context.Background(), "large/obj", bytes.NewReader(body),
		storage.PutOptions{ContentLength: size}); err != nil {
		t.Fatalf("Put of a %d-byte object: %v", size, err)
	}
	rc, err := p.Get(context.Background(), "large/obj")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if len(got) != size {
		t.Fatalf("read %d bytes, wrote %d", len(got), size)
	}
	if sha256.Sum256(got) != sum {
		t.Error("large object round-tripped with different content")
	}
	oi, err := p.Stat(context.Background(), "large/obj")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if oi.Size != size {
		t.Errorf("Stat.Size = %d, want %d", oi.Size, size)
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

// ParallelPuts exercises N concurrent IfNotExists Puts to the same key
// — exactly ONE must win. This is a MANDATORY case (wired into Run):
// single-winner consumers (the shared-DEK mint, the backup lease,
// audit-chain event slots) silently lose data over a backend that
// claims ConditionalPut but emulates it with check-then-write.
//
// The only accepted out is honesty: a backend whose
// Capabilities().ConditionalPut is FALSE is skipped with a reason —
// it makes no single-winner promise and the callers that need one
// degrade loudly at runtime. Claiming true and failing is a red build,
// never a tolerated known-failure.
func ParallelPuts(t *testing.T, open PluginOpener, n int) {
	t.Helper()
	if n < 2 {
		n = 8
	}
	p := open(t)
	if !p.Capabilities().ConditionalPut {
		t.Skipf("backend %q honestly reports ConditionalPut=false — single-winner semantics not claimed (callers degrade loudly)", p.Name())
	}
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
