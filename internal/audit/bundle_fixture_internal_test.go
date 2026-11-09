package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// fixtureSigner is a deterministic EventSigner: the same seed gives the
// same key, and Ed25519 signatures are deterministic, so a bundle built
// from a fixed fixture has a fixed signature. (backup.Signer cannot be
// used from this package: backup imports audit.)
type fixtureSigner struct{ priv ed25519.PrivateKey }

func newFixtureSigner() fixtureSigner {
	seed := sha256.Sum256([]byte("pg_hardstorage audit bundle fixture key"))
	return fixtureSigner{priv: ed25519.NewKeyFromSeed(seed[:])}
}

func (s fixtureSigner) Sign(payload []byte) []byte { return ed25519.Sign(s.priv, payload) }

func (s fixtureSigner) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

func (s fixtureSigner) PublicKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(s.PublicKey())
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PG_HARDSTORAGE ED25519 PUBLIC KEY", Bytes: der}), nil
}

// fixtureRepo is an fs-backed repository whose audit chain is written as
// files directly: Append fsyncs twice per event and stamps random IDs,
// which would make a large fixture slow and a golden one impossible.
type fixtureRepo struct {
	root  string
	sp    storage.StoragePlugin
	store *Store
}

func newFixtureRepo(t testing.TB) *fixtureRepo {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return &fixtureRepo{root: root, sp: sp, store: NewStore(sp)}
}

func (r *fixtureRepo) writeObject(t testing.TB, key string, body []byte) {
	t.Helper()
	p := filepath.Join(r.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureBase is the timestamp of the first planted event.
var fixtureBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// plantEvents writes n events spread over three shards (the global chain,
// deployment db1, tenant acme), each a valid chain with its head pointer.
//
// The shape is chosen to make the bundle's global order non-trivial:
// events share timestamps across shards (so the ID tie-break decides),
// IDs are not in insertion order and some are prefixes of others
// ("ev-1", "ev-10"), timestamps differ by single nanoseconds, one event
// carries a non-UTC offset, and bodies hold characters encoding/json
// escapes.
func (r *fixtureRepo) plantEvents(t testing.TB, n int) {
	t.Helper()
	type chain struct {
		seq  int64
		prev string
		last *Event
	}
	chains := map[string]*chain{}
	for i := 0; i < n; i++ {
		ts := fixtureBase.Add(time.Duration(i/4)*time.Second + time.Duration(i%2)*time.Nanosecond)
		if i%17 == 3 {
			ts = ts.In(time.FixedZone("", 2*3600))
		}
		ev := &Event{
			Schema:    Schema,
			ID:        fmt.Sprintf("ev-%d", (i*7919)%n),
			Timestamp: ts,
			Actor:     fmt.Sprintf("operator-%d", i%3),
			Action:    []string{"backup.create", "backup.delete", "kms.rotate", "restore.run"}[i%4],
		}
		switch i % 3 {
		case 1:
			ev.Subject = Subject{Deployment: "db1", BackupID: fmt.Sprintf("b-%04d", i)}
		case 2:
			ev.Tenant = "acme"
		}
		if i%10 == 0 {
			ev.Body = map[string]any{
				"note":   "<a&b> é ☃",
				"n":      i,
				"nested": map[string]any{"k": []any{1, 2, "x"}},
			}
		}
		// The body is stored and re-read as JSON; hash what a reader sees.
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var stored Event
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		shard := shardKeyFor(&stored)
		c := chains[shard]
		if c == nil {
			c = &chain{prev: GenesisHash}
			chains[shard] = c
		}
		stored.Sequence, stored.PrevHash = c.seq, c.prev
		h, err := ComputeHash(&stored)
		if err != nil {
			t.Fatal(err)
		}
		stored.Hash = h
		body, err := json.Marshal(&stored)
		if err != nil {
			t.Fatal(err)
		}
		r.writeObject(t, keyFor(&stored), body)
		c.seq, c.prev, c.last = c.seq+1, h, &stored
	}
	for shard, c := range chains {
		if err := r.store.writeHeadPointer(context.Background(), shard, c.last, keyFor(c.last)); err != nil {
			t.Fatal(err)
		}
	}
}

// plantAnchors writes n anchors one minute apart, plus one object that is
// not valid JSON (the exporter skips those rather than failing).
func (r *fixtureRepo) plantAnchors(t testing.TB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		a := Anchor{
			Schema:        "pg_hardstorage.audit.anchor.v1",
			ChainHeadHash: fmt.Sprintf("%064x", i+1),
			HeadSequence:  int64(i * 3),
			AnchoredAt:    fixtureBase.Add(time.Duration(i) * time.Minute),
			PublisherID:   "fixture",
			LogID:         fmt.Sprintf("%016x-%04d", i*3, (i*37)%n),
		}
		if i%2 == 1 {
			a.Shard = "d.db1"
		}
		body, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		// Keys deliberately do not sort in AnchoredAt order.
		r.writeObject(t, fmt.Sprintf("%s%04d-%016x.json", AnchorPrefix, (i*37)%n, i*3), body)
	}
	r.writeObject(t, AnchorPrefix+"zz-corrupt.json", []byte("{not json"))
}

// fixtureNow is the bundle generation time used by the fixtures.
var fixtureNow = time.Date(2026, 4, 2, 8, 30, 0, 0, time.UTC)

func fixtureExportOptions() ExportOptions {
	return ExportOptions{
		IncludeAnchors: true,
		Operator:       "auditor@example.org",
		SourceURL:      "file:///fixture/repo",
		Now:            fixtureNow,
	}
}
