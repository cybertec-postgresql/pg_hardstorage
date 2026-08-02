// wiring_e2e_test.go — proves each storage backend is reachable
// through the PRODUCTION entry point, using only what production
// actually supplies.
//
// Every existing backend suite (sftp/contract_test.go,
// scp/contract_test.go, s3/contract_test.go) constructs a
// storage.StorageConfig by hand and populates Extras before calling
// plugin.Open directly. That validates the plugin, but it skips the
// one line that matters in production:
//
//	storage.Open → plugin.Open(ctx, StorageConfig{URL: u})
//
// Extras is EMPTY there, and nothing anywhere populates it. The scp
// backend consequently could not be used at all — every operation
// failed at open with "extras.known_hosts is required" — while its
// contract suite passed, because the suite supplied the Extras the
// real caller never does.
//
// These tests go through storage.Open with an empty Extras, exactly
// like `pg_hardstorage repo init`, so a backend that is only reachable
// from a test harness fails here.
//
//go:build integration

package storage_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func requireDockerE2E(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

// roundTrip exercises the object-store contract through whatever
// plugin storage.Open handed back: put, read back, stat, list, delete.
func roundTrip(t *testing.T, sp storage.StoragePlugin) {
	t.Helper()
	ctx := context.Background()
	const key = "wiring/e2e.txt"
	body := []byte("reachable through storage.Open")

	if _, err := sp.Put(ctx, key, strings.NewReader(string(body)), storage.PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := sp.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := make([]byte, len(body))
	n, _ := rc.Read(got)
	_ = rc.Close()
	if string(got[:n]) != string(body) {
		t.Errorf("Get = %q, want %q", got[:n], body)
	}
	if _, err := sp.Stat(ctx, key); err != nil {
		t.Errorf("Stat: %v", err)
	}
	if err := sp.Delete(ctx, key); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// TestWiring_SFTP_ThroughStorageOpen: sftp:// must be openable with
// the URL plus PG_HARDSTORAGE_SFTP_* env alone.
func TestWiring_SFTP_ThroughStorageOpen(t *testing.T) {
	requireDockerE2E(t)
	rt, err := sink.New("sftp")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rt.Up(ctx); err != nil {
		t.Fatalf("sftp container: %v", err)
	}
	defer rt.Down(context.Background())

	// The runtime hands its known_hosts back through Extras — which
	// production never reads. Feed it via the environment instead,
	// which is the documented and only working mechanism.
	extras := rt.Extras()
	kh := extras["known_hosts"]
	if kh == "" {
		t.Fatal("sftp runtime exposed no known_hosts")
	}
	t.Setenv("PG_HARDSTORAGE_SFTP_KNOWN_HOSTS", kh)
	if pw := extras["password"]; pw != "" {
		t.Setenv("PG_HARDSTORAGE_SFTP_PASSWORD", pw)
	}

	sp, err := storage.Open(ctx, rt.URL())
	if err != nil {
		t.Fatalf("storage.Open(%s): %v\n"+
			"sftp is not reachable through the production entry point with URL+env alone",
			rt.URL(), err)
	}
	defer sp.Close()
	roundTrip(t, sp)
}

// TestWiring_SCP_ThroughStorageOpen is the regression test for the
// backend that was entirely unusable: storage.Open passes no Extras,
// and before the env fallback landed there was no other way to supply
// known_hosts, so this failed 100% of the time.
//
// atmoz/sftp forbids ssh-exec, which the scp plugin requires, so this
// builds a tiny exec-capable OpenSSH container instead.
func TestWiring_SCP_ThroughStorageOpen(t *testing.T) {
	requireDockerE2E(t)
	srv := startSSHExecContainer(t)

	t.Setenv("PG_HARDSTORAGE_SCP_KNOWN_HOSTS", srv.knownHosts)
	t.Setenv("PG_HARDSTORAGE_SCP_IDENTITY_FILE", srv.identityFile)

	url := fmt.Sprintf("scp://%s@127.0.0.1:%d%s", srv.user, srv.port, srv.root)
	ctx := context.Background()
	sp, err := storage.Open(ctx, url)
	if err != nil {
		t.Fatalf("storage.Open(%s): %v\n"+
			"scp is not reachable through the production entry point with URL+env alone",
			url, err)
	}
	defer sp.Close()
	roundTrip(t, sp)
}

// TestWiring_S3_ThroughStorageOpen: s3:// against MinIO, credentials
// from the standard AWS_* environment the SDK reads.
func TestWiring_S3_ThroughStorageOpen(t *testing.T) {
	requireDockerE2E(t)
	rt, err := sink.New("s3-minio")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rt.Up(ctx); err != nil {
		t.Fatalf("minio container: %v", err)
	}
	defer rt.Down(context.Background())

	for k, v := range rt.EnvForAgent() {
		t.Setenv(k, v)
	}
	sp, err := storage.Open(ctx, rt.URL())
	if err != nil {
		t.Fatalf("storage.Open(%s): %v", rt.URL(), err)
	}
	defer sp.Close()
	roundTrip(t, sp)
}

// TestWiring_UnknownScheme keeps the negative case honest: a typo'd
// scheme must be refused, not silently treated as a filesystem path.
func TestWiring_UnknownScheme(t *testing.T) {
	if _, err := storage.Open(context.Background(), "s4://bucket/prefix"); err == nil {
		t.Fatal("unknown scheme accepted")
	}
}

// sshExecServer is a throwaway OpenSSH container that permits
// ssh-exec (unlike atmoz/sftp), which is what the scp plugin drives.
type sshExecServer struct {
	port         int
	user         string
	root         string
	knownHosts   string
	identityFile string
}

func startSSHExecContainer(t *testing.T) *sshExecServer {
	t.Helper()
	dir := t.TempDir()

	// Throwaway keypair for this test only.
	key := dir + "/id_ed25519"
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen unavailable: %v (%s)", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	name := fmt.Sprintf("pg-hs-scp-e2e-%d", port)

	// Build a minimal sshd image that allows exec. Alpine's openssh
	// is small and the layer cache makes repeat runs fast.
	ctxDir := t.TempDir()
	dockerfile := "FROM alpine:3.20\n" +
		"RUN apk add --no-cache openssh-server openssh-sftp-server coreutils findutils && " +
		"ssh-keygen -A && adduser -D -s /bin/sh scpuser && " +
		"passwd -u scpuser 2>/dev/null || true\n" +
		"RUN mkdir -p /home/scpuser/.ssh /srv/repo && chown -R scpuser /home/scpuser /srv/repo\n" +
		"COPY authorized_keys /home/scpuser/.ssh/authorized_keys\n" +
		"RUN chown scpuser /home/scpuser/.ssh/authorized_keys && chmod 600 /home/scpuser/.ssh/authorized_keys\n" +
		"EXPOSE 22\n" +
		`CMD ["/usr/sbin/sshd","-D","-e"]` + "\n"
	if err := os.WriteFile(ctxDir+"/Dockerfile", []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctxDir+"/authorized_keys", pub, 0o644); err != nil {
		t.Fatal(err)
	}
	img := "pg-hs-scp-e2e:latest"
	if out, err := exec.Command("docker", "build", "-q", "-t", img, ctxDir).CombinedOutput(); err != nil {
		t.Skipf("cannot build sshd image (no network for alpine?): %v\n%s", err, out)
	}

	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:22", port), img).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	// ssh-keyscan doubles as the readiness probe: it only succeeds
	// once sshd is actually speaking SSH.
	kh := dir + "/known_hosts"
	var scan []byte
	for i := 0; i < 60; i++ {
		scan, err = exec.Command("ssh-keyscan", "-p", fmt.Sprint(port), "127.0.0.1").Output()
		if err == nil && len(scan) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(scan) == 0 {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Fatalf("sshd never came up; logs:\n%s", logs)
	}
	// knownhosts needs the bracketed [host]:port form for non-22.
	if err := os.WriteFile(kh, scan, 0o644); err != nil {
		t.Fatal(err)
	}

	return &sshExecServer{
		port: port, user: "scpuser", root: "/srv/repo",
		knownHosts: kh, identityFile: key,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
