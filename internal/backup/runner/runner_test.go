package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func makeSigner(t *testing.T) *backup.Signer {
	t.Helper()
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGenerateBackupID_FormatAndUniqueness(t *testing.T) {
	id1, err := generateBackupID("db1", backup.BackupTypeFull)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := generateBackupID("db1", backup.BackupTypeFull)
	if err != nil {
		t.Fatal(err)
	}
	// Format: db1.full.<timestamp>.<4-hex>
	for _, id := range []string{id1, id2} {
		parts := strings.Split(id, ".")
		if len(parts) != 4 {
			t.Errorf("ID %q should have 4 dot-separated parts", id)
		}
		if parts[0] != "db1" || parts[1] != "full" {
			t.Errorf("ID %q: wrong deployment/type prefix", id)
		}
		if !strings.HasSuffix(parts[2], "Z") {
			t.Errorf("ID %q: timestamp should end with Z", id)
		}
		if len(parts[3]) != 4 {
			t.Errorf("ID %q: random suffix should be 4 hex chars", id)
		}
	}
	// Two IDs in the same second must differ via the random suffix.
	if id1 == id2 {
		t.Errorf("two IDs collided: %q == %q", id1, id2)
	}
}

func TestValidateOptions_RequiredFields(t *testing.T) {
	signer := makeSigner(t)
	cases := []struct {
		name    string
		opts    TakeOptions
		wantErr string
	}{
		{"missing PGConnString", TakeOptions{RepoURL: "x", Deployment: "d", Signer: signer}, "PGConnString"},
		{"missing RepoURL", TakeOptions{PGConnString: "x", Deployment: "d", Signer: signer}, "RepoURL"},
		{"missing Deployment", TakeOptions{PGConnString: "x", RepoURL: "y", Signer: signer}, "Deployment"},
		{"missing Signer", TakeOptions{PGConnString: "x", RepoURL: "y", Deployment: "d"}, "Signer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateOptions(&c.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q", c.wantErr)
			}
			if !errors.Is(err, output.ErrUsage) {
				t.Errorf("missing-field error should wrap ErrUsage; got %v", err)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error should mention %q; got %v", c.wantErr, err)
			}
		})
	}
}

// TestValidateOptions_DeploymentCharGate pins the storage-key-safety
// check on the deployment name: path separators and control characters
// are rejected (they'd splinter the manifests/<dep>/... key or corrupt
// keys/log lines), while ordinary names — including digit-start and
// short ones config.ValidDeploymentName would reject — are accepted, so
// programmatic callers aren't newly broken.
func TestValidateOptions_DeploymentCharGate(t *testing.T) {
	signer := makeSigner(t)
	base := func(dep string) *TakeOptions {
		return &TakeOptions{PGConnString: "x", RepoURL: "y", Deployment: dep, Signer: signer}
	}

	bad := []string{
		"a/b",    // path separator → key-level splintering
		"..",     // traversal component
		"../etc", // traversal
		`a\b`,    // backslash
		"a\nb",   // newline
		"a\rb",   // carriage return
		"a\x00b", // NUL
		"a\tb",   // tab (control char)
		"x\x7fy", // DEL
	}
	for _, dep := range bad {
		o := base(dep)
		err := validateOptions(o)
		if err == nil {
			t.Errorf("deployment %q should be rejected", dep)
			continue
		}
		if !errors.Is(err, output.ErrUsage) {
			t.Errorf("deployment %q: error should wrap ErrUsage; got %v", dep, err)
		}
	}

	// Non-regression: names config is stricter about must still pass the
	// runner's narrower storage-safety gate.
	good := []string{"db1", "1db", "a", "my-deploy_2", "prod.east"}
	for _, dep := range good {
		o := base(dep)
		if err := validateOptions(o); err != nil {
			t.Errorf("deployment %q should be accepted; got %v", dep, err)
		}
	}
}

func TestValidateOptions_DerivesVerifierFromSigner(t *testing.T) {
	signer := makeSigner(t)
	opts := TakeOptions{
		PGConnString: "postgres://x", RepoURL: "file:///x", Deployment: "d",
		Signer: signer,
	}
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if opts.Verifier == nil {
		t.Fatal("Verifier should have been derived from Signer")
	}
}

