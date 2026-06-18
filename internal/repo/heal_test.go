package repo_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// healPair builds a {dst, replica} pair, both as real init'd repos,
// and returns their plugins. Reuses twoRepos from replicate_test.go.

// corruptChunkAt overwrites the chunk at h's canonical key with
// arbitrary garbage bytes — simulates the bit-rot the heal primitive
// is designed to undo.
func corruptChunkAt(t *testing.T, sp storage.StoragePlugin, h repo.Hash, garbage []byte) {
	t.Helper()
	chunkKey := repo.ChunkKey(h)
	// Delete first so the put writes fresh bytes (the fs plugin's
	// IfNotExists path would refuse otherwise).
	if err := sp.Delete(context.Background(), chunkKey); err != nil {
		t.Fatalf("delete pre-corrupt: %v", err)
	}
	if _, err := sp.Put(context.Background(), chunkKey,
		bytes.NewReader(garbage),
		storage.PutOptions{ContentLength: int64(len(garbage))}); err != nil {
		t.Fatalf("plant garbage: %v", err)
	}
}

// readChunkBytes is the test counterpart of repo.readKey.
func readChunkBytes(t *testing.T, sp storage.StoragePlugin, h repo.Hash) []byte {
	t.Helper()
	rc, err := sp.Get(context.Background(), repo.ChunkKey(h))
	if err != nil {
		t.Fatalf("get chunk: %v", err)
	}
	defer rc.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	return b.Bytes()
}

// TestHeal_HappyPath: corrupt one chunk locally, replica still has
// good bytes, heal restores it.
func TestHeal_HappyPath(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("hello-heal")
	h := putChunk(t, replica, body)

	// Plant a CORRUPTED copy at dst (simulating bit-rot after a
	// successful initial backup + replicate).
	corruptChunkAt(t, dst, h, []byte("garbage-bytes-xx"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 {
		t.Errorf("Healed=%d, want 1; result=%+v", res.Healed, res)
	}
	if res.NotAtReplica != 0 || res.Failed != 0 {
		t.Errorf("unexpected non-heal counts: %+v", res)
	}
	// Confirm dst now has the correct bytes.
	got := readChunkBytes(t, dst, h)
	if !bytes.Equal(got, body) {
		t.Errorf("post-heal local bytes wrong: got %q want %q", got, body)
	}
}

// TestHeal_AlreadyOK: dst's bytes already match replica's. No write
// happens; AlreadyOK count bumps.
func TestHeal_AlreadyOK(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("already-clean")
	h := putChunk(t, replica, body)
	// Plant the same bytes at dst.
	putRaw(t, dst, repo.ChunkKey(h), body)

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.AlreadyOK != 1 {
		t.Errorf("AlreadyOK=%d, want 1; result=%+v", res.AlreadyOK, res)
	}
	if res.Healed != 0 {
		t.Errorf("Healed=%d, want 0 (nothing to do)", res.Healed)
	}
}

// TestHeal_NotAtReplica: the replica is missing this chunk. Heal
// records NotAtReplica + a failure entry. Local copy is unchanged.
func TestHeal_NotAtReplica(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("missing-at-replica")
	h := repo.HashOf(body)
	// Only the dst has this hash, with corrupted bytes — and the
	// replica doesn't have it at all.
	corruptChunkAt(t, dst, h, []byte("definitely-not-the-right-bytes"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.NotAtReplica != 1 {
		t.Errorf("NotAtReplica=%d, want 1; result=%+v", res.NotAtReplica, res)
	}
	if res.Healed != 0 {
		t.Errorf("Healed=%d, want 0", res.Healed)
	}
	if len(res.Failures) == 0 {
		t.Errorf("expected a Failure entry for the missing chunk")
	}
	// Local bytes still corrupt (heal doesn't make things worse).
	got := readChunkBytes(t, dst, h)
	if bytes.Equal(got, body) {
		t.Errorf("local bytes were silently restored despite NotAtReplica")
	}
}

// TestHeal_DryRun reports the work but writes nothing.
func TestHeal_DryRun(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("dry-run-payload")
	h := putChunk(t, replica, body)
	corruptChunkAt(t, dst, h, []byte("bad"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Healed != 1 {
		t.Errorf("dry-run should report Healed=1; got %+v", res)
	}
	if !res.DryRun {
		t.Error("DryRun flag not propagated to result")
	}
	// Local bytes unchanged (still corrupt).
	got := readChunkBytes(t, dst, h)
	if bytes.Equal(got, body) {
		t.Errorf("dry-run wrote bytes to dst")
	}
}

// TestHeal_DstNotARepo refuses when dst lacks HSREPO.
func TestHeal_DstNotARepo(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + srcRoot}); err != nil {
		t.Fatal(err)
	}
	// dst has no HSREPO.
	src := &fs.Plugin{}
	src.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: srcRoot}})
	defer src.Close()
	dst := &fs.Plugin{}
	dst.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dstRoot}})
	defer dst.Close()

	_, err := repo.Heal(context.Background(), dst, src, nil, repo.HealOptions{})
	if !errors.Is(err, repo.ErrNotARepo) {
		t.Errorf("expected ErrNotARepo for missing dst HSREPO, got %v", err)
	}
}

