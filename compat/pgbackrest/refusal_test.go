package pgbackrest

import (
	"strings"
	"testing"
)

func TestRefuseUnknown_FormatStable(t *testing.T) {
	err := refuseUnknown("expire")
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	mustContain(t, got, "pg-hardstorage-pgbackrest:")
	mustContain(t, got, "expire:")
	mustContain(t, got, "not implemented in v1.1")
	mustContain(t, got, "native equivalent:")
}

func TestRefuseFlag_FormatStable(t *testing.T) {
	err := refuseFlag("--type=diff", "use --type=incr")
	got := err.Error()
	mustContain(t, got, "pg-hardstorage-pgbackrest:")
	mustContain(t, got, "--type=diff")
	mustContain(t, got, "native equivalent: use --type=incr")
}

func TestUnknownCommand_RoutesToRefusal(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"--stanza=db1", "expire"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not implemented in v1.1") {
		t.Fatalf("expected refusal for unknown verb, got %v", err)
	}
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("got %q; want substring %q", got, want)
	}
}
