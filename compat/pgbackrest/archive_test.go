package pgbackrest

import (
	"reflect"
	"testing"
)

func TestArchivePush(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
	}
	if err := runArchivePush(globalArgs, "/pg/data/pg_wal/000000010000000000000001"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"wal", "push", "db1", "/pg/data/pg_wal/000000010000000000000001",
		"--pg-connection", "postgres://postgres@h/postgres",
		"--repo", "file:///r",
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("args:\n got %v\nwant %v", *got, want)
	}
}

func TestArchiveGet(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
	}
	if err := runArchiveGet(globalArgs, "000000010000000000000001", "/tmp/seg"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"wal", "fetch", "db1", "000000010000000000000001", "/tmp/seg",
		"--pg-connection", "postgres://postgres@h/postgres",
		"--repo", "file:///r",
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("args:\n got %v\nwant %v", *got, want)
	}
}
