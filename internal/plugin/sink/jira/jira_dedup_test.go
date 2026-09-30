package jira_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// fuzzyJira models the part of JIRA that matters for dedup: search is
// JQL `summary ~` — a tokenised text match, not equality — so it can
// return issues whose summary merely contains the search terms, in
// relevance order. Issues created via POST become visible to later
// searches.
type fuzzyJira struct {
	mu          sync.Mutex
	issues      []fuzzyIssue // search returns these in order
	created     int
	comments    map[string]int
	searchDelay time.Duration
}

type fuzzyIssue struct{ key, summary string }

func (j *fuzzyJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/search":
		time.Sleep(j.searchDelay) // widen the find-then-create window
		j.mu.Lock()
		issues := []any{}
		for _, is := range j.issues {
			issues = append(issues, map[string]any{
				"key": is.key, "fields": map[string]any{"summary": is.summary},
			})
		}
		j.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})

	case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue":
		var body struct {
			Fields struct {
				Summary string `json:"summary"`
			} `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		j.mu.Lock()
		j.created++
		key := fmt.Sprintf("OPS-%d", 100+j.created)
		j.issues = append(j.issues, fuzzyIssue{key, body.Fields.Summary})
		j.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key})

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment"):
		key := strings.Split(r.URL.Path, "/")[5]
		j.mu.Lock()
		if j.comments == nil {
			j.comments = map[string]int{}
		}
		j.comments[key]++
		j.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))

	default:
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

// A fuzzy hit for a *different* deployment must not swallow the
// event: "deployment=db1" is a token-subset of "deployment=db1-replica"
// to JQL `~`, and it may rank first.
func TestJira_Dedup_IgnoresNonExactSummaryHits(t *testing.T) {
	j := &fuzzyJira{issues: []fuzzyIssue{
		{"OPS-7", "[pg_hardstorage] backup_failed · deployment=db1-replica"},
		{"OPS-8", "[pg_hardstorage] backup_failed · deployment=db1 (manual)"},
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	s := mustBuild(t, srv.URL, nil)

	ev := output.NewEvent(output.SeverityError, "backup", "backup_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.comments) != 0 {
		t.Errorf("commented on a non-matching issue: %v", j.comments)
	}
	if j.created != 1 {
		t.Errorf("created = %d, want 1", j.created)
	}
}

// The exact match is found even when it is not the top-ranked hit.
func TestJira_Dedup_FindsExactMatchBelowFuzzyHits(t *testing.T) {
	j := &fuzzyJira{issues: []fuzzyIssue{
		{"OPS-7", "[pg_hardstorage] backup_failed · deployment=db1-replica"},
		{"OPS-9", "[pg_hardstorage] backup_failed · deployment=db1"},
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	s := mustBuild(t, srv.URL, nil)

	ev := output.NewEvent(output.SeverityError, "backup", "backup_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.created != 0 || j.comments["OPS-9"] != 1 {
		t.Errorf("created=%d comments=%v; want 0 creates and one comment on OPS-9", j.created, j.comments)
	}
}

// The dispatcher emits each event on its own goroutine, so a burst of
// identical events reaches Emit concurrently. Find-or-create must be
// serialised per dedup key or every racer sees "no issue" and creates
// its own.
func TestJira_Dedup_ConcurrentEmitsCreateOneIssue(t *testing.T) {
	j := &fuzzyJira{searchDelay: 50 * time.Millisecond}
	srv := httptest.NewServer(j)
	defer srv.Close()
	s := mustBuild(t, srv.URL, nil)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := output.NewEvent(output.SeverityError, "backup", "backup_failed").
				WithSubject(output.Subject{Deployment: "db1"})
			errs <- s.Emit(context.Background(), ev)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	total := 0
	for _, c := range j.comments {
		total += c
	}
	if j.created != 1 || total != n-1 {
		t.Errorf("created=%d comments=%d; want 1 issue and %d comments", j.created, total, n-1)
	}
}
