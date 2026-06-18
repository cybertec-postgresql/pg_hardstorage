// wizard.go — interactive-prompt helper (choices/text/integer) shared by fleet/profile/fault add+edit commands.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// wizard is a tiny interactive-prompt helper for the
// fleet/profile/fault `add` and `edit` commands.  Three knobs:
//
//   - choices(prompt, options, default) → string picked from
//     options.  Accepts either the literal answer or its
//     1-based index.
//   - text(prompt, default) → free-text string with default
//     fallback when the user hits Enter.
//   - integer(prompt, default) → parsed int with the same
//     default-on-enter behaviour.
//
// All three respect `os.Stdin` for input; tests inject a
// strings.Reader.  When stdin isn't a terminal (CI, scripts)
// callers should fill values via flags instead and skip the
// wizard altogether — see fleetAddIsInteractive().
type wizard struct {
	in  *bufio.Reader
	out io.Writer
}

func newWizard(in io.Reader, out io.Writer) *wizard {
	return &wizard{in: bufio.NewReader(in), out: out}
}

// readLine reads up to a newline; trims the newline + leading
// / trailing whitespace.  EOF returns ("", io.EOF) so callers
// can distinguish a deliberate Enter (empty string, no error).
func (w *wizard) readLine() (string, error) {
	line, err := w.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" && err == io.EOF {
		return "", io.EOF
	}
	return line, nil
}

// choices presents an enumerated picker.  Returns the picked
// option (always one of `options`).
func (w *wizard) choices(prompt string, options []string, def string) (string, error) {
	if len(options) == 0 {
		return "", fmt.Errorf("wizard: no options for %q", prompt)
	}
	for {
		fmt.Fprintln(w.out, prompt)
		for i, o := range options {
			marker := "  "
			if o == def {
				marker = "* "
			}
			fmt.Fprintf(w.out, "%s%2d) %s\n", marker, i+1, o)
		}
		hint := ""
		if def != "" {
			hint = fmt.Sprintf(" [%s]", def)
		}
		fmt.Fprintf(w.out, "?%s ", hint)
		raw, err := w.readLine()
		if err == io.EOF {
			if def != "" {
				return def, nil
			}
			return "", fmt.Errorf("input ended with no default available")
		}
		if err != nil {
			return "", err
		}
		if raw == "" {
			if def != "" {
				return def, nil
			}
			fmt.Fprintln(w.out, "  (no default — pick one)")
			continue
		}
		// Try as 1-based index first.
		if n, perr := strconv.Atoi(raw); perr == nil && n >= 1 && n <= len(options) {
			return options[n-1], nil
		}
		// Try as literal match.
		for _, o := range options {
			if o == raw {
				return o, nil
			}
		}
		fmt.Fprintf(w.out, "  %q is not one of the options; try again.\n", raw)
	}
}

// text returns a free-text input, defaulting to def on Enter.
// An empty result is allowed only when def is empty AND the
// caller passes allowEmpty=true.
func (w *wizard) text(prompt, def string, allowEmpty bool) (string, error) {
	for {
		hint := ""
		if def != "" {
			hint = fmt.Sprintf(" [%s]", def)
		}
		fmt.Fprintf(w.out, "%s%s: ", prompt, hint)
		raw, err := w.readLine()
		if err == io.EOF && def != "" {
			return def, nil
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		if raw == "" {
			if def != "" {
				return def, nil
			}
			if allowEmpty {
				return "", nil
			}
			fmt.Fprintln(w.out, "  (required — try again)")
			continue
		}
		return raw, nil
	}
}

// integer returns a parsed int, defaulting on Enter.
func (w *wizard) integer(prompt string, def int) (int, error) {
	for {
		fmt.Fprintf(w.out, "%s [%d]: ", prompt, def)
		raw, err := w.readLine()
		if err == io.EOF {
			return def, nil
		}
		if err != nil {
			return 0, err
		}
		if raw == "" {
			return def, nil
		}
		n, perr := strconv.Atoi(raw)
		if perr != nil {
			fmt.Fprintf(w.out, "  %q is not an integer; try again.\n", raw)
			continue
		}
		return n, nil
	}
}

// stdinIsTTY reports whether os.Stdin is connected to a real
// terminal.  Called by the `add` commands to decide whether to
// run the wizard or hard-fail on missing flags.  Avoids the
// x/term dep — checks the file mode for a character device,
// which is the cross-platform Go-stdlib idiom.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
