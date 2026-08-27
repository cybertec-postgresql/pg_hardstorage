package repo_test

// heal_corrupt_replica_test.go — what repo.Heal alone can and cannot
// tell you, pinned so the split with the CLI stays visible.
//
// This is a CHARACTERISATION test, not a bug report. The behaviour it
// records is correct for this layer and the product handles the case
// properly one level up. It exists because the two halves are easy to
// mistake for one, and heal.go's own comment used to make exactly that
// mistake.
//
// The split:
//
//   - repo.Heal takes storage plugins, no CAS and no DEK. A chunk key
//     is the SHA-256 of the PLAINTEXT while the stored object is a
//     compressed+encrypted envelope, so heal physically cannot check
//     that what it wrote is the chunk its key names. Its post-write
//     step compares the readback against the bytes just fetched, which
//     proves the write round-tripped and nothing more.
//
//   - `repo replicate` copies envelopes verbatim and verifies nothing,
//     correctly, for the same reason. So corruption predating
//     replication is mirrored exactly, and a heal from that replica
//     installs the same bad bytes and counts them Healed.
//
//   - internal/cli/repair.go closes it: after heal returns,
//     reverifyChunksPlaintext re-reads every once-mismatched chunk
//     through the per-manifest CAS — which HAS the keys — and raises
//     verify.heal_unverified when the plaintext still does not match.
//     repair_heal_test.go covers that path.
//
// The danger this file guards against is not the behaviour below. It is
// somebody reading heal.go, believing heal already verifies plaintext
// (its doc comment claimed so until this was written), and deleting the
// CLI's re-verify as redundant — silently removing the only real check.

import (
	"bytes"
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestHeal_CannotDetectACorruptReplica records the limitation.
//
// Both sides hold wrong bytes for hash h — what a replicate of an
// already-rotted source produces. Heal reports Healed because from
// where it stands the copy succeeded; only a caller with the DEK can
// say otherwise.
//
// If this test ever starts failing because Healed is 0, heal has gained
// the ability to verify plaintext. That would be a real improvement —
// and the CLI's reverifyChunksPlaintext could then be reconsidered, but
// only then.
func TestHeal_CannotDetectACorruptReplica(t *testing.T) {
	dst, replica := twoRepos(t)

	body := []byte("the-original-chunk-bytes")
	h := putChunk(t, replica, env(body))

	// Corrupt both sides: the replica with one pattern, the local copy
	// with another, so the heal genuinely rewrites the local bytes.
	corruptChunkAt(t, replica, h, env([]byte("REPLICA-IS-ALSO-CORRUPT")))
	corruptChunkAt(t, dst, h, env([]byte("locally-corrupt-differently")))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}

	got := readChunkBytes(t, dst, h)
	if bytes.Equal(got, env(body)) {
		t.Fatalf("the chunk came back correct, which this test's premise says is impossible: " +
			"both copies were corrupted, so there was no good source to heal from. The " +
			"fixture is wrong and the assertion below would be meaningless.")
	}

	if res.Healed != 1 {
		t.Errorf("Healed=%d, want 1.\n\n"+
			"This test records that repo.Heal CANNOT see a corrupt replica: the chunk key is "+
			"a plaintext SHA, the stored object is an encrypted envelope, and heal holds no "+
			"DEK. A change here means heal gained plaintext verification — good, but the "+
			"CLI's reverifyChunksPlaintext was written on the assumption it had not, so "+
			"revisit that too.", res.Healed)
	}
	if res.Failed != 0 {
		t.Errorf("Failed=%d, want 0 — heal has no basis on which to fail here", res.Failed)
	}
	t.Logf("recorded: heal reports Healed=%d for a chunk that is still corrupt; "+
		"detecting this is internal/cli/repair.go's reverifyChunksPlaintext, "+
		"which raises verify.heal_unverified", res.Healed)
}

// TestHeal_PostWriteVerifyComparesTheWriteAgainstItself pins the exact
// weakness of the post-write step, so its name is not mistaken for a
// content check.
//
// The replica's bytes are garbage; the write round-trips perfectly;
// SkipVerify=false changes nothing. That is the whole point — the check
// is about the storage layer having written what it was handed, not
// about the bytes being right.
func TestHeal_PostWriteVerifyComparesTheWriteAgainstItself(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("original")
	h := putChunk(t, replica, env(body))
	corruptChunkAt(t, replica, h, env([]byte("garbage-from-the-replica")))
	corruptChunkAt(t, dst, h, env([]byte("garbage-local")))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{SkipVerify: false})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 {
		t.Errorf("Healed=%d with post-write verify ON; the check compares the readback "+
			"against the replica's bytes, so garbage in, garbage verified", res.Healed)
	}
	if got := readChunkBytes(t, dst, h); !bytes.Equal(got, env([]byte("garbage-from-the-replica"))) {
		t.Errorf("dst holds %q; the replica's bytes should have been installed verbatim", got)
	}
}
