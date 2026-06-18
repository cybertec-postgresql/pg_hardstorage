// api_truthfulness_test.go — meta-test pinning that every HTTP
// route documented in docs/reference/api/index.md actually
// responds (does not 404) when called against a live control
// plane.
//
// The class of bug this catches: docs advertise an endpoint, but
// the code's route table doesn't register it (or the path
// differs by a `/v1/` prefix vs no prefix).  Found the
// /v1/metrics vs /metrics drift while writing this — docs said
// /v1/metrics but the code serves it at /metrics per Prometheus
// convention; doc was updated.
//
// Approach:
//
//   - Parse documented endpoints from the API reference (lines
//     starting with HTTP verb + path inside fenced code blocks).
//   - Stand up a control-plane Server with no bearer token (so
//     auth doesn't muddy the test) and one configured repo.
//   - For each documented (verb, path) pair, send a request and
//     assert the response is NOT 404.
//   - A 4xx response other than 404 (e.g. 400 / 401 / 405) means
//     the route IS registered but the request isn't fully valid;
//     that's fine for this meta-test — we're pinning REACHABILITY
//     not validity.
//
// Routes with {placeholders} in the doc are turned into concrete
// paths with a fixed sentinel value (`db1` for {d}, `abc` for
// {id}, `000000010000000000000003` for {seg}) — those will most
// likely 404 at the resource layer, so the meta-test only
// asserts the *route prefix* exists by checking that the
// subtree-handler accepts the call (it then 404s at the
// resource level, but with a structured body — different from
// the "no handler" 404 ServeMux returns).
package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// apiDocPath finds docs/reference/api/index.md relative to this
// test file.
func apiDocPath(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/server → ../../docs/reference/api/index.md
	return filepath.Clean(filepath.Join(filepath.Dir(here),
		"..", "..", "docs", "reference", "api", "index.md"))
}

// documentedRoutes parses the API doc and returns a list of
// (verb, path) pairs to ping.  Pulls lines that LOOK like route
// declarations from inside fenced code blocks.
//
// Routes flagged "(v0.5+)" in the line's trailing comment are
// skipped — the docs preamble commits to shipping the v0.1
// contract today and the v0.5 contract later; this meta-test
// pins the v0.1 part.
func documentedRoutes(t *testing.T) []struct{ verb, path string } {
	t.Helper()
	body, err := os.ReadFile(apiDocPath(t))
	if err != nil {
		t.Fatalf("read api doc: %v", err)
	}
	// Match HTTP verb + path + (optional) trailing comment up to EOL.
	row := regexp.MustCompile(`(?m)^\s*(GET|POST|PUT|DELETE|PATCH)\s+(/[a-zA-Z0-9/_{}/-]+)([^\n]*)$`)
	matches := row.FindAllStringSubmatch(string(body), -1)
	var out []struct{ verb, path string }
	for _, m := range matches {
		trailing := m[3]
		if strings.Contains(trailing, "(v0.5+)") {
			// Forward-looking contract; not in the v0.1 surface.
			continue
		}
		out = append(out, struct{ verb, path string }{m[1], m[2]})
	}
	return out
}

// concretizePath replaces documented {placeholders} with stable
// test values so the route-matching layer accepts the request.
var placeholderReplacements = map[string]string{
	"{d}":   "db1",
	"{id}":  "abc",
	"{seg}": "000000010000000000000003",
}

func concretize(path string) string {
	out := path
	for placeholder, replacement := range placeholderReplacements {
		out = strings.ReplaceAll(out, placeholder, replacement)
	}
	return out
}

// TestAPI_DocumentedRoutesReachable: every documented route
// must not 404 on the registered HTTP mux.  Other status codes
// (400 for missing body, 401 for auth, 405 for wrong method)
// are acceptable — they prove the route IS registered.
func TestAPI_DocumentedRoutesReachable(t *testing.T) {
	repoDir := t.TempDir()
	s, err := server.New(server.Config{
		Listen: "127.0.0.1:0",
		// No bearer token → unauthenticated mode → no 401
		// noise.  We're testing route registration, not auth.
		Repos: []string{"file://" + repoDir},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	routes := documentedRoutes(t)
	if len(routes) == 0 {
		t.Fatal("parsed zero documented routes — regex drift")
	}

	type miss struct {
		verb, path, concrete string
		status               int
	}
	var missing []miss

	for _, r := range routes {
		concrete := concretize(r.path)
		req, err := http.NewRequest(r.verb, ts.URL+concrete, nil)
		if err != nil {
			t.Fatalf("NewRequest %s %s: %v", r.verb, concrete, err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Errorf("Do %s %s: %v", r.verb, concrete, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// 404 from net/http's ServeMux means no handler matched.
		// 404 from a registered handler (resource not found at
		// the application layer) is fine.  Distinguish via
		// Content-Type: ServeMux's "404 page not found\n" has
		// no JSON; our handlers emit Content-Type: application/json.
		if resp.StatusCode == http.StatusNotFound {
			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, "application/json") {
				missing = append(missing, miss{r.verb, r.path, concrete, resp.StatusCode})
			}
		}
	}

	if len(missing) > 0 {
		var lines []string
		for _, m := range missing {
			lines = append(lines, "  "+m.verb+" "+m.path+" (tried "+m.concrete+", got "+
				http.StatusText(m.status)+" with no JSON body — mux-level 404)")
		}
		t.Errorf("%d documented HTTP route(s) are not registered in the mux.\n"+
			"Either implement the handler in internal/server/routes.go OR remove\n"+
			"the endpoint from docs/reference/api/index.md:\n%s",
			len(missing), strings.Join(lines, "\n"))
	}
}
