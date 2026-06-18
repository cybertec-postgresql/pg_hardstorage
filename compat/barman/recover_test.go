package barman

import (
	"bytes"
	"strings"
	"testing"
)

// TestMapToNativeArgs covers every translation row in
// recoverFlagTable: each Barman input must yield the documented
// native flags (or refuse).
func TestMapToNativeArgs(t *testing.T) {
	cases := []struct {
		name     string
		in       recoverArgs
		want     []string
		wantWarn []string
		wantErr  string
	}{
		{
			name: "target-time -> --to",
			in:   recoverArgs{targetTime: "2026-04-27 09:42 UTC"},
			want: []string{"--to", "2026-04-27 09:42 UTC"},
		},
		{
			name: "target-name -> --to-backup",
			in:   recoverArgs{targetName: "release-cut"},
			want: []string{"--to-backup", "release-cut"},
		},
		{
			name: "target-immediate -> --to-lsn 0/0",
			in:   recoverArgs{targetImmed: true},
			want: []string{"--to-lsn", "0/0"},
		},
		{
			name: "target-action pause",
			in:   recoverArgs{targetAction: "pause"},
			want: []string{"--to-action", "pause"},
		},
		{
			name: "target-action promote (case-insensitive)",
			in:   recoverArgs{targetAction: "Promote"},
			want: []string{"--to-action", "promote"},
		},
		{
			name:     "target-action garbage -> warn + drop",
			in:       recoverArgs{targetAction: "wibble"},
			want:     nil,
			wantWarn: []string{"--target-action=wibble"},
		},
		{
			name:     "remote-ssh-command dropped + warn",
			in:       recoverArgs{remoteSSHCmd: "ssh restore@db"},
			wantWarn: []string{"--remote-ssh-command"},
		},
		{
			name:     "get-wal dropped + warn",
			in:       recoverArgs{getWAL: ptrBool(true)},
			wantWarn: []string{"--get-wal"},
		},
		{
			name:     "no-get-wal dropped + warn",
			in:       recoverArgs{getWAL: ptrBool(false)},
			wantWarn: []string{"--no-get-wal"},
		},
		{
			name:     "retry knobs dropped + warn",
			in:       recoverArgs{retryN: "3"},
			wantWarn: []string{"--retry-times/--retry-sleep"},
		},
		{
			name:    "target-xid refuses with remediation",
			in:      recoverArgs{targetXID: "12345"},
			wantErr: "--target-xid not supported",
		},
		{
			name: "combo: time + action",
			in:   recoverArgs{targetTime: "now", targetAction: "shutdown"},
			want: []string{"--to", "now", "--to-action", "shutdown"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			got, err := mapToNativeArgs(&tc.in, &warn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalSlices(got, tc.want) {
				t.Errorf("native args mismatch:\n got: %v\nwant: %v", got, tc.want)
			}
			for _, w := range tc.wantWarn {
				if !strings.Contains(warn.String(), w) {
					t.Errorf("missing warn fragment %q in:\n%s", w, warn.String())
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