// TestHeal_ReplicaNotARepo refuses when the replica lacks HSREPO
// (typo on the URL would fall here).
func TestHeal_ReplicaNotARepo(t *testing.T) {
	dstRoot := t.TempDir()
	replicaRoot := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + dstRoot}); err != nil {
		t.Fatal(err)
	}
	dst := &fs.Plugin{}
	dst.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dstRoot}})
	defer dst.Close()
	replica := &fs.Plugin{}
	replica.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: replicaRoot}})
	defer replica.Close()

	_, err := repo.Heal(context.Background(), dst, replica, nil, repo.HealOptions{})
	if !errors.Is(err, repo.ErrNotARepo) {
		t.Errorf("expected ErrNotARepo for missing replica HSREPO, got %v", err)
	}
}

// TestHeal_NilPlugins: obvious validation guard.
func TestHeal_NilPlugins(t *testing.T) {
	if _, err := repo.Heal(context.Background(), nil, nil, nil, repo.HealOptions{}); err == nil {
		t.Error("expected error for nil plugins")
	}
}

// TestHeal_Multiple: a list of mixed outcomes (one healable, one
// not-at-replica) reports both correctly in one run. Operators
// running heal off a scrub-mismatch list want the full damage report.
func TestHeal_Multiple(t *testing.T) {
	dst, replica := twoRepos(t)
	bodyA := []byte("heal-me-A")
	hA := putChunk(t, replica, bodyA)
	corruptChunkAt(t, dst, hA, []byte("bad-A"))

	bodyB := []byte("not-at-replica-B")
	hB := repo.HashOf(bodyB)
	// hB only at dst, corrupted.
	corruptChunkAt(t, dst, hB, []byte("bad-B"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{hA, hB},
		repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 {
		t.Errorf("Healed=%d, want 1", res.Healed)
	}
	if res.NotAtReplica != 1 {
		t.Errorf("NotAtReplica=%d, want 1", res.NotAtReplica)
	}
	if res.Considered != 2 {
		t.Errorf("Considered=%d, want 2", res.Considered)
	}
}

// TestHeal_OnProgress fires once per hash with the outcome.
func TestHeal_OnProgress(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("progress")
	h := putChunk(t, replica, body)
	corruptChunkAt(t, dst, h, []byte("bad"))

	var outcomes []string
	_, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{
			OnProgress: func(p repo.HealProgress) {
				outcomes = append(outcomes, p.Outcome)
			},
		})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0] != "healed" {
		t.Errorf("outcomes=%v, want [healed]", outcomes)
	}
}

// TestHeal_PostWriteVerify: with the default opts, the heal does a
// post-write SHA round-trip. We can't easily simulate a backend that
// silently corrupts the write AFTER our Put, so this test just
// confirms the SkipVerify=false path completes cleanly on a healthy
// backend (ensuring it doesn't false-positive on its own writes).
func TestHeal_PostWriteVerify(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("post-write-verify")
	h := putChunk(t, replica, body)
	corruptChunkAt(t, dst, h, []byte("bad"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{SkipVerify: false})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 || res.Failed != 0 {
		t.Errorf("default verify path should heal cleanly: %+v", res)
	}
}

// healRetentionSP records SetRetention calls on the dst so a test can
// assert a healed chunk is re-locked on a WORM repo.
type healRetentionSP struct {
	storage.StoragePlugin
	hit  bool
	key  string
	mode storage.WORMMode
}

func (s *healRetentionSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	s.hit = true
	s.key = key
	s.mode = mode
	return s.StoragePlugin.SetRetention(ctx, key, until, mode)
}

// TestHeal_AppliesWORMLock pins the fix: a healed chunk on a compliance
// repo must be re-locked, not left deletable (the heal's IfNotExists Put
// carries no retention).
func TestHeal_AppliesWORMLock(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("good chunk bytes for healing")
	h := putChunk(t, replica, body)
	// dst holds CORRUPT bytes at the same key, so heal rewrites it.
	putRaw(t, dst, repo.ChunkKey(h), []byte("corrupt-local-copy"))

	rec := &healRetentionSP{StoragePlugin: dst}
	until := time.Now().Add(time.Hour).UTC()
	res, err := repo.Heal(context.Background(), rec, replica, []repo.Hash{h}, repo.HealOptions{
		RetainUntil:   until,
		RetentionMode: storage.WORMCompliance,
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if res.Healed != 1 {
		t.Fatalf("Healed = %d, want 1 (failures: %+v)", res.Healed, res.Failures)
	}
	if !rec.hit {
		t.Fatal("heal must apply a WORM lock to the rewritten chunk")
	}
	if rec.key != repo.ChunkKey(h) {
		t.Errorf("retention applied to %q, want chunk key %q", rec.key, repo.ChunkKey(h))
	}
	if rec.mode != storage.WORMCompliance {
		t.Errorf("retention mode = %q, want compliance", rec.mode)
	}
}
