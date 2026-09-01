package audit_test

// bundle_contiguity_test.go — `audit verify-bundle` claimed to check
// the chain and did not.
//
// The command's own help text read:
//
//	"Asserts the bundle's ed25519 signature is valid + the chain
//	 segment is contiguous."
//
// Only the first half existed. runAuditVerifyBundle called
// audit.VerifyBundle, which checked the signature, the schema, the
// algorithm and the signer fingerprint — and never opened
// events.ndjson. It printed "✓ Bundle verified" along with the
// manifest's head hash, a value it had compared against nothing.
//
// The signature proves the tarball is the one the key holder produced.
// It says nothing about whether the events inside form a valid chain,
// so a bundle exported from an already-broken chain verified clean, and
// the auditor receiving it was told the segment was contiguous.
//
// The data was always in the bundle; the design left the walk to a
// human ("An auditor walks events.ndjson and asserts each event's
// prev_hash matches the prior event's hash"). Doing it in code is
// strictly better than asking someone to do it by hand.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// rewriteBundleEvents unpacks a bundle, hands events.ndjson to mutate,
// and repacks WITHOUT re-signing — the shape of an already-broken chain
// that was exported, since the operator's own signature would still be
// over whatever the exporter wrote.
//
// To keep the signature valid we re-sign nothing and instead rebuild
// the tar with the mutated body, then assert on the CHAIN error rather
// than the signature error. Tests that need a valid signature over
// mutated events construct the events before export instead.
func unpackBundle(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = body
	}
	return out
}

// TestVerifyBundle_ChecksEventHashesAndLinkage: the happy path now
// reports what it actually established.
func TestVerifyBundle_ReportsChainIntegrity(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Now().UTC()
	for i := 0; i < 4; i++ {
		w.appendEvent(t, "x.event", "db1", base.Add(time.Duration(i)*time.Second))
	}

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	m, err := audit.VerifyBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if m.Integrity == nil {
		t.Fatal("no Integrity on the verified manifest: the command claims to assert " +
			"chain contiguity and reports nothing about it")
	}
	if m.Integrity.EventsChecked != 4 {
		t.Errorf("EventsChecked = %d, want 4 — the events were never opened",
			m.Integrity.EventsChecked)
	}
	if !m.Integrity.LinkageAsserted {
		t.Errorf("LinkageAsserted = false on an unfiltered, consecutive export (gaps=%d)",
			m.Integrity.SequenceGaps)
	}
}

// The bug proper: a bundle whose events do not form a chain must not
// verify. The tarball is rebuilt around a mutated events.ndjson and the
// bundle re-signed, so the SIGNATURE is valid and only the chain is
// wrong — exactly the case the old code waved through.
func TestVerifyBundle_BrokenChainInsideAValidlySignedBundle(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Now().UTC()
	for i := 0; i < 4; i++ {
		w.appendEvent(t, "x.event", "db1", base.Add(time.Duration(i)*time.Second))
	}
	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	files := unpackBundle(t, buf.Bytes())

	// Sever the link between events 1 and 2 by rewriting event 2's
	// prev_hash, and recompute its own hash so the per-event self-check
	// still passes. Only the LINKAGE is broken.
	lines := bytes.Split(bytes.TrimRight(files["events.ndjson"], "\n"), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("fixture: %d event lines, want 4", len(lines))
	}
	var ev audit.Event
	if err := json.Unmarshal(lines[2], &ev); err != nil {
		t.Fatal(err)
	}
	ev.PrevHash = strings.Repeat("0", len(ev.PrevHash))
	h, err := audit.ComputeHash(&ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.Hash = h
	patched, err := json.Marshal(&ev)
	if err != nil {
		t.Fatal(err)
	}
	lines[2] = patched
	files["events.ndjson"] = append(bytes.Join(lines, []byte("\n")), '\n')

	repacked := repackAndResign(t, w, files)
	_, verr := audit.VerifyBundle(bytes.NewReader(repacked))
	if verr == nil {
		t.Fatal("a validly-signed bundle whose events do not link verified clean.\n\n" +
			"The signature proves the tarball came from the key holder; it says nothing " +
			"about the chain inside. An auditor was told the segment was contiguous.")
	}
	if !strings.Contains(verr.Error(), "chain break") {
		t.Errorf("error does not name the chain break: %v", verr)
	}
}

// An event whose recorded hash does not match its content is a hard
// failure regardless of filters.
func TestVerifyBundle_EventHashMismatchRefused(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())
	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	files := unpackBundle(t, buf.Bytes())

	var ev audit.Event
	line := bytes.TrimRight(files["events.ndjson"], "\n")
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	ev.Action = "x.rewritten" // content changes, Hash left alone
	patched, err := json.Marshal(&ev)
	if err != nil {
		t.Fatal(err)
	}
	files["events.ndjson"] = append(patched, '\n')

	repacked := repackAndResign(t, w, files)
	_, verr := audit.VerifyBundle(bytes.NewReader(repacked))
	if verr == nil {
		t.Fatal("an event rewritten inside a validly-signed bundle verified clean")
	}
	if !strings.Contains(verr.Error(), "does not hash") {
		t.Errorf("error does not name the hash mismatch: %v", verr)
	}
}

