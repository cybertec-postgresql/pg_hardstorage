//go:build integration

package restore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func kekResolver(kek [encryption.KeyLen]byte) func(string) ([encryption.KeyLen]byte, error) {
	return func(ref string) ([encryption.KeyLen]byte, error) { return kek, nil }
}

func listRepoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func treeDigest(t *testing.T, root string) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			m[rel] = b
		}
		return nil
	})
	return m
}

// TestSecure_TamperEveryRepoFile flips one byte in every repository
// file in turn (manifest, signature, chunks, WAL, sidecars...) and
// requires that restore either refuses or yields a byte-identical
// data directory (the file was irrelevant to this restore). Any
// silently-different restore is a finding.
func TestSecure_TamperEveryRepoFile(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) { tamperEveryFile(t, encrypted) })
	}
}

func tamperEveryFile(t *testing.T, encrypted bool) {
	src := startLocalSource(t)
	src.SQL(t, "CREATE TABLE t AS SELECT g, md5(g::text) FROM generate_series(1,20000) g")
	repoURL, signer, verifier := testRepo(t)
	stubWALFetch(t)
	repoDir := strings.TrimPrefix(repoURL, "file://")

	var kek [encryption.KeyLen]byte
	_, _ = rand.Read(kek[:])
	opts := runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
		IncludeManifest: true,
	}
	if encrypted {
		opts.Encryption = &runner.EncryptionConfig{KEK: kek, KEKRef: "local:default"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	b, err := runner.Take(ctx, opts)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	ropts := func(target string) restore.Options {
		o := restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: b.BackupID,
			TargetDir: target, Verifier: verifier, VerifyMode: "off",
		}
		if encrypted {
			o.KEKForRef = kekResolver(kek)
		}
		return o
	}
	good := filepath.Join(t.TempDir(), "good")
	if _, err := restore.Restore(ctx, ropts(good)); err != nil {
		t.Fatalf("baseline restore: %v", err)
	}
	want := treeDigest(t, good)

	var files []string
	nchunk := 0
	for _, f := range listRepoFiles(t, repoDir) {
		if strings.Contains(f, "/chunks/") {
			// sample: every 17th chunk keeps runtime bounded
			nchunk++
			if nchunk%17 != 1 {
				continue
			}
		}
		files = append(files, f)
	}
	t.Logf("%d repo files", len(files))
	for _, f := range files {
		orig, err := os.ReadFile(f)
		if err != nil || len(orig) == 0 {
			continue
		}
		rel, _ := filepath.Rel(repoDir, f)
		for _, mode := range []string{"flip", "truncate", "append"} {
			mut := append([]byte(nil), orig...)
			switch mode {
			case "flip":
				mut[len(mut)/2] ^= 0x01
			case "truncate":
				mut = mut[:len(mut)-1]
			case "append":
				mut = append(mut, 0)
			}
			if err := os.WriteFile(f, mut, 0o600); err != nil {
				t.Fatal(err)
			}
			tgt := filepath.Join(t.TempDir(), "r")
			_, rerr := restore.Restore(ctx, ropts(tgt))
			if rerr == nil {
				got := treeDigest(t, tgt)
				if !sameTree(want, got) {
					t.Errorf("SILENT CORRUPTION: %s of %s restored OK but differs", mode, rel)
				} else {
					t.Logf("irrelevant (restore identical): %s %s", mode, rel)
				}
			} else if rerr != nil {
				if d, _ := os.ReadDir(tgt); len(d) > 0 {
					t.Logf("note: refused but left partial target for %s %s: %d entries", mode, rel, len(d))
				}
			}
			_ = os.WriteFile(f, orig, 0o600)
		}
	}
	_ = errors.New
	_ = bytes.Equal
}

func sameTree(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

// TestSecure_WrongKeyFailsClosed: restoring with a wrong KEK or none
// must error and must not leave plaintext data behind.
func TestSecure_WrongKeyFailsClosed(t *testing.T) {
	src := startLocalSource(t)
	src.SQL(t, "CREATE TABLE t AS SELECT g FROM generate_series(1,5000) g")
	repoURL, signer, verifier := testRepo(t)
	stubWALFetch(t)
	var kek, bad [encryption.KeyLen]byte
	_, _ = rand.Read(kek[:])
	_, _ = rand.Read(bad[:])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
		Encryption: &runner.EncryptionConfig{KEK: kek, KEKRef: "local:default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, res := range map[string]func(string) ([encryption.KeyLen]byte, error){
		"wrong": kekResolver(bad),
		"none":  nil,
	} {
		tgt := filepath.Join(t.TempDir(), "r-"+name)
		_, err := restore.Restore(ctx, restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: b.BackupID,
			TargetDir: tgt, Verifier: verifier, VerifyMode: "off", KEKForRef: res,
		})
		if err == nil {
			t.Fatalf("%s key: restore succeeded", name)
		}
		if d, _ := os.ReadDir(tgt); len(d) > 0 {
			t.Errorf("%s key: refused but left %d entries in target", name, len(d))
		}
	}
}

// TestSecure_ManifestRollbackReplay: swap an older, validly-signed
// manifest in under a newer backup id.
func TestSecure_ManifestRollbackReplay(t *testing.T) {
	src := startLocalSource(t)
	src.SQL(t, "CREATE TABLE t(g int)")
	repoURL, signer, verifier := testRepo(t)
	stubWALFetch(t)
	repoDir := strings.TrimPrefix(repoURL, "file://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	take := func() string {
		b, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b.BackupID
	}
	a := take()
	src.SQL(t, "INSERT INTO t SELECT generate_series(1,100)")
	b := take()
	var ma, mb []string
	for _, f := range listRepoFiles(t, repoDir) {
		if strings.Contains(f, a) && strings.Contains(f, "manifest") {
			ma = append(ma, f)
		}
		if strings.Contains(f, b) && strings.Contains(f, "manifest") {
			mb = append(mb, f)
		}
	}
	t.Logf("A manifests %v ; B manifests %v", ma, mb)
	if len(ma) == 0 || len(ma) != len(mb) {
		t.Skip("cannot pair manifest files")
	}
	for i := range mb {
		data, _ := os.ReadFile(ma[i])
		if err := os.WriteFile(mb[i], data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tgt := filepath.Join(t.TempDir(), "r")
	_, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: b,
		TargetDir: tgt, Verifier: verifier, VerifyMode: "off",
	})
	if err == nil {
		t.Fatalf("restore of backup %s with manifest of %s succeeded (replay not detected)", b, a)
	}
	t.Logf("refused: %v", err)
}
