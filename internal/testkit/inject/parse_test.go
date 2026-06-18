package inject_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

func TestParseAction_HappyPath(t *testing.T) {
	cases := []struct {
		in     string
		prefix string
		args   map[string]string
	}{
		{
			"disk_full(target=repo, fill=98%)",
			"disk_full",
			map[string]string{"target": "repo", "fill": "98%"},
		},
		{
			"signal(target=agent_random, sig=9)",
			"signal",
			map[string]string{"target": "agent_random", "sig": "9"},
		},
		{
			"patroni_switchover()",
			"patroni_switchover",
			map[string]string{},
		},
		{
			`sql("SELECT pg_drop_replication_slot('foo')")`,
			"sql",
			map[string]string{"_positional": "SELECT pg_drop_replication_slot('foo')"},
		},
		{
			"libfaketime(skew=+10m, target=agent)",
			"libfaketime",
			map[string]string{"skew": "+10m", "target": "agent"},
		},
		{
			"flip_random_byte(prefix=chunks/)",
			"flip_random_byte",
			map[string]string{"prefix": "chunks/"},
		},
		{
			// Whitespace-tolerant
			"toxiproxy(  rate = 80% , status = 503 ,  dur = 5m  )",
			"toxiproxy",
			map[string]string{"rate": "80%", "status": "503", "dur": "5m"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.in, func(t *testing.T) {
			got, err := inject.ParseAction(tt.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got.Prefix != tt.prefix {
				t.Errorf("prefix: got %q want %q", got.Prefix, tt.prefix)
			}
			if len(got.Args) != len(tt.args) {
				t.Errorf("args size: got %d (%v), want %d (%v)",
					len(got.Args), got.Args, len(tt.args), tt.args)
			}
			for k, v := range tt.args {
				if got.Args[k] != v {
					t.Errorf("args[%q]: got %q want %q", k, got.Args[k], v)
				}
			}
		})
	}
}

func TestParseAction_Errors(t *testing.T) {
	cases := []struct {
		in   string
		errs string
	}{
		{"", "empty action"},
		{"disk_full", "missing '('"},
		{"disk_full(target=repo", "missing trailing ')'"},
		{"(target=repo)", "empty prefix"},
		{"sql('unbalanced)", "unbalanced"},
		{"signal(target=a, target=b)", "duplicate arg"},
		{"signal(=value)", "empty arg key"},
	}
	for _, tt := range cases {
		t.Run(tt.in, func(t *testing.T) {
			_, err := inject.ParseAction(tt.in)
			if err == nil {
				t.Fatalf("expected error containing %q", tt.errs)
			}
			if !strings.Contains(err.Error(), tt.errs) {
				t.Errorf("err = %v; want substr %q", err, tt.errs)
			}
		})
	}
}

func TestArgs_String_StableOrder(t *testing.T) {
	a := inject.Args{"target": "agent", "sig": "9", "wait": "5s"}
	got := a.String()
	want := "sig=9, target=agent, wait=5s"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestArgs_Require(t *testing.T) {
	a := inject.Args{"target": "repo"}
	if _, err := a.Require("target"); err != nil {
		t.Errorf("present arg should not error: %v", err)
	}
	if _, err := a.Require("missing"); err == nil {
		t.Errorf("missing arg should error")
	}
}
