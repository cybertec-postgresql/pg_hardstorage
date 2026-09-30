package inject

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeDocker(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// `docker cp CONTAINER:PATH -` writes a TAR stream, not the file. CopyOut
// returned that stream as the file's bytes, so flip_random_byte flipped a
// byte somewhere in a tar header or padding and wrote the whole archive
// back as the "file".
func TestDockerTargetCopyOutUnwrapsTar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chunk"), []byte("chunk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &DockerTarget{Container: "c", DockerBin: writeFakeDocker(t,
		`[ "$1" = cp ] && exec tar -cf - -C `+dir+` chunk
exit 1
`)}
	got, err := d.CopyOut(context.Background(), "/repo/chunks/chunk")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "chunk-bytes" {
		t.Fatalf("CopyOut = %q (%d bytes), want the file's content", truncate(got, 64), len(got))
	}
}

// `docker cp - CONTAINER:DIR` reads a TAR stream and extracts it into
// DIR. CopyIn piped the raw bytes, which docker rejects (or, for a body
// that happens to parse, unpacks garbage), so libfaketime's
// /etc/faketimerc was never written.
func TestDockerTargetCopyInWrapsTar(t *testing.T) {
	dir := t.TempDir()
	stdin := filepath.Join(dir, "stdin")
	args := filepath.Join(dir, "args")
	d := &DockerTarget{Container: "c", DockerBin: writeFakeDocker(t, `case "$1" in
  exec) echo "0 0 644"; exit 0 ;;
  cp) echo "$*" > `+args+`; cat > `+stdin+`; exit 0 ;;
esac
exit 1
`)}
	if err := d.CopyIn(context.Background(), "/etc/faketimerc", []byte("+2d\n")); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(args)
	if !strings.Contains(string(a), "c:/etc") || strings.Contains(string(a), "c:/etc/faketimerc") {
		t.Errorf("docker cp must target the parent directory; args: %s", a)
	}
	body, _ := os.ReadFile(stdin)
	tr := tar.NewReader(bytes.NewReader(body))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("CopyIn did not send a tar stream: %v (%q)", err, truncate(body, 64))
	}
	content, _ := io.ReadAll(tr)
	if hdr.Name != "faketimerc" || string(content) != "+2d\n" {
		t.Errorf("tar entry %q = %q, want faketimerc = %q", hdr.Name, content, "+2d\n")
	}
}
