// Always-built helpers shared between the firecracker stub
// and the real (`-tags firecracker`) backend.  Lives outside
// the build-tagged files so the parser / validator surface
// is unit-testable in the default CI sweep without an HSM
// or KVM.

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// magicPrefix is the line the rootfs init script prints to
// signal the verify outcome.  Frozen for the 24-month
// compatibility window — operators baking custom rootfs
// images depend on this exact byte sequence.
const magicPrefix = "__PG_HARDSTORAGE_VERIFY__:"

// magicResult captures what we got out of the rootfs's
// magic line.
type magicResult struct {
	Verdict magicVerdict
	Detail  string
}

type magicVerdict int

const (
	verdictUnknown magicVerdict = iota
	verdictPass
	verdictFail
	verdictSkip
)

// parseMagic walks the captured console output looking for
// the magicPrefix line.  Returns the parsed verdict + any
// detail field.  If no magic line is present, returns an
// error (the rootfs didn't honour the contract).
func parseMagic(out string) (magicResult, error) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, magicPrefix)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len(magicPrefix):])
		fields := strings.SplitN(rest, " ", 2)
		verdict := strings.ToUpper(fields[0])
		detail := ""
		if len(fields) == 2 {
			detail = fields[1]
		}
		switch verdict {
		case "OK", "PASS":
			return magicResult{Verdict: verdictPass}, nil
		case "FAIL", "FAILED":
			return magicResult{Verdict: verdictFail, Detail: detail}, nil
		case "SKIPPED", "SKIP":
			return magicResult{Verdict: verdictSkip, Detail: detail}, nil
		}
	}
	return magicResult{}, errors.New("rootfs did not emit __PG_HARDSTORAGE_VERIFY__ line on console (rootfs contract not satisfied)")
}

// stripControl removes non-printable bytes from the captured
// console — the kernel boot log carries control sequences we
// don't want in the JSON Result.
func stripControl(in string) string {
	return strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 {
			return r
		}
		if r == '\n' || r == '\t' {
			return r
		}
		return -1
	}, in)
}

// validateFirecrackerOpts is the pre-flight gate for the
// Firecracker backend.  Refuses obviously-wrong inputs early
// rather than letting the microVM panic 200ms in.  Lives
// here (always-built) so unit tests can exercise it without
// -tags firecracker.
func validateFirecrackerOpts(opts Options) error {
	if opts.FirecrackerKernel == "" {
		return errors.New("sandbox/firecracker: FirecrackerKernel is required (path to vmlinux)")
	}
	if opts.FirecrackerRootfs == "" {
		return errors.New("sandbox/firecracker: FirecrackerRootfs is required (path to rootfs.ext4)")
	}
	if _, err := os.Stat(opts.FirecrackerKernel); err != nil {
		return fmt.Errorf("sandbox/firecracker: kernel image %q not readable: %w", opts.FirecrackerKernel, err)
	}
	if _, err := os.Stat(opts.FirecrackerRootfs); err != nil {
		return fmt.Errorf("sandbox/firecracker: rootfs image %q not readable: %w", opts.FirecrackerRootfs, err)
	}
	st, err := os.Stat(opts.DataDir)
	if err != nil {
		return fmt.Errorf("sandbox/firecracker: DataDir %q not readable: %w", opts.DataDir, err)
	}
	if st.IsDir() {
		return fmt.Errorf("sandbox/firecracker: DataDir %q is a directory; the firecracker backend needs a block image (mkfs.ext4 -d <pgdata> pgdata.img <size>)", opts.DataDir)
	}
	return nil
}
