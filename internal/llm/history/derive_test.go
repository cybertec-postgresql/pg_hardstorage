package history_test

import (
	"bytes"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/history"
)

func TestDeriveDEK_Deterministic(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	a, err := history.DeriveDEK(kek, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.DeriveDEK(kek, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("derivation not deterministic")
	}
	if len(a) != 32 {
		t.Errorf("DEK length = %d, want 32", len(a))
	}
}

func TestDeriveDEK_PerPrincipalIsolation(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	host, _ := history.DeriveDEK(kek, "")
	alice, _ := history.DeriveDEK(kek, "alice")
	bob, _ := history.DeriveDEK(kek, "bob")
	aliceAgain, _ := history.DeriveDEK(kek, "alice")

	if bytes.Equal(host, alice) {
		t.Error("host-scope DEK should differ from per-principal DEK")
	}
	if bytes.Equal(alice, bob) {
		t.Error("alice and bob must derive distinct DEKs from same KEK")
	}
	if !bytes.Equal(alice, aliceAgain) {
		t.Error("same principal must derive identical DEK across calls")
	}
}

func TestDeriveDEK_DistinctKEKsProduceDistinctDEKs(t *testing.T) {
	a, _ := history.DeriveDEK(bytes.Repeat([]byte{1}, 32), "alice")
	b, _ := history.DeriveDEK(bytes.Repeat([]byte{2}, 32), "alice")
	if bytes.Equal(a, b) {
		t.Error("different KEKs must produce different DEKs")
	}
}

func TestDeriveDEK_RejectsBadKEKLength(t *testing.T) {
	if _, err := history.DeriveDEK(make([]byte, 16), ""); err == nil {
		t.Error("16-byte KEK should be rejected")
	}
	if _, err := history.DeriveDEK(make([]byte, 64), ""); err == nil {
		t.Error("64-byte KEK should be rejected")
	}
}

func TestDeriveDEK_DEKDistinctFromKEK(t *testing.T) {
	// Sanity check: the DEK must not be the KEK in disguise
	// (catch a degenerate implementation where info bytes
	// don't propagate into the output).
	kek := bytes.Repeat([]byte{7}, 32)
	dek, _ := history.DeriveDEK(kek, "")
	if bytes.Equal(kek, dek) {
		t.Error("DEK must be cryptographically distinct from KEK")
	}
}
