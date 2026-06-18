package history_test

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/history"
)

func newStore(t *testing.T) *history.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var dek [32]byte
	rand.Read(dek[:])
	if err := s.SetDEK(dek[:]); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip_BasicSession(t *testing.T) {
	s := newStore(t)
	w, err := s.Open(history.SessionMeta{
		Principal:  "alice",
		Deployment: "db1",
		Skill:      "restore",
		SkillVer:   "1.0",
		Model:      "gpt-4o",
		Provider:   "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(history.Entry{Role: "user", Op: "prompt", Body: json.RawMessage(`"how do I restore?"`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(history.Entry{Role: "assistant", Op: "response", Body: json.RawMessage(`"run pg_hardstorage restore"`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(0); err != nil {
		t.Fatal(err)
	}

	entries, meta, err := s.Read("alice", "db1", w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
	if meta.Skill != "restore" {
		t.Errorf("meta.Skill = %q", meta.Skill)
	}
	if meta.EntryCount != 2 {
		t.Errorf("meta.EntryCount = %d", meta.EntryCount)
	}
	if meta.ExitCode != 0 {
		t.Errorf("meta.ExitCode = %d", meta.ExitCode)
	}
}

func TestRequiresDEK(t *testing.T) {
	s, err := history.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(history.SessionMeta{Principal: "x", Skill: "y"}); err != history.ErrNoDEK {
		t.Errorf("expected ErrNoDEK, got %v", err)
	}
}

func TestSetDEK_RejectsBadLength(t *testing.T) {
	s, _ := history.New(t.TempDir())
	if err := s.SetDEK(make([]byte, 16)); err == nil {
		t.Error("expected error for 16-byte key")
	}
	if err := s.SetDEK(make([]byte, 32)); err != nil {
		t.Errorf("32-byte key should be accepted: %v", err)
	}
}

func TestRead_TamperingDetected(t *testing.T) {
	s := newStore(t)
	w, _ := s.Open(history.SessionMeta{Principal: "alice", Skill: "x"})
	w.Append(history.Entry{Role: "user", Op: "prompt"})
	w.Close(0)

	// Wrong DEK should surface as ErrTampered (AEAD auth fail).
	other, _ := history.New(t.TempDir())
	wrongDEK := make([]byte, 32)
	for i := range wrongDEK {
		wrongDEK[i] = 7
	}
	if err := other.SetDEK(wrongDEK); err != nil {
		t.Fatal(err)
	}
	// Re-open via the original store but with a different key.
	if err := s.SetDEK(wrongDEK); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Read("alice", "", w.SessionID())
	if err == nil || !strings.Contains(err.Error(), "tampering") {
		t.Errorf("expected ErrTampered; got %v", err)
	}
}

func TestList_OrdersByStartedAt(t *testing.T) {
	s := newStore(t)
	for _, skill := range []string{"alpha", "beta", "gamma"} {
		w, _ := s.Open(history.SessionMeta{Principal: "alice", Skill: skill, StartedAt: time.Now().UTC()})
		w.Append(history.Entry{Role: "user", Op: "prompt"})
		w.Close(0)
		time.Sleep(2 * time.Millisecond)
	}
	metas, err := s.List("alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 3 {
		t.Fatalf("expected 3 metas, got %d", len(metas))
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if metas[i].Skill != want {
			t.Errorf("metas[%d].Skill = %q, want %q", i, metas[i].Skill, want)
		}
	}
}

func TestShred_DeletesSessions(t *testing.T) {
	s := newStore(t)
	w, _ := s.Open(history.SessionMeta{Principal: "alice", Deployment: "db1", Skill: "x"})
	w.Append(history.Entry{Role: "user", Op: "prompt"})
	w.Close(0)

	count, err := s.Shred("alice", "db1")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("Shred returned %d, want 1", count)
	}
	if _, _, err := s.Read("alice", "db1", w.SessionID()); err == nil {
		t.Error("Read after Shred should fail")
	}
}

func TestPrincipalIsolation(t *testing.T) {
	s := newStore(t)
	w, _ := s.Open(history.SessionMeta{Principal: "alice", Skill: "x"})
	w.Append(history.Entry{Role: "user", Op: "prompt"})
	w.Close(0)

	// `bob` should not see alice's sessions.
	bobMetas, _ := s.List("bob", "")
	if len(bobMetas) != 0 {
		t.Errorf("expected 0 metas for bob; got %v", bobMetas)
	}
}

func TestSessionIDIsAssignedWhenMissing(t *testing.T) {
	s := newStore(t)
	w, err := s.Open(history.SessionMeta{Principal: "alice", Skill: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if w.SessionID() == "" {
		t.Error("Open should auto-assign a session ID")
	}
}

func TestEmptySessionIsNotPersisted(t *testing.T) {
	s := newStore(t)
	w, _ := s.Open(history.SessionMeta{Principal: "alice", Skill: "x"})
	if err := w.Close(0); err != nil {
		t.Fatal(err)
	}
	metas, _ := s.List("alice", "")
	if len(metas) != 0 {
		t.Errorf("empty session shouldn't write files; got %d", len(metas))
	}
}

func TestHashPrincipal_Stable(t *testing.T) {
	a := history.HashPrincipal("alice@acme.example.com")
	b := history.HashPrincipal("alice@acme.example.com")
	if a != b {
		t.Errorf("HashPrincipal not stable: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("HashPrincipal length = %d, want 16", len(a))
	}
}
