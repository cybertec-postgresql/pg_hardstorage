package integrity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// signerFromKey is a tiny test-side Signer (mirrors the pattern in
// threshold's tests).
type signerFromKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s signerFromKey) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s signerFromKey) PublicKey() ed25519.PublicKey { return s.pub }

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// fixture is a self-contained read-world: a fresh repo, a CAS, an
// operator keystore, a manifest store, and a couple of committed
// manifests with known chunks.
type fixture struct {
	sp        storage.StoragePlugin
	cas       *repo.CAS
	manifests *backup.ManifestStore
	signer    *backup.Signer
	verifier  *backup.Verifier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	keyringDir := t.TempDir()
	signer, verifier, err := keystore.LoadOrGenerate(keyringDir)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(repoURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return &fixture{
		sp:        sp,
		cas:       casdefault.New(sp),
		manifests: backup.NewManifestStore(sp),
		signer:    signer,
		verifier:  verifier,
	}
}

// commitBackup produces and commits a tiny manifest with N chunks
// (each carrying a known plaintext so we can deterministically
// reference + later verify them).  Returns the committed manifest.
func (f *fixture) commitBackup(t *testing.T, deployment, suffix string, chunks int) *backup.Manifest {
	t.Helper()
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	files := []backup.FileEntry{}
	chunkRefs := []backup.ChunkRef{}
	var totalSize int64
	for i := 0; i < chunks; i++ {
		body := []byte("chunk-" + suffix + "-" + itoa(i))
		ci, err := f.cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatalf("PutChunk: %v", err)
		}
		chunkRefs = append(chunkRefs, backup.ChunkRef{
			Hash:   ci.Hash,
			Offset: totalSize, // contiguous — Manifest.Validate requires this
			Len:    int64(len(body)),
		})
		totalSize += int64(len(body))
	}
	files = append(files, backup.FileEntry{
		Path:   "PG_VERSION",
		Size:   totalSize, // chunk sum == size — Manifest.Validate requires this
		Mode:   0o600,
		Chunks: chunkRefs,
	})
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
	if err := f.manifests.Commit(context.Background(), m, f.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return m
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// ----- strategy validation -----

func TestExecute_BadStrategy(t *testing.T) {
	f := newFixture(t)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	cases := []struct {
		name string
		s    integrity.Strategy
	}{
		{"unknown-mode", integrity.Strategy{Mode: "exotic"}},
		{"sample-no-percent-or-count", integrity.Strategy{Mode: "content-sample"}},
		{"percent-out-of-range", integrity.Strategy{Mode: "content-sample", Percent: 150}},
		{"negative-count", integrity.Strategy{Mode: "content-sample", Count: -3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(context.Background(), "", tc.s, "")
			if !errors.Is(err, integrity.ErrInvalidStrategy) {
				t.Errorf("err = %v, want ErrInvalidStrategy", err)
			}
		})
	}
}

// ----- run scenarios -----

func TestExecute_ManifestsOnly_HappyPath(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "20260501T120000Z", 3)
	f.commitBackup(t, "db1", "20260501T130000Z", 2)

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, err := eng.Execute(context.Background(), "", integrity.Strategy{Mode: "manifests-only"}, "weekly")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Status != integrity.StatusOK {
		t.Errorf("Status = %s, want ok", r.Status)
	}
	if r.Manifests.Total != 2 || r.Manifests.SignaturesOK != 2 {
		t.Errorf("Manifests: %+v", r.Manifests)
	}
	if r.Chunks.PresenceChecked != 0 {
		t.Errorf("manifests-only should not check chunks, got %d", r.Chunks.PresenceChecked)
	}
}

func TestExecute_PresenceMode_HappyPath(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "a", 4)
	f.commitBackup(t, "db1", "b", 2)

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, err := eng.Execute(context.Background(), "", integrity.DefaultStrategy(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Status != integrity.StatusOK {
		t.Errorf("Status = %s", r.Status)
	}
	if r.Manifests.Total != 2 {
		t.Errorf("Total = %d, want 2", r.Manifests.Total)
	}
	if r.Chunks.DistinctReferenced != 6 {
		t.Errorf("DistinctReferenced = %d, want 6", r.Chunks.DistinctReferenced)
	}
	if r.Chunks.PresenceChecked != 6 {
		t.Errorf("PresenceChecked = %d, want 6", r.Chunks.PresenceChecked)
	}
	if r.Chunks.Missing != 0 || len(r.Chunks.Failures) != 0 {
		t.Errorf("Failures: %+v", r.Chunks.Failures)
	}
}

func TestExecute_ContentSample_50Percent(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 10)

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
		CAS: f.cas,
	})
	r, err := eng.Execute(context.Background(), "", integrity.Strategy{
		Mode: "content-sample", Percent: 50, Seed: 42,
	}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Status != integrity.StatusOK {
		t.Errorf("Status = %s", r.Status)
	}
	if r.Chunks.Sampled != 5 {
		t.Errorf("Sampled = %d, want 5", r.Chunks.Sampled)
	}
	if r.Chunks.Verified != 5 {
		t.Errorf("Verified = %d, want 5", r.Chunks.Verified)
	}
}

