//go:build linux

package external_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

// procState returns the /proc state letter of pid ("" when the pid is
// gone, i.e. reaped).
func procState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	return string(s[i+2])
}

// TestClient_Call_ErrorPathReapsPlugin pins the LOW finding: Call
// killed the plugin on its error paths without cmd.Wait, so every
// failed RPC (a malformed response, a failed write) left a zombie in
// the process table for the agent's lifetime.
func TestClient_Call_ErrorPathReapsPlugin(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	plugin := filepath.Join(dir, "pg-hardstorage-plugin-garbage")
	body := "#!/bin/sh\necho $$ > '" + pidFile + "'\nread line\necho 'this is not json'\nexec sleep 30\n"
	if err := os.WriteFile(plugin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cli := &external.Client{Path: plugin, Timeout: 20 * time.Second}
	if _, err := cli.Call(context.Background(), "X", nil); err == nil {
		t.Fatal("Call succeeded on a malformed response")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := procState(pid)
		if st == "" {
			return // reaped
		}
		if st == "Z" {
			t.Fatalf("plugin pid %d left as a zombie after a failed Call (killed, never waited)", pid)
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin pid %d still running (state %s) after Call returned", pid, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
