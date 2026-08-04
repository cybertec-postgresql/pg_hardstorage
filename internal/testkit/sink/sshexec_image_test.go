package sink

// sshexec_image_test.go — the ssh-exec image tag must be a pure
// function of what defines the image.
//
// This is load-bearing, not cosmetic. The tag used to be a fixed
// string with each instance's public key baked into the image, so two
// packages building concurrently re-pointed it under each other and
// whichever lost saw every operation fail with "unable to authenticate
// [none publickey]" — attributed to the scp plugin, not the fixture.
// Content-addressing the tag is what removed that race: instances that
// would produce identical images converge on one tag, so no instance
// can re-point a tag another is about to run.
//
// These run without docker: the tag is derived from strings.

import (
	"strings"
	"testing"
)

// TestSSHExecImageTag_IsDeterministic pins the property the race fix
// depends on. Two instances computing the tag from the same definition
// must agree; if they did not, they would build separate images under
// separate tags and the cache would never hit.
func TestSSHExecImageTag_IsDeterministic(t *testing.T) {
	df := sshExecDockerfile()
	a := sshExecImageTag(df, sshExecEntrypoint)
	b := sshExecImageTag(df, sshExecEntrypoint)
	if a != b {
		t.Fatalf("tag is not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "pg-hardstorage-testkit-sshexec:") {
		t.Errorf("tag %q lost its repository prefix", a)
	}
	if tagPart := a[strings.LastIndex(a, ":")+1:]; len(tagPart) != 12 {
		t.Errorf("tag suffix %q is %d chars, want 12 hex", tagPart, len(tagPart))
	}
}

// TestSSHExecImageTag_TracksBothInputs is the one that would actually
// catch a mistake.
//
// The entrypoint is a SEPARATE file in the build context, so editing
// it does not change the Dockerfile text. Hash only the Dockerfile —
// the obvious thing to write — and an entrypoint change silently
// reuses the previous image: docker finds the tag present, and the
// fixture runs yesterday's script while the source says otherwise.
func TestSSHExecImageTag_TracksBothInputs(t *testing.T) {
	df := sshExecDockerfile()
	base := sshExecImageTag(df, sshExecEntrypoint)

	if got := sshExecImageTag(df+"\nRUN apk add --no-cache curl\n", sshExecEntrypoint); got == base {
		t.Error("editing the Dockerfile did not change the tag; a build would reuse the " +
			"stale image")
	}
	if got := sshExecImageTag(df, sshExecEntrypoint+"\necho changed\n"); got == base {
		t.Error("editing the ENTRYPOINT did not change the tag — it is a separate file in " +
			"the build context, so hashing the Dockerfile alone would leave the fixture " +
			"running a script that no longer matches the source")
	}
}

// TestSSHExecEntrypoint_InstallsKeyBeforeSshd pins the ordering the
// readiness probe relies on.
//
// The container answers SSH only once sshd starts, so if the key is
// installed FIRST then "answers SSH" implies "will accept this
// instance's key". Reverse the two — exec sshd, then write the key —
// and a connection can arrive in the gap and be rejected, which is
// exactly the intermittent failure the fixture was rewritten to
// eliminate.
func TestSSHExecEntrypoint_InstallsKeyBeforeSshd(t *testing.T) {
	e := sshExecEntrypoint
	keyIdx := strings.Index(e, "authorized_keys")
	sshdIdx := strings.Index(e, "exec /usr/sbin/sshd")
	if keyIdx < 0 || sshdIdx < 0 {
		t.Fatalf("entrypoint no longer installs a key and execs sshd:\n%s", e)
	}
	if keyIdx > sshdIdx {
		t.Error("the entrypoint execs sshd before installing authorized_keys; a client " +
			"connecting in that window is rejected, and readiness (which only proves sshd " +
			"answers) would report the container usable")
	}
	// The image must carry no key of its own — that was the original bug.
	if strings.Contains(sshExecDockerfile(), "authorized_keys") {
		t.Error("the Dockerfile references authorized_keys: per-instance key material must " +
			"not be baked into an image whose tag is shared between instances")
	}
	// An empty variable must fail loudly rather than start an sshd
	// that authorises nobody and times out at the probe instead.
	if !strings.Contains(e, sshExecAuthKeyEnv) || !strings.Contains(e, "exit 1") {
		t.Error("the entrypoint does not refuse an empty " + sshExecAuthKeyEnv)
	}
}
