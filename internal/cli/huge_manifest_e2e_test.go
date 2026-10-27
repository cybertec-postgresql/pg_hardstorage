package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestHugeManifest_AllCommandsStream is the end-to-end proof for issue #81
// ("object exceeds the 1073741824-byte read limit" on a 4.2 GB manifest).
// The read caps are shrunk far below the manifest's size (so a command that
// still loads the whole object fails exactly as in the field) and the
// auto-stream threshold is shrunk below it, then every command that touches
// a manifest is run through the real CLI.
func TestHugeManifest_AllCommandsStream(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	cas := casdefault.New(w.sp)

	var bodies [][]byte
	var infos []backup.ChunkRef
	for i := 0; i < 32; i++ {
		b := []byte(fmt.Sprintf("huge manifest chunk body %03d %s", i, strings.Repeat("z", 40+i)))
		ci, err := cas.PutChunk(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
		infos = append(infos, backup.ChunkRef{Hash: ci.Hash, Len: int64(len(b))})
	}

	const dep = "huge"
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	id := dep + ".full." + ts.Format("20060102T150405Z") + ".0001"
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: id, Deployment: dep, Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 180000, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: ts, StoppedAt: ts.Add(30 * time.Second),
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
	}
	for f := 0; f < 160; f++ {
		fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/%d", 100000+f), Mode: 0o600}
		var off int64
		for k := 0; k < 8; k++ {
			c := infos[(f+k)%len(infos)]
			fe.Chunks = append(fe.Chunks, backup.ChunkRef{Hash: c.Hash, Offset: off, Len: c.Len})
			off += c.Len
		}
		fe.Size = off
		m.Files = append(m.Files, fe)
	}
	// PG_VERSION so restore planning finds a plausible data dir.
	w.plantArchivedWAL(t, dep, 1)
	if err := w.store.Commit(ctx, m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := w.sp.Stat(ctx, backup.PrimaryPath(dep, id))
	if err != nil {
		t.Fatal(err)
	}

	const cap = 16 << 10
	if st.Size < 4*cap {
		t.Fatalf("fixture manifest only %d bytes; need well over the lowered cap %d", st.Size, cap)
	}
	oldM, oldMeta, oldAuto := backup.MaxManifestBytes, storage.MaxMetadataBytes, backup.AutoStreamThreshold
	backup.MaxManifestBytes, storage.MaxMetadataBytes, backup.AutoStreamThreshold = cap, cap, 4<<10
	t.Cleanup(func() {
		backup.MaxManifestBytes, storage.MaxMetadataBytes, backup.AutoStreamThreshold = oldM, oldMeta, oldAuto
	})

	run := func(name string, args ...string) string {
		t.Helper()
		var out string
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runCLI(t, args...)
			out = stdout
			all := stdout + stderr
			if exit != 0 || strings.Contains(all, "read limit") || strings.Contains(all, "-byte limit") || strings.Contains(all, "failed verification") {
				t.Errorf("%v: exit=%d\nstdout=%s\nstderr=%s", args, exit, stdout, stderr)
			}
		})
		return out
	}

	R := w.repoURL
	// Reads.
	run("list", "list", dep, "--repo", R, "-o", "json")
	run("show", "show", dep, id, "--repo", R, "-o", "json")
	run("manifest-show", "manifest", "show", dep, id, "--repo", R, "-o", "json")
	run("status", "status", dep, "--repo", R, "-o", "json")
	run("verify", "verify", dep, id, "--repo", R, "-o", "json")
	run("verify-full", "verify", dep, id, "--full", "--repo", R, "-o", "json")
	run("repo-check", "repo", "check", R, "-o", "json")
	run("repo-audit", "repo", "audit", R, "-o", "json")
	run("repo-usage", "repo", "usage", R, "-o", "json")
	run("repo-scrub", "repo", "scrub", R, "--full", "-o", "json")
	run("repo-gc-dry", "repo", "gc", R, "-o", "json")
	run("wal-prune-dry", "wal", "prune", dep, "--repo", R, "-o", "json")
	run("rotate-dry", "rotate", dep, "--repo", R, "-o", "json")
	run("graph", "backup", "graph", dep, "--repo", R, "-o", "json")
	run("fleet-search", "fleet", "search", "--query", "deployment:"+dep, "--repo", R, "-o", "json")
	run("kms-verify", "kms", "verify", "--repo", R, "-o", "json")
	run("kms-shred-dry", "kms", "shred", "--dry-run", "--repo", R, "-o", "json")
	run("compliance", "compliance", "report", R, "-o", "json")
	run("readiness", "recovery", "readiness", dep, "--repo", R, "-o", "json")
	run("windows", "recovery", "windows", dep, "--repo", R, "-o", "json")
	run("dsa", "dsa", "locate", "--tenant", "default", "--subject-id", "s1", "--skip-sign", "--repo", R, "-o", "json")
	run("integrity-presence", "integrity", "run", "--repo", R, "--strategy", "presence", "--skip-sign", "-o", "json")
	run("integrity-content", "integrity", "run", "--repo", R, "--strategy", "content-full", "--skip-sign", "-o", "json")
	bundle := t.TempDir() + "/b.tar"
	run("bundle-export", "repo", "bundle", "export", "--deployment", dep, "--repo", R, "--out", bundle, "-o", "json")
	// Import the bundle into a fresh repo, replicate into another, and
	// verify the replica; both carry the (over-cap) manifest and replica.
	impURL := "file://" + t.TempDir()
	repRoot := t.TempDir()
	repURL := "file://" + repRoot
	for _, u := range []string{impURL, repURL} {
		if _, _, exit := runCLI(t, "repo", "init", u); exit != 0 {
			t.Fatalf("repo init %s", u)
		}
	}
	run("bundle-import", "repo", "bundle", "import", "--in", bundle, "--to", impURL, "-o", "json")
	run("import-verify", "verify", dep, id, "--repo", impURL, "-o", "json")
	run("replicate", "repo", "replicate", "--from", R, "--to", repURL, "-o", "json")
	run("replicate-verify", "repo", "replicate", "verify", "--from", R, "--to", repURL, "-o", "json")
	run("replica-verify", "verify", dep, id, "--repo", repURL, "-o", "json")
	run("drill", "recovery", "drill", dep, "--repo", R, "--skip-verify", "--no-history", "-o", "json")
	writeReadWorldConfig(t, w, manifestSigConfigYAML(R, dep))
	doc := run("doctor", "doctor", "-o", "json")
	if strings.Contains(doc, "manifest_signature_failures") {
		t.Errorf("doctor: manifest_signature_failures:\n%s", doc)
	}
	run("slo", "slo", "report", dep, "-o", "json")

	// Restore.
	target := t.TempDir() + "/restored"
	run("restore", "restore", dep, id, "--repo", R, "--target", target, "-o", "json")
	for _, f := range []string{"base/16384/100000", "base/16384/100159"} {
		if _, err := os.Stat(filepath.Join(target, f)); err != nil {
			t.Errorf("restored file missing: %v", err)
		}
	}

	// Repair: corrupt the primary, heal from the replica; then force a
	// re-sign of the (valid) streamed manifest.
	primaryPath := filepath.Join(strings.TrimPrefix(R, "file://"), backup.PrimaryPath(dep, id))
	good, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), good...)
	copy(bad[len(bad)/2:], "CORRUPTED-IN-THE-MIDDLE")
	if err := os.WriteFile(primaryPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	// With the primary corrupt, reads fall back to the replica.
	run("show-via-replica", "show", dep, id, "--repo", R, "-o", "json")
	run("repair-manifest", "repair", "manifest", dep, id, "--repo", R, "-o", "json")
	if after, _ := os.ReadFile(primaryPath); !bytes.Equal(after, good) {
		t.Errorf("repair manifest did not restore the primary byte for byte")
	}
	run("repair-manifest-force", "repair", "manifest", dep, id, "--force", "--repo", R, "-o", "json")
	run("repair-attestation-force", "repair", "attestation", dep, id, "--force", "--repo", R, "-o", "json")
	// Key rotation on a streamed manifest: a second backup signed by a
	// retired key is re-signed under the current key via --trusted-key,
	// and refused without it.
	retired, _, err := keystore.LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := retired.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	pubFile := filepath.Join(t.TempDir(), "retired.pem")
	if err := os.WriteFile(pubFile, pub, 0o644); err != nil {
		t.Fatal(err)
	}
	id2 := dep + ".full." + ts.Add(time.Minute).Format("20060102T150405Z") + ".0002"
	m2 := *m
	m2.BackupID, m2.StartedAt, m2.StoppedAt, m2.Attestation = id2, ts.Add(time.Minute), ts.Add(90*time.Second), nil
	if err := w.store.Commit(ctx, &m2, retired, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, exit := runCLI(t, "repair", "attestation", dep, id2, "--repo", R, "-o", "json"); exit == 0 {
		t.Errorf("repair attestation re-signed a manifest from an untrusted key")
	}
	run("repair-attestation-trusted", "repair", "attestation", dep, id2, "--trusted-key", pubFile, "--repo", R, "-o", "json")
	run("verify-rotated", "verify", dep, id2, "--repo", R, "-o", "json")
	run("verify-after-resign", "verify", dep, id, "--repo", R, "-o", "json")

	// Delete / undelete.
	run("backup-delete", "backup", "delete", dep, id, "--yes", "--repo", R, "-o", "json")
	run("backup-undelete", "backup", "undelete", dep, id, "--repo", R, "-o", "json")
}
