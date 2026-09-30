package safety_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

// Operators do not type verbs as bare, punctuation-free, >=3-char
// tokens.  "gc" (two chars) could never become on-topic, and "rotate
// the key." or "run gc?" left the verb glued to its punctuation, so the
// command was refused as severe no matter what the operator asked.
func TestAnomaly_OperatorPhrasingPutsVerbOnTopic(t *testing.T) {
	cases := []struct{ said, cmd string }{
		{"please run gc on the repo", "pg_hardstorage repo gc s3://r/"},
		{"can you run gc?", "pg_hardstorage repo gc s3://r/"},
		{"Rotate the key.", "pg_hardstorage kms rotate"},
		{"we need a key rotation now", "pg_hardstorage kms rotate"},
		{"I deleted the wrong one, delete db1's oldest backup", "pg_hardstorage backup delete db1 x"},
		{"purge (the expired ones)", "pg_hardstorage repo purge"},
	}
	for _, c := range cases {
		d := &safety.AnomalyDetector{RecentTopicTokens: safety.ExtractTopicTokens(c.said)}
		if dec := d.Score(c.cmd); dec.Score != safety.ScoreNormal {
			t.Errorf("said %q, cmd %q: %v (%s), want normal", c.said, c.cmd, dec.Score, dec.Reason)
		}
	}
	// Still severe when the operator never raised it.
	d := &safety.AnomalyDetector{RecentTopicTokens: safety.ExtractTopicTokens("what is the WAL lag on db1?")}
	if dec := d.Score("pg_hardstorage repo gc s3://r/"); dec.Score != safety.ScoreSevere {
		t.Errorf("off-topic gc: %v, want severe", dec.Score)
	}
}
