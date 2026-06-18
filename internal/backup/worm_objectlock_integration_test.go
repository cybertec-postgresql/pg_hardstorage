//go:build integration

// worm_objectlock_integration_test.go — proves ManifestStore.Commit
// applies WORM retention to the COMMITTED manifest against a real S3
// Object-Lock bucket (MinIO).
//
// This is the regression guard for the tmp-retention bug: earlier
// revisions locked the staging tmp object instead of the committed
// one, which on a Compliance bucket made the atomic rename's
// source-delete fail — the commit errored after the copy had already
// landed.  Writing the tmp UNLOCKED and applying SetRetention to the
// committed key fixes it; this test exercises that end-to-end through
// the AWS SDK so the Compliance-bucket path is validated for real,
// not just against the in-memory fake.
package backup_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	s3plugin "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func TestManifestStore_Commit_WORM_RealObjectLockBucket(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping S3 Object-Lock integration test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable; skipping")
	}
	ctx := context.Background()

	// Bring up a fresh MinIO. Single-node-single-drive mode on this
	// pinned release supports versioning + Object Lock.
	s, err := sink.New("s3-minio")
	if err != nil {
		t.Fatalf("sink.New(s3-minio): %v", err)
	}
	if err := s.Up(ctx); err != nil {
		t.Fatalf("sink.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Down(context.Background()) })

	// Creds via the AWS default chain; endpoint parsed from the sink
	// URL (s3://<bucket>?endpoint=...&path_style=true&region=...).
	for k, v := range s.EnvForAgent() {
		t.Setenv(k, v)
	}
	sinkURL, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %q: %v", s.URL(), err)
	}
	endpoint := sinkURL.Query().Get("endpoint")
	if endpoint == "" {
		t.Fatalf("sink URL %q has no endpoint param", s.URL())
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	// A bucket born with Object Lock enabled (the sink's pre-created
	// bucket is a plain one, so we make our own via the API).
	const lockBucket = "worm-objectlock-test"
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{
		Bucket:                     aws.String(lockBucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	}); err != nil {
		t.Fatalf("create object-lock bucket: %v", err)
	}

	// Open the storage plugin against the Object-Lock bucket.
	pluginURL, err := url.Parse("s3://" + lockBucket + "?endpoint=" + endpoint + "&path_style=true&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	p := &s3plugin.Plugin{}
	if err := p.Open(ctx, storage.StorageConfig{URL: pluginURL}); err != nil {
		t.Fatalf("s3 plugin open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.worm.objectlock",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	// The whole point: this Commit must SUCCEED on a Compliance bucket.
	// The buggy tmp-lock variant failed here, because the rename's
	// source-delete cannot remove a Compliance-locked tmp.
	if err := store(p).Commit(ctx, m, signer, backup.CommitOptions{
		RetainUntil:   until,
		RetentionMode: storage.WORMCompliance,
	}); err != nil {
		t.Fatalf("Commit against Object-Lock bucket failed: %v", err)
	}

	// The committed manifest itself must carry the retention.
	key := backup.PrimaryPath(m.Deployment, m.BackupID)
	ret, err := client.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{
		Bucket: aws.String(lockBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObjectRetention(%s): %v", key, err)
	}
	if ret.Retention == nil {
		t.Fatalf("committed manifest %s has no retention", key)
	}
	if ret.Retention.Mode != awss3types.ObjectLockRetentionModeCompliance {
		t.Errorf("retention mode = %q, want COMPLIANCE", ret.Retention.Mode)
	}
	if ret.Retention.RetainUntilDate == nil || !ret.Retention.RetainUntilDate.Equal(until) {
		t.Errorf("retain-until = %v, want %v", ret.Retention.RetainUntilDate, until)
	}

	// No staging file may survive the commit.
	for info, lerr := range p.List(ctx, "manifests/") {
		if lerr != nil {
			t.Fatalf("list: %v", lerr)
		}
		if strings.Contains(info.Key, ".tmp.") {
			t.Errorf("staging file survived the commit: %s", info.Key)
		}
	}
}

// store wraps a plugin in a ManifestStore — a one-liner kept separate
// so the test body reads as the scenario, not the plumbing.
func store(p storage.StoragePlugin) *backup.ManifestStore {
	return backup.NewManifestStore(p)
}
