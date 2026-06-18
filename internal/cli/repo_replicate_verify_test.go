package cli_test

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// replicateVerifyView mirrors the v1 contract's top-level shape.
type replicateVerifyView struct {
	Schema              string `json:"schema"`
	SourceURL           string `json:"source_url"`
	DestURL             string `json:"dest_url"`
	Verdict             string `json:"verdict"`
	ManifestsConsidered int    `json:"manifests_considered"`
	ManifestsPresent    int    `json:"manifests_present"`
	ManifestsMissing    int    `json:"manifests_missing"`
	ChunksConsidered    int    `json:"chunks_considered"`
	ChunksPresent       int    `json:"chunks_present"`
	ChunksMissing       int    `json:"chunks_missing"`
	ChunksContentDrift  int    `json:"chunks_content_drift"`
	Failures            []struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	} `json:"failures"`
}

// dualReadWorld returns the existing readWorld + a freshly-init'd
// destination repo that operators can replicate INTO.  The
// destination is open via its own fs.Plugin so tests can mutate it.
type dualReadWorld struct {
	*readWorld
	dstSP  storage.StoragePlugin
	dstURL string
}

func newDualReadWorld(t *testing.T) *dualReadWorld {
	t.Helper()
	w := newReadWorld(t)
	root := t.TempDir()
	dstURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: dstURL}); err != nil {
		t.Fatal(err)
	}
	dstSP := &fs.Plugin{}
	if err := dstSP.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dstSP.Close() })
	return &dualReadWorld{readWorld: w, dstSP: dstSP, dstURL: dstURL}
}

// replicate copies primary → replica via the existing CLI command
// (which exercises the same code path operators use).
func (w *dualReadWorld) replicate(t *testing.T) {
	t.Helper()
	_, _, exit := runCLI(t, "repo", "replicate",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo replicate exit = %d", exit)
	}
}

// TestRepoReplicateVerify_RequiresBothFlags
func TestRepoReplicateVerify_RequiresBothFlags(t *testing.T) {
	_ = newDualReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", "file:///nope", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--to missing: exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	_, errb, exit = runCLI(t, "repo", "replicate", "verify",
		"--to", "file:///nope", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--from missing: exit = %d, want ExitMisuse", exit)
	}
}

// TestRepoReplicateVerify_SameURLRefused
func TestRepoReplicateVerify_SameURLRefused(t *testing.T) {
	w := newDualReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRepoReplicateVerify_BadFormat
func TestRepoReplicateVerify_BadFormat(t *testing.T) {
	w := newDualReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL,
		"--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRepoReplicateVerify_BothEmpty: two fresh repos verify clean.
func TestRepoReplicateVerify_BothEmpty(t *testing.T) {
	w := newDualReadWorld(t)
	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view replicateVerifyView
	bodyOf(t, stdout, &view)
	if view.Verdict != "consistent" {
		t.Errorf("Verdict = %q, want consistent", view.Verdict)
	}
}

// TestRepoReplicateVerify_Consistent: replicated → consistent
// verdict + exit 0.
func TestRepoReplicateVerify_Consistent(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload"))
	w.replicate(t)

	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view replicateVerifyView
	bodyOf(t, stdout, &view)
	if view.Verdict != "consistent" {
		t.Errorf("Verdict = %q\n%+v", view.Verdict, view)
	}
	if view.ManifestsPresent != 1 || view.ChunksPresent != 1 {
		t.Errorf("counts off: %+v", view)
	}
}

// TestRepoReplicateVerify_Broken_NoReplicate: source has a backup,
// destination is empty → broken verdict, exit 9.
func TestRepoReplicateVerify_Broken_NoReplicate(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload"))
	// NOT calling replicate.

	stdout, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed (%d)\nstderr:\n%s",
			exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.replica_inconsistent") {
		t.Errorf("expected verify.replica_inconsistent:\n%s", errb)
	}
	// Body still rendered to stdout.
	var view replicateVerifyView
	if err := unmarshalDrillBody(stdout, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Verdict != "broken" {
		t.Errorf("Verdict = %q, want broken", view.Verdict)
	}
}

// TestRepoReplicateVerify_DeploymentFilter
func TestRepoReplicateVerify_DeploymentFilter(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w.readWorld, "db2", 1, []byte("b"))
	w.replicate(t)
	// Now plant another backup on src (not replicated) for db2.
	commitVerifiableBackup(t, w.readWorld, "db2", 5, []byte("c"))

	// Without filter: detects db2's missing backup.
	_, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("global exit = %d, want ExitVerifyFailed", exit)
	}

	// With deployment=db1 filter: db2's drift is invisible →
	// consistent.
	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL,
		"--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("filtered exit = %d\n%s", exit, stdout)
	}
	var view replicateVerifyView
	bodyOf(t, stdout, &view)
	if view.Verdict != "consistent" {
		t.Errorf("filtered Verdict = %q", view.Verdict)
	}
}

