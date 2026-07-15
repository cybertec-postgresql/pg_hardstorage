package cli

import (
	"encoding/json"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// Regression: backupCompareBody used to wrap ComparisonResult under a
// nested "result" key, so consumers saw the payload at
// `.result.result.*` — double-nested relative to every sibling command.
// The fields must inline next to "deployment".
func TestBackupCompareBody_JSONNotDoubleNested(t *testing.T) {
	b := backupCompareBody{
		Deployment:       "db1",
		ComparisonResult: &backup.ComparisonResult{},
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, nested := m["result"]; nested {
		t.Errorf("compare body still nests under \"result\": %s", raw)
	}
	if _, ok := m["deployment"]; !ok {
		t.Errorf("compare body lost \"deployment\": %s", raw)
	}
}

// Regression: an empty deployment produced `"backups": null` (nil slice)
// — breaking every consumer that iterates `.result.backups[]`. The empty
// case must marshal as [].
func TestListBody_EmptyBackupsIsArray(t *testing.T) {
	body := listBody{Deployment: "db1", Backups: []backupSummary{}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	arr, ok := m["backups"].([]any)
	if !ok || arr == nil {
		t.Errorf("backups = %v (%T), want empty JSON array", m["backups"], m["backups"])
	}
}

// (Regression for the third inconvenience fix lives in
// hold_remove_notfound_test.go — kept separate: it needs the CLI
// runner + a live repo fixture.)
