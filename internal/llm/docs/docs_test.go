package docs_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/docs"
)

func TestAll_LoadsBundledCorpus(t *testing.T) {
	all, err := docs.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// We must see at least the seven runbooks + CHANGELOG + README.
	if len(all) < 9 {
		t.Fatalf("expected ≥ 9 bundled docs (R1-R7 + CHANGELOG + README); got %d", len(all))
	}
	wantIDs := map[string]bool{
		"R1": false, "R2": false, "R3": false, "R4": false,
		"R5": false, "R6": false, "R7": false,
		"CHANGELOG": false, "README": false,
	}
	for _, d := range all {
		if _, ok := wantIDs[d.ID]; ok {
			wantIDs[d.ID] = true
		}
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("expected %s in corpus", id)
		}
	}
}

func TestGet_RunbookByID(t *testing.T) {
	d, err := docs.Get("R3")
	if err != nil {
		t.Fatalf("Get(R3): %v", err)
	}
	if d.ID != "R3" {
		t.Errorf("ID = %q, want R3", d.ID)
	}
	if d.Title == "" {
		t.Errorf("Title should be derived from H1; got empty")
	}
	if !strings.Contains(strings.ToLower(d.Body), "cold") {
		t.Errorf("R3 body should mention 'cold' (cold-start runbook); got %q...", d.Body[:min(80, len(d.Body))])
	}
}

func TestGet_CaseInsensitive(t *testing.T) {
	a, _ := docs.Get("r1")
	b, _ := docs.Get("R1")
	if a.ID != b.ID || a.ID == "" {
		t.Errorf("case-insensitive lookup mismatch: %q vs %q", a.ID, b.ID)
	}
}

func TestGet_NotFound(t *testing.T) {
	_, err := docs.Get("R99")
	if err == nil {
		t.Fatal("expected error for unknown ID")
	}
	if !strings.Contains(err.Error(), "R99") {
		t.Errorf("error should name the unknown ID; got %v", err)
	}
}

func TestSearch_FindsHits(t *testing.T) {
	matches, err := docs.Search("recovery")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected ≥ 1 match for 'recovery'")
	}
	for _, m := range matches {
		if len(m.Excerpts) == 0 {
			t.Errorf("match %s has no excerpts", m.Doc.ID)
		}
		// Every excerpt should contain the query (case-insensitively).
		for _, e := range m.Excerpts {
			if !strings.Contains(strings.ToLower(e), "recovery") {
				t.Errorf("excerpt missing query term: %q", e)
			}
		}
	}
}

func TestSearch_EmptyQueryRejected(t *testing.T) {
	if _, err := docs.Search(""); err == nil {
		t.Error("empty query should error")
	}
	if _, err := docs.Search("   "); err == nil {
		t.Error("whitespace-only query should error")
	}
}

func TestRunbookIndex_OnlyR_Prefixed(t *testing.T) {
	idx, err := docs.RunbookIndex()
	if err != nil {
		t.Fatalf("RunbookIndex: %v", err)
	}
	if len(idx) != 7 {
		t.Errorf("expected 7 runbooks (R1-R7); got %d", len(idx))
	}
	for _, e := range idx {
		if e.ID == "" || e.ID[0] != 'R' {
			t.Errorf("RunbookIndex entry has unexpected ID %q (only R-prefixed should appear)", e.ID)
		}
		if e.Title == "" {
			t.Errorf("entry %s missing Title", e.ID)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
