package cli_test

// tombstone_lifecycle_test.go — the undelete window, exercised as a
// whole rather than one command at a time.
//
// walprune.go promises: "a backup whose tombstone marker is YOUNGER
// than [the grace] still counts toward the WAL frontier, so the WAL it
// needs survives and a `backup undelete` inside the window can still
// recover it." backup_undelete.go promises the complement: "The window
// is bounded: once `repo gc --apply` has reclaimed the chunks the
// manifest references, undelete will succeed but the resulting backup
// will fail to restore."
//
// Each half is unit-tested in isolation — the frontier honours a young
// tombstone (TestWALPrune_YoungTombstoneKeepsWAL), gc honours the same
// grace, undelete flips a marker. Nothing runs the three tools in the
// sequence an operator actually lives: delete, then the scheduled
// gc + wal prune fire, then the operator notices the mistake and
// undeletes. The promise being tested is a property of the
// COMPOSITION: every tool shares one grace default, and the window is
// only real if all of them honour it together.
//
// Both directions are asserted, because each is a distinct failure:
//   - inside the window, anything reclaimed early makes undelete
//     resurrect a broken backup while reporting success;
//   - after the window, an undelete that LOOKS successful must at
//     least be caught by --check-chunks, or the operator gets a
//     phantom restore point.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitBackupLSN commits a signed, restorable backup with
// caller-chosen LSNs and a UNIQUE chunk body. Unique matters: a chunk
// shared between backups is kept alive by the survivor, and the
// reclamation half of the window would pass vacuously.
func commitBackupLSN(t *testing.T, w *readWorld, deployment, id, startLSN, stopLSN string, when time.Time) []byte {
	t.Helper()
	body := []byte("data-of-" + id + "\n")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         startLSN,
		StopLSN:          stopLSN,
		Timeline:         1,
		StartedAt:        when,
		StoppedAt:        when.Add(time.Minute),
		BackupLabel:      "START WAL LOCATION: " + startLSN + "\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf(body), Offset: 0, Len: int64(len(body))}}},
		},
	}
	if _, err := repo.NewCAS(w.sp).PutChunk(context.Background(), body); err != nil {
		t.Fatalf("seed chunk for %s: %v", id, err)
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
	return body
}

// segPresent reports whether a WAL segment manifest is still in the
// repo — the direct form of "the WAL it needs survives".
func segPresent(t *testing.T, repoURL, deployment string, tli uint32, segName string) bool {
	t.Helper()
	root := strings.TrimPrefix(repoURL, "file://")
	_, err := os.Stat(filepath.Join(root, "wal", deployment,
		fmt.Sprintf("%08X", tli), segName+".json"))
	return err == nil
}

// seedLifecycle builds the standing scene: B1 (old, segment 3) and B2
// (newer, segment 6), with WAL segments 2..7 archived. B1's
// consistency window lives in segment 3; everything below B2's
// frontier (segment 6) is prunable once B1 stops counting.
func seedLifecycle(t *testing.T, w *readWorld) (b1Body []byte, segs []string) {
	t.Helper()
	now := time.Now().UTC()
	b1Body = commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", now.Add(-48*time.Hour))
	commitBackupLSN(t, w, "db1", "b2", "0/6000028", "0/60001A0", now.Add(-1*time.Hour))
	for i := 2; i <= 7; i++ {
		segName := fmt.Sprintf("0000000100000000%08X", i)
		endLSN := fmt.Sprintf("0/%X000000", i+1)
		plantWALSegmentAtCLI(t, w.repoURL, "db1", 1, segName, endLSN, now.Add(-36*time.Hour))
		segs = append(segs, segName)
	}
	return b1Body, segs
}