func TestExecute_ContentFull(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 4)

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier, CAS: f.cas,
	})
	r, err := eng.Execute(context.Background(), "", integrity.Strategy{Mode: "content-full"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Chunks.Sampled != 4 || r.Chunks.Verified != 4 {
		t.Errorf("Sampled/Verified = %d/%d, want 4/4", r.Chunks.Sampled, r.Chunks.Verified)
	}
}

func TestExecute_DeploymentScoped(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "a", 2)
	f.commitBackup(t, "db2", "a", 3)
	f.commitBackup(t, "db2", "b", 1)

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, err := eng.Execute(context.Background(), "db2",
		integrity.Strategy{Mode: "presence"}, "scoped")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Manifests.Total != 2 {
		t.Errorf("Total = %d, want 2", r.Manifests.Total)
	}
	if r.Chunks.DistinctReferenced != 4 {
		t.Errorf("DistinctReferenced = %d, want 4", r.Chunks.DistinctReferenced)
	}
	if r.Deployment != "db2" {
		t.Errorf("Deployment = %q", r.Deployment)
	}
}

// TestExecute_DetectsMissingChunk: simulate bit-rot by deleting one
// chunk after the manifest is committed.  Run should surface it.
func TestExecute_DetectsMissingChunk(t *testing.T) {
	f := newFixture(t)
	m := f.commitBackup(t, "db1", "x", 3)

	// Delete the on-disk object for the first chunk.
	first := m.Files[0].Chunks[0].Hash
	if err := f.sp.Delete(context.Background(), repo.ChunkKey(first)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, err := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Status != integrity.StatusFoundIssues {
		t.Errorf("Status = %s, want found_issues", r.Status)
	}
	if r.Chunks.Missing != 1 {
		t.Errorf("Missing = %d, want 1", r.Chunks.Missing)
	}
	if len(r.Chunks.Failures) != 1 {
		t.Fatalf("len(Failures) = %d, want 1", len(r.Chunks.Failures))
	}
	if r.Chunks.Failures[0].Reason != "missing" {
		t.Errorf("Failure.Reason = %q", r.Chunks.Failures[0].Reason)
	}
	if r.Chunks.Failures[0].ChunkHash != first.String() {
		t.Errorf("hash mismatch: got %q, want %q",
			r.Chunks.Failures[0].ChunkHash, first.String())
	}
}

// ----- sign / verify -----

func TestSignAndVerifyRun_RoundTrip(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 2)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "round-trip")

	pub, priv := mustKeypair(t)
	if err := integrity.SignRun(r, signerFromKey{pub: pub, priv: priv}); err != nil {
		t.Fatalf("SignRun: %v", err)
	}
	if r.Signature == "" || r.BodyHash == "" || r.PublicKeyFingerprint == "" {
		t.Errorf("signing fields not populated")
	}
	if err := integrity.VerifyRun(r, &integrity.SingleKeyResolver{Key: pub}); err != nil {
		t.Errorf("VerifyRun: %v", err)
	}
}

func TestVerifyRun_TamperedStatus(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 1)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "")
	pub, priv := mustKeypair(t)
	_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})

	r.Status = integrity.StatusFoundIssues // tamper
	if err := integrity.VerifyRun(r, &integrity.SingleKeyResolver{Key: pub}); err == nil {
		t.Errorf("expected verify failure after tampering")
	}
}

func TestVerifyRun_TamperedSignature(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 1)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "")
	pub, priv := mustKeypair(t)
	_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})

	// Flip one byte in the signature → must fail.
	if r.Signature[0] == 'A' {
		r.Signature = "B" + r.Signature[1:]
	} else {
		r.Signature = "A" + r.Signature[1:]
	}
	if err := integrity.VerifyRun(r, &integrity.SingleKeyResolver{Key: pub}); !errors.Is(err, integrity.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerifyRun_WrongKey(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 1)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "")
	pub, priv := mustKeypair(t)
	_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})

	otherPub, _ := mustKeypair(t)
	if err := integrity.VerifyRun(r, &integrity.SingleKeyResolver{Key: otherPub}); !errors.Is(err, integrity.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// ----- store round-trip -----

func TestRunStore_RoundTrip(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 2)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "stored")
	pub, priv := mustKeypair(t)
	_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})

	store := integrity.NewRunStore(f.sp)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != r.ID || got.Status != r.Status {
		t.Errorf("round-trip drift: %+v vs %+v", got, r)
	}
	if err := integrity.VerifyRun(got, &integrity.SingleKeyResolver{Key: pub}); err != nil {
		t.Errorf("VerifyRun on read-back: %v", err)
	}
}

