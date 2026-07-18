// dispatch.go — DispatchClient: operator-side HTTP client for control-plane enqueue+poll.
package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// DispatchClient is a tiny HTTP client for the operator-side control-
// plane interactions: enqueue a job, poll for completion, stream
// progress events through the local dispatcher so the operator's
// terminal looks the same as a direct local invocation.
//
// Why a fresh client and not re-use internal/agent's ControlPlaneClient?
// The agent client is the long-lived heartbeat-and-claim loop on the
// agent host; this is the short-lived enqueue-and-poll path on the
// operator's host. Different lifecycles, different concerns. Sharing
// the bearer-token + TLS configuration logic with the agent client
// is fine, but the request/poll loop is bespoke.
type DispatchClient struct {
	BaseURL string
	Token   string

	// CAFile pins the control plane's TLS server certificate. When
	// empty, system roots are used; mTLS-enabled control planes
	// require the operator to also configure ClientCert/ClientKey
	// via flag.
	CAFile     string
	ClientCert string
	ClientKey  string

	// PollInterval is the cadence at which we GET /v1/jobs/<id>.
	// Default 1s — fast enough that operator UX feels live, slow
	// enough that a 1000-deployment fleet doesn't melt the control
	// plane.
	PollInterval time.Duration

	// HTTPClient lets tests inject a custom transport. When nil, we
	// build one from CAFile / ClientCert / ClientKey at first use.
	HTTPClient *http.Client
}

// EnqueueRestore POSTs the body to /v1/deployments/<deployment>/restores
// and returns the resulting Job ID. Body fields ride into Job.Args
// — see internal/server/routes.go:handleEnqueueRestore for the
// schema.
func (c *DispatchClient) EnqueueRestore(ctx context.Context, deployment string, body map[string]any) (string, error) {
	return c.enqueue(ctx, fmt.Sprintf("/v1/deployments/%s/restores", deployment), body)
}

// EnqueueBackup POSTs to /v1/deployments/<deployment>/backups.
// Symmetric to EnqueueRestore; the+ backup --control-plane mode
// rides this path.
func (c *DispatchClient) EnqueueBackup(ctx context.Context, deployment string, body map[string]any) (string, error) {
	return c.enqueue(ctx, fmt.Sprintf("/v1/deployments/%s/backups", deployment), body)
}

// EnqueueVerify POSTs to /v1/deployments/<deployment>/verifies.
// Used by `pg_hardstorage verify --control-plane <url>`. The agent's
// VerifyExecutor performs the full restore-to-sandbox + pg_verifybackup
// loop on its own host; the operator's machine doesn't need Docker.
func (c *DispatchClient) EnqueueVerify(ctx context.Context, deployment string, body map[string]any) (string, error) {
	return c.enqueue(ctx, fmt.Sprintf("/v1/deployments/%s/verifies", deployment), body)
}

// enqueue is the shared POST shape: marshal body → POST → expect 202
// with envelope.result.id.
func (c *DispatchClient) enqueue(ctx context.Context, path string, body map[string]any) (string, error) {
	if err := c.ensureClient(); err != nil {
		return "", err
	}
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("dispatch: marshal body: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("dispatch: build request: %w", err)
	}
	if len(bodyBytes) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dispatch: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("dispatch: POST %s: status=%d body=%s",
			path, resp.StatusCode, raw)
	}
	var env struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("dispatch: decode response: %w (body=%s)", err, raw)
	}
	if env.Error != nil {
		return "", fmt.Errorf("dispatch: server error %s: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result.ID == "" {
		return "", fmt.Errorf("dispatch: no job ID in response: %s", raw)
	}
	return env.Result.ID, nil
}

// PolledJob mirrors the server-side server.Job shape — duplicated
// here so the CLI doesn't depend on internal/server (control plane
// might be a different version than the operator's binary).
type PolledJob struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Deployment  string         `json:"deployment"`
	State       string         `json:"state"`
	AssignedTo  string         `json:"assigned_to,omitempty"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Result      map[string]any `json:"result,omitempty"`
	Failure     string         `json:"failure,omitempty"`
	Progress    []ProgressEvt  `json:"progress,omitempty"`
}

// ProgressEvt mirrors server.ProgressEvent.
type ProgressEvt struct {
	At   time.Time      `json:"at"`
	Op   string         `json:"op,omitempty"`
	Body map[string]any `json:"body,omitempty"`
}

// PollUntilTerminal polls /v1/jobs/<id> until the state is one of
// completed/failed/cancelled, forwarding new progress events to
// onProgress so the operator's terminal sees them as they happen.
//
// onProgress is called with each event the operator hasn't seen yet
// (de-duplicated by index, since /jobs/<id> returns the cumulative
// slice on every poll). Pass nil to skip event forwarding.
func (c *DispatchClient) PollUntilTerminal(ctx context.Context, id string, onProgress func(ProgressEvt)) (*PolledJob, error) {
	if err := c.ensureClient(); err != nil {
		return nil, err
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	emitted := 0
	t := time.NewTicker(c.PollInterval)
	defer t.Stop()
	for {
		j, err := c.getJob(ctx, id)
		if err != nil {
			return nil, err
		}
		// Forward any new progress events the caller hasn't seen.
		if onProgress != nil {
			for i := emitted; i < len(j.Progress); i++ {
				onProgress(j.Progress[i])
			}
		}
		emitted = len(j.Progress)
		switch j.State {
		case "completed", "failed", "cancelled":
			return j, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// getJob is the shared GET /v1/jobs/<id> helper.
func (c *DispatchClient) getJob(ctx context.Context, id string) (*PolledJob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/jobs/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("dispatch: build request: %w", err)
	}
	c.applyAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch: GET /v1/jobs/%s: %w", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dispatch: GET /v1/jobs/%s: status=%d body=%s",
			id, resp.StatusCode, raw)
	}
	var env struct {
		Result *PolledJob `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("dispatch: decode job: %w (body=%s)", err, raw)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("dispatch: empty job response: %s", raw)
	}
	return env.Result, nil
}