// TestTombstoneWindow_UndeleteInsideGraceIsWhole is the promise.
func TestTombstoneWindow_UndeleteInsideGraceIsWhole(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	b1Body, _ := seedLifecycle(t, w)

	if _, errb, exit := runCLI(t, "backup", "delete", "db1", "b1",
		"--repo", w.repoURL, "--reason", "operator mistake", "--yes", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("delete b1: exit=%d\n%s", exit, errb)
	}

	// The scheduled janitors fire, defaults intact. This is the exact
	// sequence a cron runs between the mistake and its discovery.
	if _, errb, exit := runCLI(t, "repo", "gc",
		"--repo", w.repoURL, "--apply", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("repo gc --apply: exit=%d\n%s", exit, errb)
	}
	if _, errb, exit := runCLI(t, "wal", "prune", "db1",
		"--repo", w.repoURL, "--apply", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal prune --apply: exit=%d\n%s", exit, errb)
	}

	// B1's consistency WAL (segment 3) must have survived: the young
	// tombstone still holds the frontier down at B1's start_lsn.
	if !segPresent(t, w.repoURL, "db1", 1, "000000010000000000000003") {
		t.Fatalf("wal prune deleted segment 3 while b1's tombstone was inside the grace " +
			"window.\n\nThe frontier is supposed to stay at the YOUNG tombstone's start_lsn " +
			"precisely so an undelete inside the window recovers a backup that can still " +
			"reach consistency. With its start..stop WAL gone, the resurrected b1 restores " +
			"its files and then cannot replay to a consistent state — broken in the way " +
			"only a real recovery attempt would reveal.")
	}

	// Undelete with the pre-flight on: it must pass, because nothing
	// was reclaimed.
	if _, errb, exit := runCLI(t, "backup", "undelete", "db1", "b1",
		"--repo", w.repoURL, "--check-chunks", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("undelete inside the grace window failed: exit=%d\n%s\n\n"+
			"gc --apply ran before the undelete, so if this fails the grace was not "+
			"honoured for chunks.", exit, errb)
	}

	// And the resurrected backup is genuinely whole: restore it and
	// compare bytes.
	target := filepath.Join(t.TempDir(), "restored")
	if _, errb, exit := runCLI(t, "restore", "db1", "b1",
		"--repo", w.repoURL, "--target", target, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("restore of the undeleted b1: exit=%d\n%s", exit, errb)
	}
	got, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if string(got) != string(b1Body) {
		t.Errorf("restored bytes = %q, want %q", got, b1Body)
	}
}

// TestTombstoneWindow_ExpiryIsBoundedAndCaught is the complement: once
// the grace has passed, the janitors reclaim — and the failure mode an
// operator meets must be the loud pre-flight, not a quiet phantom.
func TestTombstoneWindow_ExpiryIsBoundedAndCaught(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	seedLifecycle(t, w)

	if _, errb, exit := runCLI(t, "backup", "delete", "db1", "b1",
		"--repo", w.repoURL, "--reason", "expired case", "--yes", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("delete b1: exit=%d\n%s", exit, errb)
	}

	// The same janitors with BOTH safety windows explicitly zeroed.
	//
	// Both, because the undelete window is bounded by two independent
	// knobs and the first version of this test only zeroed one: with
	// --tombstone-grace 0s alone, gc still kept b1's chunk — not
	// through the tombstone grace but through --min-chunk-age, the
	// floor that stops an --apply from reaping chunks of an in-flight
	// backup whose manifest has not committed yet. A test chunk
	// planted seconds ago sits under that floor. Real 24h-old chunks
	// would not, so zeroing only the tombstone grace models "24h
	// later" wrong in exactly the way that makes the reclamation half
	// pass vacuously.
	if _, errb, exit := runCLI(t, "repo", "gc",
		"--repo", w.repoURL, "--apply", "--tombstone-grace", "0s",
		"--min-chunk-age", "0s", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("repo gc --apply (grace 0, chunk-age 0): exit=%d\n%s", exit, errb)
	}
	if _, errb, exit := runCLI(t, "wal", "prune", "db1",
		"--repo", w.repoURL, "--apply", "--tombstone-grace", "0s", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal prune --apply --tombstone-grace 0s: exit=%d\n%s", exit, errb)
	}

	// The frontier has moved to B2: B1's WAL is gone, B2's is kept.
	if segPresent(t, w.repoURL, "db1", 1, "000000010000000000000003") {
		t.Errorf("segment 3 survived an expired tombstone; the frontier did not advance to " +
			"b2, so retention is holding WAL for a backup that no longer counts")
	}
	if !segPresent(t, w.repoURL, "db1", 1, "000000010000000000000006") {
		t.Errorf("segment 6 was deleted; that is B2's consistency window and B2 is LIVE — " +
			"this is data loss for a backup nobody deleted")
	}

	// The operator tries to undelete anyway. The pre-flight is the
	// only thing between them and a phantom restore point.
	_, errb, exit := runCLI(t, "backup", "undelete", "db1", "b1",
		"--repo", w.repoURL, "--check-chunks", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("undelete --check-chunks succeeded after gc reclaimed b1's chunks.\n\n" +
			"The window's boundedness is only survivable because the pre-flight names it. " +
			"If this passes, the operator gets a backup in `list` that cannot restore, " +
			"discovered during an incident.")
	}
	if !strings.Contains(errb, "chunks_missing") {
		t.Errorf("refusal code is not conflict.chunks_missing:\n%s", errb)
	}

	// And WITHOUT --check-chunks: the store-level Undelete fails closed
	// on its own, so even a plain undelete must refuse. This assertion
	// exists because the layering was misunderstood once already — the
	// CLI's --check-chunks pre-pass is only the BATCH layer (it gives
	// --skip-missing its atomic semantics); the per-ID safety is the
	// store's, and is not optional.
	_, errb2, exit2 := runCLI(t, "backup", "undelete", "db1", "b1",
		"--repo", w.repoURL, "-o", "json")
	if exit2 == int(output.ExitOK) {
		t.Fatalf("PLAIN undelete (no --check-chunks) resurrected a backup whose chunks are "+
			"gone:\n%s", errb2)
	}
	if !strings.Contains(errb2, "chunks_missing") {
		t.Errorf("plain undelete refused with the wrong code:\n%s", errb2)
	}
}

