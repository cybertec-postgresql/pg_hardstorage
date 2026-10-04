package cli

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Issue #70: an email for wal.stream/reconnecting named no deployment,
// so an operator running several could not tell which one it was about.
func TestStampDeployment(t *testing.T) {
	e := stampDeployment(output.NewEvent(output.SeverityWarning, "wal.stream", "reconnecting"), "prod")
	if e.Subject.Deployment != "prod" {
		t.Fatalf("wal stream events must carry their deployment, got %q", e.Subject.Deployment)
	}
	e = stampDeployment(output.NewEvent(output.SeverityInfo, "wal.stream", "x").
		WithSubject(output.Subject{Deployment: "other"}), "prod")
	if e.Subject.Deployment != "other" {
		t.Fatalf("an explicit deployment must be kept, got %q", e.Subject.Deployment)
	}
}

func TestTaskDeployment(t *testing.T) {
	for task, want := range map[string]string{
		"backup:prod":       "prod",
		"rotate:db-1":       "db-1",
		"drill:timescale":   "timescale",
		"audit-anchor:prod": "prod",
		"fleet-wide-task":   "",
	} {
		if got := taskDeployment(task); got != want {
			t.Errorf("taskDeployment(%q) = %q, want %q", task, got, want)
		}
	}
}
