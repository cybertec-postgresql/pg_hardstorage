package repo_test

import (
	"crypto/sha256"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestHashOf_MatchesSHA256(t *testing.T) {
	body := []byte("the quick brown fox")
	want := sha256.Sum256(body)
	got := repo.HashOf(body)
	if [32]byte(got) != want {
		t.Errorf("HashOf differs from sha256.Sum256")
	}
}

func TestHash_String_Hex(t *testing.T) {
	h := repo.HashOf([]byte("hello"))
	s := h.String()
	if len(s) != 64 {
		t.Errorf("String() len = %d, want 64", len(s))
	}
	if strings.ToLower(s) != s {
		t.Errorf("String() should be lowercase: %q", s)
	}
	parsed, err := repo.ParseHash(s)
	if err != nil {
		t.Fatalf("ParseHash: %v", err)
	}
	if parsed != h {
		t.Error("round-trip via String/ParseHash failed")
	}
}

func TestHash_IsZero(t *testing.T) {
	if !(repo.Hash{}).IsZero() {
		t.Error("zero Hash must report IsZero")
	}
	if repo.HashOf([]byte("x")).IsZero() {
		t.Error("non-zero Hash must not report IsZero")
	}
}

func TestHash_JSONMarshal_Hex(t *testing.T) {
	h := repo.HashOf([]byte("hello"))
	type wrap struct {
		H repo.Hash `json:"h"`
	}
	b, err := stdjson.Marshal(wrap{H: h})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"h":"` + h.String() + `"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestHash_JSONUnmarshal_Hex(t *testing.T) {
	h := repo.HashOf([]byte("hello"))
	type wrap struct {
		H repo.Hash `json:"h"`
	}
	in := []byte(`{"h":"` + h.String() + `"}`)
	var got wrap
	if err := stdjson.Unmarshal(in, &got); err != nil {
		t.Fatal(err)
	}
	if got.H != h {
		t.Error("round-trip mismatch")
	}
}

func TestHash_UnmarshalText_RejectsBad(t *testing.T) {
	bad := []string{
		"",
		"abcd",                  // too short
		strings.Repeat("z", 64), // not hex
		strings.Repeat("a", 63), // off by one
		strings.Repeat("A", 64), // uppercase ok actually (hex.Decode accepts)
	}
	for _, s := range bad {
		var h repo.Hash
		err := h.UnmarshalText([]byte(s))
		if s == strings.Repeat("A", 64) {
			if err != nil {
				t.Errorf("uppercase 64-char hex should decode; got err %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("UnmarshalText(%q) should error", s)
		}
	}
}

// TestHash_RawArrayConvertibility documents that Hash and [32]byte are
// freely interconvertible — a property we rely on at boundaries with
// crypto/sha256 (which returns [32]byte).
func TestHash_RawArrayConvertibility(t *testing.T) {
	a := sha256.Sum256([]byte("x"))
	h := repo.Hash(a)
	a2 := [32]byte(h)
	if a != a2 {
		t.Error("conversion round-trip failed")
	}
}