// TestRepoReplicateVerify_DeepFlag: a deep verify catches
// same-size-different-bytes drift.
func TestRepoReplicateVerify_DeepFlag(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload-X"))
	w.replicate(t)

	// Mutate a chunk on dst with same-size but different bytes.
	for info, lerr := range w.dstSP.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		srcInfo, err := w.sp.Stat(context.Background(), info.Key)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, srcInfo.Size)
		for i := range body {
			body[i] = 'X'
		}
		_ = w.dstSP.Delete(context.Background(), info.Key)
		if _, err := w.dstSP.Put(context.Background(), info.Key,
			bytesReaderFor(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatalf("put: %v", err)
		}
		break
	}

	// Stat-only: same size → consistent.
	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("stat-only exit = %d\n%s", exit, stdout)
	}

	// Deep mode: byte mismatch detected → drifted → exit 9.
	_, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "--deep", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("deep exit = %d, want ExitVerifyFailed\n%s", exit, errb)
	}
	if !strings.Contains(errb, "drifted") {
		t.Errorf("expected drifted in stderr:\n%s", errb)
	}
}

// TestRepoReplicateVerify_TextFormat
func TestRepoReplicateVerify_TextFormat(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload"))
	w.replicate(t)

	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"repo replicate verify",
		"Verdict:",
		"CONSISTENT",
		"Manifests:",
		"Chunks:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoReplicateVerify_MarkdownFormat
func TestRepoReplicateVerify_MarkdownFormat(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload"))
	w.replicate(t)

	stdout, _, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL,
		"--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"# pg_hardstorage repo replicate verify",
		"## Verdict",
		"## Counters",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoReplicateVerify_HelpDiscoverable
func TestRepoReplicateVerify_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "repo", "replicate", "verify", "--help")
	for _, want := range []string{
		"--from", "--to", "--deployment", "--include-wal",
		"--deep", "--format",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("repo replicate verify --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "repo", "replicate", "--help")
	if !strings.Contains(stdout, "verify") {
		t.Errorf("repo replicate --help missing verify subcommand:\n%s", stdout)
	}
}

// TestRepoReplicateVerify_FailuresInBody: failures slice is
// surfaced in the JSON body.
func TestRepoReplicateVerify_FailuresInBody(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("payload"))
	// NOT replicated.

	stdout, _, _ := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL, "-o", "json")
	var view replicateVerifyView
	if err := unmarshalDrillBody(stdout, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Failures) == 0 {
		t.Errorf("expected failures in body: %+v", view)
	}
	hasMissingManifest := false
	for _, f := range view.Failures {
		if f.Kind == "missing_manifest" {
			hasMissingManifest = true
		}
	}
	if !hasMissingManifest {
		t.Errorf("expected missing_manifest in failures: %+v", view.Failures)
	}
}

// TestRepoReplicateVerify_BadRepoURL: pointing at a nonexistent
// repo surfaces a clean error.
func TestRepoReplicateVerify_BadRepoURL(t *testing.T) {
	w := newDualReadWorld(t)
	_, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", "file:///does/not/exist",
		"--to", w.dstURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit; got %d", exit)
	}
	if !strings.Contains(errb, "notfound") &&
		!strings.Contains(errb, "open") {
		t.Errorf("expected structured error:\n%s", errb)
	}
}

// bytesReaderFor returns a *bytes.Reader so io.EOF flows
// correctly through the fs storage plugin's body-reader.
func bytesReaderFor(b []byte) *bytes.Reader { return bytes.NewReader(b) }