// A filtered export is legitimately non-contiguous. It must verify, and
// must say that contiguity could not be asserted rather than claiming
// it — condemning every filtered bundle would be as wrong as blessing
// a broken one.
func TestVerifyBundle_FilteredExportReportsUnassertedLinkage(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Now().UTC()
	w.appendEvent(t, "keep.me", "db1", base)
	w.appendEvent(t, "drop.me", "db1", base.Add(time.Second))
	w.appendEvent(t, "keep.me", "db1", base.Add(2*time.Second))

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		Filters: audit.ListFilters{Action: "keep.me"},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := audit.VerifyBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("a filtered bundle must still verify: %v", err)
	}
	if m.Integrity == nil {
		t.Fatal("no Integrity reported")
	}
	if m.Integrity.LinkageAsserted {
		t.Error("a filtered (non-contiguous) export claimed asserted linkage; consecutive " +
			"included events legitimately do not link and saying otherwise is a false claim")
	}
	if m.Integrity.SequenceGaps == 0 {
		t.Error("SequenceGaps = 0 on a filtered export that skipped an event")
	}
}

// repackAndResign rebuilds a bundle tarball from files and signs it
// with the world's key, so the SIGNATURE is genuinely valid over the
// mutated contents. That is the whole point: it isolates chain
// integrity from signature integrity, and reproduces what an operator
// exporting an already-broken chain would ship.
//
// The canonical signing input is reimplemented here from the format the
// bundle README documents, rather than reused from the package — an
// auditor reproducing the signature has only that description, so a
// test that reproduces it independently checks the description too.
func repackAndResign(t *testing.T, w *bundleWorld, files map[string][]byte) []byte {
	t.Helper()
	var manifest audit.BundleManifest
	if err := json.Unmarshal(files["bundle.json"], &manifest); err != nil {
		t.Fatal(err)
	}

	// Canonical input: u64 count, then per file u32 nameLen, name,
	// u64 bodyLen, body — in manifest.SignedFiles order.
	var canon []byte
	appendU64 := func(b []byte, v uint64) []byte {
		var x [8]byte
		for i := 7; i >= 0; i-- {
			x[i] = byte(v)
			v >>= 8
		}
		return append(b, x[:]...)
	}
	appendU32 := func(b []byte, v uint32) []byte {
		var x [4]byte
		for i := 3; i >= 0; i-- {
			x[i] = byte(v)
			v >>= 8
		}
		return append(b, x[:]...)
	}
	canon = appendU64(canon, uint64(len(manifest.SignedFiles)))
	for _, name := range manifest.SignedFiles {
		body, ok := files[name]
		if !ok {
			t.Fatalf("signed file %q missing from the repack set", name)
		}
		canon = appendU32(canon, uint32(len(name)))
		canon = append(canon, name...)
		canon = appendU64(canon, uint64(len(body)))
		canon = append(canon, body...)
	}
	files["signature.sig"] = w.signer.Sign(canon)

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	names := append(append([]string(nil), manifest.SignedFiles...), "signature.sig")
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
