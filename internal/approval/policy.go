// policy.go — the operator-side trust anchor for the approval gate.
package approval

import (
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// Environment variables that configure the approval policy. They are
// read on the machine running the destructive op, never from the
// repository: the request body lives in the repo, so anything in it —
// approver keys and threshold included — is chosen by whoever can
// write there. The gate therefore trusts only what the operator
// configured locally.
const (
	// EnvRoster names the trusted-approver roster: a directory of
	// PEM-encoded ed25519 public keys (*.pem / *.pub), or a single file
	// holding one or more PEM blocks. Default: <config-dir>/approvers.
	EnvRoster = "PG_HARDSTORAGE_APPROVAL_ROSTER"
	// EnvMinThreshold is the minimum number of distinct trusted
	// approvals any gated op needs, whatever the request's own
	// threshold says. Default DefaultMinThreshold.
	EnvMinThreshold = "PG_HARDSTORAGE_APPROVAL_MIN_THRESHOLD"
)

// DefaultMinThreshold is two so a single operator can never authorise
// a destructive op alone unless the operator explicitly lowers it.
const DefaultMinThreshold = 2

// Policy is the operator's approval policy: whose votes count and how
// many of them a gated op needs.
type Policy struct {
	// TrustedKeys is the roster, keyed by keyFingerprint.
	TrustedKeys map[string]ed25519.PublicKey
	// MinThreshold floors every request's threshold.
	MinThreshold int
	// Source says where the roster came from, for error messages.
	Source string
}

// Policy errors. They are distinct from ErrThresholdNotMet so the
// operator is told the request can never pass as filed, rather than
// "collect more approvals".
var (
	ErrNoTrustedRoster    = errors.New("approval: no trusted approver roster configured")
	ErrUntrustedApprovals = errors.New("approval: not enough approvals from the trusted approver roster")
	ErrUntrustedApprover  = errors.New("approval: approver key is not in the trusted approver roster")
	ErrBelowMinThreshold  = errors.New("approval: request threshold is below the configured minimum")
)

// NewPolicy builds a Policy from PEM-encoded public keys.
func NewPolicy(minThreshold int, pems ...[]byte) (*Policy, error) {
	p := &Policy{TrustedKeys: map[string]ed25519.PublicKey{}, MinThreshold: minThreshold}
	for i, raw := range pems {
		if err := p.addPEMs(raw, fmt.Sprintf("key %d", i)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// LoadPolicy reads the policy from the environment (EnvRoster,
// EnvMinThreshold), falling back to <config-dir>/approvers. A missing
// roster yields a Policy with no trusted keys — Gate then fails closed
// with ErrNoTrustedRoster — so that an unconfigured host refuses
// rather than trusting the request's own key list.
func LoadPolicy() (*Policy, error) {
	minT := DefaultMinThreshold
	if v := strings.TrimSpace(os.Getenv(EnvMinThreshold)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("approval: %s=%q: must be an integer ≥ 1", EnvMinThreshold, v)
		}
		minT = n
	}
	src := os.Getenv(EnvRoster)
	if src == "" {
		p, err := paths.Resolve(paths.DefaultOptions())
		if err != nil {
			return nil, fmt.Errorf("approval: resolve config dir for approver roster: %w", err)
		}
		src = filepath.Join(p.Config.Value, "approvers")
	}
	pol := &Policy{TrustedKeys: map[string]ed25519.PublicKey{}, MinThreshold: minT, Source: src}
	fi, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pol, nil
		}
		return nil, fmt.Errorf("approval: approver roster %s: %w", src, err)
	}
	if !fi.IsDir() {
		body, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("approval: read approver roster %s: %w", src, err)
		}
		return pol, pol.addPEMs(body, src)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, fmt.Errorf("approval: read approver roster %s: %w", src, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".pem") || strings.HasSuffix(e.Name(), ".pub")) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(src, n)
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("approval: read approver roster %s: %w", path, err)
		}
		if err := pol.addPEMs(body, path); err != nil {
			return nil, err
		}
	}
	return pol, nil
}

// addPEMs adds every public-key PEM block in raw. A roster entry that
// does not parse is an error, not a skip: silently dropping a trusted
// approver turns into a mysterious "not enough approvals" later.
func (p *Policy) addPEMs(raw []byte, where string) error {
	n := 0
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			break
		}
		raw = rest
		pub, err := parseED25519PublicKeyPEM(pem.EncodeToMemory(block))
		if err != nil {
			return fmt.Errorf("approval: approver roster %s: %w", where, err)
		}
		p.TrustedKeys[keyFingerprint(pub)] = pub
		n++
	}
	if n == 0 {
		return fmt.Errorf("approval: approver roster %s: no PEM public key found", where)
	}
	return nil
}

// trusts reports whether the PEM-encoded key is on the roster.
func (p *Policy) trusts(pemKey []byte) bool {
	pub, err := parseED25519PublicKeyPEM(pemKey)
	if err != nil {
		return false
	}
	_, ok := p.TrustedKeys[keyFingerprint(pub)]
	return ok
}

// requiredApprovals is the number of distinct trusted votes a request
// needs: its own threshold, floored by the operator's minimum.
func (p *Policy) requiredApprovals(r *Request) int {
	if r.Threshold > p.MinThreshold {
		return r.Threshold
	}
	return p.MinThreshold
}

// CheckRequest reports whether a request, as filed, can ever satisfy
// the policy: every approver key must be trusted and the threshold
// must meet the minimum. Create uses it to refuse doomed requests
// up front; Gate enforces the substance independently.
func (p *Policy) CheckRequest(threshold int, approverKeys [][]byte) error {
	if len(p.TrustedKeys) == 0 {
		return p.noRosterErr()
	}
	if threshold < p.MinThreshold {
		return fmt.Errorf("%w: threshold %d, minimum %d (set %s to change the minimum)",
			ErrBelowMinThreshold, threshold, p.MinThreshold, EnvMinThreshold)
	}
	for i, k := range approverKeys {
		if !p.trusts(k) {
			return fmt.Errorf("%w: approver key %d (add it to %s)", ErrUntrustedApprover, i, p.Source)
		}
	}
	return nil
}

func (p *Policy) noRosterErr() error {
	where := p.Source
	if where == "" {
		where = "the policy"
	}
	return fmt.Errorf("%w: no approver public keys at %s (set %s)", ErrNoTrustedRoster, where, EnvRoster)
}

// trustedApprovals counts distinct valid votes on r whose key is both
// on the request's allowlist and on the operator roster.
func (p *Policy) trustedApprovals(r *Request) (int, error) {
	valid, err := validApprovers(r)
	if err != nil {
		return 0, err
	}
	n := 0
	for fp := range valid {
		if _, ok := p.TrustedKeys[fp]; ok {
			n++
		}
	}
	return n, nil
}
