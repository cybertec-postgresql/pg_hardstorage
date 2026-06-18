// Package sink — TLS-MinIO runtime.
//
// Boots a single-node MinIO with a self-signed TLS certificate
// so the testkit can exercise the agent's S3 storage plugin
// over HTTPS — and, more importantly, exercise the compat
// shims (pg-hardstorage-pgbackrest / pg-hardstorage-barman)
// against an S3 endpoint that requires TLS.  Unit tests stub
// HTTP transports; only a real container with a real
// certificate catches the cert-trust class of regressions
// (operator hits MinIO with the AWS SDK; SDK rejects
// untrusted issuer; archive_command fails silently if any
// shim/native error path is broken — the very pattern the
// L2 compat scenarios are designed to surface).
//
// The certificate is generated at Up() time:
//
//   - Per-instance ED25519 keypair (cheap, deterministic across
//     runs only in seed; we don't pin a CA — every Up gets a
//     fresh one).
//   - SANs: 127.0.0.1 + localhost.  No DNS magic needed; the
//     URL the runtime emits points straight at 127.0.0.1.
//   - Validity: 24 hours.  Each test run gets a fresh cert;
//     longer windows would be wasted.
//   - Persisted to a per-instance host tempdir as
//     `<dir>/public.crt` + `<dir>/private.key`, then bind-
//     mounted into MinIO at /root/.minio/certs.
//
// Trust is plumbed via AWS_CA_BUNDLE in EnvForAgent and via
// the Extras map (key "ca_bundle"), so the testkit's compat
// runner can also bind-mount the cert file into the in-
// container shim and export AWS_CA_BUNDLE there.

package sink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

type tlsMinioRuntime struct {
	bucket    string
	accessKey string
	secretKey string

	container string
	dataDir   string
	certDir   string
	caBundle  string // absolute path to public.crt for AWS_CA_BUNDLE
	port      int
}

var tlsMinioCounter atomic.Uint64

func newTLSMinIO() *tlsMinioRuntime {
	return &tlsMinioRuntime{
		bucket:    "testkit",
		accessKey: "testkit",
		secretKey: "testkitsecret",
	}
}

// Name returns "tls-minio".
func (m *tlsMinioRuntime) Name() string { return "tls-minio" }