// TestTombstoneWindow_ForceRecoversMetadataOnly pins the forensic path:
// --force bypasses the store's fail-closed check and resurrects the
// manifest of a backup whose chunks are gone. That is its documented
// purpose, and it must keep working — an operator doing forensics on
// what a deleted backup CONTAINED needs the metadata even though the
// data is unrecoverable. The resurrected backup must then fail to
// restore, loudly.
func TestTombstoneWindow_ForceRecoversMetadataOnly(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	seedLifecycle(t, w)

	if _, errb, exit := runCLI(t, "backup", "delete", "db1", "b1",
		"--repo", w.repoURL, "--reason", "forensic case", "--yes", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("delete b1: exit=%d\n%s", exit, errb)
	}
	if _, errb, exit := runCLI(t, "repo", "gc",
		"--repo", w.repoURL, "--apply", "--tombstone-grace", "0s",
		"--min-chunk-age", "0s", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("repo gc: exit=%d\n%s", exit, errb)
	}

	// --force resurrects the metadata despite the missing chunks.
	if _, errb, exit := runCLI(t, "backup", "undelete", "db1", "b1",
		"--repo", w.repoURL, "--force", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("undelete --force refused: exit=%d\n%s\n\n"+
			"--force exists precisely for this state — recovering the manifest of a backup "+
			"whose data is gone. If it cannot, the flag has no purpose at all.", exit, errb)
	}

	// And the phantom is loud where it matters: restore fails.
	target := filepath.Join(t.TempDir(), "restored")
	if _, errb, exit := runCLI(t, "restore", "db1", "b1",
		"--repo", w.repoURL, "--target", target, "-o", "json"); exit == int(output.ExitOK) {
		t.Fatalf("restore of the force-resurrected b1 SUCCEEDED with its chunks gone:\n%s", errb)
	}
}

