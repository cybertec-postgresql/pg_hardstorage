package cli_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestRepair_Chunks_OrphansDryRun_ListsButDoesntDelete(t *testing.T) {
	repoURL := initRepoForTest(t)
	// Drop one chunk into the CAS with no manifest referencing it.
	_, sp, _ := repo.Open(context.Background(), repoURL)
	cas := casdefault.New(sp)
	info, err := cas.PutChunk(context.Background(), []byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	sp.Close()

	out, _, exit := runCmd(t,
		"repair", "chunks", "--orphans",
		"--repo", repoURL,
		// Disable the chunk-age floor: the orphan was written
		// milliseconds ago and the default 24h floor (guarding
		// in-flight backups) would otherwise protect it.
		"--min-chunk-age", "0",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"dry_run": true`) {
		t.Errorf("default mode should be dry-run; got:\n%s", out)
	}
	if !strings.Contains(out, info.Hash.String()) {
		t.Errorf("orphan hash should appear in result; got:\n%s", out)
	}

	// Without --apply, the chunk must still exist.
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if has, _ := casdefault.New(sp2).HasChunk(context.Background(), info.Hash); !has {
		t.Errorf("dry-run unexpectedly deleted the chunk")
	}
}

func TestRepair_Chunks_OrphansApply_DeletesIt(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, _ := repo.Open(context.Background(), repoURL)
	cas := casdefault.New(sp)
	info, _ := cas.PutChunk(context.Background(), []byte("orphan"))
	sp.Close()

	_, _, exit := runCmd(t,
		"repair", "chunks", "--orphans", "--apply",
		"--repo", repoURL,
		"--min-chunk-age", "0",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if has, _ := casdefault.New(sp2).HasChunk(context.Background(), info.Hash); has {
		t.Errorf("--apply should have deleted the orphan")
	}
}

// TestRepair_Chunks_Apply_WarnsWhenFloorDisabled mirrors the gc guard:
// `repair chunks --orphans --apply --min-chunk-age 0` disarms the
// in-flight-backup floor, so it must emit the same loud
// safety_floor_disabled warning `repo gc --apply` does — not silently
// reap young chunks out from under a concurrent backup.
func TestRepair_Chunks_Apply_WarnsWhenFloorDisabled(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, errb, exit := runCmd(t,
		"repair", "chunks", "--orphans", "--apply",
		"--repo", repoURL, "--min-chunk-age", "0", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, out, errb)
	}
	all := out + errb
	for _, want := range []string{"safety_floor_disabled", "min-chunk-age"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in output:\nstdout=%s\nstderr=%s", want, out, errb)
		}
	}
}

// TestRepair_Chunks_Apply_NoWarnWithDefaultFloor: the default floor must
// NOT warn — only an explicit --min-chunk-age 0 opt-out does.
func TestRepair_Chunks_Apply_NoWarnWithDefaultFloor(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, errb, exit := runCmd(t,
		"repair", "chunks", "--orphans", "--apply",
		"--repo", repoURL, "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, out, errb)
	}
	if strings.Contains(out+errb, "safety_floor_disabled") {
		t.Errorf("default floor must not warn:\nstdout=%s\nstderr=%s", out, errb)
	}
}

func TestRepair_Chunks_RequiresExactlyOneFlag(t *testing.T) {
	repoURL := initRepoForTest(t)
	cases := [][]string{
		{"repair", "chunks", "--repo", repoURL, "--output", "json"},                           // neither
		{"repair", "chunks", "--orphans", "--missing", "--repo", repoURL, "--output", "json"}, // both
	}
	for _, args := range cases {
		_, _, exit := runCmd(t, args...)
		if exit != 2 {
			t.Errorf("args %v should exit 2 (Misuse); got %d", args, exit)
		}
	}
}

// repair manifest pulls a corrupt primary back from its replica.
// We need a real signed manifest pair (primary + replica) so the
// signature verification path engages. The readWorld fixture (used
// by verify / hold) gives us the keypair + commit machinery.
func TestRepair_Manifest_RecoversFromReplica(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))

	primary := backup.PrimaryPath("db1", id)
	// Corrupt the primary by overwriting with garbage. The replica
	// is untouched at manifests/_replicas/<id>.manifest.json.
	if err := w.sp.Delete(context.Background(), primary); err != nil {
		t.Fatal(err)
	}
	if _, err := w.sp.Put(context.Background(), primary,
		strings.NewReader(`{"corrupted": true}`),
		storage.PutOptions{ContentLength: 19}); err != nil {
		t.Fatal(err)
	}

	out, errb, exit := runCLI(t,
		"repair", "manifest", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", exit, out, errb)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"primary_was_valid": false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// After recovery, the manifest should round-trip via the read path.
	store := backup.NewManifestStore(w.sp)
	if _, err := store.Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Errorf("manifest should be readable after recovery: %v", err)
	}
}

// TestRepair_Manifest_RebuildsMissingReplicaFromPrimary pins the
// data-loss #3 fix: when the REPLICA is the missing side but the
// primary is intact, `repair manifest` rebuilds the replica from the
// primary (restoring the lost redundancy) instead of erroring.
func TestRepair_Manifest_RebuildsMissingReplicaFromPrimary(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))
	if err := w.sp.Delete(context.Background(), backup.ReplicaPath(id)); err != nil {
		t.Fatal(err)
	}
	out, errb, exit := runCLI(t,
		"repair", "manifest", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\nout:\n%s\nerr:\n%s", exit, out, errb)
	}
	if !strings.Contains(out, `"rebuilt_replica": true`) {
		t.Errorf("expected rebuilt_replica=true in:\n%s", out)
	}
	if _, err := w.sp.Stat(context.Background(), backup.ReplicaPath(id)); err != nil {
		t.Errorf("replica should exist after repair: %v", err)
	}
}

// TestRepair_Manifest_RebuildsCorruptReplicaFromPrimary pins the
// corrupt-replica recovery gap: when the REPLICA is present but corrupt
// and the primary is valid, `repair manifest` must rebuild the replica
// from the primary (restoring redundancy) — exactly as it does for a
// MISSING replica. Before the fix, a present-but-corrupt replica skipped
// the rebuild branch (the read succeeded) and bailed with "no
// recoverable copy" without ever checking the valid primary.
func TestRepair_Manifest_RebuildsCorruptReplicaFromPrimary(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))

	// Corrupt the replica in place (present, but bad bytes). Primary
	// is left valid.
	replica := backup.ReplicaPath(id)
	if err := w.sp.Delete(context.Background(), replica); err != nil {
		t.Fatal(err)
	}
	if _, err := w.sp.Put(context.Background(), replica,
		strings.NewReader(`{"corrupted":"replica"}`),
		storage.PutOptions{ContentLength: 23}); err != nil {
		t.Fatal(err)
	}

	out, errb, exit := runCLI(t,
		"repair", "manifest", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("corrupt replica + valid primary must recover; exit=%d\nout:\n%s\nerr:\n%s", exit, out, errb)
	}
	if !strings.Contains(out, `"rebuilt_replica": true`) {
		t.Errorf("expected rebuilt_replica=true in:\n%s", out)
	}
	// The replica must now verify via the read path.
	store := backup.NewManifestStore(w.sp)
	if _, err := store.Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Errorf("manifest should be readable after replica rebuild: %v", err)
	}
	// And the on-disk replica must itself verify now (not just the
	// primary-first happy path).
	rb := readKeyBytes(t, w.sp, replica)
	if _, err := backup.ParseAndVerify(rb, w.verifier); err != nil {
		t.Errorf("rebuilt replica must verify on its own: %v", err)
	}
}

func TestRepair_Manifest_RefusesValidPrimaryWithoutForce(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))

	_, errb, exit := runCLI(t,
		"repair", "manifest", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitAborted) {
		t.Errorf("expected ExitAborted on valid primary; got %d\nstderr: %s", exit, errb)
	}
	if !strings.Contains(errb, "aborted.primary_intact") {
		t.Errorf("expected aborted.primary_intact code:\n%s", errb)
	}
}

func TestRepair_Manifest_NoReplica_NotFound(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))
	// Delete the replica AND corrupt the primary so we have no
	// recovery copy.
	primary := backup.PrimaryPath("db1", id)
	replica := backup.ReplicaPath(id)
	_ = w.sp.Delete(context.Background(), replica)
	_ = w.sp.Delete(context.Background(), primary)
	_, _ = w.sp.Put(context.Background(), primary,
		strings.NewReader(`{"corrupted": true}`),
		storage.PutOptions{ContentLength: 19})

	_, errb, exit := runCLI(t,
		"repair", "manifest", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitNotFound) {
		t.Errorf("expected ExitNotFound; got %d\nstderr: %s", exit, errb)
	}
	if !strings.Contains(errb, "notfound.replica_manifest") {
		t.Errorf("expected notfound.replica_manifest:\n%s", errb)
	}
}

// repair attestation re-signs a manifest. The use case is keypair
// rotation where the manifest's embedded pubkey doesn't match the
// current local one. We can simulate this by tampering the
// attestation block (replacing it with a different keypair's
// signature) — no need to actually rotate the test keypair.
func TestRepair_Attestation_RefusesValidWithoutForce(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))

	_, errb, exit := runCLI(t,
		"repair", "attestation", "db1", id,
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitAborted) {
		t.Errorf("expected ExitAborted on valid manifest; got %d\nstderr: %s",
			exit, errb)
	}
	if !strings.Contains(errb, "aborted.attestation_valid") {
		t.Errorf("expected aborted.attestation_valid:\n%s", errb)
	}
}

func TestRepair_Attestation_ForceRecordsAuditEvent(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))

	out, errb, exit := runCLI(t,
		"repair", "attestation", "db1", id,
		"--repo", w.repoURL,
		"--actor", "ops@acme",
		"--reason", "rotation drill",
		"--force",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", exit, out, errb)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"forced": true`,
		`"audit_event_id":`,
		`"new_fingerprint":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Manifest must still verify after re-sign.
	store := backup.NewManifestStore(w.sp)
	if _, err := store.Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Errorf("manifest should be readable after re-sign: %v", err)
	}
	// Audit event written?
	auditOut, _, exit := runCLI(t,
		"audit", "search",
		"--repo", w.repoURL,
		"--action", "repair.attestation.resigned",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search failed: %d", exit)
	}
	if !strings.Contains(auditOut, `"count": 1`) {
		t.Errorf("expected exactly 1 repair.attestation.resigned event:\n%s", auditOut)
	}
	// audit search currently returns the row metadata (id / seq /
	// hash / actor / deployment) — it does NOT inline the event
	// Body. The reason="rotation drill" is durable in the event's
	// JSON file but only surfaces via a future `audit show <id>`
	// surface. The presence + correct action of the event is
	// what we pin here.
	if !strings.Contains(auditOut, `"action": "repair.attestation.resigned"`) {
		t.Errorf("audit event action mismatch:\n%s", auditOut)
	}
	if !strings.Contains(auditOut, `"actor": "ops@acme"`) {
		t.Errorf("audit event actor missing:\n%s", auditOut)
	}
}

// TestRepair_Attestation_ReSignsReplicaToo pins the redundancy bug:
// `repair attestation` must re-sign BOTH the primary AND the replica
// copy. The manifest is stored twice and Read falls back to the
// replica when the primary is lost ("survivability against a single
// corrupted primary"). A key rotation breaks BOTH copies' signatures
// with the same stale key, so re-signing only the primary leaves the
// replica stale-signed: the happy path still verifies (primary first),
// but the redundancy is silently gone — a later primary loss falls
// back to a replica that no longer verifies. This forges that exact
// rotation (both copies signed by a foreign key) and asserts the
// replica verifies after repair, including via the primary-loss
// fallback path.
func TestRepair_Attestation_ReSignsReplicaToo(t *testing.T) {
	w := newReadWorld(t)

	// Forge a key rotation: commit a manifest signed by a throwaway
	// key the current keyring (w.verifier) does NOT recognise. This
	// stamps BOTH the primary and replica with the stale signature.
	foreign, _, err := keystore.LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := commitManifestSignedBy(t, w, "db1", foreign,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	store := backup.NewManifestStore(w.sp)
	// Precondition: with both copies foreign-signed, the manifest does
	// NOT verify against the current keyring (neither primary nor the
	// replica fallback).
	if _, err := store.Read(context.Background(), "db1", id, w.verifier); err == nil {
		t.Fatal("precondition: foreign-signed manifest should NOT verify before repair")
	}

	// No --force: against the current verifier the primary is invalid,
	// so the re-sign proceeds without it.
	out, errb, exit := runCLI(t,
		"repair", "attestation", "db1", id,
		"--repo", w.repoURL,
		"--actor", "ops@acme",
		"--reason", "key rotation",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", exit, out, errb)
	}

	// The replica copy must now verify against the current keyring on
	// its own — read it directly and verify, bypassing the primary.
	replicaBytes := readKeyBytes(t, w.sp, backup.ReplicaPath(id))
	if _, err := backup.ParseAndVerify(replicaBytes, w.verifier); err != nil {
		t.Fatalf("replica must verify after repair attestation (redundancy bug): %v", err)
	}

	// End-to-end: simulate a later primary loss. Read MUST fall back to
	// the replica and still verify — the whole point of the replica.
	if err := w.sp.Delete(context.Background(), backup.PrimaryPath("db1", id)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Errorf("after primary loss, replica fallback must verify: %v", err)
	}
}

// readKeyBytes reads an object from a storage plugin in full, failing
// the test on any error.
func readKeyBytes(t *testing.T, sp storage.StoragePlugin, key string) []byte {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

func TestRepair_Index_EmptyRepo(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, _, exit := runCmd(t,
		"repair", "index",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"total_chunks": 0`,
		`"unique_buckets": 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRepair_Index_PopulatedRepo(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, _ := repo.Open(context.Background(), repoURL)
	cas := casdefault.New(sp)
	for _, body := range [][]byte{
		[]byte("alpha"), []byte("bravo"), []byte("charlie"),
	} {
		if _, err := cas.PutChunk(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}
	sp.Close()

	out, _, exit := runCmd(t,
		"repair", "index",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"total_chunks": 3`) {
		t.Errorf("expected 3 chunks; got:\n%s", out)
	}
	if !strings.Contains(out, `"buckets":`) {
		t.Errorf("expected buckets list:\n%s", out)
	}
}

// Garbage in chunks/sha256/ surfaces as Unparseable. The CAS would
// silently ignore these (ParseChunkKey returns ErrNotAChunkKey);
// repair index is the diagnostic that reports them.
func TestRepair_Index_FlagsUnparseable(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, _ := repo.Open(context.Background(), repoURL)
	defer sp.Close()
	if _, err := sp.Put(context.Background(),
		"chunks/sha256/zz/zz/garbage.txt",
		strings.NewReader("not a chunk"),
		storage.PutOptions{ContentLength: 11},
	); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t,
		"repair", "index",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"unparseable":`) {
		t.Errorf("expected unparseable count:\n%s", out)
	}
}

// repair slot is an alias for wal repair. Both share the same
// runWalRepair body; this test pins that the command path is wired
// (a missing flag produces ExitMisuse, NOT the stub's notimpl exit).
func TestRepair_Slot_RequiresPGConnection(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"repair", "slot", "db1",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("repair slot without --pg-connection should exit 2; got %d", exit)
	}
}

func TestRepair_Scrub_OK(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"repair", "scrub",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Errorf("scrub on empty repo should exit 0; got %d", exit)
	}
}

// TestRepair_Attestation_RefusesTamperedContent pins the fix: a manifest
// whose signature no longer validates its own content (tampered or corrupt,
// NOT a key rotation) must be REFUSED, never re-signed — re-signing would
// launder altered bytes under the operator's trusted key.
func TestRepair_Attestation_RefusesTamperedContent(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))
	ctx := context.Background()

	key := backup.PrimaryPath("db1", id)
	rc, err := w.sp.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	m, err := backup.ParseAttestationless(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Break the attestation so it no longer matches the content (flip one
	// base64 char of the signature) while keeping the manifest's identity.
	sigb := []byte(m.Attestation.Signature)
	if sigb[0] == 'A' {
		sigb[0] = 'B'
	} else {
		sigb[0] = 'A'
	}
	m.Attestation.Signature = string(sigb)
	tampered, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.sp.Put(ctx, key, strings.NewReader(string(tampered)),
		storage.PutOptions{ContentLength: int64(len(tampered))}); err != nil {
		t.Fatal(err)
	}

	_, errb, exit := runCLI(t, "repair", "attestation", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("repair attestation must REFUSE a manifest whose signature doesn't match its content; got exit OK\nstderr: %s", errb)
	}
	if !strings.Contains(errb, "attestation_tampered") {
		t.Errorf("expected attestation_tampered refusal:\n%s", errb)
	}
}
