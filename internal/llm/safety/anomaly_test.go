package safety_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

func TestAnomaly_NormalCommand(t *testing.T) {
	d := &safety.AnomalyDetector{
		RecentTopicTokens: safety.ExtractTopicTokens("the operator wants to restore db1"),
	}
	dec := d.Score("pg_hardstorage status db1")
	if dec.Score != safety.ScoreNormal {
		t.Errorf("expected normal, got %v: %+v", dec.Score, dec)
	}
}

func TestAnomaly_HighRiskVerbOffTopicSevere(t *testing.T) {
	d := &safety.AnomalyDetector{
		RecentTopicTokens: safety.ExtractTopicTokens("how do I tail the logs for a deployment?"),
	}
	dec := d.Score("pg_hardstorage kms shred --tenant T")
	if dec.Score != safety.ScoreSevere {
		t.Errorf("expected severe (off-topic shred); got %v: %+v", dec.Score, dec)
	}
	if dec.Verb != "shred" {
		t.Errorf("Verb = %q, want shred", dec.Verb)
	}
}

func TestAnomaly_HighRiskVerbOnTopicNormal(t *testing.T) {
	d := &safety.AnomalyDetector{
		RecentTopicTokens: safety.ExtractTopicTokens("Run a GDPR shred for tenant T as required by Art. 17"),
	}
	dec := d.Score("pg_hardstorage kms shred --tenant T --yes")
	if dec.Score != safety.ScoreNormal {
		t.Errorf("expected normal (verb on-topic); got %v: %+v", dec.Score, dec)
	}
}

func TestAnomaly_DeploymentScopeRefusesDifferent(t *testing.T) {
	d := &safety.AnomalyDetector{
		DeploymentScope:   "db1",
		RecentTopicTokens: safety.ExtractTopicTokens("show status of db1"),
	}
	dec := d.Score("pg_hardstorage backup db2")
	if dec.Score != safety.ScoreSevere {
		t.Errorf("expected severe (different deployment); got %v: %+v", dec.Score, dec)
	}
	if dec.Token != "db2" {
		t.Errorf("Token = %q, want db2", dec.Token)
	}
}

func TestAnomaly_DeploymentScopeAllowsSame(t *testing.T) {
	d := &safety.AnomalyDetector{
		DeploymentScope:   "db1",
		RecentTopicTokens: safety.ExtractTopicTokens("show status of db1"),
	}
	dec := d.Score("pg_hardstorage status db1")
	if dec.Score != safety.ScoreNormal {
		t.Errorf("expected normal; got %v: %+v", dec.Score, dec)
	}
}

func TestAnomaly_EmptyCommandIsNormal(t *testing.T) {
	d := &safety.AnomalyDetector{}
	dec := d.Score("")
	if dec.Score != safety.ScoreNormal {
		t.Errorf("empty command should be normal; got %v", dec.Score)
	}
}

func TestAnomaly_FlagFormVerbDetected(t *testing.T) {
	d := &safety.AnomalyDetector{
		RecentTopicTokens: safety.ExtractTopicTokens("show me the status"),
	}
	dec := d.Score("pg_hardstorage backup db1 --force")
	if dec.Score != safety.ScoreSevere {
		t.Errorf("expected severe (--force flag with unrelated topic); got %v", dec.Score)
	}
}

func TestExtractTopicTokens(t *testing.T) {
	out := safety.ExtractTopicTokens("Restore db1 from yesterday's backup; the WAL gap was reported at 09:42")
	if _, ok := out["restore"]; !ok {
		t.Error("expected 'restore' in tokens")
	}
	if _, ok := out["db1"]; !ok {
		t.Error("expected 'db1' in tokens")
	}
	if _, ok := out["the"]; ok {
		t.Error("stop-word 'the' should be filtered")
	}
}

func TestMergeAndSortedTokens(t *testing.T) {
	a := safety.ExtractTopicTokens("alpha beta gamma")
	b := safety.ExtractTopicTokens("gamma delta")
	merged := safety.MergeTopicTokens(a, b)
	got := safety.SortedTokens(merged)
	want := []string{"alpha", "beta", "delta", "gamma"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SortedTokens = %v, want %v", got, want)
	}
}

func TestScoreString(t *testing.T) {
	for _, tc := range []struct {
		s    safety.AnomalyScore
		want string
	}{
		{safety.ScoreNormal, "normal"},
		{safety.ScoreWarn, "warn"},
		{safety.ScoreSevere, "severe"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("Score(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}
