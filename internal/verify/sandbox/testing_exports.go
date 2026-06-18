// Test-only exports of unexported helpers.  Lives in the
// production package (no `_test.go` suffix) so external test
// packages (`sandbox_test`) can reach the parser + validator
// without cracking package boundaries open in production.
//
// Idiom: every exported symbol here ends in ForTesting and
// is documented as such.  Operators reading the godoc see
// the test-only intent at a glance.

package sandbox

// MagicVerdictTestForm is the test-friendly serialisation of
// the internal magicResult.  Test code asserts on string
// verdicts ("PASS"/"FAIL"/"SKIP"/"UNKNOWN") rather than
// internal enum values so the contract stays readable.
type MagicVerdictTestForm struct {
	Verdict string
	Detail  string
	Err     error
}

// ParseMagicForTesting exposes parseMagic for unit tests.
// Returns a stringly-typed verdict + any detail field; on
// no-magic-line, Err is set.
func ParseMagicForTesting(console string) MagicVerdictTestForm {
	res, err := parseMagic(console)
	out := MagicVerdictTestForm{Detail: res.Detail, Err: err}
	switch res.Verdict {
	case verdictPass:
		out.Verdict = "PASS"
	case verdictFail:
		out.Verdict = "FAIL"
	case verdictSkip:
		out.Verdict = "SKIP"
	default:
		out.Verdict = "UNKNOWN"
	}
	return out
}

// StripControlForTesting exposes stripControl for unit
// tests.  The function is small enough that mirroring it in
// test code would invite drift; expose-for-test is cleaner.
func StripControlForTesting(in string) string { return stripControl(in) }

// ValidateFirecrackerOptsForTesting exposes
// validateFirecrackerOpts for unit tests so the pre-flight
// surface is exercised in default CI without -tags
// firecracker.
func ValidateFirecrackerOptsForTesting(opts Options) error {
	return validateFirecrackerOpts(opts)
}
