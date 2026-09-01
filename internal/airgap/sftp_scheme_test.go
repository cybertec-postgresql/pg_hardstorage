package airgap_test

// The sftp and scp storage backends pass their whole repo URL to
// EndpointAllowed:
//
//	airgap.Default().EndpointAllowed(cfg.URL.String())
//
// The scheme switch did not list sftp or scp, so they hit the default
// branch and were refused OUTRIGHT — before any host classification
// ran. In air-gapped mode those two backends were therefore unusable no
// matter how they were configured: an allowlisted host was refused, and
// so was an RFC1918 address, while the error text told the operator to
// "add it to airgap.allowlist or use a loopback/RFC1918 address".
//
// An air-gapped site reaching an internal NAS over SFTP is a mainstream
// configuration for exactly this deployment shape, so the combination
// that failed is the one most likely to be used.
//
// The refusal was fail-CLOSED, so this was never a security hole — it
// made a documented feature combination impossible and pointed the
// operator at a remedy that could not work.

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

func strictPolicy() airgap.Policy {
	return airgap.Policy{
		Mode:      airgap.ModeStrict,
		Allowlist: []string{"nas.internal", "backup.corp:2222"},
	}
}

func TestEndpointAllowed_SFTPAndSCPReachHostClassification(t *testing.T) {
	p := strictPolicy()
	allowed := []string{
		"sftp://nas.internal/backups",    // allowlisted by name
		"sftp://nas.internal:22/backups", // name allowlisted, any port
		"scp://nas.internal/backups",     // same, scp
		"sftp://10.0.0.5/backups",        // RFC1918
		"sftp://192.168.1.10:22/b",       // RFC1918 with port
		"scp://127.0.0.1/b",              // loopback
		"sftp://backup.corp:2222/b",      // host:port allowlist entry
		"sftp://localhost/b",             // loopback by name
	}
	for _, u := range allowed {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("%s was refused in air-gapped mode: %v\n"+
				"    an allowlisted or RFC1918 SFTP/SCP host must be reachable — otherwise "+
				"the backend cannot be used at all", u, err)
		}
	}
}

// Falling through must be a CHECK, not a blanket allowance: a public
// SFTP host is still refused, which is the whole point of the mode.
func TestEndpointAllowed_SFTPToPublicHostStillRefused(t *testing.T) {
	p := strictPolicy()
	for _, u := range []string{
		"sftp://sftp.example.com/backups",
		"scp://198.51.100.7/backups", // TEST-NET-2, publicly routable
		"sftp://8.8.8.8/b",
		"sftp://backup.corp:9999/b", // allowlist entry pins port 2222
	} {
		err := p.EndpointAllowed(u)
		if err == nil {
			t.Errorf("%s was ALLOWED — adding the scheme must let the host be classified, "+
				"not bypass classification", u)
			continue
		}
		if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
			t.Errorf("%s: wrong error type: %v", u, err)
		}
		// And the refusal must be about the HOST now, not the scheme.
		if strings.Contains(err.Error(), "scheme") {
			t.Errorf("%s: still refused on the scheme rather than the host:\n%v", u, err)
		}
	}
}

// Object-store schemes stay unrecognised on purpose: their URL host is
// the BUCKET name, not a network host, so classifying it would be
// meaningless. Those backends pass their real https endpoint separately.
func TestEndpointAllowed_ObjectStoreSchemesRemainUnrecognised(t *testing.T) {
	p := strictPolicy()
	for _, u := range []string{"s3://bucket/prefix", "gs://bucket/p", "azblob://acct/c"} {
		err := p.EndpointAllowed(u)
		if err == nil {
			t.Errorf("%s was allowed; its host is a bucket name and must not be treated "+
				"as a network host", u)
		}
	}
}

// Off mode must remain a complete bypass.
func TestEndpointAllowed_OffModeUnaffected(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeOff}
	for _, u := range []string{"sftp://sftp.example.com/b", "s3://bucket/p", "https://8.8.8.8"} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("air-gap off must allow %s: %v", u, err)
		}
	}
}
