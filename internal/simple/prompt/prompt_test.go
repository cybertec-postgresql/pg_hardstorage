package prompt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// driveScript wires a Prompter against a canned-input reader and
// a buffer we can assert on.  Tests use this to simulate the
// operator typing answers + see exactly what the binary printed.
func driveScript(input string) (*Prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return NewTestPrompter(strings.NewReader(input), out), out
}

func TestPromptLine_AcceptsDefaultOnEnter(t *testing.T) {
	p, _ := driveScript("\n")
	got, err := p.PromptLine("name?", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "alice" {
		t.Errorf("got %q, want %q", got, "alice")
	}
}

func TestPromptLine_TrimsCRLF(t *testing.T) {
	p, _ := driveScript("bob\r\n")
	got, _ := p.PromptLine("name?", "")
	if got != "bob" {
		t.Errorf("CRLF not trimmed: %q", got)
	}
}

func TestPromptLine_EOFReturnsDefault(t *testing.T) {
	p, _ := driveScript("")
	got, err := p.PromptLine("name?", "fallback")
	if err != nil {
		t.Fatalf("EOF should be a soft fallback: %v", err)
	}
	if got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

func TestPromptValid_RepromptsOnValidationFail(t *testing.T) {
	// First answer rejected, second answer accepted.
	p, out := driveScript("nope\ngood\n")
	validator := func(s string) error {
		if s == "good" {
			return nil
		}
		return errors.New("not good enough")
	}
	got, err := p.PromptValid("?", "", validator)
	if err != nil {
		t.Fatal(err)
	}
	if got != "good" {
		t.Errorf("got %q, want %q", got, "good")
	}
	if !strings.Contains(out.String(), "not good enough") {
		t.Errorf("error message not shown to operator:\n%s", out.String())
	}
}

func TestPromptValid_GivesUpAfterMaxReprompts(t *testing.T) {
	// Always-fail validator; the loop must bound itself rather
	// than spinning on an interactive operator's typo storm or
	// (worse) a test that forgot to feed enough lines.
	bad := strings.Repeat("x\n", maxReprompts+2)
	p, _ := driveScript(bad)
	_, err := p.PromptValid("?", "", func(string) error { return errors.New("nope") })
	if err == nil {
		t.Fatal("expected error after max reprompts; got nil")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error should say it gave up; got %q", err.Error())
	}
}

func TestYesNo_DefaultYes(t *testing.T) {
	p, _ := driveScript("\n")
	got, _ := p.YesNo("?", true)
	if !got {
		t.Error("default yes not honoured on empty answer")
	}
}

func TestYesNo_ExplicitN(t *testing.T) {
	p, _ := driveScript("n\n")
	got, _ := p.YesNo("?", true)
	if got {
		t.Error(`"n" answer not honoured`)
	}
}

func TestYesNo_RepromptsOnGarbage(t *testing.T) {
	p, out := driveScript("maybe\ny\n")
	got, err := p.YesNo("?", false)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("y after re-prompt not honoured")
	}
	if !strings.Contains(out.String(), "answer y or n") {
		t.Error("re-prompt hint not shown")
	}
}

func TestPromptChoice_PicksByNumber(t *testing.T) {
	p, _ := driveScript("2\n")
	choices := []Choice{{Label: "alpha"}, {Label: "bravo"}, {Label: "charlie"}}
	got, err := p.PromptChoice("which?", choices, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("got idx %d, want 1", got)
	}
}

func TestPromptChoice_QuitReturnsErrQuit(t *testing.T) {
	p, _ := driveScript("q\n")
	choices := []Choice{{Label: "alpha"}}
	_, err := p.PromptChoice("?", choices, -1)
	if !errors.Is(err, ErrQuit) {
		t.Errorf("want ErrQuit; got %v", err)
	}
}

func TestPromptChoice_RejectsOutOfRange(t *testing.T) {
	// 99 is out of range; next answer is valid.
	p, out := driveScript("99\n1\n")
	got, err := p.PromptChoice("?", []Choice{{Label: "alpha"}}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if !strings.Contains(out.String(), "pick 1..1") {
		t.Error("range hint not shown")
	}
}

func TestPromptChoice_DefaultOnEnter(t *testing.T) {
	p, _ := driveScript("\n")
	choices := []Choice{{Label: "alpha"}, {Label: "bravo"}}
	got, err := p.PromptChoice("?", choices, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("default not honoured: got %d, want 1", got)
	}
}
