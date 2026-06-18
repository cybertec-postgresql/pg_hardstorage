package pgbackrest

import (
	"strings"
	"testing"
)

func TestInfo_TextDefault(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	if err := runInfo(globalArgs, "text"); err != nil {
		t.Fatal(err)
	}
	if (*got)[0] != "list" || (*got)[1] != "db1" {
		t.Errorf("expected list db1; got %v", *got)
	}
	if sliceContainsPair(*got, "--output", "json") {
		t.Errorf("text default should not pass --output json; got %v", *got)
	}
}

func TestInfo_JSONForwardsOutput(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	if err := runInfo(globalArgs, "json"); err != nil {
		t.Fatal(err)
	}
	if !sliceContainsPair(*got, "--output", "json") {
		t.Errorf("expected --output json; got %v", *got)
	}
}

func TestInfo_UnsupportedOutputRefused(t *testing.T) {
	captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	err := runInfo(globalArgs, "yaml")
	if err == nil || !strings.Contains(err.Error(), "unsupported --output") {
		t.Fatalf("expected unsupported-output error, got %v", err)
	}
}

func TestCheck_DispatchesDoctor(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1"}
	if err := runCheck(globalArgs); err != nil {
		t.Fatal(err)
	}
	want := []string{"doctor", "db1"}
	if len(*got) < 2 || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Errorf("expected doctor db1; got %v", *got)
	}
}

func TestVerify_DispatchesVerifyLatest(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	if err := runVerify(globalArgs, false); err != nil {
		t.Fatal(err)
	}
	if (*got)[0] != "verify" || (*got)[1] != "db1" || (*got)[2] != "latest" {
		t.Errorf("expected verify db1 latest; got %v", *got)
	}
}

func TestVerify_FullForwarded(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	if err := runVerify(globalArgs, true); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range *got {
		if a == "--full" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --full in args; got %v", *got)
	}
}

func TestStanzaCreate_DispatchesRepoInit(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r"}
	if err := runStanzaCreate(globalArgs); err != nil {
		t.Fatal(err)
	}
	if len(*got) < 3 || (*got)[0] != "repo" || (*got)[1] != "init" || (*got)[2] != "file:///r" {
		t.Errorf("expected repo init file:///r; got %v", *got)
	}
}

func TestStanzaCreate_RequiresRepo(t *testing.T) {
	captureDispatch(t)
	globalArgs = pgbackrestArgs{stanza: "db1"}
	err := runStanzaCreate(globalArgs)
	if err == nil || !strings.Contains(err.Error(), "--repo1-path or --repo1-s3-bucket") {
		t.Fatalf("expected repo-required error, got %v", err)
	}
}
