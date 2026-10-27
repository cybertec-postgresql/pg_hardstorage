package azurekv

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
)

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: farFuture()}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func realClientFor(t *testing.T, status int, body string) (*realClient, *[]string) {
	t.Helper()
	var paths []string
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	cli, err := azkeys.NewClient("https://v.vault.azure.net", fakeCred{}, &azkeys.ClientOptions{
		ClientOptions:                        azcore.ClientOptions{Transport: tr},
		DisableChallengeResourceVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &realClient{client: cli, key: "k"}, &paths
}

// The vault answers with the KID of the version that wrapped; the
// provider records it, so a rotation is visible in the manifest.
func TestRealClient_WrapVersionedReportsTheVersionTheVaultUsed(t *testing.T) {
	rc, paths := realClientFor(t, 200,
		`{"kid":"https://v.vault.azure.net/keys/k/abc123","value":"d3JhcHBlZA"}`)
	ct, ver, err := rc.WrapVersioned(context.Background(), "", "RSA-OAEP-256", []byte("dek"))
	if err != nil {
		t.Fatal(err)
	}
	if ver != "abc123" || string(ct) != "wrapped" {
		t.Fatalf("got ct=%q version=%q", ct, ver)
	}
	if len(*paths) == 0 || !strings.Contains((*paths)[0], "/keys/k/") {
		t.Fatalf("request paths %v", *paths)
	}
	// Wrap is the version-discarding view of the same call.
	ct2, err := rc.Wrap(context.Background(), "", "RSA-OAEP-256", []byte("dek"))
	if err != nil || string(ct2) != "wrapped" {
		t.Fatalf("Wrap: %q %v", ct2, err)
	}
}

func TestRealClient_WrapVersionedKeepsRequestedVersionWithoutKID(t *testing.T) {
	rc, _ := realClientFor(t, 200, `{"value":"d3JhcHBlZA"}`)
	_, ver, err := rc.WrapVersioned(context.Background(), "pinned", "RSA-OAEP-256", []byte("dek"))
	if err != nil || ver != "pinned" {
		t.Fatalf("version=%q err=%v", ver, err)
	}
}

func TestRealClient_WrapVersionedPropagatesVaultError(t *testing.T) {
	rc, _ := realClientFor(t, 403, `{"error":{"code":"Forbidden","message":"no"}}`)
	if _, _, err := rc.WrapVersioned(context.Background(), "", "RSA-OAEP-256", []byte("dek")); err == nil {
		t.Fatal("a vault refusal must surface as an error")
	}
}

func farFuture() time.Time { return time.Now().Add(24 * time.Hour) }
