package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// integrityRunSummaryView matches integrityRunSummary.
type integrityRunSummaryView struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Strategy         string    `json:"strategy"`
	StartedAt        time.Time `json:"started_at"`
	Deployment       string    `json:"deployment,omitempty"`
	ManifestsTotal   int       `json:"manifests_total"`
	SignaturesFail   int       `json:"signatures_fail"`
	ChunksReferenced int       `json:"chunks_referenced"`
	ChunksMissing    int       `json:"chunks_missing"`
	ChunksMismatched int       `json:"chunks_mismatched"`
}

type integrityListView struct {
	Count   int                       `json:"count"`
	Entries []integrityRunSummaryView `json:"entries"`
}

type integrityRunView struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Strategy struct {
		Mode string `json:"mode"`
	} `json:"strategy"`
	Manifests struct {
		Total          int `json:"total"`
		SignaturesOK   int `json:"signatures_ok"`
		SignaturesFail int `json:"signatures_fail"`
	} `json:"manifests"`
	Chunks struct {
		DistinctReferenced int `json:"distinct_referenced"`
		PresenceChecked    int `json:"presence_checked"`
		Missing            int `json:"missing"`
		Sampled            int `json:"sampled"`
		Verified           int `json:"verified"`
	} `json:"chunks"`
	Deployment           string `json:"deployment,omitempty"`
	Note                 string `json:"note,omitempty"`
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	BodyHash             string `json:"body_hash,omitempty"`
	Signature            string `json:"signature,omitempty"`
}

type integrityVerifyView struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
	SignatureValid       bool   `json:"signature_valid"`
	Reason               string `json:"reason,omitempty"`
}

// commitMinimalManifest plants a tiny but realistic manifest into the
// repo using the readWorld's signer + manifest store, with N chunks
// of known plaintext via the CAS.  Returns the manifest.
func commitMinimalManifest(t *testing.T, w *readWorld, deployment, suffix string, chunks int) *backup.Manifest {
	t.Helper()
	cas := casdefault.New(w.sp)
	files := []backup.FileEntry{}
	chunkRefs := []backup.ChunkRef{}
	var totalSize int64
	for i := 0; i < chunks; i++ {
		body := []byte(suffix + "-c-" + string(rune('a'+i)))
		ci, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatalf("PutChunk: %v", err)
		}
		chunkRefs = append(chunkRefs, backup.ChunkRef{
			Hash:   ci.Hash,
			Offset: totalSize, // contiguous offsets — Validate requires this
			Len:    int64(len(body)),
		})
		totalSize += int64(len(body))
	}
	files = append(files, backup.FileEntry{
		Path:   "PG_VERSION",
		Size:   totalSize, // chunk sum == size — Validate requires this
		Mode:   0o600,
		Chunks: chunkRefs,
	})
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + suffix,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            files,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return m
}

// ----- run -----

func TestIntegrityRun_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "run", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestIntegrityRun_BadStrategy(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "exotic-mode",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestIntegrityRun_HappyPath_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "manifests-only",
		"--note", "first run",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v integrityRunView
	bodyOf(t, stdout, &v)
	if v.Status != "ok" {
		t.Errorf("Status = %q, want ok", v.Status)
	}
	if v.Manifests.Total != 0 {
		t.Errorf("Total = %d, want 0", v.Manifests.Total)
	}
	if v.PublicKeyFingerprint == "" || v.Signature == "" {
		t.Errorf("expected signed run; got fingerprint=%q sig=%q",
			v.PublicKeyFingerprint, v.Signature)
	}
}

func TestIntegrityRun_HappyPath_WithBackups(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "a", 3)
	commitMinimalManifest(t, w, "db1", "b", 2)

	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "presence",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v integrityRunView
	bodyOf(t, stdout, &v)
	if v.Status != "ok" {
		t.Errorf("Status = %q, want ok", v.Status)
	}
	if v.Manifests.Total != 2 || v.Manifests.SignaturesOK != 2 {
		t.Errorf("Manifests: %+v", v.Manifests)
	}
	if v.Chunks.DistinctReferenced != 5 {
		t.Errorf("DistinctReferenced = %d, want 5", v.Chunks.DistinctReferenced)
	}
}

func TestIntegrityRun_DetectsMissingChunk(t *testing.T) {
	w := newReadWorld(t)
	m := commitMinimalManifest(t, w, "db1", "x", 3)
	// Bit-rot one chunk.
	first := m.Files[0].Chunks[0].Hash
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(first)); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "presence",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want ExitVerifyFailed (9)\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.integrity_issues") {
		t.Errorf("expected verify.integrity_issues:\n%s", errb)
	}
	// Dual-stream: body still on stdout.
	var v integrityRunView
	bodyOf(t, stdout, &v)
	if v.Status != "found_issues" {
		t.Errorf("Status = %q", v.Status)
	}
	if v.Chunks.Missing != 1 {
		t.Errorf("Missing = %d, want 1", v.Chunks.Missing)
	}
}

