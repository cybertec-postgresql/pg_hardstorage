package cli_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// healWorld extends readWorld with a replica repo that the primary
// has been replicated into. Tests that exercise --heal want both
// repos opened, one signed manifest committed against the primary
// referencing one specific chunk hash, and the replica populated
// with the same chunk bytes.
type healWorld struct {
	*readWorld
	replicaURL string
	replicaSP  storage.StoragePlugin
}

func newHealWorld(t *testing.T) *healWorld {
	t.Helper()
	w := newReadWorld(t)

	// Init a replica repo at a separate path. Same HOME (so the
	// keypair is shared) — the replica is just another fs:// store.
	replicaRoot := t.TempDir()
	replicaURL := "file://" + replicaRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: replicaURL}); err != nil {
		t.Fatalf("repo init replica: %v", err)
	}
	replicaSP := &fs.Plugin{}
	if err := replicaSP.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: replicaRoot},
	}); err != nil {
		t.Fatalf("open replica: %v", err)
	}
	t.Cleanup(func() { _ = replicaSP.Close() })
	return &healWorld{
		readWorld:  w,
		replicaURL: replicaURL,
		replicaSP:  replicaSP,
	}
}

// commitChunkedManifest plants ONE signed backup manifest at the
// primary referencing exactly one chunk whose plaintext we control.
// Returns the chunk hash + the chunk's on-disk envelope bytes from
// the primary, so the test can plant the SAME envelope at the replica
// (matching what `repo replicate` would have copied). The on-disk
// envelope has codec metadata layered on top of the plaintext, so we
// MUST read it back from the primary's CAS rather than rebuilding it.
func (hw *healWorld) commitChunkedManifest(t *testing.T, deployment, suffix string, chunkBody []byte) (repo.Hash, []byte) {
	t.Helper()
	// Write the chunk via the CAS so the on-disk envelope is what
	// the production path produces.
	cas := newCAS(t, hw.sp)
	info, err := cas.PutChunk(context.Background(), chunkBody)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + suffix,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path: "data",
			Size: int64(len(chunkBody)),
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    int64(len(chunkBody)),
			}},
		}},
	}
	if err := hw.store.Commit(context.Background(), m, hw.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit manifest: %v", err)
	}

	// Read back the on-disk envelope from the primary so we can
	// mirror it at the replica byte-identically.
	rc, err := hw.sp.Get(context.Background(), repo.ChunkKey(info.Hash))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var envelope bytes.Buffer
	if _, err := envelope.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	return info.Hash, envelope.Bytes()
}

func newCAS(t *testing.T, sp storage.StoragePlugin) *repo.CAS {
	t.Helper()
	return repo.NewCAS(sp)
}

// plantAtReplica writes raw bytes at a key on the replica.
func plantAtReplica(t *testing.T, hw *healWorld, key string, body []byte) {
	t.Helper()
	if _, err := hw.replicaSP.Put(context.Background(), key,
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("plant at replica: %v", err)
	}
}

// corruptPrimaryChunk overwrites the primary's chunk at h with
// arbitrary garbage bytes — simulating bit-rot.
func corruptPrimaryChunk(t *testing.T, hw *healWorld, h repo.Hash, garbage []byte) {
	t.Helper()
	chunkKey := repo.ChunkKey(h)
	if err := hw.sp.Delete(context.Background(), chunkKey); err != nil {
		t.Fatal(err)
	}
	if _, err := hw.sp.Put(context.Background(), chunkKey,
		bytes.NewReader(garbage),
		storage.PutOptions{ContentLength: int64(len(garbage))}); err != nil {
		t.Fatal(err)
	}
}

