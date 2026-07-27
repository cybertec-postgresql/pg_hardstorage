package cli_test

// Idempotency sweep: every repo-mutating maintenance operation is run
// TWICE against the same repo; the second run must succeed AND leave
// the repo's durable state byte-identical (audit events excluded —
// each run legitimately appends its own).
//
// Failure-class rationale: re-running a maintenance command is the
// documented resume flow after partial failures, and "nobody ever ran
// it twice" has already produced a production bug — a re-run of
// `kms rotate` hard-failed at the shared-DEK migration because the
// slots were already migrated. This sweep makes second-run coverage
// automatic for the whole maintenance surface.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

// repoDigest hashes the repo's durable state, excluding the audit
// chain (every run appends its own events) and mtime-only changes.
func repoDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, "audit/") {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		fmt.Fprintf(h, "%s\n", rel)
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(h, f)
		_ = f.Close()
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// seedIdempotencyRepo plants: a live full, a full+incremental chain
// that is tombstoned (gc fodder), an orphan chunk, and (encrypted
// variant) manifests wrapped under the keyring KEK with the shared-DEK
// object present.
func seedIdempotencyRepo(t *testing.T, encrypted bool) (repoURL, repoDir string) {
	t.Helper()
	// Per-test config dir: these fixtures generate keystore material
	// (signing keys, encryption KEK); leaking a kek.bin into the shared
	// ambient config dir flips OTHER tests' "no KEK present" branches.
	_ = configDir(t)
	repoURL = initRepoForTest(t)
	repoDir = strings.TrimPrefix(repoURL, "file://")

	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		t.Fatal(err)
	}

	var enc *backup.EncryptionInfo
	if encrypted {
		kek, _, kerr := keystore.LoadOrGenerateKEK(p.Keyring.Value)
		if kerr != nil {
			t.Fatal(kerr)
		}
		res, merr := sharedkey.ResolveOrMint(context.Background(), sp, keystore.KEKRefLocal,
			func(w []byte) ([]byte, error) {
				d, e := encryption.Unwrap(kek, w)
				if e != nil {
					return nil, e
				}
				return d[:], nil
			},
			func(dek [encryption.KeyLen]byte) ([]byte, error) { return encryption.Wrap(kek, dek) })
		if merr != nil || !res.Have {
			t.Fatalf("mint shared DEK: have=%v err=%v", res.Have, merr)
		}
		wrapped, werr := encryption.Wrap(kek, res.DEK)
		if werr != nil {
			t.Fatal(werr)
		}
		enc = &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: keystore.KEKRefLocal,
			WrappedDEK: base64Std(wrapped), EnvelopeVersion: 2,
		}
	}

	cas := casdefault.New(sp)
	commit := func(id, parent string, btype backup.BackupType, mins int) {
		t.Helper()
		body := []byte("idem-" + id)
		info, cerr := cas.PutChunk(context.Background(), body)
		if cerr != nil {
			t.Fatal(cerr)
		}
		ts := time.Now().UTC().Add(-time.Duration(mins) * time.Minute)
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1", Tenant: "default",
			Type: btype, ParentBackupID: parent,
			PGVersion: 17, SystemIdentifier: "7000000000000000001",
			StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Encryption:  enc,
			Files: []backup.FileEntry{{Path: "data/f", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}}}},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}
	commit("db1.full.keep", "", backup.BackupTypeFull, 10)
	commit("db1.full.old", "", backup.BackupTypeFull, 500)
	commit("db1.incremental_lsn.old", "db1.full.old", backup.BackupTypeIncremental, 490)

	// Tombstone the old chain (leaf-first) so gc has real work.
	if _, err := store.SoftDeleteCascade(context.Background(), "db1", "db1.full.old", "manual", "idem-test"); err != nil {
		t.Fatal(err)
	}
	// Orphan chunk, backdated past every floor.
	if _, err := cas.PutChunk(context.Background(), []byte("orphan-idem")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = filepath.WalkDir(filepath.Join(repoDir, "chunks"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})
	return repoURL, repoDir
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestIdempotencySweep(t *testing.T) {
	cases := []struct {
		name      string
		encrypted bool
		args      func(repoURL string, aux map[string]string) []string
		setup     func(t *testing.T, aux map[string]string) // e.g. generate a new KEK file
	}{
		{
			name: "repo_gc_apply",
			args: func(u string, _ map[string]string) []string {
				return []string{"repo", "gc", u, "--apply", "--tombstone-grace", "1ms", "--min-chunk-age", "1ms", "--output", "json"}
			},
		},
		{
			name: "rotate_apply",
			args: func(u string, _ map[string]string) []string {
				return []string{"rotate", "db1", "--repo", u, "--policy", "simple", "--keep-for", "240h", "--apply", "--output", "json"}
			},
		},
		{
			name: "undelete_already_live",
			args: func(u string, _ map[string]string) []string {
				return []string{"backup", "undelete", "db1", "db1.full.keep", "--repo", u, "--output", "json"}
			},
		},
		{
			name: "hold_purge_expired",
			args: func(u string, _ map[string]string) []string {
				return []string{"hold", "purge-expired", "db1", "--repo", u, "--yes", "--output", "json"}
			},
		},
		{
			name:      "kms_rotate_apply",
			encrypted: true,
			setup: func(t *testing.T, aux map[string]string) {
				dir := t.TempDir()
				newKEK := filepath.Join(dir, "kek2.bin")
				b := make([]byte, 32)
				if _, err := rand.Read(b); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(newKEK, b, 0o600); err != nil {
					t.Fatal(err)
				}
				p, err := paths.Resolve(paths.DefaultOptions())
				if err != nil {
					t.Fatal(err)
				}
				aux["oldKEK"] = filepath.Join(p.Keyring.Value, keystore.KEKFileName)
				aux["newKEK"] = newKEK
			},
			args: func(u string, aux map[string]string) []string {
				return []string{"kms", "rotate", "--repo", u,
					"--old-kek-ref", "local:default", "--old-kek-file", aux["oldKEK"],
					"--new-kek-ref", "local:v2", "--new-kek-file", aux["newKEK"],
					"--apply", "--output", "json"}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repoURL, repoDir := seedIdempotencyRepo(t, tc.encrypted)
			aux := map[string]string{}
			if tc.setup != nil {
				tc.setup(t, aux)
			}
			argv := tc.args(repoURL, aux)

			if _, stderr, exit := runCmd(t, argv...); exit != 0 {
				t.Fatalf("first run failed (exit %d):\n%s", exit, stderr)
			}
			digestAfterFirst := repoDigest(t, repoDir)

			if _, stderr, exit := runCmd(t, argv...); exit != 0 {
				t.Fatalf("SECOND run failed (exit %d) — the documented resume flow is broken:\n%s", exit, stderr)
			}
			if got := repoDigest(t, repoDir); got != digestAfterFirst {
				t.Errorf("second run CHANGED durable repo state — the operation is not idempotent")
			}
		})
	}
}
