package cli_test

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// --- Bug 37: `repo scrub --repo <url>` accepted; conflict detected ---

// TestRepoScrub_RepoFlagAsAlternative is the #37 regression: --repo is
// registered and documented as an alternative to the positional <url>,
// so `repo scrub --repo <url>` must be accepted (it was rejected
// before because Args was ExactArgs(1)).
func TestRepoScrub_RepoFlagAsAlternative(t *testing.T) {
	w := newReadWorld(t)
	stdout, stderr, exit := runCLI(t, "repo", "scrub", "--repo", w.repoURL, "--full", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo scrub --repo should succeed: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"mismatch_count": 0`) {
		t.Errorf("expected a clean scrub result: %s", stdout)
	}
}

// TestRepoScrub_PositionalFlagConflict is the #37 regression: a
// positional <url> that disagrees with --repo must be a usage.repo_conflict
// error, not a silent scrub of the wrong URL.
func TestRepoScrub_PositionalFlagConflict(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"repo", "scrub", w.repoURL, "--repo", "file:///nonexistent-other", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("conflicting --repo and positional must not exit 0; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict; stderr=%s", stderr)
	}
}

// TestRepoScrub_PositionalFlagAgree: when --repo and the positional
// match, that's fine (no conflict).
func TestRepoScrub_PositionalFlagAgree(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"repo", "scrub", w.repoURL, "--repo", w.repoURL, "--full", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("matching --repo and positional should succeed; exit=%d stderr=%s", exit, stderr)
	}
}

// --- Bug 63: status chain-event count excludes per-shard head pointers ---

// TestStatus_ChainEventCount_ExcludesShardHeads is the #63 regression:
// per-shard head pointers (audit/shards/<shard>/_head.json) are perf
// caches, not chain events, and must not inflate chain_event_count.
func TestStatus_ChainEventCount_ExcludesShardHeads(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	store := audit.NewStore(w.sp)

	// Two deployment-scoped events → two distinct shards, each of which
	// writes its own audit/shards/<shard>/_head.json head pointer.
	for _, dep := range []string{"sharda", "shardb"} {
		ev := &audit.Event{Action: "scoped.tick"}
		ev.Subject.Deployment = dep
		if err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr, exit := runCLI(t, "status", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var body struct {
		AuditAnchor struct {
			ChainEventCount int `json:"chain_event_count"`
		} `json:"audit_anchor"`
	}
	bodyOf(t, stdout, &body)
	// Exactly the two scoped events count — NOT the two shard head
	// pointers (which the old code counted, yielding 4).
	if body.AuditAnchor.ChainEventCount != 2 {
		t.Errorf("chain_event_count = %d, want 2 (per-shard _head.json must be excluded)\n%s",
			body.AuditAnchor.ChainEventCount, stdout)
	}
}

// TestStatus_AnchorFresh_UsesHeadPointer is the #39 regression: status
// judges anchor freshness against the authoritative head pointer (like
// doctor), not the chainKeys-1 event count. A freshly-anchored chain
// must read as fresh.
func TestStatus_AnchorFresh_UsesHeadPointer(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	store := audit.NewStore(w.sp)
	log := audit.NewStorageBackedLog(w.sp)

	for i := 0; i < 4; i++ {
		if err := store.Append(ctx, &audit.Event{Action: "test.tick"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Anchor(ctx, log, "status-test"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runCLI(t, "status", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status exit=%d\nstderr=%s", exit, stderr)
	}
	var body struct {
		AuditAnchor struct {
			Present      bool `json:"present"`
			Fresh        bool `json:"fresh"`
			BehindEvents int  `json:"behind_events"`
		} `json:"audit_anchor"`
	}
	bodyOf(t, stdout, &body)
	if !body.AuditAnchor.Present || !body.AuditAnchor.Fresh {
		t.Errorf("anchor should be present and fresh; got %+v\n%s", body.AuditAnchor, stdout)
	}
	if body.AuditAnchor.BehindEvents != 0 {
		t.Errorf("fresh anchor must report 0 behind_events; got %d", body.AuditAnchor.BehindEvents)
	}
}

// --- Bug 62: repo bundle export flushes + closes before reporting success ---

// TestRepoBundle_Export_FsyncsBeforeSuccess is the #62 regression: the
// export must flush and close the output file BEFORE reporting success,
// so a completed export leaves a fully-written tar on disk (not a
// truncated one hidden behind a deferred, error-ignored Close). We
// prove completeness by round-tripping the exported bundle through
// import into a second repo.
func TestRepoBundle_Export_FsyncsBeforeSuccess(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	// commitManifest references a chunk for "17\n" but doesn't store
	// its bytes; export copies chunks, so put it into the CAS first.
	if _, err := casdefault.New(w.sp).PutChunk(context.Background(), []byte("17\n")); err != nil {
		t.Fatalf("put chunk: %v", err)
	}

	tmp := t.TempDir()
	out := filepath.Join(tmp, "bundle.tar")
	stdout, stderr, exit := runCLI(t,
		"repo", "bundle", "export",
		"--repo", w.repoURL,
		"--deployment", "db1",
		"--out", out,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("export exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("export reported success but no output file: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatalf("export produced an empty tar despite success")
	}
	// The tar must be COMPLETE: walkable end-to-end without an
	// unexpected EOF. A close-time flush failure (the #62 bug) would
	// leave the final blocks unwritten and tripping io.ErrUnexpectedEOF
	// here — the whole point of flushing + closing before success.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	entries := 0
	for {
		_, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			t.Fatalf("exported tar is truncated / malformed: %v", terr)
		}
		entries++
	}
	if entries == 0 {
		t.Fatalf("exported tar has no entries — export was not fully written")
	}
}

// --- Bug 66: audit search --since accepts the "7d" day form ---

// TestAuditSearch_SinceDayForm is the #66 regression: the --since flag
// help advertises "7d" but time.ParseDuration rejected the day unit,
// so `audit search --since 7d` failed with a usage error. It must now
// be accepted and return the seeded events.
func TestAuditSearch_SinceDayForm(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, stderr, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--since", "7d", "--output", "json")
	if exit != 0 {
		t.Fatalf("audit search --since 7d must be accepted; exit=%d\nstderr=%s", exit, stderr)
	}
	// All 7 seeded events fall inside the last 7 days.
	if !strings.Contains(stdout, `"count": 7`) {
		t.Errorf("--since 7d should include all recent events; got:\n%s", stdout)
	}
}

// TestAuditSearch_SinceGarbageStillRejected: a genuinely bad --since
// value is still a usage error (the day-form fix didn't loosen
// validation).
func TestAuditSearch_SinceGarbageStillRejected(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, stderr, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--since", "notaduration", "--output", "json")
	if exit == 0 {
		t.Fatalf("garbage --since should be rejected; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag; stderr=%s", stderr)
	}
}

// --- Bug 65: list --include-deleted counts key mismatches + notice ---

// TestList_IncludeDeleted_CountsKeyMismatch is the #65 regression: the
// --include-deleted / --only-deleted branch previously counted a
// verification failure only into `skipped` and never checked
// ErrPublicKeyMismatch, so the "signed with a DIFFERENT key" notice
// silently disappeared under those flags. It must now count
// SignatureMismatches like the normal (live-only) path.
func TestList_IncludeDeleted_CountsKeyMismatch(t *testing.T) {
	w := newReadWorld(t)
	foreign, _, err := keystore.LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commitManifestSignedBy(t, w, "db1", foreign,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	stdout, stderr, exit := runCLI(t,
		"list", "db1", "--repo", w.repoURL, "--include-deleted", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list --include-deleted exit=%d\nstderr=%s", exit, stderr)
	}
	var body struct {
		SignatureMismatches int `json:"signature_mismatches"`
		Skipped             int `json:"skipped"`
	}
	bodyOf(t, stdout, &body)
	if body.SignatureMismatches != 1 {
		t.Errorf("signature_mismatches = %d, want 1 under --include-deleted\n%s",
			body.SignatureMismatches, stdout)
	}

	// Text mode must surface the operator-facing notice.
	txt, _, _ := runCLI(t,
		"list", "db1", "--repo", w.repoURL, "--include-deleted", "-o", "text")
	if !strings.Contains(txt, "DIFFERENT key") {
		t.Errorf("expected the 'signed with a DIFFERENT key' notice:\n%s", txt)
	}
}