// applyAuth adds the bearer token if configured.
func (c *DispatchClient) applyAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// ensureClient initialises HTTPClient if the caller hasn't supplied
// one. Sets up TLS pinning + mTLS client cert per the configured
// CAFile / ClientCert / ClientKey.
func (c *DispatchClient) ensureClient() error {
	if c.HTTPClient != nil {
		return nil
	}
	if c.BaseURL == "" {
		return errors.New("dispatch: BaseURL is required")
	}
	tlsCfg, err := c.buildTLSConfig()
	if err != nil {
		return err
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsCfg,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	c.HTTPClient = &http.Client{
		Transport: transport,
		Timeout:   2 * time.Minute,
	}
	return nil
}

func (c *DispatchClient) buildTLSConfig() (*tls.Config, error) {
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		body, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("dispatch: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(body) {
			return nil, errors.New("dispatch: no usable certs in CA file")
		}
		cfg.RootCAs = pool
	}
	if c.ClientCert != "" || c.ClientKey != "" {
		if c.ClientCert == "" || c.ClientKey == "" {
			return nil, errors.New("dispatch: --client-cert and --client-key must both be set for mTLS")
		}
		cert, err := tls.LoadX509KeyPair(c.ClientCert, c.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("dispatch: load client cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// dispatchAuthFlags is the shared flag bundle for any CLI command
// that wants to enqueue against a control plane. Embed it on the
// command's options struct, register via registerDispatchFlags, and
// build a DispatchClient via newDispatchClient when the
// --control-plane URL is set.
type dispatchAuthFlags struct {
	controlPlane     string
	tokenFile        string
	caFile           string
	clientCertFile   string
	clientKeyFile    string
	pollIntervalSecs int
}

// registerDispatchFlags attaches the --control-plane / TLS / token
// flags to a cobra command. Callers add their command-specific flags
// alongside.
func registerDispatchFlags(c *cobra.Command, f *dispatchAuthFlags) {
	c.Flags().StringVar(&f.controlPlane, "control-plane", "",
		"control-plane base URL — when set, the command is dispatched there instead of running locally")
	c.Flags().StringVar(&f.tokenFile, "control-plane-token-file", "",
		"file containing the bearer token for the control plane")
	c.Flags().StringVar(&f.caFile, "control-plane-ca", "",
		"PEM file pinning the control plane's TLS server certificate")
	c.Flags().StringVar(&f.clientCertFile, "control-plane-client-cert", "",
		"client certificate for mTLS to the control plane")
	c.Flags().StringVar(&f.clientKeyFile, "control-plane-client-key", "",
		"client key for mTLS to the control plane")
	c.Flags().IntVar(&f.pollIntervalSecs, "control-plane-poll-secs", 1,
		"poll cadence (seconds) for /v1/jobs/<id>")
}

// rejectChangedDispatchFlags prevents a control-plane invocation from
// accepting command flags that the remote job protocol cannot represent.
// Silently dropping one of these flags can make an agent execute different
// work from what the operator requested.
func rejectChangedDispatchFlags(cmd *cobra.Command, operation string, names ...string) error {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return output.NewError("usage.unsupported_flag",
				fmt.Sprintf("%s --control-plane: --%s is not supported by remote dispatch; remove the flag or run the command locally", operation, name)).
				Wrap(output.ErrUsage)
		}
	}
	return nil
}

// newDispatchClient builds a *DispatchClient from the parsed flags.
// Returns nil + structured error if the control-plane URL is set but
// some required piece (e.g. token file unreadable) can't be loaded.
//
// Caller is expected to have already checked f.controlPlane != ""
// — this helper is unconditional.
func newDispatchClient(f *dispatchAuthFlags) (*DispatchClient, error) {
	c := &DispatchClient{
		BaseURL:      strings.TrimRight(f.controlPlane, "/"),
		CAFile:       f.caFile,
		ClientCert:   f.clientCertFile,
		ClientKey:    f.clientKeyFile,
		PollInterval: time.Duration(f.pollIntervalSecs) * time.Second,
	}
	if f.tokenFile != "" {
		body, err := os.ReadFile(f.tokenFile)
		if err != nil {
			return nil, output.NewError("config.bad_token_file",
				fmt.Sprintf("dispatch: read --control-plane-token-file: %v", err)).Wrap(err)
		}
		c.Token = strings.TrimSpace(string(body))
	}
	return c, nil
}
