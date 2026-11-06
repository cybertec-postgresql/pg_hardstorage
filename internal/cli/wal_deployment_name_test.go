package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// writeTestSegment writes a full-size WAL segment file named name and
// returns its path.
func writeTestSegment(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*13 + 7) % 251)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pushTestSegment(t *testing.T, repoURL, deployment, segment string) (stderr string, exit int) {
	t.Helper()
	_, stderr, exit = runCLI(t, "wal", "push", deployment, writeTestSegment(t, segment),
		"--repo", repoURL, "--system-identifier", "7000000000000000001", "-o", "json")
	return stderr, exit
}

// repoTree lists every path under the repository root, relative to it.
func repoTree(t *testing.T, repoURL string) []string {
	t.Helper()
	root := strings.TrimPrefix(repoURL, "file://")
	var out []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A deployment name is one component of every key under wal/. `wal push`
// interpolated it unchecked, so "../audit" wrote segment manifests into
// the audit tree (verify-chain then reported the chain broken),
// "../chunks" into the chunk store, and "../x" and "a/../../x" were two
// names for one namespace. Every such name must be refused, as a usage
// error, before anything reaches storage.
func TestWalPush_RejectsDeploymentNamesThatLeaveTheWALPrefix(t *testing.T) {
	w := newReadWorld(t)
	before := repoTree(t, w.repoURL)
	for _, name := range []string{
		"../escape", "../chunks", "../audit", "../HSREPO", "../../etc",
		"a/../../escape", "..", ".", `a\b`, "a\tb", "a/b",
	} {
		stderr, exit := pushTestSegment(t, w.repoURL, name, "000000010000000000000001")
		if exit != int(output.ExitMisuse) || !strings.Contains(stderr, "usage.bad_deployment") {
			t.Errorf("wal push %q: exit=%d, want %d with usage.bad_deployment\n%s", name, exit, output.ExitMisuse, stderr)
		}
	}
	if after := repoTree(t, w.repoURL); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("a refused push changed the repository:\nbefore: %v\nafter:  %v", before, after)
	}
	// The audit chain is what the "../audit" push used to break.
	if stdout, stderr, exit := runCLI(t, "audit", "verify-chain", "--repo", w.repoURL, "-o", "json"); exit != int(output.ExitOK) {
		t.Errorf("audit verify-chain exit=%d after refused pushes\n%s\n%s", exit, stdout, stderr)
	}
}