// commitChainLink commits one signed link of an incremental chain.
// parent == "" makes it the full.
func commitChainLink(t *testing.T, w *readWorld, deployment, id, parent, startLSN, stopLSN string, when time.Time) {
	t.Helper()
	body := []byte("chain-data-of-" + id + "\n")
	typ := backup.BackupTypeFull
	if parent != "" {
		typ = backup.BackupTypeIncremental
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             typ,
		ParentBackupID:   parent,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         startLSN,
		StopLSN:          stopLSN,
		Timeline:         1,
		StartedAt:        when,
		StoppedAt:        when.Add(time.Minute),
		BackupLabel:      "START WAL LOCATION: " + startLSN + "\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf(body), Offset: 0, Len: int64(len(body))}}},
		},
	}
	if _, err := repo.NewCAS(w.sp).PutChunk(context.Background(), body); err != nil {
		t.Fatalf("seed chunk for %s: %v", id, err)
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

// TestCascadeUnwind_RoundTripRestoresTheChain composes the promise the
// delete/undelete pair document but nothing exercises:
//
//	backup_delete.go:  "the cascade response's cascade_deleted slice
//	                    ... is exactly what you pass back to undelete"
//	backup_undelete.go: "Pairs with `backup delete --cascade` ... to
//	                    unwind a wrong cascade."
//
// A wrong cascade is precisely the situation where the operator is
// following instructions verbatim under stress, so the slice has to
// work AS RETURNED — same IDs, same order — with no editing. If the
// unwind needs the operator to reorder leaf-first or re-derive IDs, the
// documentation is a trap.
func TestCascadeUnwind_RoundTripRestoresTheChain(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	now := time.Now().UTC()
	commitChainLink(t, w, "db1", "db1.full.A", "", "0/3000028", "0/30001A0", now.Add(-3*time.Hour))
	commitChainLink(t, w, "db1", "db1.inc.B", "db1.full.A", "0/30001A0", "0/3000300", now.Add(-2*time.Hour))
	commitChainLink(t, w, "db1", "db1.inc.C", "db1.inc.B", "0/3000300", "0/3000400", now.Add(-1*time.Hour))

	// The wrong cascade.
	outb, errb, exit := runCLI(t, "backup", "delete", "db1", "db1.full.A",
		"--repo", w.repoURL, "--cascade", "--reason", "oops", "--yes", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("cascade delete: exit=%d\n%s", exit, errb)
	}
	var env struct {
		Result struct {
			CascadeDeleted []string `json:"cascade_deleted"`
		} `json:"result"`
	}
	start := strings.Index(outb, "{")
	if start < 0 || json.Unmarshal([]byte(outb[start:]), &env) != nil {
		t.Fatalf("could not parse the delete result:\n%s", outb)
	}
	unwind := env.Result.CascadeDeleted
	if len(unwind) != 3 {
		t.Fatalf("cascade_deleted has %d entries, want 3: %v", len(unwind), unwind)
	}

	// The unwind, verbatim: deployment + the slice as returned.
	args := append([]string{"backup", "undelete", "db1"}, unwind...)
	args = append(args, "--repo", w.repoURL, "--check-chunks", "-o", "json")
	if _, errb, exit := runCLI(t, args...); exit != int(output.ExitOK) {
		t.Fatalf("undelete of the cascade_deleted slice AS RETURNED failed: exit=%d\n%s\n\n"+
			"The docs tell the operator this slice is exactly what to pass back. If it "+
			"needs reordering or editing first, the unwind instructions are a trap sprung "+
			"during an incident.", exit, errb)
	}

	// Everything is live again and the chain is whole.
	listOut, _, listExit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if listExit != int(output.ExitOK) {
		t.Fatalf("list: exit=%d", listExit)
	}
	for _, id := range []string{"db1.full.A", "db1.inc.B", "db1.inc.C"} {
		if !strings.Contains(listOut, id) {
			t.Errorf("%s is not live after the unwind:\n%s", id, listOut)
		}
	}

	// And the anchor genuinely restores.
	target := filepath.Join(t.TempDir(), "restored")
	if _, errb, exit := runCLI(t, "restore", "db1", "db1.full.A",
		"--repo", w.repoURL, "--target", target, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("restore of the unwound anchor: exit=%d\n%s", exit, errb)
	}
	if got, err := os.ReadFile(filepath.Join(target, "PG_VERSION")); err != nil ||
		string(got) != "chain-data-of-db1.full.A\n" {
		t.Errorf("restored bytes = %q, err=%v", got, err)
	}
}
