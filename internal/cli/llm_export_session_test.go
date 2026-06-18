package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	stdjson "encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestLlmExportSession_HappyPath: seed an audit chain with llm.*
// events for a session, run export-session, verify the produced
// .tar.gz contains transcript + manifest + chain proof.
func TestLlmExportSession_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Seed three llm.* events all carrying the same session_id.
	repoMeta, sp, err := openRepoForTest(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	ctx := context.Background()
	for i, action := range []string{"llm.session_started", "llm.prompt", "llm.response"} {
		if err := store.Append(ctx, &audit.Event{
			Action:    action,
			Timestamp: time.Now().Add(time.Duration(i) * time.Millisecond).UTC(),
			Body: map[string]any{
				"session_id": "test-session-1",
				"skill":      "ask",
				"provider":   "mock",
				"action_idx": i,
			},
		}); err != nil {
			t.Fatalf("seed event %s: %v", action, err)
		}
	}

	bundlePath := filepath.Join(tmp, "out.tar.gz")
	stdout, stderr, exit := runCLI(t,
		"llm", "export-session", "test-session-1",
		"--repo", repoURL,
		"--out", bundlePath,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(res.Result)
	var got struct {
		SessionID  string `json:"session_id"`
		Path       string `json:"path"`
		EventCount int    `json:"event_count"`
		FirstHash  string `json:"first_hash"`
		LastHash   string `json:"last_hash"`
	}
	if err := stdjson.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "test-session-1" {
		t.Errorf("session_id = %q", got.SessionID)
	}
	if got.EventCount != 3 {
		t.Errorf("event_count = %d, want 3", got.EventCount)
	}
	if got.FirstHash == "" || got.LastHash == "" {
		t.Errorf("hashes should be set; got %+v", got)
	}

	// Inspect the bundle.
	tarBody, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, tarBody)
	if _, has := files["transcript.ndjson"]; !has {
		t.Error("transcript.ndjson missing from bundle")
	}
	if _, has := files["manifest.json"]; !has {
		t.Error("manifest.json missing from bundle")
	}
	if _, has := files["audit_chain_proof.json"]; !has {
		t.Error("audit_chain_proof.json missing from bundle")
	}
	// Transcript should have 3 lines (one per event).
	lines := strings.Split(strings.TrimRight(string(files["transcript.ndjson"]), "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("transcript has %d lines, want 3", len(lines))
	}
	// Manifest should mention the session id + skill + provider.
	mani := string(files["manifest.json"])
	for _, want := range []string{"test-session-1", "ask", "mock"} {
		if !strings.Contains(mani, want) {
			t.Errorf("manifest should mention %q; got %s", want, mani)
		}
	}
	// Chain proof should have first_event_hash + last_event_hash.
	proof := string(files["audit_chain_proof.json"])
	for _, want := range []string{"first_event_hash", "last_event_hash"} {
		if !strings.Contains(proof, want) {
			t.Errorf("chain proof should include %q; got %s", want, proof)
		}
	}
}

func TestLlmExportSession_NoEvents(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(repoDir, 0o755)
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	_, stderr, exit := runCLI(t,
		"llm", "export-session", "no-such-session",
		"--repo", repoURL,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit for missing session; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "notfound.session") {
		t.Errorf("expected notfound.session error; got %s", stderr)
	}
}

func TestLlmExportSession_MissingRepoFlag(t *testing.T) {
	_, stderr, exit := runCLI(t, "llm", "export-session", "x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse without --repo; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag error; got %s", stderr)
	}
}

// ----- helpers -----

func readTarGz(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = buf
	}
	return out
}

// openRepoForTest opens the named repo via the public
// repo.Open API (the cli package's openRepo is unexported).
func openRepoForTest(repoURL string) (*repo.Metadata, storage.StoragePlugin, error) {
	return repo.Open(context.Background(), repoURL)
}
