package commitlsn

import (
	"encoding/binary"
	"testing"

	"github.com/jackc/pglogrepl"
)

func TestFromMessage(t *testing.T) {
	commit := make([]byte, 26)
	commit[0] = 'C'
	binary.BigEndian.PutUint64(commit[2:], 0x1000)
	binary.BigEndian.PutUint64(commit[10:], 0x1040)

	stream := make([]byte, 30)
	stream[0] = 'c'
	binary.BigEndian.PutUint64(stream[6:], 0x2000)
	binary.BigEndian.PutUint64(stream[14:], 0x2040)

	prepared := make([]byte, 35)
	prepared[0] = 'K'
	binary.BigEndian.PutUint64(prepared[2:], 0x3000)
	binary.BigEndian.PutUint64(prepared[10:], 0x3040)

	for _, tc := range []struct {
		name string
		data []byte
		want pglogrepl.LSN
		ok   bool
	}{
		{"commit", commit, 0x1040, true},
		{"built commit", Message(0x5000), 0x5000, true},
		{"stream commit", stream, 0x2040, true},
		{"commit prepared", prepared, 0x3040, true},
		{"insert", []byte{'I', 0, 0, 0, 1}, 0, false},
		{"begin", append([]byte{'B'}, make([]byte, 20)...), 0, false},
		{"truncated commit", commit[:12], 0, false},
		{"empty", nil, 0, false},
	} {
		got, ok := FromMessage(tc.data)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: FromMessage = (%s, %v), want (%s, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
