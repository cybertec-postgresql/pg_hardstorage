package cli_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"

	// Side-effect import path that the production binary
	// transitively pulls in.  See sinks_registered_test.go
	// for the rationale.
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// TestProductionRegistration_KMSFire is the regression test
// for the audit-v26 finding that surfaced this whole pass:
// kms.DefaultRegistry was empty in production because nothing
// imported the awskms / gcpkms packages.  After adding
// internal/cli/plugins_register.go the bug is fixed; this
// test pins it down so a future refactor that drops the
// side-effect import gets caught.
func TestProductionRegistration_KMSFire(t *testing.T) {
	want := []string{"aws-kms", "azure-kv", "gcp-kms", "pkcs11", "vault-transit"}
	sort.Strings(want)

	got := kms.DefaultRegistry.Schemes()
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}

	var missing []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("KMS provider schemes not registered in production binary: %s\nregistered: %s",
			strings.Join(missing, ", "),
			strings.Join(got, ", "))
	}
}
