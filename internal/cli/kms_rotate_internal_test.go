package cli

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestKmsRotateBody_ReplicaFailuresBlockRetireVerdict pins the safety
// fix: a rotation where primaries rotated but some REPLICAS failed (and
// thus still hold the old KEK) must NOT print the "old KEK can be
// retired" green light — retiring it would strand those replicas and
// make them undecryptable. A fully-clean rotation still green-lights it.
func TestKmsRotateBody_ReplicaFailuresBlockRetireVerdict(t *testing.T) {
	replicaFailed := kmsRotateBody{RotateKEKResult: backup.RotateKEKResult{
		OldKEKRef: "k:old", NewKEKRef: "k:new",
		Considered: 3, Rotated: 3, ReplicaFailures: 2, Failed: 0,
	}}
	var sb strings.Builder
	if err := replicaFailed.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "can be retired") {
		t.Errorf("must NOT green-light KEK retirement when replicas failed:\n%s", out)
	}
	if !strings.Contains(out, "DO NOT retire") {
		t.Errorf("must explicitly warn DO NOT retire the old KEK:\n%s", out)
	}

	clean := kmsRotateBody{RotateKEKResult: backup.RotateKEKResult{
		OldKEKRef: "k:old", NewKEKRef: "k:new",
		Considered: 3, Rotated: 3, ReplicaFailures: 0, Failed: 0,
	}}
	var sb2 strings.Builder
	if err := clean.WriteText(&sb2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb2.String(), "can be retired") {
		t.Errorf("a fully-clean rotation should green-light retirement:\n%s", sb2.String())
	}
}