func TestValidateOptions_DefaultsTenantToDefault(t *testing.T) {
	opts := TakeOptions{
		PGConnString: "x", RepoURL: "y", Deployment: "d",
		Signer: makeSigner(t),
	}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.Tenant != "default" {
		t.Errorf("Tenant = %q, want \"default\"", opts.Tenant)
	}
}

func TestValidateOptions_ForcesIncludeManifestOn(t *testing.T) {
	opts := TakeOptions{
		PGConnString: "x", RepoURL: "y", Deployment: "d",
		Signer: makeSigner(t), IncludeManifest: false,
	}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}
	if !opts.IncludeManifest {
		t.Error("IncludeManifest should be forced on by default in v0.1")
	}
}

// Build a synthetic Manifest with known chunk-ref distribution so we
// can verify summarize's accounting independent of any live PG.
func TestSummarize_DedupAccounting(t *testing.T) {
	hashA := repo.HashOf([]byte("alpha"))
	hashB := repo.HashOf([]byte("beta"))
	hashC := repo.HashOf([]byte("gamma"))

	m := &backup.Manifest{
		BackupID:   "test",
		Deployment: "db1",
		Tenant:     "default",
		Files: []backup.FileEntry{
			{Path: "f1", Size: 100, Chunks: []backup.ChunkRef{
				{Hash: hashA, Offset: 0, Len: 50},
				{Hash: hashB, Offset: 50, Len: 50},
			}},
			{Path: "f2", Size: 50, Chunks: []backup.ChunkRef{
				{Hash: hashB, Offset: 0, Len: 50}, // duplicate of f1 chunk
			}},
			{Path: "f3", Size: 30, Chunks: []backup.ChunkRef{
				{Hash: hashC, Offset: 0, Len: 30},
			}},
		},
		Tablespaces: []backup.Tablespace{{OID: 1663}},
	}
	opts := TakeOptions{Deployment: "db1", Tenant: "default"}
	identity := pg.SystemIdentity{SystemID: "s"}
	startedAt := time.Now().UTC()
	stoppedAt := startedAt.Add(123 * time.Millisecond)

	res := summarize(opts, m, nil, identity, 17, startedAt, stoppedAt)
	if res.LogicalBytes != 180 {
		t.Errorf("LogicalBytes = %d, want 180 (100+50+30)", res.LogicalBytes)
	}
	if res.TotalChunkRefs != 4 {
		t.Errorf("TotalChunkRefs = %d, want 4", res.TotalChunkRefs)
	}
	if res.UniqueChunkCount != 3 {
		t.Errorf("UniqueChunkCount = %d, want 3 (A, B, C; B is reused)", res.UniqueChunkCount)
	}
	if res.UniqueChunkBytes != 130 {
		t.Errorf("UniqueChunkBytes = %d, want 130 (50+50+30)", res.UniqueChunkBytes)
	}
	if res.FileCount != 3 {
		t.Errorf("FileCount = %d, want 3", res.FileCount)
	}
	if res.TablespaceCount != 1 {
		t.Errorf("TablespaceCount = %d, want 1", res.TablespaceCount)
	}
	if res.Duration != 123*time.Millisecond {
		t.Errorf("Duration = %v", res.Duration)
	}
	if res.StartedAt != startedAt || res.StoppedAt != stoppedAt {
		t.Errorf("times not propagated: %v / %v", res.StartedAt, res.StoppedAt)
	}
	if res.Deployment != "db1" || res.Tenant != "default" {
		t.Errorf("deployment/tenant: %q / %q", res.Deployment, res.Tenant)
	}
}

// TestTake_RejectsBadOptions confirms the entry point fails fast on
// missing required fields, before any I/O.
func TestTake_RejectsBadOptions(t *testing.T) {
	_, err := Take(context.Background(), TakeOptions{})
	if err == nil {
		t.Fatal("expected validation error on empty options")
	}
	if !errors.Is(err, output.ErrUsage) {
		t.Errorf("validation error should wrap ErrUsage; got %v", err)
	}
}
