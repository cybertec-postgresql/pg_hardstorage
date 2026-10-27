package sharedkey

import (
	"io"
	"strings"
	"testing"
)

// countingReader reports how many bytes the scanner pulled.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestScanEncryptionEnvelope_StopsBeforeFiles(t *testing.T) {
	huge := strings.Repeat(`{"path":"x","size":1,"chunks":[]},`, 200000)
	doc := `{"schema":"x","backup_label":"l","encryption":{"scheme":"aes-256-gcm","kek_ref":"k","wrapped_dek":"AA=="},"files":[` + huge + `{"path":"y"}]}`
	cr := &countingReader{r: strings.NewReader(doc)}
	m, err := scanEncryptionEnvelope(cr)
	if err != nil || m.Encryption == nil || m.Encryption.KEKRef != "k" {
		t.Fatalf("got %+v, %v", m, err)
	}
	if cr.n > 1<<16 {
		t.Fatalf("scanner read %d bytes of a %d-byte document; it must stop at the encryption block", cr.n, len(doc))
	}
}

func TestScanEncryptionEnvelope_AbsentStopsAtFiles(t *testing.T) {
	huge := strings.Repeat(`{"path":"x","size":1,"chunks":[]},`, 200000)
	doc := `{"schema":"x","files":[` + huge + `{"path":"y"}]}`
	cr := &countingReader{r: strings.NewReader(doc)}
	m, err := scanEncryptionEnvelope(cr)
	if err != nil || m.Encryption != nil {
		t.Fatalf("got %+v, %v", m, err)
	}
	if cr.n > 1<<16 {
		t.Fatalf("scanner read %d bytes; it must stop at the files array", cr.n)
	}
}

func TestScanEncryptionEnvelope_WALShapeAndGarbage(t *testing.T) {
	m, err := scanEncryptionEnvelope(strings.NewReader(`{"segment":"0001","chunks":[{"a":1}],"encryption":{"scheme":"aes-256-gcm","kek_ref":"k","wrapped_dek":"AA=="}}`))
	if err != nil || m.Encryption == nil {
		t.Fatalf("got %+v, %v", m, err)
	}
	if _, err := scanEncryptionEnvelope(strings.NewReader(`[1]`)); err == nil {
		t.Fatal("array document must be rejected")
	}
	if _, err := scanEncryptionEnvelope(strings.NewReader(`{"a":`)); err == nil {
		t.Fatal("truncated document must error")
	}
}
