package repo_test

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// M93: a chunk that rotted BEFORE it was replicated is byte-identical in
// the local repository and at the replica. Heal compared the two first
// and returned AlreadyOK — reporting as healthy a chunk scrub had just
// flagged and that no copy can restore. The replica's copy must be
// content-verified before "identical" means anything.
func TestHeal_IdenticalCorruptionIsNotAlreadyOK(t *testing.T) {
	dst, replica := twoRepos(t)
	h := repo.HashOf([]byte("the bytes this address names"))
	rotted := []byte("rot that was replicated verbatim")
	putMisaddressedChunk(t, dst, h, rotted)
	putMisaddressedChunk(t, replica, h, rotted)

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{Codecs: healCodecs()})
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyOK != 0 {
		t.Fatalf("AlreadyOK=%d: identical corruption on both sides was reported healthy", res.AlreadyOK)
	}
	if res.Failed != 1 {
		t.Fatalf("Failed=%d, want 1 (the replica cannot heal this chunk)", res.Failed)
	}

	// Identical and INTACT is still a genuine AlreadyOK.
	dst2, replica2 := twoRepos(t)
	good := putAddressedChunk(t, dst2, []byte("intact"))
	putAddressedChunk(t, replica2, []byte("intact"))
	res, err = repo.Heal(context.Background(), dst2, replica2, []repo.Hash{good}, repo.HealOptions{Codecs: healCodecs()})
	if err != nil || res.AlreadyOK != 1 {
		t.Fatalf("intact identical pair: AlreadyOK=%d err=%v; want 1", res.AlreadyOK, err)
	}
}