func TestIntegrityRun_PersistsRun(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "a", 1)
	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "manifests-only",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("first run exit = %d", exit)
	}
	var run integrityRunView
	bodyOf(t, stdout, &run)

	// List should now show 1 entry, status ok, with that ID.
	stdout, _, exit = runCLI(t, "integrity", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var list integrityListView
	bodyOf(t, stdout, &list)
	if list.Count != 1 {
		t.Fatalf("Count = %d, want 1", list.Count)
	}
	if list.Entries[0].ID != run.ID || list.Entries[0].Status != "ok" {
		t.Errorf("list entry mismatch: %+v vs %+v", list.Entries[0], run)
	}
}

func TestIntegrityRun_ContentSampleDeterminism(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "x", 8)

	args := []string{"integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "content-sample",
		"--percent", "50",
		"--seed", "42",
		"-o", "json"}
	stdout, _, exit := runCLI(t, args...)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v integrityRunView
	bodyOf(t, stdout, &v)
	if v.Chunks.Sampled != 4 {
		t.Errorf("Sampled = %d, want 4", v.Chunks.Sampled)
	}
	if v.Chunks.Verified != 4 {
		t.Errorf("Verified = %d, want 4", v.Chunks.Verified)
	}
}

// ----- list -----

func TestIntegrityList_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "list", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestIntegrityList_BadStatus(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "list",
		"--repo", w.repoURL, "--status", "exotic", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestIntegrityList_BadSince(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "list",
		"--repo", w.repoURL, "--since", "yesterday", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestIntegrityList_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "integrity", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v integrityListView
	bodyOf(t, stdout, &v)
	if v.Count != 0 {
		t.Errorf("Count = %d", v.Count)
	}
}

// ----- show -----

func TestIntegrityShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "show", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.run") {
		t.Errorf("expected notfound.run:\n%s", errb)
	}
}

func TestIntegrityShow_Happy(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "a", 1)
	// Create a run.
	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "manifests-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("run exit = %d", exit)
	}
	var run integrityRunView
	bodyOf(t, stdout, &run)

	// Show.
	stdout, _, exit = runCLI(t, "integrity", "show", run.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show exit = %d\n%s", exit, stdout)
	}
	var got integrityRunView
	bodyOf(t, stdout, &got)
	if got.ID != run.ID || got.Status != run.Status {
		t.Errorf("show drift: %+v vs %+v", got, run)
	}
}

// ----- verify -----

func TestIntegrityVerify_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "integrity", "verify", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.run") {
		t.Errorf("expected notfound.run:\n%s", errb)
	}
}

func TestIntegrityVerify_Happy(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "a", 1)
	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "manifests-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("run exit = %d", exit)
	}
	var run integrityRunView
	bodyOf(t, stdout, &run)

	stdout, _, exit = runCLI(t, "integrity", "verify", run.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit = %d\n%s", exit, stdout)
	}
	var v integrityVerifyView
	bodyOf(t, stdout, &v)
	if !v.SignatureValid {
		t.Errorf("SignatureValid = false")
	}
	if v.Status != run.Status {
		t.Errorf("Status drift")
	}
}

func TestIntegrityVerify_Tampered(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "a", 1)
	stdout, _, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL,
		"--strategy", "manifests-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("run exit = %d", exit)
	}
	var run integrityRunView
	bodyOf(t, stdout, &run)

	// Tamper: flip the run's persisted status from "ok" to
	// "found_issues" by re-loading + mutating via the integrity
	// store directly.
	store := integrity.NewRunStore(w.sp)
	r, err := store.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = integrity.StatusFoundIssues
	if err := store.Put(context.Background(), r); err == nil {
		// Put refuses to overwrite (RenameIfNotExists); so we need to
		// reach in via the storage plugin directly.
		t.Skip("Put unexpectedly accepted overwrite — refactor needed")
	}
	// Use Storage plugin to delete + rewrite the underlying object.
	if err := w.sp.Delete(context.Background(), "integrity/runs/"+run.ID+".json"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("re-Put: %v", err)
	}

	stdout, errb, exit := runCLI(t, "integrity", "verify", run.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("verify exit = %d, want ExitVerifyFailed\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.integrity_signature") {
		t.Errorf("expected verify.integrity_signature:\n%s", errb)
	}
	// Dual-stream: body emitted with SignatureValid=false.
	var v integrityVerifyView
	bodyOf(t, stdout, &v)
	if v.SignatureValid {
		t.Errorf("SignatureValid = true after tamper")
	}
}

// TestIntegrity_HelpDiscoverable: parent help names every subcommand.
func TestIntegrity_HelpDiscoverable(t *testing.T) {
	stdout, _, exit := runCLI(t, "integrity", "--help")
	if exit != int(output.ExitOK) {
		t.Fatalf("help exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{"run", "list", "show", "verify"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
}
