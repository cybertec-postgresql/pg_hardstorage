package pgbackrest

import (
	"strings"
	"testing"
)

func TestRestore_RequiresPGDATA(t *testing.T) {
	captureDispatch(t)
	t.Setenv("PGDATA", "")
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	err := runRestore(globalArgs)
	if err == nil || !strings.Contains(err.Error(), "PGDATA env var must be set") {
		t.Fatalf("expected PGDATA error, got %v", err)
	}
}

func TestRestore_BasicForwardsTarget(t *testing.T) {
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/var/lib/postgresql/16/main")
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
	}
	if err := runRestore(globalArgs); err != nil {
		t.Fatal(err)
	}
	want := []string{"restore", "db1", "latest"}
	for i, w := range want {
		if (*got)[i] != w {
			t.Fatalf("arg[%d]: got %q want %q", i, (*got)[i], w)
		}
	}
	if !sliceContainsPair(*got, "--target", "/var/lib/postgresql/16/main") {
		t.Errorf("expected --target /var/lib/postgresql/16/main; got %v", *got)
	}
}

func TestRestore_TargetForms(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		typeHint string
		wantPair [2]string
	}{
		{"time auto", "2026-04-27 09:42 UTC", "", [2]string{"--to", "2026-04-27 09:42 UTC"}},
		{"lsn auto", "0/3000028", "", [2]string{"--to-lsn", "0/3000028"}},
		{"name auto", "name:cutover", "", [2]string{"--to-name", "cutover"}},
		{"explicit lsn", "0/3000028", "lsn", [2]string{"--to-lsn", "0/3000028"}},
		{"explicit time", "0/abcd", "time", [2]string{"--to", "0/abcd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureDispatch(t)
			t.Setenv("PGDATA", "/srv/pg")
			globalArgs = pgbackrestArgs{
				stanza: "db1", pg1Host: "h", repo1Path: "/r",
				target: tt.target, targetType: tt.typeHint,
			}
			if err := runRestore(globalArgs); err != nil {
				t.Fatal(err)
			}
			if !sliceContainsPair(*got, tt.wantPair[0], tt.wantPair[1]) {
				t.Errorf("expected pair %v in %v", tt.wantPair, *got)
			}
		})
	}
}

func TestRestore_TargetActionForwarded(t *testing.T) {
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/srv/pg")
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
		targetAction: "Promote",
	}
	if err := runRestore(globalArgs); err != nil {
		t.Fatal(err)
	}
	if !sliceContainsPair(*got, "--to-action", "promote") {
		t.Errorf("expected lower-case --to-action promote; got %v", *got)
	}
}

func TestLooksLikeLSN(t *testing.T) {
	tests := map[string]bool{
		"0/3000028":  true,
		"abcd/EF01":  true,
		"abcd":       false,
		"/abcd":      false,
		"abcd/":      false,
		"name:foo":   false,
		"2026-04-27": false,
	}
	for in, want := range tests {
		if got := looksLikeLSN(in); got != want {
			t.Errorf("looksLikeLSN(%q): got %v want %v", in, got, want)
		}
	}
}
