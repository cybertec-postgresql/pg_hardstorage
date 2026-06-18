// azurite.go — Azure Blob emulator sink (Microsoft Azurite).
package sink

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	az "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// azuriteRuntime brings up Microsoft's Azure Blob emulator
// container.  Account name + key are well-known constants
// (Microsoft documents these as the canonical Azurite
// defaults — `devstoreaccount1` / `Eby8vdM02xN…`); operators
// use them for any Azurite test, so we hardcode them here
// for consistency.
//
// The agent's azblob plugin parses azblob://<account>/<container>
// with an optional `?endpoint=` query for emulator override.
// We ship that exact URL shape so the plugin doesn't need
// any Azurite-specific knowledge.
type azuriteRuntime struct {
	container string
	port      int
}

// Microsoft's documented Azurite defaults.  Don't change
// these — they're the well-known credentials operators
// expect when pointing tools at Azurite.
const (
	azuriteAccount   = "devstoreaccount1"
	azuriteKey       = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	azuriteContainer = "testkit"
)

var azuriteCounter atomic.Uint64

func newAzurite() *azuriteRuntime { return &azuriteRuntime{} }

// Name returns "azurite".
func (a *azuriteRuntime) Name() string { return "azurite" }

// Up runs the Azurite container on a free port, waits for the
// blob-service TCP listener, and creates the testkit container
// via the azblob SDK (Azurite, unlike MinIO, does not
// auto-create on first PUT).  Errors here best-effort clean up
// so the caller can retry.
func (a *azuriteRuntime) Up(ctx context.Context) error {
	if a.container != "" {
		return errors.New("azuriteRuntime: already up")
	}
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("azurite sink: pick port: %w", err)
	}
	a.port = port
	a.container = fmt.Sprintf("pg-hs-azurite-%d-%d",
		time.Now().UnixMilli(), azuriteCounter.Add(1))

	args := []string{
		"run", "-d",
		"--name", a.container,
		"-p", fmt.Sprintf("127.0.0.1:%d:10000", a.port),
		SinkImages["azurite"],
		// --blobHost 0.0.0.0 so the bound docker port forwards
		// reach the blob service; default 127.0.0.1 inside the
		// container would be unreachable from the host port map.
		// --skipApiVersionCheck so newer Azure SDK API versions
		// (the SDK ships with year-stamped versions; Azurite
		// 3.33 lags) work against pinned Azurite tags without
		// forcing operators to bump the image on every SDK
		// release.
		"azurite-blob", "--blobHost", "0.0.0.0", "--silent",
		"--skipApiVersionCheck",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		a.container = ""
		return fmt.Errorf("azurite sink: docker run: %w (output: %s)",
			err, truncate(out, 256))
	}
	if err := waitTCPReady(ctx, a.port, 30*time.Second); err != nil {
		_ = a.Down(context.Background())
		return err
	}
	// Azurite, unlike MinIO, does NOT auto-create
	// containers on first PUT — we must explicitly create
	// the container before the agent's first write.  Issue
	// a SharedKey-signed PUT against /<account>/<container>
	// ?restype=container.  Errors here best-effort-clean
	// up the container so the caller can retry.
	if err := a.createContainer(ctx); err != nil {
		_ = a.Down(context.Background())
		return fmt.Errorf("azurite sink: create container: %w", err)
	}
	return nil
}