// tmpRecordingSP wraps a StoragePlugin and records every Put key, so a
// test can assert the staging path RunStore.Put uses is randomised rather
// than a fixed key+".tmp" (which would tear under concurrent first-writes
// of the same run ID).
type tmpRecordingSP struct {
	storage.StoragePlugin
	mu      sync.Mutex
	putKeys []string
}

func (w *tmpRecordingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	w.mu.Lock()
	w.putKeys = append(w.putKeys, key)
	w.mu.Unlock()
	return w.StoragePlugin.Put(ctx, key, r, opts)
}

// TestRunStore_RandomisedTmp pins the torn-overwrite fix: the staging key
// RunStore.Put writes before the atomic rename must be a randomised
// key+".tmp.<rand>", never the fixed key+".tmp" two concurrent writers
// would collide on.
func TestRunStore_RandomisedTmp(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 2)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	r, _ := eng.Execute(context.Background(), "",
		integrity.Strategy{Mode: "presence"}, "stored")
	pub, priv := mustKeypair(t)
	_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})

	rec := &tmpRecordingSP{StoragePlugin: f.sp}
	store := integrity.NewRunStore(rec)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var tmp string
	for _, k := range rec.putKeys {
		if strings.Contains(k, ".tmp") {
			tmp = k
		}
	}
	if tmp == "" {
		t.Fatalf("no .tmp staging Put observed; keys=%v", rec.putKeys)
	}
	final := strings.SplitN(tmp, ".tmp", 2)[0]
	if tmp == final+".tmp" {
		t.Errorf("staging key is fixed %q — torn-overwrite race on concurrent same-ID writes", tmp)
	}
	if !strings.HasPrefix(tmp, final+".tmp.") {
		t.Errorf("staging key %q is not the expected randomised %q.tmp.<rand> shape", tmp, final)
	}
}

func TestRunStore_GetMissing(t *testing.T) {
	f := newFixture(t)
	store := integrity.NewRunStore(f.sp)
	_, err := store.Get(context.Background(), "ghost")
	if !errors.Is(err, integrity.ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestRunStore_ListFiltering(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 1)
	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
	})
	store := integrity.NewRunStore(f.sp)
	pub, priv := mustKeypair(t)

	// Write three runs at different times.  Because newRunID is keyed
	// off (UTC seconds + deployment), we must space them >= 1 s apart
	// to get distinct IDs.
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		eng2 := integrity.NewEngine(integrity.EngineOptions{
			Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
			Now: func() time.Time { return now.Add(time.Duration(i) * time.Hour) },
		})
		r, _ := eng2.Execute(context.Background(), "",
			integrity.Strategy{Mode: "presence"}, "")
		_ = integrity.SignRun(r, signerFromKey{pub: pub, priv: priv})
		if err := store.Put(context.Background(), r); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	all, err := store.List(context.Background(), integrity.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
	// Newest first.
	if !all[0].StartedAt.After(all[1].StartedAt) ||
		!all[1].StartedAt.After(all[2].StartedAt) {
		t.Errorf("not newest-first")
	}

	// Since filter (cuts off the oldest 1).
	since := now.Add(30 * time.Minute)
	scoped, err := store.List(context.Background(), integrity.ListFilter{
		Since: &since,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Errorf("len = %d, want 2", len(scoped))
	}
	_ = eng
}

// ----- run id determinism -----

func TestRunID_LexSortable(t *testing.T) {
	// Two runs at different times must produce IDs that lex-sort by
	// time (the leading 020d-unix-seconds prefix guarantees that).
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 1)

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	eng1 := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
		Now: func() time.Time { return t1 },
	})
	eng2 := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier,
		Now: func() time.Time { return t2 },
	})
	r1, _ := eng1.Execute(context.Background(), "",
		integrity.Strategy{Mode: "manifests-only"}, "")
	r2, _ := eng2.Execute(context.Background(), "",
		integrity.Strategy{Mode: "manifests-only"}, "")
	// IDs are zero-padded to 20 digits; with current epoch they
	// start with the same 9 leading zeros + the timestamp.
	if !strings.HasPrefix(r1.ID, "00000000001") ||
		!strings.HasPrefix(r2.ID, "00000000001") {
		t.Errorf("not 020d-prefixed: %q %q", r1.ID, r2.ID)
	}
	if r1.ID >= r2.ID {
		t.Errorf("expected r1.ID < r2.ID, got %q vs %q", r1.ID, r2.ID)
	}
}
