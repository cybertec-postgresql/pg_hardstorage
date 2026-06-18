package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestRepoInit_Text(t *testing.T) {
	tmp := t.TempDir()
	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"repo", "init", "file://" + tmp, "-o", "text"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Repository initialised", "URL:", "ID:", "Schema:", "pg_hardstorage.repo.v1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestRepoInit_JSON(t *testing.T) {
	tmp := t.TempDir()
	root := cli.NewRoot()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"repo", "init", "file://" + tmp, "-o", "json"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var res output.Result
	if err := stdjson.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if res.IsError() {
		t.Fatal("should not be an error")
	}
	bodyBytes, _ := stdjson.Marshal(res.Result)
	var body struct {
		URL    string `json:"url"`
		ID     string `json:"id"`
		Schema string `json:"schema"`
	}
	if err := stdjson.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatal(err)
	}
	if body.URL != "file://"+tmp {
		t.Errorf("url = %q", body.URL)
	}
	if len(body.ID) != 32 {
		t.Errorf("id length = %d", len(body.ID))
	}
	if body.Schema != "pg_hardstorage.repo.v1" {
		t.Errorf("schema = %q", body.Schema)
	}
}

func TestRepoInit_AlreadyExists_ConflictExit7(t *testing.T) {
	tmp := t.TempDir()
	url := "file://" + tmp

	// First init succeeds.
	root := cli.NewRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"repo", "init", url})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("first init exit = %d", exit)
	}

	// Second init must hit conflict and exit 7.
	root2 := cli.NewRoot()
	var stderr bytes.Buffer
	root2.SetOut(&bytes.Buffer{})
	root2.SetErr(&stderr)
	root2.SetArgs([]string{"repo", "init", url, "-o", "json"})
	exit := cli.Run(root2)
	if exit != int(output.ExitConflict) {
		t.Errorf("exit = %d, want ExitConflict(%d)", exit, output.ExitConflict)
	}
	var res output.Result
	if err := stdjson.Unmarshal(stderr.Bytes(), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, stderr.String())
	}
	if !res.IsError() {
		t.Fatal("should be an error result")
	}
	if res.Error.Code != "conflict.repo_exists" {
		t.Errorf("code = %q", res.Error.Code)
	}
}

func TestRepoInit_RequiresURL(t *testing.T) {
	root := cli.NewRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"repo", "init"})
	exit := cli.Run(root)
	if exit == int(output.ExitOK) {
		t.Errorf("missing URL should fail; got exit 0")
	}
}