// createContainer issues `PUT /<account>/<container>?restype=container`
// via the azblob SDK.  Using the SDK rather than hand-rolling
// the SharedKey signature: the SDK is already a dep (the
// internal/plugin/storage/azblob plugin imports it), and the
// SigV2 + canonicalised-headers shape is exactly the
// brittle-detail surface that's worth letting Azure's own
// code handle rather than re-implementing.
//
// Idempotent — a 409 ContainerAlreadyExists is treated as
// success so a sink restart against pre-existing state
// works cleanly.
//
// Retry: under concurrent docker load (the `go test ./...`
// shape where 8+ packages spin up containers in parallel),
// Azurite's HTTP listener accepts the TCP connection but
// immediately EOFs the first request or two while still
// finalising its in-process init.  The testcontainers wait
// strategy doesn't catch this because the listener IS up;
// the failure is HTTP-layer.  Retry up to 30 × 500ms = 15s
// with EOF/connection-refused/connection-reset treated as
// transient.  Non-transient errors (auth, malformed url,
// real 4xx) propagate immediately.
func (a *azuriteRuntime) createContainer(ctx context.Context) error {
	serviceURL := fmt.Sprintf("http://127.0.0.1:%d/%s", a.port, azuriteAccount)
	cred, err := az.NewSharedKeyCredential(azuriteAccount, azuriteKey)
	if err != nil {
		return fmt.Errorf("shared-key cred: %w", err)
	}
	// Azurite is HTTP-only by default; the SDK refuses
	// SharedKey over HTTP unless we opt in.  This applies
	// to the testkit's sink, not to operators' production
	// Azure endpoints (which are HTTPS).
	opts := &az.ClientOptions{}
	opts.InsecureAllowCredentialWithHTTP = true
	cli, err := az.NewClientWithSharedKeyCredential(serviceURL, cred, opts)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	const maxAttempts = 30
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := cli.CreateContainer(ctx, azuriteContainer, nil)
		if err == nil {
			return nil
		}
		// 409 ContainerAlreadyExists is the idempotent-
		// success path.
		if strings.Contains(err.Error(), "ContainerAlreadyExists") {
			return nil
		}
		// Transient: connection-layer errors while Azurite
		// is still finishing startup.  Anything else fails
		// fast so a genuine misconfiguration doesn't sit
		// retrying for 15s.
		msg := err.Error()
		transient := strings.Contains(msg, "EOF") ||
			strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "i/o timeout")
		if !transient {
			return fmt.Errorf("CreateContainer: %w", err)
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("CreateContainer after %d transient retries: %w", maxAttempts, lastErr)
}

// Down removes the Azurite container and clears the recorded
// port.  Idempotent.
func (a *azuriteRuntime) Down(ctx context.Context) error {
	if a.container != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", a.container).Run()
		a.container = ""
	}
	a.port = 0
	return nil
}

// URL points at Azurite via the azblob plugin's `endpoint=`
// override.  Three query params worth calling out:
//
//   - endpoint  — Azurite's blob service path
//     (http://host:port/<account>)
//   - allow_http=true — Azurite is HTTP-only by default;
//     the Azure SDK refuses authenticated
//     requests over HTTP unless the plugin
//     opts in via this flag.  Production
//     deployments that hit a real Azure
//     endpoint don't set this — TLS is the
//     only way to keep the SharedKey
//     signature off the wire.
//   - account_key — the SharedKey credential.  Lives in
//     the URL rather than EnvForAgent because
//     the plugin uses the URL's account_key
//     query param directly; AZURE_STORAGE_*
//     env vars are still set in EnvForAgent
//     for callers that prefer that path.
func (a *azuriteRuntime) URL() string {
	return fmt.Sprintf(
		"azblob://%s/%s?endpoint=http://127.0.0.1:%d/%s&allow_http=true&account_key=%s",
		azuriteAccount, azuriteContainer, a.port, azuriteAccount, azuriteKey)
}

// EnvForAgent supplies the Azure SDK credentials via the
// AZURE_STORAGE_ACCOUNT / AZURE_STORAGE_KEY pair (the SDK's
// SharedKey credential reads them by default).  The
// connection string env var is an alternative shape but
// less portable across SDK versions.
func (a *azuriteRuntime) EnvForAgent() map[string]string {
	return map[string]string{
		"AZURE_STORAGE_ACCOUNT": azuriteAccount,
		"AZURE_STORAGE_KEY":     azuriteKey,
	}
}

// ContainerName implements Runtime.
func (a *azuriteRuntime) ContainerName() string { return a.container }

// Extras implements Runtime.  Azure plugin reads creds
// from URL query params + env vars; no Extras needed.
func (a *azuriteRuntime) Extras() map[string]string { return nil }