// Up generates a fresh 24-hour ED25519 self-signed cert under
// a per-instance tempdir, runs MinIO with TLS enabled
// (bind-mounting the cert at /root/.minio/certs), waits until
// the HTTPS port returns a valid TLS handshake, and remembers
// the CA bundle path for AWS_CA_BUNDLE plumbing.
func (m *tlsMinioRuntime) Up(ctx context.Context) error {
	if m.container != "" {
		return errors.New("tlsMinioRuntime: already up (call Down first)")
	}

	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("tls-minio sink: pick port: %w", err)
	}
	m.port = port

	dir, err := os.MkdirTemp("", "pg-hs-tls-minio-*")
	if err != nil {
		return fmt.Errorf("tls-minio sink: tempdir: %w", err)
	}
	m.dataDir = filepath.Join(dir, "data")
	m.certDir = filepath.Join(dir, "certs")
	if err := os.MkdirAll(filepath.Join(m.dataDir, m.bucket), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("tls-minio sink: mkdir bucket: %w", err)
	}
	if err := os.MkdirAll(m.certDir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("tls-minio sink: mkdir certs: %w", err)
	}

	// Generate a self-signed cert + key for 127.0.0.1 / localhost.
	// MinIO reads /root/.minio/certs/public.crt + private.key —
	// other paths are ignored, so the filenames are fixed.
	if err := writeSelfSignedCert(m.certDir); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("tls-minio sink: cert: %w", err)
	}
	m.caBundle = filepath.Join(m.certDir, "public.crt")

	// See minio.go's comment on the equivalent name composition:
	// UnixNano + Getpid + counter avoids container-name collisions
	// when run_compat_testing.sh --parallel N forks disjoint
	// testkit processes that each restart the counter at 1.
	m.container = fmt.Sprintf("pg-hs-tls-minio-%d-%d-%d",
		time.Now().UnixNano(), os.Getpid(), tlsMinioCounter.Add(1))

	// `:z` on data + cert bind-mounts: SELinux-enforcing hosts
	// (Fedora, RHEL, Alma, Rocky) deny the MinIO container's
	// writes to /data and reads of /root/.minio/certs when the
	// host directories carry a non-container SELinux label.
	// Without this, the container starts but TLS handshake
	// fails (cert not readable inside) and writes silently
	// drop, the readiness probe times out after 60s, and the
	// scenario fails as `did not become TLS-ready within ...`.
	// The flag is a no-op on systems without SELinux.
	args := []string{
		"run", "-d",
		"--name", m.container,
		// MinIO recommends nofile≥65536; without an explicit ulimit
		// the container inherits the daemon default (1024 soft on
		// many distros) and TLS init fails as
		// "FATAL Unable to load the TLS configuration: too many
		// open files" when a campaign launches many cells in
		// parallel.  We set hard==soft so MinIO sees the high cap
		// without needing to raise its own soft limit at startup.
		"--ulimit", "nofile=65536:65536",
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", m.port),
		"-v", fmt.Sprintf("%s:/data:z", m.dataDir),
		"-v", fmt.Sprintf("%s:/root/.minio/certs:ro,z", m.certDir),
		"-e", "MINIO_ROOT_USER=" + m.accessKey,
		"-e", "MINIO_ROOT_PASSWORD=" + m.secretKey,
		SinkImages["s3-minio"], // same MinIO image; TLS triggers on cert presence
		"server", "/data",
	}
	cmd := execCommand(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		m.dataDir, m.certDir, m.caBundle = "", "", ""
		container := m.container
		m.container = ""
		return fmt.Errorf("tls-minio sink: docker run %s: %w (output: %s)",
			container, err, truncate(out, 256))
	}

	// MinIO TLS readiness: poll /minio/health/ready over HTTPS
	// with cert verification disabled (we want server-up, not
	// trust validation — that's what the AWS SDK exercises
	// later through AWS_CA_BUNDLE).  TCP-only readiness was
	// previously enough on fast distros, but on slower base
	// images (opensuse/leap:15) the TCP listener is up before
	// the TLS layer is, and the FIRST AWS SDK request after
	// our return raced into a "connection reset by peer"
	// during the TLS handshake.  Polling the HTTPS endpoint
	// directly closes that gap.
	// 120s budget.  TLS handshake adds startup overhead vs the
	// plain MinIO sink; concurrent test campaigns (compat +
	// soak + scenario-sweep + k8s minikube all together)
	// observed in 2026-05-09 surface bumped past the prior 60s
	// cap on opensuse-leap15 + debian-13 cells.  120s gives a
	// 4× margin over the typical 30s warm-host bring-up while
	// still catching a real hang well within human patience.
	//
	// On timeout, we also include the container's docker logs
	// in the error so the operator can see WHY MinIO didn't
	// start — the prior message ("did not become TLS-ready
	// within 1m0s") forced a separate `docker logs` invocation
	// to diagnose, which is friction the operator pays every
	// time the readiness check fails.
	if err := waitTLSReady(ctx, m.port, 120*time.Second); err != nil {
		dockerLogs := captureDockerLogs(m.container)
		_ = m.Down(context.Background())
		if dockerLogs != "" {
			return fmt.Errorf("%w (container logs tail: %s)", err, dockerLogs)
		}
		return err
	}
	return nil
}

// captureDockerLogs pulls the last ~2 KiB of stdout+stderr
// from the named container — best-effort diagnostic for
// readiness-timeout paths.  Returns an empty string on any
// docker error so the calling error path never gets blocked
// on a docker daemon issue.
func captureDockerLogs(container string) string {
	if container == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", "30", container)
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) == 0 {
		return ""
	}
	return truncate(out, 2048)
}

