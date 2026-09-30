package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

type fleetSearchView struct {
	Count      int `json:"count"`
	Unreadable int `json:"unreadable"`
	Hits       []struct {
		Deployment string `json:"deployment"`
		BackupID   string `json:"backup_id"`
	} `json:"hits"`
}

// TestFleetSearch_FindsSignedManifests pins that `fleet search`
// verifies manifests with the operator's keyring. Every committed
// manifest is signed; searching with no verifier made ParseAndVerify
// reject all of them, and the walk dropped each rejection silently —
// every query answered "0 hits", exit 0, on a populated repository.
func TestFleetSearch_FindsSignedManifests(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "tenant-a", "", 1)
	commitTenantManifest(t, w, "db2", "tenant-b", "", 2)

	stdout, _, exit := runCLI(t, "fleet", "search",
		"--repo", w.repoURL, "--query", "deployment:db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v fleetSearchView
	bodyOf(t, stdout, &v)
	if v.Count != 1 || len(v.Hits) != 1 || v.Hits[0].Deployment != "db1" {
		t.Fatalf("hits = %+v (count %d), want exactly db1", v.Hits, v.Count)
	}
	if v.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0", v.Unreadable)
	}
}

// TestFleetSearch_SurfacesUnreadableManifests pins that a manifest the
// search could not read or verify is COUNTED and reported, not skipped
// without a trace: "0 hits" must not be indistinguishable from "every
// manifest failed verification".
func TestFleetSearch_SurfacesUnreadableManifests(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "tenant-a", "", 1)
	// Corrupt the committed manifest in place so it no longer verifies.
	var key string
	for info, err := range w.sp.List(context.Background(), "manifests/db1/backups/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, "/manifest.json") {
			key = info.Key
		}
	}
	if key == "" {
		t.Fatal("no manifest key found")
	}
	body := []byte(`{"schema":"garbage"}`)
	if _, err := w.sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "fleet", "search",
		"--repo", w.repoURL, "--query", "deployment:db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v fleetSearchView
	bodyOf(t, stdout, &v)
	if v.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (body %s)", v.Unreadable, stdout)
	}
}
