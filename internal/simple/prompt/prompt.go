// Package prompt is the input-handling layer for pg_hardstorage_simple.
//
// One small Prompter holds an io.Reader (stdin or a test stub), an
// io.Writer (stdout), and a "isTTY" hint that gates ANSI colour codes.
// Every input shape goes through the same routine: print the question,
// echo the default in brackets, read a line, trim trailing newline.
// No flags, no readline, no GNU getopt drama — the binary's whole
// design promise is "no command-line arguments", so the prompts are
// the entire interface.
//
// Validators are plain `func(string) error` — return non-nil to make
// the Prompter print the message and re-ask.  Loops bound at
// maxReprompts to keep typo storms from wedging an inattentive
// operator's terminal.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ErrQuit is what a Prompter returns from Choice when the operator
// picks the "quit" option ("q").  Flows that see this should unwind
// cleanly — quit means "I'm done, no error".
var ErrQuit = errors.New("operator quit")

// maxReprompts caps the re-ask loop in PromptValid so a stuck input
// stream (the doctest harness piping a wrong answer in a loop)
// terminates instead of spinning.
const maxReprompts = 5

// Prompter is the input/output surface for one pg_hardstorage_simple
// session.  Construct with NewPrompter (production: stdin/stdout +
// TTY auto-detect) or NewTestPrompter (drive from a string buffer).
type Prompter struct {
	r     *bufio.Reader
	w     io.Writer
	isTTY bool
}

// NewPrompter is the production constructor: stdin/stdout with TTY
// auto-detection.  We avoid pulling in golang.org/x/term as a dep
// just to call IsTerminal — the only consumer is the colour gate
// and a missing dep would force-disable colour, which is the safe
// default anyway.
func NewPrompter() *Prompter {
	return &Prompter{
		r:     bufio.NewReader(os.Stdin),
		w:     os.Stdout,
		isTTY: isTerminal(os.Stdin),
	}
}

// NewTestPrompter wires a reader of canned answers and a writer the
// test inspects.  Tests pass a *bytes.Buffer or strings.NewReader
// and assert on what the binary printed.
func NewTestPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{
		r:     bufio.NewReader(in),
		w:     out,
		isTTY: false,
	}
}

// Printf writes plain text to the operator.  Use this for the
// "About to do X — continue?" lead-in lines and the success banners.
func (p *Prompter) Printf(format string, args ...any) {
	fmt.Fprintf(p.w, format, args...)
}

// Println adds a trailing newline.
func (p *Prompter) Println(s string) {
	fmt.Fprintln(p.w, s)
}

// IsTTY reports whether the Prompter is talking to a real terminal.
// Flows use this to decide between spinner-with-cursor-resets vs.
// plain "still going..." line dumps.
func (p *Prompter) IsTTY() bool { return p.isTTY }

// Line reads one line, trimmed of \n and \r.  Returns an error only
// on EOF before any input was read (operator hit Ctrl-D at a prompt).
func (p *Prompter) Line() (string, error) {
	s, err := p.r.ReadString('\n')
	s = strings.TrimRight(s, "\r\n")
	if err == io.EOF && s == "" {
		return "", io.EOF
	}
	return s, nil
}

// PromptLine prints the question, accepts a default (echoed in
// brackets), and returns the operator's choice or the default if
// they hit Enter.  EOF returns the default too — the test harness's
// stdin runs out, that's "user accepted everything".
func (p *Prompter) PromptLine(question, def string) (string, error) {
	if def == "" {
		p.Printf("  %s\n  > ", question)
	} else {
		p.Printf("  %s\n  [%s]> ", question, def)
	}
	s, err := p.Line()
	if errors.Is(err, io.EOF) {
		return def, nil
	}
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

// PromptValid is PromptLine plus a validator.  If the validator
// returns non-nil, the error is shown and the prompt re-asks (up to
// maxReprompts).  An empty default with no input + validator
// rejection counts as a re-ask, not as accepting the empty string.
func (p *Prompter) PromptValid(question, def string, validate func(string) error) (string, error) {
	for i := 0; i < maxReprompts; i++ {
		s, err := p.PromptLine(question, def)
		if err != nil {
			return "", err
		}
		if verr := validate(s); verr != nil {
			p.Printf("  ✗ %s\n\n", verr.Error())
			continue
		}
		return s, nil
	}
	return "", fmt.Errorf("prompt: gave up after %d invalid answers", maxReprompts)
}

// YesNo asks a [Y/n] / [y/N] question and returns the boolean.
// Empty answer (just Enter) means the default; "y"/"yes"/"Y" → true;
// "n"/"no"/"N" → false; anything else re-asks.
func (p *Prompter) YesNo(question string, defaultYes bool) (bool, error) {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	for i := 0; i < maxReprompts; i++ {
		p.Printf("  %s %s ", question, hint)
		s, err := p.Line()
		if errors.Is(err, io.EOF) {
			return defaultYes, nil
		}
		if err != nil {
			return false, err
		}
		s = strings.TrimSpace(strings.ToLower(s))
		switch s {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Printf("  ✗ answer y or n (got %q)\n\n", s)
	}
	return false, fmt.Errorf("prompt: gave up after %d invalid yes/no answers", maxReprompts)
}

// Choice is one item in a numbered menu.
type Choice struct {
	// Label is the right-hand text shown after the number.
	Label string
	// Detail, when non-empty, is shown indented under the label.
	// Used for "deployment: db1 — last backed up 4 hours ago"-style
	// per-option supplementary info.
	Detail string
}

// PromptChoice prints a numbered menu and returns the selected index
// (0-based).  A literal "q" answer returns ErrQuit so the main loop
// can exit cleanly.
//
// When defaultIdx is non-negative, hitting Enter selects it.
func (p *Prompter) PromptChoice(question string, choices []Choice, defaultIdx int) (int, error) {
	if len(choices) == 0 {
		return 0, errors.New("prompt: no choices supplied")
	}
	for i := 0; i < maxReprompts; i++ {
		p.Printf("  %s\n\n", question)
		for j, c := range choices {
			marker := " "
			if j == defaultIdx {
				marker = "*"
			}
			p.Printf("    %s%d. %s\n", marker, j+1, c.Label)
			if c.Detail != "" {
				p.Printf("        %s\n", c.Detail)
			}
		}
		p.Printf("     q. quit\n\n")
		def := ""
		if defaultIdx >= 0 {
			def = strconv.Itoa(defaultIdx + 1)
		}
		s, err := p.PromptLine("pick a number", def)
		if err != nil {
			return 0, err
		}
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "q" || s == "quit" {
			return 0, ErrQuit
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			p.Printf("  ✗ %q is not a number\n\n", s)
			continue
		}
		if n < 1 || n > len(choices) {
			p.Printf("  ✗ pick 1..%d (got %d)\n\n", len(choices), n)
			continue
		}
		return n - 1, nil
	}
	return 0, fmt.Errorf("prompt: gave up after %d invalid choices", maxReprompts)
}
