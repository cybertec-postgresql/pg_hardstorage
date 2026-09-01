package cli

// scrub_window_coverage_test.go — the end-to-end half of the rotation
// fix: rot outside the first window must eventually be found.
//
// The arithmetic is pinned in scrub_window_test.go. This asserts the
// property an operator actually depends on: a chunk that is NOT in the
// first window is invisible to run 0 and IS found by the run whose
// window contains it. Before the fix, every run was run 0, so a
// corrupted chunk anywhere past `limit` positions into the walk was
// never re-hashed by the tool whose only job is to find bit rot.

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// scrubWorld plants one deployment with n single-chunk files, so the
// walk order over chunks is exactly file order and a window is easy to
// reason about.
func scrubWorld(t *testing.T, n int) (storage.StoragePlugin, []repo.Hash) {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	// Sign with the SAME keypair loadVerifier() will hand the scrub.
	// A fixture signed by an ad-hoc keypair is skipped by ms.List as
	// unverifiable, and the scrub then samples nothing at all — which
	// looks exactly like a passing test.
	setTestKeyringHome(t)
	pth, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := keystore.LoadOrGenerate(pth.Keyring.Value)
	if err != nil {
		t.Fatal(err)
	}

	cas := casdefault.New(sp)
	files := make([]backup.FileEntry, 0, n)
	hashes := make([]repo.Hash, 0, n)
	for i := 0; i < n; i++ {
		body := []byte{byte('a' + i), byte(i), 'c', 'h', 'u', 'n', 'k'}
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, info.Hash)
		files = append(files, backup.FileEntry{
			Path: "base/f" + string(rune('0'+i)), Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		})
	}
	ts := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.20260430T120000Z.aaaa",
		Deployment: "db1", Tenant: "default", Type: backup.BackupTypeFull,
		PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: ts, StoppedAt: ts.Add(time.Minute),
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       files,
	}
	if err := backup.NewManifestStore(sp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return sp, hashes
}

// rotChunk replaces a chunk's stored bytes with garbage — bit rot. The
// delete is required: a content-addressed chunk Put is conditional, so
// writing over a live key without removing it first is a no-op and the
// "corruption" never lands.
func rotChunk(t *testing.T, sp storage.StoragePlugin, h repo.Hash, garbage string) {
	t.Helper()
	key := repo.ChunkKey(h)
	if err := sp.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), key, strings.NewReader(garbage),
		storage.PutOptions{ContentLength: int64(len(garbage))}); err != nil {
		t.Fatal(err)
	}
}

func TestScrubManifestAware_RotationFindsRotOutsideTheFirstWindow(t *testing.T) {
	const chunks, limit = 8, 2 // 4 windows
	sp, hashes := scrubWorld(t, chunks)

	// Corrupt a chunk in the LAST window — position 7 of 8, which the
	// pre-fix scrub (always window 0, chunks 0-1) could never reach.
	victim := hashes[7]
	rotChunk(t, sp, victim, "rotted-bytes-that-do-not-hash-to-the-key")

	// Window 0 is the slice the old code examined on EVERY run.
	agg0, total, err := scrubManifestAware(context.Background(), sp, limit, 0)
	if err != nil {
		t.Fatalf("scrub window 0: %v", err)
	}
	if total != chunks {
		t.Fatalf("referenced total = %d, want %d", total, chunks)
	}
	if len(agg0.Mismatches) != 0 {
		t.Fatalf("window 0 found the rot; the fixture does not exercise the blind spot")
	}
	if agg0.WindowCount != 4 {
		t.Fatalf("WindowCount = %d, want 4", agg0.WindowCount)
	}

	// Somewhere in a full cycle, a run must find it.
	found := false
	for w := 0; w < agg0.WindowCount; w++ {
		agg, _, err := scrubManifestAware(context.Background(), sp, limit, w)
		if err != nil {
			t.Fatalf("scrub window %d: %v", w, err)
		}
		for _, h := range agg.Mismatches {
			if h == victim {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("a full rotation cycle never re-hashed the corrupted chunk.\n\n" +
			"Bit rot outside the first window is invisible to the only tool whose job " +
			"is to find it, while the output reports a sample percent that reads as " +
			"rotating coverage.")
	}
}

// A full scan (limit 0) must still see everything in one run.
func TestScrubManifestAware_FullScanUnaffectedByRotation(t *testing.T) {
	const chunks = 8
	sp, hashes := scrubWorld(t, chunks)
	rotChunk(t, sp, hashes[7], "rotted-bytes-that-do-not-hash-to-the-key")
	agg, _, err := scrubManifestAware(context.Background(), sp, 0, 12345)
	if err != nil {
		t.Fatalf("full scrub: %v", err)
	}
	if len(agg.Mismatches) != 1 {
		t.Fatalf("full scan found %d mismatches, want 1 — a whole-repo scan must not be "+
			"windowed", len(agg.Mismatches))
	}
	if agg.WindowCount != 1 {
		t.Errorf("WindowCount = %d on a full scan, want 1", agg.WindowCount)
	}
}

// A scrub that could not read a single manifest must not report a
// clean repo.
//
// This was found while building the rotation fixture above: an early
// version signed its manifest with an ad-hoc keypair, so ms.List
// rejected it, the walk skipped it, and the scrub returned
// "total=8 sampled=0 ok=0 mismatches=0, err=nil" — a clean bill of
// health over a repo it had never looked inside. The skip itself is
// right (one bad manifest must not stop the rest being scrubbed); the
// silence was not. The old comment said unreadable manifests "will
// surface in other tools (verify, list)", which is true and beside the
// point: the operator running scrub is running it AS their integrity
// check.
func TestScrubManifestAware_UnverifiableManifestsAreCounted(t *testing.T) {
	const chunks = 4
	sp, _ := scrubWorld(t, chunks)

	// A different keyring => the fixture's signature no longer
	// verifies, exactly as a tampered manifest would not.
	setTestKeyringHome(t)

	agg, total, err := scrubManifestAware(context.Background(), sp, 0, 0)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if agg.Sampled != 0 {
		t.Fatalf("fixture broken: sampled %d chunks, expected the manifest to be rejected", agg.Sampled)
	}
	if agg.UnverifiableManifests == 0 {
		t.Fatalf("scrub reported sampled=0, mismatches=%d, unverifiable=0 over %d referenced "+
			"chunks.\n\nThat is a clean bill of health for a repo whose every manifest failed "+
			"verification — the one answer an integrity check must never give.",
			len(agg.Mismatches), total)
	}
}