// waitTLSReady polls MinIO on https://127.0.0.1:<port> until both
// (1) /minio/health/ready returns 200 — TLS handshake + HTTP server
// running — and (2) a HEAD on the bucket URL returns anything other
// than 503 — storage layer initialised.  Returns nil when both
// stages succeed.
//
// Stage 2 closes the gap that surfaced as
// "XMinioServerNotInitialized: Server not initialized yet, please
// try again" on the FIRST S3 PutObject after sink_up: /health/ready
// only checks the HTTP server, not the storage backend; under
// campaign load (parallel cells launching minio concurrently) the
// gap between ready=200 and storage-ready can stretch into hundreds
// of ms, long enough for the AWS SDK's 3-attempt retry to exhaust
// itself with 503s. Same shape as minio.go's waitReady so both
// runtimes converge on the same readiness contract.
//
// Uses InsecureSkipVerify on the TLS config because we're polling
// a brand-new self-signed cert that no system trust store has —
// the AWS SDK's later requests do verify via AWS_CA_BUNDLE, so this
// short-circuit is bounded to the readiness probe.
func waitTLSReady(ctx context.Context, port int, total time.Duration) error {
	deadline := time.Now().Add(total)
	healthURL := fmt.Sprintf("https://127.0.0.1:%d/minio/health/ready", port)
	bucketURL := fmt.Sprintf("https://127.0.0.1:%d/testkit/", port)
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	transport := &http.Transport{
		TLSClientConfig:   tlsCfg,
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Stage 1: liveness — HTTPS server + health endpoint up.
		hresp, err := client.Get(healthURL)
		if err != nil || hresp.StatusCode != http.StatusOK {
			if hresp != nil {
				_ = hresp.Body.Close()
			}
			goto sleep
		}
		_ = hresp.Body.Close()
		// Stage 2: real S3-shaped op against the bucket URL.  503
		// means storage/IAM not initialised yet — keep polling.
		// 404 (NoSuchBucket) is fine: auth+path resolved, only the
		// bucket doesn't exist. 200 / 403 are also acceptable.
		{
			req, _ := http.NewRequestWithContext(ctx, "HEAD", bucketURL, nil)
			bresp, berr := client.Do(req)
			if berr == nil {
				code := bresp.StatusCode
				_ = bresp.Body.Close()
				if code != http.StatusServiceUnavailable {
					return nil
				}
			}
		}
	sleep:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("tls-minio sink: 127.0.0.1:%d did not become TLS-ready within %s",
		port, total)
}

// Down removes the container, nukes the parent tempdir (data
// + certs share it), and clears the recorded port.
// Idempotent.
func (m *tlsMinioRuntime) Down(ctx context.Context) error {
	if m.container != "" {
		_ = execCommand(ctx, "docker", "rm", "-f", m.container).Run()
		m.container = ""
	}
	if m.dataDir != "" {
		// Both dataDir and certDir live under the same parent
		// tempdir; nuking the parent reclaims everything.
		_ = os.RemoveAll(filepath.Dir(m.dataDir))
		m.dataDir, m.certDir, m.caBundle = "", "", ""
	}
	m.port = 0
	return nil
}

// URL emits the same query-param shape as the plain MinIO
// runtime, but with https:// — the agent's S3 plugin reads
// `endpoint=...` and feeds it to the AWS SDK as BaseEndpoint;
// the SDK upgrades to TLS automatically when the scheme is
// https.
func (m *tlsMinioRuntime) URL() string {
	return fmt.Sprintf("s3://%s?endpoint=https://127.0.0.1:%d&path_style=true&region=us-east-1",
		m.bucket, m.port)
}

// EnvForAgent returns the access/secret pair plus
// AWS_CA_BUNDLE pointing at this instance's public.crt so the
// AWS SDK trusts the self-signed cert.
func (m *tlsMinioRuntime) EnvForAgent() map[string]string {
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     m.accessKey,
		"AWS_SECRET_ACCESS_KEY": m.secretKey,
		"AWS_REGION":            "us-east-1",
		// AWS SDK v2 honours AWS_CA_BUNDLE for the entire
		// process — the testkit's S3 storage plugin gets it
		// for free, and we plumb the same path into shim
		// containers via Extras["ca_bundle"].
		"AWS_CA_BUNDLE": m.caBundle,
	}
}

// ContainerName returns the docker container name this
// instance is running in, or "" before Up.
func (m *tlsMinioRuntime) ContainerName() string { return m.container }

// Extras carries the CA-bundle host path so the compat-scenario
// runner can bind-mount the SAME file into the in-container
// shim and export AWS_CA_BUNDLE there.  Without this, the
// shim's child AWS SDK would reject the self-signed cert.
func (m *tlsMinioRuntime) Extras() map[string]string {
	return map[string]string{"ca_bundle": m.caBundle}
}

// writeSelfSignedCert generates an ED25519 keypair + a
// self-signed X.509 cert valid for 24 h with SANs covering
// 127.0.0.1 / localhost / ::1, and writes them to
// <dir>/public.crt + <dir>/private.key in PEM form (the
// shapes MinIO expects under /root/.minio/certs).
//
// ED25519 chosen over RSA: smaller (32-byte key), faster to
// generate, and the SDK + Go stdlib both accept it.  No
// security argument either way for a 24-hour test cert.
func writeSelfSignedCert(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("ed25519 keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}
	notBefore := time.Now().Add(-1 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pg-hardstorage-testkit-tls-minio"},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{
			net.IPv4(127, 0, 0, 1), net.IPv6loopback,
		},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// Marshal the private key in PKCS#8 form — the only
	// encoding x509.MarshalPKCS8PrivateKey supports for ED25519.
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal pkcs8: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// MinIO requires 0644 on public.crt + 0600 on private.key
	// (it refuses to start otherwise) — and rejects 0640 on
	// the key with "tls: failed to find any PEM data in key
	// input".  Tightest allowed mode wins.
	if err := os.WriteFile(filepath.Join(dir, "public.crt"), certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private.key"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

// guard against unused imports if the build prunes things.
var _ = exec.CommandContext
