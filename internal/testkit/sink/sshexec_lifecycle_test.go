// sshexec_lifecycle_test.go — the fixture's own guarantees.
//
// Everything here is about the fixture failing HONESTLY. When it
// silently handed out a container that would reject the caller's key,
// the result was ~20 failures inside the scp contract suite naming the
// plugin, and the real cause took a CI matrix and a local repro to
// find. A fixture that lies costs more than one that breaks.
//
// In-package so the readiness probe and the runtime's own fields are
// reachable; the exported surface cannot express "point a good runtime
// at a bad key".
//
//go:build integration

package sink

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func upSSHExec(t *testing.T) *sshExecRuntime {
	t.Helper()
	rt := newSSHExec()
	if err := rt.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { _ = rt.Down(context.Background()) })
	return rt
}

// containerExists reports whether docker still knows the name.
func containerExists(t *testing.T, name string) bool {
	t.Helper()
	if name == "" {
		return false
	}
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+name+"$").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// TestSSHExec_ProbeRejectsForeignKey is the guarantee that turns a
// fixture fault into a fixture error.
//
// A live container is pointed at a DIFFERENT private key. The probe
// must reject it — that is precisely the state that used to escape
// Up() and reappear as authentication failures attributed to the scp
// plugin.
func TestSSHExec_ProbeRejectsForeignKey(t *testing.T) {
	rt := upSSHExec(t)

	// A key the container has never authorised.
	foreignDir := t.TempDir()
	foreign := filepath.Join(foreignDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", foreign).
		CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v (%s)", err, out)
	}

	// Same container, same known_hosts — only the identity is wrong.
	imposter := &sshExecRuntime{
		container:    rt.container,
		port:         rt.port,
		dir:          foreignDir,
		identityFile: foreign,
		knownHosts:   rt.knownHosts,
	}

	err := imposter.probeAuth(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("probeAuth accepted a key the container never authorised — the probe is " +
			"not actually authenticating, so a mismatched fixture would be handed to the " +
			"caller and fail later as a plugin error")
	}
	if !strings.Contains(err.Error(), "rejected the instance's own key") {
		t.Errorf("probe failure should name the cause; got: %v", err)
	}
}

// TestSSHExec_ProbeAcceptsOwnKey is the control. Without it the test
// above passes just as well against a probe that rejects everything.
func TestSSHExec_ProbeAcceptsOwnKey(t *testing.T) {
	rt := upSSHExec(t)
	if err := rt.probeAuth(context.Background(), 15*time.Second); err != nil {
		t.Fatalf("probeAuth rejected the instance's own key: %v", err)
	}
}

// TestSSHExec_DownIsIdempotentAndCleansUp covers teardown.
//
// Down runs from t.Cleanup, sometimes after a failure path has already
// called it, so a second call must be a no-op rather than an error. And
// what it leaves behind matters on a shared CI runner: a leaked
// container holds its port and a leaked temp dir holds a private key.
func TestSSHExec_DownIsIdempotentAndCleansUp(t *testing.T) {
	rt := newSSHExec()
	if err := rt.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	name, dir := rt.container, rt.dir
	if name == "" || dir == "" {
		t.Fatal("Up did not record its container name and temp dir")
	}
	if !containerExists(t, name) {
		t.Fatalf("container %s is not running after Up", name)
	}

	if err := rt.Down(context.Background()); err != nil {
		t.Fatalf("first Down: %v", err)
	}
	if containerExists(t, name) {
		t.Errorf("container %s survived Down — on a shared runner it keeps its port and "+
			"accumulates across a suite", name)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s survived Down (stat err = %v); it holds the instance's "+
			"private key", dir, err)
	}

	// Second Down: Cleanup may call it after an error path already did.
	if err := rt.Down(context.Background()); err != nil {
		t.Errorf("second Down returned %v; teardown must be idempotent", err)
	}
}

// TestSSHExec_UpTwiceIsRefused pins that a second Up on a live runtime
// fails instead of overwriting the fields that identify the running
// container — which would leak it beyond any Down's reach.
func TestSSHExec_UpTwiceIsRefused(t *testing.T) {
	rt := upSSHExec(t)
	name, port := rt.container, rt.port

	if err := rt.Up(context.Background()); err == nil {
		t.Fatal("a second Up on a live runtime succeeded; the first container's name would " +
			"be overwritten and nothing could remove it")
	}
	if rt.container != name || rt.port != port {
		t.Errorf("the refused Up mutated the runtime (container %q→%q, port %d→%d); the "+
			"original container is now unreachable by Down", name, rt.container, port, rt.port)
	}
}

// TestSSHExec_EnvAndExtrasAgree pins that the two configuration
// channels describe the same instance.
//
// Callers pick one or the other — EnvForAgent for anything driven
// through storage.Open, Extras for direct plugin.Open — and a
// divergence would make a test pass through one path and fail through
// the other for reasons having nothing to do with the code under test.
func TestSSHExec_EnvAndExtrasAgree(t *testing.T) {
	rt := upSSHExec(t)
	env, extras := rt.EnvForAgent(), rt.Extras()

	for _, p := range []struct{ envKey, extrasKey string }{
		{"PG_HARDSTORAGE_SCP_KNOWN_HOSTS", "known_hosts"},
		{"PG_HARDSTORAGE_SCP_IDENTITY_FILE", "identity_file"},
	} {
		if env[p.envKey] == "" {
			t.Errorf("EnvForAgent has no %s", p.envKey)
		}
		if env[p.envKey] != extras[p.extrasKey] {
			t.Errorf("%s = %q but extras[%q] = %q — the two channels describe different "+
				"instances", p.envKey, env[p.envKey], p.extrasKey, extras[p.extrasKey])
		}
	}
	// Both must point at files that exist, or the failure surfaces
	// inside the plugin as a config error.
	for k, v := range extras {
		if _, err := os.Stat(v); err != nil {
			t.Errorf("extras[%q] = %q does not exist: %v", k, v, err)
		}
	}
}
