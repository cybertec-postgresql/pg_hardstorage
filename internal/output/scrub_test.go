package output

import (
	"strings"
	"testing"
)

func TestScrubSecrets(t *testing.T) {
	const pw = "SECRETPW"
	for _, in := range []string{
		`open repo sftp://user:SECRETPW@127.0.0.1:1/x: boom`,
		`s3://AKIA:SECRETPW@bucket/p`,
		`pw with at sftp://u:SECRETPW@x@host/p`,
		`azblob://acct/c?sig=SECRETPW`,
		`s3://b/p?a=1&secret_key=SECRETPW&endpoint=http://h`,
		`s3://b/p?X-Amz-Signature=SECRETPW`,
		`dial: host=h password=SECRETPW dbname=d`,
		`dial: host=h password = 'SECRETPW'`,
	} {
		if out := ScrubSecrets(in); strings.Contains(out, pw) {
			t.Errorf("ScrubSecrets(%q) = %q leaks", in, out)
		}
	}
	if got := ScrubSecrets("no creds file:///var/lib/repo?x=1"); got != "no creds file:///var/lib/repo?x=1" {
		t.Errorf("benign text altered: %q", got)
	}
	if e := NewError("c", "bad sftp://u:SECRETPW@h/x"); strings.Contains(e.Error(), pw) {
		t.Errorf("NewError leaks: %v", e)
	}
	if e := NewError("c", "m").WithSuggestion(&Suggestion{Command: "pg_hardstorage repo init sftp://u:SECRETPW@h/x"}); strings.Contains(e.Suggestion.Command, pw) {
		t.Errorf("Suggestion leaks: %v", e.Suggestion.Command)
	}
}
