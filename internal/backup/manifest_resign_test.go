package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// ResignStreamed must produce exactly the document Sign + MarshalToBytes
// produces, for any entry count: `repair attestation` replaces both
// manifest copies with it.
func TestResignStreamed_IdenticalToSign(t *testing.T) {
	oldPriv, _, _ := backup.GenerateKeypair(rand.Reader)
	oldSigner, _ := backup.LoadSigner(oldPriv)
	newPriv, newPub, _ := backup.GenerateKeypair(rand.Reader)
	newSigner, _ := backup.LoadSigner(newPriv)
	newVerifier, _ := backup.LoadVerifier(newPub)

	for _, n := range []int{1, 9, 1500} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m := validManifest()
			m.Files = manyFiles(n)
			if err := m.Sign(oldSigner); err != nil {
				t.Fatal(err)
			}
			src, err := m.MarshalToBytes()
			if err != nil {
				t.Fatal(err)
			}
			re, err := backup.ResignStreamed(bytes.NewReader(src), newSigner)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()

			want := *m
			want.Attestation = nil
			if err := want.Sign(newSigner); err != nil {
				t.Fatal(err)
			}
			wantBytes, _ := want.MarshalToBytes()
			rc, err := re.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, wantBytes) || int64(len(got)) != re.Size() {
				for i := range got {
					if i >= len(wantBytes) || got[i] != wantBytes[i] {
						lo := max(i-80, 0)
						t.Logf("first diff at %d:\n got  %q\n want %q", i, got[lo:min(i+80, len(got))], wantBytes[lo:min(i+80, len(wantBytes))])
						break
					}
				}
				t.Fatalf("re-signed stream differs from Sign+Marshal (%d vs %d bytes, Size()=%d)", len(got), len(wantBytes), re.Size())
			}
			if _, err := backup.VerifyStream(bytes.NewReader(got), newVerifier); err != nil {
				t.Errorf("re-signed document does not verify under the new key: %v", err)
			}
		})
	}
}

// A manifest whose primary is gone and whose replica is over the held
// threshold must be streamed, not read whole (the held reader would fall
// back to the replica and hit the cap).
func TestReadAuto_StreamsOversizedReplicaWhenPrimaryLost(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store, sp := storeAt(t)
	ctx := context.Background()
	m := validManifest()
	m.Files = manyFiles(400)
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(ctx, backup.PrimaryPath(m.Deployment, m.BackupID)); err != nil {
		t.Fatal(err)
	}
	defer func(a, b int64) { backup.AutoStreamThreshold, backup.MaxManifestBytes = a, b }(backup.AutoStreamThreshold, backup.MaxManifestBytes)
	backup.AutoStreamThreshold, backup.MaxManifestBytes = 4<<10, 8<<10
	got, err := store.ReadAuto(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatalf("ReadAuto with the primary lost and a large replica: %v", err)
	}
	if !got.Streaming() || got.NumFiles() != 400 {
		t.Errorf("streaming=%v files=%d, want a streamed manifest of 400 files", got.Streaming(), got.NumFiles())
	}
}