// The read and maintenance verbs take the same argument and must refuse
// the same names: `wal list ../x` and `wal fetch ../x` used to address
// whatever the traversal resolved to.
func TestWalCommands_RejectTraversalDeploymentNames(t *testing.T) {
	w := newReadWorld(t)
	target := filepath.Join(t.TempDir(), "out.seg")
	for _, args := range [][]string{
		{"wal", "list", "../escape"},
		{"wal", "fetch", "../escape", "000000010000000000000001", target},
		{"wal", "prune", "../escape"},
		{"wal", "gaps", "../escape"},
		{"wal", "audit", "../escape"},
		{"wal", "gap-purge", "../escape", "--all", "--yes"},
	} {
		_, stderr, exit := runCLI(t, append(args, "--repo", w.repoURL, "-o", "json")...)
		if exit != int(output.ExitMisuse) || !strings.Contains(stderr, "usage.bad_deployment") {
			t.Errorf("%v: exit=%d, want %d with usage.bad_deployment\n%s", args, exit, output.ExitMisuse, stderr)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a refused wal fetch created its target file")
	}
}

// The rule is the backup path's storage-safety rule, no stricter: a name
// that is a single key component keeps archiving. Tightening it would
// fail archive_command on a running primary after an upgrade.
func TestWalPush_SingleComponentNamesStillArchive(t *testing.T) {
	w := newReadWorld(t)
	for _, name := range []string{"db1", "db.prod", "1db", "a b"} {
		if stderr, exit := pushTestSegment(t, w.repoURL, name, "000000010000000000000001"); exit != int(output.ExitOK) {
			t.Errorf("wal push %q: exit=%d\n%s", name, exit, stderr)
		}
	}
}

// A repository whose only content is archived WAL — a WAL-first
// deployment before its first backup — is not empty. `repo check` and
// `repo audit` enumerated deployments from manifests/ alone and reported
// "deployments: []" over it.
func TestRepoCheckAndAudit_ListWALOnlyDeployments(t *testing.T) {
	w := newReadWorld(t)
	for _, seg := range []string{"000000010000000000000001", "000000010000000000000002"} {
		if stderr, exit := pushTestSegment(t, w.repoURL, "walonly", seg); exit != int(output.ExitOK) {
			t.Fatalf("wal push exit=%d\n%s", exit, stderr)
		}
	}
	commitVerifiableBackup(t, w, "db1", 0, []byte("a backup body"))

	stdout, stderr, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo check exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	var check struct {
		Deployments []struct {
			Name          string `json:"name"`
			LiveManifests int    `json:"live_manifests"`
			WALSegments   int    `json:"wal_segments"`
		} `json:"deployments"`
		WALSegments int  `json:"wal_segments"`
		Healthy     bool `json:"healthy"`
	}
	bodyOf(t, stdout, &check)
	got := map[string][2]int{}
	for _, d := range check.Deployments {
		got[d.Name] = [2]int{d.LiveManifests, d.WALSegments}
	}
	// (The backup fixture archives WAL of its own for db1.)
	if len(got) != 2 || got["walonly"] != [2]int{0, 2} || got["db1"][0] != 1 {
		t.Errorf("repo check deployments = %v, want walonly:{0 manifests, 2 segments} and db1 with 1 manifest", got)
	}
	if check.WALSegments != 2+got["db1"][1] || !check.Healthy {
		t.Errorf("repo check wal_segments=%d healthy=%v, want the per-deployment sum and true", check.WALSegments, check.Healthy)
	}

	stdout, stderr, exit = runCLI(t, "repo", "audit", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo audit exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	var audit struct {
		Deployments []struct {
			Name        string `json:"name"`
			Active      int    `json:"active"`
			WALSegments int    `json:"wal_segments"`
		} `json:"deployments"`
	}
	bodyOf(t, stdout, &audit)
	got = map[string][2]int{}
	for _, d := range audit.Deployments {
		got[d.Name] = [2]int{d.Active, d.WALSegments}
	}
	if len(got) != 2 || got["walonly"] != [2]int{0, 2} || got["db1"][0] != 1 {
		t.Errorf("repo audit deployments = %v, want walonly:{0 active, 2 segments} and db1 with 1 active", got)
	}
}

// chunkFiles returns every chunk object of a file:// repository.
func chunkFiles(t *testing.T, repoURL string) []string {
	t.Helper()
	root := filepath.Join(strings.TrimPrefix(repoURL, "file://"), "chunks")
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
		return err
	})
	if err != nil || len(out) == 0 {
		t.Fatalf("no chunk files under %s (err=%v)", root, err)
	}
	return out
}

// `wal fetch` must tell a chunk that is damaged from one that is gone.
// Both were wal.fetch.chunk_missing; the repair for "missing" — archive
// the segment again — deduplicates against a corrupt chunk and fixes
// nothing.
func TestWalFetch_CorruptChunkIsNotReportedAsMissing(t *testing.T) {
	const seg = "000000010000000000000001"
	fetch := func(w *readWorld) (string, int, string) {
		target := filepath.Join(t.TempDir(), "out.seg")
		_, stderr, exit := runCLI(t, "wal", "fetch", "db1", seg, target, "--repo", w.repoURL, "-o", "json")
		return stderr, exit, target
	}

	t.Run("corrupt", func(t *testing.T) {
		w := newReadWorld(t)
		if stderr, exit := pushTestSegment(t, w.repoURL, "db1", seg); exit != int(output.ExitOK) {
			t.Fatalf("wal push exit=%d\n%s", exit, stderr)
		}
		for _, p := range chunkFiles(t, w.repoURL) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0xff
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stderr, exit, target := fetch(w)
		if exit != int(output.ExitVerifyFailed) || !strings.Contains(stderr, "verify.checksum_mismatch") {
			t.Errorf("exit=%d, want %d with verify.checksum_mismatch\n%s", exit, output.ExitVerifyFailed, stderr)
		}
		if strings.Contains(stderr, "chunk_missing") {
			t.Errorf("a present-but-corrupt chunk was reported as missing\n%s", stderr)
		}
		if _, err := os.Stat(target); err == nil {
			t.Error("a failed fetch left a target file")
		}
	})

	t.Run("missing", func(t *testing.T) {
		w := newReadWorld(t)
		if stderr, exit := pushTestSegment(t, w.repoURL, "db1", seg); exit != int(output.ExitOK) {
			t.Fatalf("wal push exit=%d\n%s", exit, stderr)
		}
		for _, p := range chunkFiles(t, w.repoURL) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		stderr, exit, _ := fetch(w)
		if exit == int(output.ExitOK) || !strings.Contains(stderr, "wal.fetch.chunk_missing") {
			t.Errorf("exit=%d, want wal.fetch.chunk_missing\n%s", exit, stderr)
		}
	})
}
