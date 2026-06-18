package reproducer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/reproducer"
)

// readBundle inflates a bundle written by Write and returns the
// map of {filename → body} for assertion.
func readBundle(t *testing.T, body []byte) map[string][]byte {
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
		buf := bytes.NewBuffer(nil)
		if _, err := io.Copy(buf, tr); err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = buf.Bytes()
	}
	return out
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBundle_BasicFiles(t *testing.T) {
	dir := t.TempDir()
	fleetPath := writeFile(t, dir, "fleet.yaml", "schema: pg_hardstorage.testkit.fleet.v1\n")
	profilesPath := writeFile(t, dir, "profiles.yaml", "schema: pg_hardstorage.testkit.profile.v1\n")
	faultsPath := writeFile(t, dir, "faults.yaml", "schema: pg_hardstorage.testkit.fault.v1\n")
	composePath := writeFile(t, dir, "docker-compose.yaml", "name: pgvalidate\n")

	b := &reproducer.Bundle{
		FailingCell:      "u24-pg17",
		Iteration:        42,
		Seed:             12345,
		FailureMessage:   "pg_verifybackup exited 1",
		ProjectName:      "pgvalidate-soak",
		FleetYAMLPath:    fleetPath,
		ProfilesYAMLPath: profilesPath,
		FaultsYAMLPath:   faultsPath,
		ComposeYAMLPath:  composePath,
	}

	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}

	files := readBundle(t, buf.Bytes())
	for _, f := range []string{
		"bundle-manifest.json", "fleet.yaml", "profiles.yaml",
		"faults.yaml", "docker-compose.yaml", "replay.sh",
	} {
		if _, ok := files[f]; !ok {
			t.Errorf("bundle missing %q (have: %v)", f, keys(files))
		}
	}

	// bundle-manifest must round-trip the failure metadata.
	var m struct {
		Schema         string `json:"schema"`
		FailingCell    string `json:"failing_cell"`
		Iteration      int    `json:"iteration"`
		Seed           int64  `json:"seed"`
		FailureMessage string `json:"failure_message"`
		ProjectName    string `json:"project_name"`
	}
	if err := json.Unmarshal(files["bundle-manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.FailingCell != "u24-pg17" || m.Iteration != 42 || m.Seed != 12345 {
		t.Errorf("manifest payload wrong: %+v", m)
	}
	if !strings.HasPrefix(m.Schema, "pg_hardstorage.testkit.reproducer.v1") {
		t.Errorf("schema: %q", m.Schema)
	}

	// replay.sh contains the seed + iteration.
	rep := string(files["replay.sh"])
	if !strings.Contains(rep, "SEED=12345") {
		t.Errorf("replay.sh missing seed: %s", rep)
	}
	if !strings.Contains(rep, "ITER=42") {
		t.Errorf("replay.sh missing iteration: %s", rep)
	}
	if !strings.Contains(rep, "--only-cell \"$CELL\"") {
		t.Errorf("replay.sh missing --only-cell flag: %s", rep)
	}
}

func TestBundle_MetadataPaths(t *testing.T) {
	dir := t.TempDir()
	mPath := writeFile(t, dir, "manifest.json",
		`{"backup_id":"u24-pg17.full.20260505T120000Z"}`)

	subdir := filepath.Join(dir, "audit")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, subdir, "001.json", `{"event":"backup_started"}`)
	writeFile(t, subdir, "002.json", `{"event":"backup_completed"}`)

	fleetPath := writeFile(t, dir, "fleet.yaml", "schema: x\n")

	b := &reproducer.Bundle{
		FailingCell:   "u24-pg17",
		FleetYAMLPath: fleetPath,
		MetadataPaths: []string{mPath, subdir},
		ProjectName:   "p",
	}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, buf.Bytes())

	if _, ok := files["metadata/manifest.json"]; !ok {
		t.Errorf("expected metadata/manifest.json (have: %v)", keys(files))
	}
	if _, ok := files["metadata/audit/001.json"]; !ok {
		t.Errorf("expected metadata/audit/001.json (have: %v)", keys(files))
	}
	if _, ok := files["metadata/audit/002.json"]; !ok {
		t.Errorf("expected metadata/audit/002.json (have: %v)", keys(files))
	}
}

func TestBundle_RejectsEmptyCell(t *testing.T) {
	b := &reproducer.Bundle{}
	var buf bytes.Buffer
	if err := b.Write(&buf); err == nil {
		t.Errorf("expected error for empty FailingCell")
	}
}

func TestBundle_WriteToFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "failure.tar.gz")
	b := &reproducer.Bundle{
		FailingCell: "x", ProjectName: "p", CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := b.WriteToFile(target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected mode 0600; got %v", info.Mode().Perm())
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
