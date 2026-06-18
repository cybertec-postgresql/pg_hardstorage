package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFilteredEnv_RedactsSecrets asserts that secret-shaped
// env vars are redacted in the fixture but the keys are kept
// (so the operator fixture diff still shows what it sets).
func TestFilteredEnv_RedactsSecrets(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-keep-me")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "drop-me")
	t.Setenv("WALG_S3_PREFIX", "s3://keep-me")
	t.Setenv("PGPASSWORD", "drop-me-too")
	t.Setenv("DB_API_KEY", "drop")
	t.Setenv("MY_TOKEN_VALUE", "drop")

	got := filteredEnv()
	gotMap := map[string]string{}
	for _, kv := range got {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		gotMap[kv[:eq]] = kv[eq+1:]
	}

	cases := []struct {
		key         string
		wantValue   string
		wantPresent bool
	}{
		{"AWS_ACCESS_KEY_ID", "AKIA-keep-me", true},   // not redacted
		{"AWS_SECRET_ACCESS_KEY", "<redacted>", true}, // "secret"
		{"WALG_S3_PREFIX", "s3://keep-me", true},      // benign
		{"PGPASSWORD", "<redacted>", true},            // "password"
		{"DB_API_KEY", "<redacted>", true},            // "api_key"
		{"MY_TOKEN_VALUE", "<redacted>", true},        // "token"
	}
	for _, c := range cases {
		v, ok := gotMap[c.key]
		if !ok {
			if c.wantPresent {
				t.Errorf("%s missing from filtered env", c.key)
			}
			continue
		}
		if v != c.wantValue {
			t.Errorf("%s = %q; want %q", c.key, v, c.wantValue)
		}
	}
}

// TestWriteFixture_AppendsNDJSON verifies the recorder writes
// one JSON line per invocation (NDJSON), and that the fixture
// file grows on each call.
func TestWriteFixture_AppendsNDJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.ndjson")
	t.Setenv("PGHS_ARGV_FIXTURE", path)

	for i := 0; i < 3; i++ {
		writeFixture(fixtureEntry{
			At:   "2026-05-08T00:00:00Z",
			Tool: "barman-cloud-backup",
			Argv: []string{"barman-cloud-backup", "--cloud-provider=aws-s3", "s3://foo", "stanza"},
			Pid:  i,
		})
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	count := 0
	for sc.Scan() {
		count++
		var entry fixtureEntry
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Errorf("line %d: invalid JSON: %v", count, err)
		}
		if entry.Tool != "barman-cloud-backup" {
			t.Errorf("line %d: tool = %q", count, entry.Tool)
		}
	}
	if count != 3 {
		t.Errorf("expected 3 NDJSON lines; got %d", count)
	}
}

// TestWriteFixture_BestEffort: an unwritable path doesn't
// crash the recorder.  Operator backups must never break
// because the fixture file is unwritable.
func TestWriteFixture_BestEffort(t *testing.T) {
	t.Setenv("PGHS_ARGV_FIXTURE", "/proc/cant-write-here.ndjson")
	// Just ensure we don't panic.
	writeFixture(fixtureEntry{Tool: "barman-cloud-backup"})
}