// TestRepairScrub_HealRequiresReplica: --heal without --replica
// is a usage error.
func TestRepairScrub_HealRequiresReplica(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", w.repoURL,
		"--heal",
		"--output", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("--heal alone should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestRepairScrub_HealRefusesSameURL: --replica == --repo is a
// usage error (operator typo guard).
func TestRepairScrub_HealRefusesSameURL(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", w.repoURL,
		"--heal", "--replica", w.repoURL,
		"--output", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("--replica==--repo should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
}

// TestRepairScrub_NoMismatch_HealNoOp: no scrub findings means
// --heal is a no-op (the heal block isn't entered; the scrub body
// is returned as a clean result).
func TestRepairScrub_NoMismatch_HealNoOp(t *testing.T) {
	hw := newHealWorld(t)
	stdout, _, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", hw.replicaURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("clean scrub should exit OK; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"mismatch_count": 0`) {
		t.Errorf("expected mismatch_count=0:\n%s", stdout)
	}
	// No heal section in body when there were no mismatches.
	if strings.Contains(stdout, `"heal":`) {
		t.Errorf("heal section should be absent when nothing was wrong:\n%s", stdout)
	}
}

// TestRepairScrub_HealHappyPath: corrupt the primary's chunk,
// plant the original envelope at the replica, run scrub --heal,
// verify it heals and the local chunk is now correct.
func TestRepairScrub_HealHappyPath(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("the-real-bytes")
	h, envelope := hw.commitChunkedManifest(t, "db1", "20260430T0900Z.000", body)

	// Mirror the envelope to the replica (what `repo replicate`
	// would have done).
	plantAtReplica(t, hw, repo.ChunkKey(h), envelope)

	// Corrupt the primary's chunk.
	corruptPrimaryChunk(t, hw, h, []byte("totally-bogus-garbage-bytes"))

	stdout, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", hw.replicaURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("heal should restore + exit OK; got %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	// The body should carry a heal section with healed=1.
	if !strings.Contains(stdout, `"heal":`) {
		t.Errorf("expected heal section in body:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"healed": 1`) {
		t.Errorf("expected healed=1:\n%s", stdout)
	}
	// Confirm the primary's bytes are now back to the envelope.
	rc, err := hw.sp.Get(context.Background(), repo.ChunkKey(h))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), envelope) {
		t.Errorf("primary's bytes weren't restored: got %d bytes, want %d",
			got.Len(), len(envelope))
	}
}

// TestRepairScrub_HealCorruptReplica_Unverified pins the silent-heal
// bug: repo.Heal runs at the storage layer without keys, so it confirms
// only that the replica BYTES were copied — not that they decrypt to
// the expected plaintext hash. If the replica's own copy of the chunk
// is ALSO corrupt, the old code installed it and reported healed=1,
// exit OK — a silent claim of success while the chunk is still broken.
// runRepairScrub must re-verify the plaintext after heal and surface
// verify.heal_unverified instead.
func TestRepairScrub_HealCorruptReplica_Unverified(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("the-real-bytes")
	h, envelope := hw.commitChunkedManifest(t, "db1", "20260430T1300Z.000", body)

	// Plant a DIFFERENT corrupt envelope at the replica (bit-rot on the
	// replica too) — distinct from the primary's corruption so heal does
	// not short-circuit on a bytes-equal AlreadyOK.
	badReplica := append([]byte("replica-rot-"), envelope...)
	badReplica[len(badReplica)-1] ^= 0xFF
	plantAtReplica(t, hw, repo.ChunkKey(h), badReplica)

	// Corrupt the primary so scrub flags a mismatch and heal engages.
	corruptPrimaryChunk(t, hw, h, []byte("totally-bogus-garbage-bytes"))

	stdout, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", hw.replicaURL,
		"--output", "json",
	)
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("corrupt-replica heal must exit ExitVerifyFailed (9); got %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code": "verify.heal_unverified"`) {
		t.Errorf("expected verify.heal_unverified code:\n%s", stderr)
	}
	if !strings.Contains(stderr, h.String()) {
		t.Errorf("expected the still-bad chunk hash %s in the error result:\n%s", h, stderr)
	}
}

// TestRepairScrub_HealNotAtReplica: replica is missing the chunk →
// heal can't complete; exit code flips to ExitVerifyFailed with
// `verify.heal_incomplete` so cron-driven heal runs alarm.
func TestRepairScrub_HealNotAtReplica(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("only-on-primary")
	h, _ := hw.commitChunkedManifest(t, "db1", "20260430T0930Z.000", body)
	// Replica deliberately empty (no chunk planted).
	_ = h
	corruptPrimaryChunk(t, hw, h, []byte("garbage"))

	stdout, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", hw.replicaURL,
		"--output", "json",
	)
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("not-at-replica should exit ExitVerifyFailed (9); got %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code": "verify.heal_incomplete"`) {
		t.Errorf("expected verify.heal_incomplete code in error result:\n%s", stderr)
	}
}

