package repo_test

// Heal must not copy a replica's corruption into the repository and
// call it a repair.
//
// Heal used to check only that the replica's bytes PARSE as a chunk
// envelope. A parseable envelope is not an intact one: rot inside the
// compressed payload leaves the header perfectly well-formed. So a
// replica copy that no longer hashes to the address it is filed under
// was written over the local copy, read back intact, and counted as
// "healed" -- and the mismatch surfaced later at restore, where
// GetChunkBytes does verify the plaintext hash. The operator is told
// the repository was repaired and finds out otherwise when they need
// it.
//
// bundle.Import -- the other path that writes chunks into a repository
// from an outside source -- has always taken a codec registry for
// exactly this check (verifyChunkPayload). Heal now does too.
//
// NOTE ON FIXTURES: chunks are addressed by the hash of their
// PLAINTEXT (CAS.PutChunk hashes the body, then compresses and
// encrypts; bundle's verifyChunkPayload decodes the envelope before
// comparing to the key). The older heal tests file chunks under the
// hash of the ENVELOPE, which no real repository does -- that fixture
// convention is part of why the missing check went unnoticed, since it
// makes the content address unverifiable by construction. These tests
// build chunks the way the CAS does.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/none"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// healCodecs mirrors what the CLI passes (bundleCodecs) for the
// none-codec fixtures used here.
func healCodecs() *compression.CodecRegistry {
	r := compression.NewRegistry()
	r.Register(compression.AlgoNone, none.Compressor{})
	return r
}

// putAddressedChunk stores plaintext as a chunk envelope under the key
// a real CAS would use: ChunkKey(HashOf(plaintext)).
func putAddressedChunk(t *testing.T, sp storage.StoragePlugin, plaintext []byte) repo.Hash {
	t.Helper()
	h := repo.HashOf(plaintext)
	putRaw(t, sp, repo.ChunkKey(h), env(plaintext))
	return h
}

// putMisaddressedChunk stores a WELL-FORMED envelope whose plaintext is
// not what the key says: the replica-rot case.
func putMisaddressedChunk(t *testing.T, sp storage.StoragePlugin, h repo.Hash, actual []byte) {
	t.Helper()
	putRaw(t, sp, repo.ChunkKey(h), env(actual))
}

func TestHeal_RefusesAReplicaCopyThatIsNotTheChunkItClaimsToBe(t *testing.T) {
	dst, replica := twoRepos(t)
	good := []byte("the real chunk contents")
	h := repo.HashOf(good)

	// The replica's copy parses fine but is NOT chunk h.
	putMisaddressedChunk(t, replica, h, []byte("rotted contents, same length-ish"))
	// The local copy is corrupt, which is why heal was asked to run.
	corruptChunkAt(t, dst, h, env([]byte("local rot")))
	before := readChunkBytes(t, dst, h)

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{Codecs: healCodecs()})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 0 {
		t.Errorf("Healed=%d, want 0 — the replica's copy does not hash to the address it "+
			"is filed under, so copying it in would replace local corruption with remote "+
			"corruption and report success; result=%+v", res.Healed, res)
	}
	if res.Failed != 1 {
		t.Errorf("Failed=%d, want 1; result=%+v", res.Failed, res)
	}
	if len(res.Failures) != 1 ||
		!strings.Contains(res.Failures[0].Err, "content address") {
		t.Errorf("failure does not name the cause an operator must act on "+
			"(repair the replica, or heal from a different one): %+v", res.Failures)
	}
	// The local copy must be left exactly as found: refusing to heal
	// must not also destroy what was there.
	if after := readChunkBytes(t, dst, h); !bytes.Equal(before, after) {
		t.Error("a refused heal modified the local copy anyway")
	}
}

func TestHeal_AcceptsAReplicaCopyThatMatchesItsAddress(t *testing.T) {
	dst, replica := twoRepos(t)
	good := []byte("the real chunk contents")
	h := putAddressedChunk(t, replica, good)
	corruptChunkAt(t, dst, h, env([]byte("local rot")))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{Codecs: healCodecs()})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 || res.Failed != 0 {
		t.Fatalf("a correctly-addressed replica chunk was not healed: %+v", res)
	}
	if res.HealedUnverified != 0 {
		t.Errorf("HealedUnverified=%d, want 0 — this chunk's content WAS checked "+
			"against its address", res.HealedUnverified)
	}
	if got := readChunkBytes(t, dst, h); !bytes.Equal(got, env(good)) {
		t.Errorf("post-heal bytes wrong: %q", got)
	}
}

// Without a codec registry heal cannot check content addresses at all.
// It must still heal — an operator without the codecs is not helped by
// a refusal — but the report must not claim more than was checked.
func TestHeal_WithoutCodecsCountsTheHealAsUnverified(t *testing.T) {
	dst, replica := twoRepos(t)
	h := repo.HashOf([]byte("payload"))
	putMisaddressedChunk(t, replica, h, []byte("not the payload"))
	corruptChunkAt(t, dst, h, env([]byte("local rot")))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{}) // no Codecs
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 {
		t.Fatalf("Healed=%d, want 1: %+v", res.Healed, res)
	}
	if res.HealedUnverified != 1 {
		t.Errorf("HealedUnverified=%d, want 1.\n\nThis heal wrote a chunk whose content "+
			"was never checked against its address — as it happens, the wrong one. "+
			"\"The bytes I wrote landed\" is a weaker claim than \"this chunk is the "+
			"chunk it claims to be\", and the report must not blur them.",
			res.HealedUnverified)
	}
}

// An encrypted chunk cannot be content-checked here (the plaintext hash
// needs the DEK). It must heal, and be counted as unverified.
func TestHeal_EncryptedChunkHealsButIsCountedUnverified(t *testing.T) {
	dst, replica := twoRepos(t)
	h := repo.HashOf([]byte("secret payload"))
	enc := compression.WriteEnvelope(compression.AlgoNone,
		compression.EncryptionFields{EncryptionAlgo: 1},
		[]byte("ciphertext-ish"))
	putRaw(t, replica, repo.ChunkKey(h), enc)
	corruptChunkAt(t, dst, h, env([]byte("local rot")))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h},
		repo.HealOptions{Codecs: healCodecs()})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 || res.Failed != 0 {
		t.Fatalf("encrypted chunk should still heal: %+v", res)
	}
	if res.HealedUnverified != 1 {
		t.Errorf("HealedUnverified=%d, want 1 — an encrypted chunk's plaintext hash "+
			"cannot be checked without the DEK, and the report should say so rather "+
			"than imply the content was validated", res.HealedUnverified)
	}
}
