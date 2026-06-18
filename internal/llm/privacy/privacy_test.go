package privacy_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/privacy"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want privacy.Mode
		err  bool
	}{
		{"", privacy.ModeStandard, false},
		{"strict", privacy.ModeStrict, false},
		{"STANDARD", privacy.ModeStandard, false},
		{"open", privacy.ModeOpen, false},
		{"local-only", privacy.ModeLocalOnly, false},
		{"local_only", privacy.ModeLocalOnly, false},
		{"localonly", privacy.ModeLocalOnly, false},
		{"loose", "", true},
	}
	for _, tc := range cases {
		got, err := privacy.Parse(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("Parse(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEndpointAllowed_LocalOnly(t *testing.T) {
	cases := []struct {
		endpoint string
		ok       bool
	}{
		// Allowed: loopback + RFC-1918.
		{"http://127.0.0.1:11434/v1", true},
		{"http://localhost:8080", true},
		{"http://10.0.0.5:8080", true},
		{"http://192.168.1.10/v1", true},
		{"http://172.16.5.5/v1", true},
		{"http://172.31.0.1/v1", true},
		// Refused: public.
		{"https://api.openai.com", false},
		{"https://my-resource.openai.azure.com", false},
		{"", false}, // implicit default = api.openai.com → refused
		// Refused: 172.32 isn't private.
		{"http://172.32.0.1/v1", false},
		// Refused: public hostnames that textually start with a
		// private-range prefix must NOT bypass the gate — host is
		// parsed as an IP, not prefix-matched.
		{"http://10.evil.example.com/v1", false},
		{"http://192.168.1.1.attacker.com/v1", false},
		{"http://172.16.0.0.attacker.com/v1", false},
		{"https://127.0.0.1.attacker.com/v1", false},
		{"http://localhost.attacker.com/v1", false},
	}
	for _, tc := range cases {
		err := privacy.EndpointAllowed(privacy.ModeLocalOnly, tc.endpoint)
		got := err == nil
		if got != tc.ok {
			t.Errorf("EndpointAllowed(local-only, %q) = (ok=%v), want %v (err=%v)", tc.endpoint, got, tc.ok, err)
		}
	}
}

func TestEndpointAllowed_OtherModesAllowEverything(t *testing.T) {
	for _, m := range []privacy.Mode{privacy.ModeStrict, privacy.ModeStandard, privacy.ModeOpen} {
		if err := privacy.EndpointAllowed(m, "https://api.openai.com"); err != nil {
			t.Errorf("mode %s should allow public endpoint; got %v", m, err)
		}
	}
}

func TestRedact_Open_StripsCredentialsOnly(t *testing.T) {
	in := "user is alice@example.com and AWS key is AKIAIOSFODNN7EXAMPLE; api_key=sk-secret"
	got := privacy.Redact(privacy.ModeOpen, in)
	if !strings.Contains(got, "alice@example.com") {
		t.Errorf("open mode should NOT redact email; got %q", got)
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("open mode should redact AWS key; got %q", got)
	}
	if strings.Contains(got, "sk-secret") {
		t.Errorf("open mode should redact api_key; got %q", got)
	}
}

func TestRedact_Standard_StripsPIIAndCredentials(t *testing.T) {
	in := "operator alice@example.com from 10.0.0.5 used api_key=sk-secret on s3://bucket/x"
	got := privacy.Redact(privacy.ModeStandard, in)
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("standard should redact email; got %q", got)
	}
	if strings.Contains(got, "10.0.0.5") {
		t.Errorf("standard should redact IP; got %q", got)
	}
	if strings.Contains(got, "sk-secret") {
		t.Errorf("standard should redact api_key; got %q", got)
	}
	// Plain s3:// without creds is preserved (it's a key
	// reference, not a secret).
	if !strings.Contains(got, "s3://bucket/x") {
		t.Errorf("standard should preserve plain s3 URI; got %q", got)
	}
}

func TestRedact_Standard_PostgresConnString(t *testing.T) {
	in := "connection: postgres://backup:supersecret@db1.example.com:5432/postgres"
	got := privacy.Redact(privacy.ModeStandard, in)
	if strings.Contains(got, "supersecret") {
		t.Errorf("standard should mask the password; got %q", got)
	}
}

func TestRedact_Standard_KMSSecret(t *testing.T) {
	in := "configured api_key_secret: kms-secret://prod/openai/key"
	got := privacy.Redact(privacy.ModeStandard, in)
	if strings.Contains(got, "prod/openai/key") {
		t.Errorf("kms-secret path should be redacted; got %q", got)
	}
	if !strings.Contains(got, "kms-secret://") {
		t.Errorf("kms-secret scheme should remain so reviewers see it WAS a secret ref; got %q", got)
	}
}

func TestRedact_Strict_KeepsErrorCodesAndRunbooks(t *testing.T) {
	in := "the doctor reported restore.target_in_wal_gap on db1; see runbook R6 for resolution. WAL lag was 47s."
	got := privacy.Redact(privacy.ModeStrict, in)
	if !strings.Contains(got, "restore.target_in_wal_gap") {
		t.Errorf("strict should keep structured error codes; got %q", got)
	}
	if !strings.Contains(got, "R6") {
		t.Errorf("strict should keep runbook IDs; got %q", got)
	}
	if !strings.Contains(got, "47s") {
		t.Errorf("strict should keep short durations (metric values); got %q", got)
	}
	// "db1" is a deployment name — strict drops it.
	if strings.Contains(got, "db1") {
		t.Errorf("strict should redact deployment names; got %q", got)
	}
	// Plain English words drop.
	if strings.Contains(got, "doctor") {
		t.Errorf("strict should redact prose; got %q", got)
	}
}

func TestRedact_Strict_DoesNotLeakViaCredentials(t *testing.T) {
	in := "the api key is sk-supersecret123"
	got := privacy.Redact(privacy.ModeStrict, in)
	if strings.Contains(got, "sk-supersecret123") {
		t.Errorf("strict should redact creds (and everything else); got %q", got)
	}
}

func TestRedact_LocalOnlyEqualsStandard(t *testing.T) {
	in := "alice@example.com 10.0.0.1 deployment db1"
	a := privacy.Redact(privacy.ModeLocalOnly, in)
	b := privacy.Redact(privacy.ModeStandard, in)
	if a != b {
		t.Errorf("local-only and standard should redact identically (egress is local but classification floor still applies):\n  local-only: %q\n  standard:   %q", a, b)
	}
}

func TestRedact_Idempotent(t *testing.T) {
	for _, m := range []privacy.Mode{privacy.ModeStrict, privacy.ModeStandard, privacy.ModeOpen, privacy.ModeLocalOnly} {
		in := "alice@example.com 10.0.0.1 api_key=sk-x runbook R3 restore.target_in_wal_gap"
		once := privacy.Redact(m, in)
		twice := privacy.Redact(m, once)
		if once != twice {
			t.Errorf("mode %s should be idempotent:\n  once:  %q\n  twice: %q", m, once, twice)
		}
	}
}

func TestRedact_EmptyInput(t *testing.T) {
	for _, m := range []privacy.Mode{privacy.ModeStrict, privacy.ModeStandard, privacy.ModeOpen, privacy.ModeLocalOnly} {
		if got := privacy.Redact(m, ""); got != "" {
			t.Errorf("mode %s: empty in should yield empty out; got %q", m, got)
		}
	}
}

func TestRedact_UnknownModeFailsClosed(t *testing.T) {
	// An unknown mode falls through to strict — defence in
	// depth so a typo doesn't accidentally enable open mode.
	got := privacy.Redact(privacy.Mode("loose"), "alice@example.com")
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("unknown mode should fall through to strict (no PII leakage); got %q", got)
	}
}