// TestRepairScrub_NoHeal_KeepsExistingExitCode: a mismatch without
// --heal still exits ExitVerifyFailed (the original behaviour).
// And the suggestion now points at the actual heal command — we
// confirm the suggestion text mentions --heal --replica.
func TestRepairScrub_NoHeal_KeepsExistingExitCode(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("mismatch-only")
	h, _ := hw.commitChunkedManifest(t, "db1", "20260430T1000Z.000", body)
	corruptPrimaryChunk(t, hw, h, []byte("garbage"))

	_, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--output", "json",
	)
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("mismatch w/o --heal should exit ExitVerifyFailed; got %d\n%s",
			exit, stderr)
	}
	if !strings.Contains(stderr, "--heal --replica") {
		t.Errorf("suggestion should mention --heal --replica:\n%s", stderr)
	}
}

// TestRepairScrub_HealResultStructure_Stable: the JSON body's heal
// fields are present in the documented v1 shape (cron tooling
// depends on these field names).
func TestRepairScrub_HealResultStructure_Stable(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("schema-stability")
	h, envelope := hw.commitChunkedManifest(t, "db1", "20260430T1100Z.000", body)
	plantAtReplica(t, hw, repo.ChunkKey(h), envelope)
	corruptPrimaryChunk(t, hw, h, []byte("garbage"))

	stdout, _, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", hw.replicaURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// Decode envelope, then re-marshal the inner Result compactly.
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
	bodyBytes, _ := stdjson.Marshal(env.Result)
	for _, want := range []string{
		`"heal":`,
		`"schema":"pg_hardstorage.repo.heal.v1"`,
		`"healed":1`,
		`"already_ok":0`,
		`"not_at_replica":0`,
		`"failed":0`,
		`"replica_url":"` + hw.replicaURL + `"`,
	} {
		if !strings.Contains(string(bodyBytes), want) {
			t.Errorf("heal body missing %q:\n%s", want, bodyBytes)
		}
	}
}

// Sanity: the test infra uses tmp dirs that exist outside our reach.
// A non-existent --replica URL maps to notfound.repo at openRepo —
// the operator's first contact with a typo'd replica is a clean
// structured error, not a panic.
func TestRepairScrub_HealNonexistentReplica(t *testing.T) {
	hw := newHealWorld(t)
	body := []byte("typo-replica")
	h, _ := hw.commitChunkedManifest(t, "db1", "20260430T1200Z.000", body)
	corruptPrimaryChunk(t, hw, h, []byte("garbage"))

	bogus := "file://" + filepath.Join(t.TempDir(), "does-not-exist")
	if err := os.MkdirAll(filepath.Dir(strings.TrimPrefix(bogus, "file://")), 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, exit := runCmd(t,
		"repair", "scrub",
		"--repo", hw.repoURL,
		"--heal", "--replica", bogus,
		"--output", "json",
	)
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit for missing replica; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "notfound.repo") {
		t.Errorf("expected notfound.repo on missing replica:\n%s", stderr)
	}
}
