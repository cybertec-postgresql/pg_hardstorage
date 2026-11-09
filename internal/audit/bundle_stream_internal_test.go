package audit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// forceSearchSpill makes the ordered-search sorter flush a run file every
// few records, so the tests exercise the merge a real large window takes
// rather than the in-memory shortcut a small fixture would get.
func forceSearchSpill(t testing.TB, memBytes int) {
	t.Helper()
	old := searchSortMemBytes
	searchSortMemBytes = memBytes
	t.Cleanup(func() { searchSortMemBytes = old })
}

// capturingSigner records the exact bytes it is asked to sign: the
// exporter's canonical signing input, however it was assembled.
type capturingSigner struct {
	fixtureSigner
	payload *[]byte
}

func (c capturingSigner) Sign(p []byte) []byte {
	*c.payload = append([]byte(nil), p...)
	return c.fixtureSigner.Sign(p)
}

// tarMember is one archive entry with every header field the exporter
// sets.
type tarMember struct {
	Name    string
	Mode    int64
	Size    int64
	ModTime time.Time
	Type    byte
	Body    []byte
}

func readTarGz(t testing.TB, archive []byte) []tarMember {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out []tarMember
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body %s: %v", h.Name, err)
		}
		out = append(out, tarMember{Name: h.Name, Mode: h.Mode, Size: h.Size,
			ModTime: h.ModTime.UTC(), Type: h.Typeflag, Body: body})
	}
}

// assertSameBundle runs the pre-#87 in-memory builder and the streaming
// exporter over the same repository, clock and key and requires the same
// bundle from both, layer by layer: the canonical signing input, the
// signature, the archive members, the archive bytes, the result.
func assertSameBundle(t *testing.T, sp storage.StoragePlugin, opts ExportOptions) *BundleResult {
	t.Helper()
	ctx := context.Background()
	signer := newFixtureSigner()

	var refBuf bytes.Buffer
	refRes, refCanon, err := refExportBundle(ctx, sp, &refBuf, signer, opts)
	if err != nil {
		t.Fatalf("reference builder: %v", err)
	}

	spoolDir := t.TempDir()
	opts.SpoolDir = spoolDir
	var gotCanon []byte
	var gotBuf bytes.Buffer
	gotRes, err := ExportBundle(ctx, sp, &gotBuf, capturingSigner{signer, &gotCanon}, opts)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	assertEmptyDir(t, spoolDir)

	if !bytes.Equal(gotCanon, refCanon) {
		t.Fatalf("canonical signing input differs: got %d bytes, reference %d bytes (first difference at %d)",
			len(gotCanon), len(refCanon), firstDiff(gotCanon, refCanon))
	}

	refMembers, gotMembers := readTarGz(t, refBuf.Bytes()), readTarGz(t, gotBuf.Bytes())
	if len(refMembers) != len(gotMembers) {
		t.Fatalf("archive has %d members, reference %d", len(gotMembers), len(refMembers))
	}
	for i := range refMembers {
		r, g := refMembers[i], gotMembers[i]
		if r.Name != g.Name || r.Mode != g.Mode || r.Size != g.Size || !r.ModTime.Equal(g.ModTime) || r.Type != g.Type {
			t.Errorf("member %d header: got %s mode=%o size=%d mtime=%s type=%c; reference %s mode=%o size=%d mtime=%s type=%c",
				i, g.Name, g.Mode, g.Size, g.ModTime, g.Type, r.Name, r.Mode, r.Size, r.ModTime, r.Type)
		}
		if !bytes.Equal(r.Body, g.Body) {
			t.Errorf("member %s: contents differ from the reference (first difference at %d)", r.Name, firstDiff(g.Body, r.Body))
		}
	}
	if last := gotMembers[len(gotMembers)-1]; last.Name != "signature.sig" {
		t.Errorf("last member is %s, want signature.sig", last.Name)
	}
	if !bytes.Equal(gotBuf.Bytes(), refBuf.Bytes()) {
		t.Errorf("tar.gz bytes differ from the reference (%d vs %d bytes)", gotBuf.Len(), refBuf.Len())
	}

	// The results agree on everything but the wall-clock fields.
	gotRes.StoppedAt, gotRes.DurationMS = refRes.StoppedAt, refRes.DurationMS
	if !reflect.DeepEqual(gotRes, refRes) {
		g, _ := json.Marshal(gotRes)
		r, _ := json.Marshal(refRes)
		t.Errorf("result differs:\n got %s\n ref %s", g, r)
	}
	if sum := sha256.Sum256(gotBuf.Bytes()); hex.EncodeToString(sum[:]) != gotRes.SHA256 || int64(gotBuf.Len()) != gotRes.BundleBytes {
		t.Errorf("result digest/size do not describe the bytes written")
	}

	// Both directions of compatibility with the verifier, which did not
	// change: it accepts what the old exporter wrote and what the new
	// one writes.
	for name, b := range map[string][]byte{"reference": refBuf.Bytes(), "streaming": gotBuf.Bytes()} {
		m, err := VerifyBundle(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("VerifyBundle(%s bundle): %v", name, err)
		}
		if m.EventCount != refRes.EventCount {
			t.Errorf("VerifyBundle(%s bundle): %d events, want %d", name, m.EventCount, refRes.EventCount)
		}
	}
	return gotRes
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func assertEmptyDir(t testing.TB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 0 {
		t.Errorf("temp files left behind in the spool dir: %v", names)
	}
}

// The streaming exporter must not change the bundle format in any byte.
// Every option the exporter honours is run against the previous
// implementation, with the sorter both in memory and spilling.
func TestExportBundle_ByteIdenticalToInMemoryBuilder(t *testing.T) {
	r := newFixtureRepo(t)
	r.plantEvents(t, 240)
	r.plantAnchors(t, 12)

	at := func(sec int) time.Time { return fixtureBase.Add(time.Duration(sec) * time.Second) }
	cases := []struct {
		name string
		opts func(o *ExportOptions)
		want int // events the case must select
	}{
		{"everything with anchors", func(o *ExportOptions) {}, 240},
		{"no anchors", func(o *ExportOptions) { o.IncludeAnchors = false }, 240},
		{"window", func(o *ExportOptions) { o.Filters.Since, o.Filters.Until = at(20), at(50) }, 120},
		// Half of each second's events carry one extra nanosecond.
		{"window edge on a nanosecond", func(o *ExportOptions) {
			o.Filters.Since = at(20).Add(time.Nanosecond)
		}, 158},
		{"deployment shard", func(o *ExportOptions) { o.Filters.Deployment = "db1" }, 80},
		{"tenant shard", func(o *ExportOptions) { o.Filters.Tenant = "acme" }, 80},
		{"action prefix across shards", func(o *ExportOptions) { o.Filters.ActionPrefix = "backup." }, 120},
		{"actor substring", func(o *ExportOptions) { o.Filters.ActorContains = "tor-2" }, 80},
		{"one backup", func(o *ExportOptions) { o.Filters.BackupID = "b-0007" }, 1},
		{"empty window", func(o *ExportOptions) { o.Filters.Since = at(100000) }, 0},
		{"limit", func(o *ExportOptions) { o.Filters.Limit = 37 }, 37},
		{"limit of one", func(o *ExportOptions) { o.Filters.Limit = 1 }, 1},
		{"reverse", func(o *ExportOptions) { o.Filters.Reverse = true }, 240},
		{"reverse with limit", func(o *ExportOptions) { o.Filters.Reverse, o.Filters.Limit = true, 41 }, 41},
	}
	// 512 bytes hold an event or two, so the full window is over a
	// hundred run files: more than one merge pass (the sorter merges 64
	// at a time), the path a multi-gigabyte window takes.
	for _, mem := range []struct {
		name  string
		bytes int
	}{{"sorter in memory", 0}, {"sorter spilling", 512}} {
		t.Run(mem.name, func(t *testing.T) {
			forceSearchSpill(t, mem.bytes)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					opts := fixtureExportOptions()
					tc.opts(&opts)
					res := assertSameBundle(t, r.sp, opts)
					if res.EventCount != tc.want {
						t.Errorf("bundle holds %d events, want %d (the case no longer tests what it names)", res.EventCount, tc.want)
					}
				})
			}
		})
	}
}

// A repository with no audit chain at all (no events, no head pointer)
// still exports the same, valid, empty bundle.
func TestExportBundle_ByteIdenticalOnEmptyRepo(t *testing.T) {
	r := newFixtureRepo(t)
	for _, anchors := range []bool{false, true} {
		opts := fixtureExportOptions()
		opts.IncludeAnchors = anchors
		assertSameBundle(t, r.sp, opts)
	}
}

// The values below were produced by the exporter as it stood BEFORE the
// streaming rework (commit 40a6774, the in-memory builder in bundle.go
// itself, not the copy in bundle_reference_internal_test.go) over this
// exact fixture. They pin the format independently of the reference copy:
// Ed25519 is deterministic, so the same key signing the same canonical
// bytes always yields this signature, on any Go version.
//
// A failure here means the bytes an auditor verifies have changed. That
// is a format break for every bundle already handed out; it is not fixed
// by updating the constants.
const (
	goldenBundleEvents       = 200
	goldenBundleAnchors      = 12
	goldenBundleCanonicalLen = 74506
	goldenBundleCanonicalSHA = "52acd507eb0f9988e216abc84a62ee6abf51cbd2569faa8e386fd90b85c1b0b6"
	goldenBundleEventsSHA    = "07978507d2d1d4189e0af051887cb48da09ee5180f66d4798189d40973f2defe"
	goldenBundleAnchorsSHA   = "fb5ea7bbfd9ee445b5cc9173756a0c05ddf07f522080e559d53b9341c92296c9"
	goldenBundleSignature    = "c9fb40e888e1828374a504f21acb22c2f51b0257de9d357e83c4d543a0a9b368419b25c7e9bd1defb9fc2cdd1564e9e69557c6a502b75b7d3c7eb18cb52eb107"
	goldenBundleHeadHash     = "43fec671e324e6510ea9a79a4a6d69f6e8417bafbe1ed4f38531a747cf4bafed"
	goldenBundleHeadSequence = 66
)

func TestExportBundle_GoldenFromInMemoryExporter(t *testing.T) {
	r := newFixtureRepo(t)
	r.plantEvents(t, goldenBundleEvents)
	r.plantAnchors(t, goldenBundleAnchors)

	for _, mem := range []int{0, 2 << 10} {
		forceSearchSpill(t, mem)
		var canon []byte
		var buf bytes.Buffer
		res, err := ExportBundle(context.Background(), r.sp, &buf,
			capturingSigner{newFixtureSigner(), &canon}, fixtureExportOptions())
		if err != nil {
			t.Fatal(err)
		}
		sha := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
		members := map[string][]byte{}
		for _, m := range readTarGz(t, buf.Bytes()) {
			members[m.Name] = m.Body
		}
		if len(canon) != goldenBundleCanonicalLen || sha(canon) != goldenBundleCanonicalSHA {
			t.Errorf("canonical signing input: %d bytes sha256 %s, golden %d bytes %s",
				len(canon), sha(canon), goldenBundleCanonicalLen, goldenBundleCanonicalSHA)
		}
		if got := sha(members["events.ndjson"]); got != goldenBundleEventsSHA {
			t.Errorf("events.ndjson sha256 %s, golden %s", got, goldenBundleEventsSHA)
		}
		if got := sha(members["anchors.ndjson"]); got != goldenBundleAnchorsSHA {
			t.Errorf("anchors.ndjson sha256 %s, golden %s", got, goldenBundleAnchorsSHA)
		}
		if got := hex.EncodeToString(members["signature.sig"]); got != goldenBundleSignature {
			t.Errorf("signature %s, golden %s", got, goldenBundleSignature)
		}
		if res.EventCount != goldenBundleEvents || res.AnchorCount != goldenBundleAnchors ||
			res.HeadHash != goldenBundleHeadHash || res.HeadSequence != goldenBundleHeadSequence {
			t.Errorf("result events=%d anchors=%d head=%s/%d", res.EventCount, res.AnchorCount, res.HeadHash, res.HeadSequence)
		}
	}
}

// SearchEach is Search without the slice: same events, same order, same
// Limit and Reverse meaning.
func TestSearchEach_MatchesSearch(t *testing.T) {
	r := newFixtureRepo(t)
	r.plantEvents(t, 200)
	ctx := context.Background()
	filters := map[string]ListFilters{
		"all":            {},
		"reverse":        {Reverse: true},
		"limit":          {Limit: 23},
		"reverse limit":  {Reverse: true, Limit: 23},
		"limit over":     {Limit: 5000},
		"deployment":     {Deployment: "db1"},
		"window reverse": {Since: fixtureBase.Add(10 * time.Second), Until: fixtureBase.Add(50 * time.Second), Reverse: true},
		"nothing":        {Action: "no.such.action"},
	}
	for _, mem := range []int{0, 2 << 10} {
		forceSearchSpill(t, mem)
		for name, f := range filters {
			want, err := r.store.Search(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			var got []*Event
			if err := r.store.SearchEach(ctx, f, func(ev *Event) error {
				got = append(got, ev)
				return nil
			}); err != nil {
				t.Fatalf("%s: SearchEach: %v", name, err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s (mem %d): SearchEach yielded %d events, Search %d", name, mem, len(got), len(want))
			}
			for i := range want {
				w, _ := json.Marshal(want[i])
				g, _ := json.Marshal(got[i])
				if !bytes.Equal(w, g) {
					t.Fatalf("%s (mem %d): event %d differs:\n got %s\nwant %s", name, mem, i, g, w)
				}
			}
		}
	}

	// fn's error stops the walk and is returned as is.
	stop := errors.New("stop")
	n := 0
	err := r.store.SearchEach(ctx, ListFilters{}, func(*Event) error {
		n++
		if n == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || n != 3 {
		t.Errorf("SearchEach after fn error: err=%v calls=%d", err, n)
	}
}

// The sort key must order events exactly as sortAuditEvents does, and
// its complement exactly backwards, for IDs and instants chosen to break
// a naive encoding: IDs that are prefixes of each other, IDs holding
// 0x00 and 0xFF, instants before 1970 and a nanosecond apart, the same
// instant in different zones.
func TestAppendEventOrderKey_OrdersLikeSearch(t *testing.T) {
	ids := []string{"", "a", "ab", "a\x00", "a\x00\x00", "a\x00\xff", "a\x01", "a\xff", "a\xff\x00", "b", "ev-1", "ev-10", "ev-2", "\x00", "\xff"}
	instants := []time.Time{
		{},
		time.Unix(-5, 999_999_999),
		time.Unix(-5, 0),
		time.Unix(0, 0),
		time.Unix(0, 1),
		time.Unix(1_700_000_000, 0),
		time.Unix(1_700_000_000, 1).In(time.FixedZone("", -7*3600)),
		time.Unix(1_700_000_000, 1).UTC(),
		time.Unix(1_700_000_001, 0),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
	}
	var evs []*Event
	for _, ts := range instants {
		for _, id := range ids {
			evs = append(evs, &Event{ID: id, Timestamp: ts})
		}
	}
	rng := rand.New(rand.NewSource(1))
	rng.Shuffle(len(evs), func(i, j int) { evs[i], evs[j] = evs[j], evs[i] })

	// less is sortAuditEvents' comparison.
	less := func(a, b *Event) bool {
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		return a.ID < b.ID
	}
	for _, reverse := range []bool{false, true} {
		for _, a := range evs {
			for _, b := range evs {
				ka, kb := appendEventOrderKey(nil, a, reverse), appendEventOrderKey(nil, b, reverse)
				want := 0
				switch {
				case less(a, b):
					want = -1
				case less(b, a):
					want = 1
				}
				if reverse {
					want = -want
				}
				if got := bytes.Compare(ka, kb); got != want {
					t.Fatalf("reverse=%v: key order of (%s %q) vs (%s %q) is %d, want %d",
						reverse, a.Timestamp, a.ID, b.Timestamp, b.ID, got, want)
				}
			}
		}
	}

	// And end to end against the real sort.
	sorted := append([]*Event(nil), evs...)
	sortAuditEvents(sorted)
	byKey := append([]*Event(nil), evs...)
	sort.SliceStable(byKey, func(i, j int) bool {
		return bytes.Compare(appendEventOrderKey(nil, byKey[i], false), appendEventOrderKey(nil, byKey[j], false)) < 0
	})
	for i := range sorted {
		if !sorted[i].Timestamp.Equal(byKey[i].Timestamp) || sorted[i].ID != byKey[i].ID {
			t.Fatalf("position %d: key order has (%s %q), sortAuditEvents (%s %q)",
				i, byKey[i].Timestamp, byKey[i].ID, sorted[i].Timestamp, sorted[i].ID)
		}
	}
}

// hookedStorage runs a callback before each Get, for sampling the heap
// and injecting faults at a chosen point of the scan.
type hookedStorage struct {
	storage.StoragePlugin
	onGet func(key string) error
}

func (h *hookedStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if h.onGet != nil {
		if err := h.onGet(key); err != nil {
			return nil, err
		}
	}
	return h.StoragePlugin.Get(ctx, key)
}

// hookedWriter runs a callback, given the write's size, before each
// Write of the archive.
type hookedWriter struct {
	w       io.Writer
	onWrite func(n int) error
}

func (h *hookedWriter) Write(p []byte) (int, error) {
	if h.onWrite != nil {
		if err := h.onWrite(len(p)); err != nil {
			return 0, err
		}
	}
	return h.w.Write(p)
}

// Every way out of ExportBundle removes its temp files and releases
// their descriptors and the signing mapping: success, a storage failure
// half-way through the scan, a signer that fails, an output that fails,
// and cancellation before, during the scan, during the anchor walk and
// while the archive is being written.
func TestExportBundle_RemovesTempFilesOnEveryExit(t *testing.T) {
	// A small buffer so the sorter has run files on disk when the
	// failure hits; otherwise there would be nothing to leak.
	forceSearchSpill(t, 4<<10)
	r := newFixtureRepo(t)
	r.plantEvents(t, 300)
	r.plantAnchors(t, 12)
	base := leakcheck.Take()

	boom := errInjected
	isAnchor := func(key string) bool { return strings.HasPrefix(key, AnchorPrefix) }

	type world struct {
		ctx    context.Context
		cancel context.CancelFunc
		sp     *hookedStorage
		out    *hookedWriter
		signer EventSigner
	}
	cases := []struct {
		name    string
		arrange func(w *world)
		wantErr error // nil = success
	}{
		{"success", func(w *world) {}, nil},
		{"storage fails mid-scan", func(w *world) {
			n := 0
			w.sp.onGet = func(string) error {
				if n++; n == 150 {
					return boom
				}
				return nil
			}
		}, boom},
		{"signer cannot render its key", func(w *world) { w.signer = failingPEMSigner{newFixtureSigner()} }, boom},
		{"output fails", func(w *world) {
			n := 0
			w.out.onWrite = func(int) error {
				if n++; n == 2 {
					return boom
				}
				return nil
			}
		}, boom},
		{"cancelled before start", func(w *world) { w.cancel() }, context.Canceled},
		{"cancelled mid-scan", func(w *world) {
			n := 0
			w.sp.onGet = func(string) error {
				if n++; n == 150 {
					w.cancel()
				}
				return nil
			}
		}, context.Canceled},
		{"cancelled during the anchor walk", func(w *world) {
			w.sp.onGet = func(key string) error {
				if isAnchor(key) {
					w.cancel()
				}
				return nil
			}
		}, context.Canceled},
		{"cancelled while writing the archive", func(w *world) {
			w.out.onWrite = func(int) error {
				w.cancel()
				return nil
			}
		}, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var buf bytes.Buffer
			w := &world{ctx: ctx, cancel: cancel, sp: &hookedStorage{StoragePlugin: r.sp},
				out: &hookedWriter{w: &buf}, signer: newFixtureSigner()}
			tc.arrange(w)

			opts := fixtureExportOptions()
			opts.SpoolDir = t.TempDir()
			res, err := ExportBundle(w.ctx, w.sp, w.out, w.signer, opts)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ExportBundle: %v", err)
				}
				if _, err := VerifyBundle(bytes.NewReader(buf.Bytes())); err != nil {
					t.Fatalf("VerifyBundle: %v", err)
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ExportBundle error = %v, want %v", err, tc.wantErr)
				}
				// A failed export still reports when it stopped.
				if res == nil || res.StoppedAt.IsZero() {
					t.Errorf("failed export returned result %+v, want one with StoppedAt set", res)
				}
				// Whatever reached the output before the failure, it must
				// not be a bundle that verifies.
				if _, verr := VerifyBundle(bytes.NewReader(buf.Bytes())); verr == nil {
					t.Errorf("a failed export left a bundle that verifies")
				}
			}
			assertEmptyDir(t, opts.SpoolDir)
		})
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

var errInjected = errors.New("injected failure")

type failingPEMSigner struct{ fixtureSigner }

func (failingPEMSigner) PublicKeyPEM() ([]byte, error) { return nil, errInjected }

// syntheticChain is a read-only repository holding one global audit
// chain of n events that are generated when read. The memory test needs
// a window many times larger than the export may hold; as files that
// costs tens of thousands of opens per export, and as a slice it would
// itself be the memory being measured. Only the hashes are kept, so the
// chain is a real one (each event's prev_hash is its predecessor's hash).
//
// Only List and Get are implemented: the exporter calls nothing else.
type syntheticChain struct {
	storage.StoragePlugin
	pad    string
	hashes []string
}

func newSyntheticChain(t testing.TB, n, eventBytes int) *syntheticChain {
	t.Helper()
	c := &syntheticChain{pad: strings.Repeat("x", eventBytes), hashes: make([]string, n)}
	for i := range c.hashes {
		ev := c.event(i)
		h, err := ComputeHash(ev)
		if err != nil {
			t.Fatal(err)
		}
		c.hashes[i] = h
	}
	return c
}

func (c *syntheticChain) event(i int) *Event {
	ev := &Event{Schema: Schema, Action: "backup.create", Actor: "scale",
		Timestamp: fixtureBase.Add(time.Duration(i) * time.Millisecond), ID: fmt.Sprintf("ev-%08d", i),
		Sequence: int64(i), PrevHash: GenesisHash, Hash: c.hashes[i], Body: map[string]any{"pad": c.pad}}
	if i > 0 {
		ev.PrevHash = c.hashes[i-1]
	}
	return ev
}

// List yields the event keys in a scrambled order, so the sorter's runs
// overlap and the export's order comes from a real merge.
func (c *syntheticChain) List(_ context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if prefix != shardEventDir("") {
			return // no shards, no anchors
		}
		n := len(c.hashes)
		for i := 0; i < n; i++ {
			seq := int64(i) * 7919 % int64(n)
			if !yield(storage.ObjectInfo{Key: keyFor(&Event{Sequence: seq})}, nil) {
				return
			}
		}
	}
}

func (c *syntheticChain) Get(_ context.Context, key string) (io.ReadCloser, error) {
	seq, ok := seqFromEventKey(key)
	if !ok || seq < 0 || seq >= int64(len(c.hashes)) {
		return nil, storage.ErrNotFound
	}
	body, err := json.Marshal(c.event(int(seq)))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// heapProbe tracks the peak live heap over a baseline. Each sample
// forces a collection first, so what is measured is memory the export
// still holds, not garbage awaiting the next cycle.
type heapProbe struct {
	base, peak uint64
}

func newHeapProbe() *heapProbe {
	p := &heapProbe{}
	// sync.Pool contents survive one collection in the victim cache.
	// Whatever an earlier export left pooled (megabytes of encoder and
	// compressor state) would otherwise sit in the baseline and be
	// freed mid-measurement, hiding that much growth.
	runtime.GC()
	runtime.GC()
	p.base = p.live()
	p.peak = p.base
	return p
}

func (p *heapProbe) live() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func (p *heapProbe) sample() {
	if h := p.live(); h > p.peak {
		p.peak = h
	}
}

func (p *heapProbe) growth() int64 { return int64(p.peak) - int64(p.base) }

// measureExportHeap runs export over sp and returns the peak live-heap
// growth, sampled throughout the scan (every sampleEvery event reads)
// and throughout the archive write.
func measureExportHeap(t testing.TB, sp storage.StoragePlugin, sampleEvery int,
	export func(sp storage.StoragePlugin, w io.Writer) error) (growth int64) {
	t.Helper()
	probe := newHeapProbe()
	gets := 0
	hs := &hookedStorage{StoragePlugin: sp, onGet: func(string) error {
		if gets++; gets%sampleEvery == 0 {
			probe.sample()
		}
		return nil
	}}
	// The compressor emits a few hundred bytes per write; sampling by
	// output volume keeps this to a handful of collections. The first
	// write is the moment the signature has just been made, when
	// everything the export built is at its largest.
	const sampleBytes = 128 << 10
	sinceSample := sampleBytes
	hw := &hookedWriter{w: io.Discard, onWrite: func(n int) error {
		if sinceSample += n; sinceSample >= sampleBytes {
			sinceSample = 0
			probe.sample()
		}
		return nil
	}}
	if err := export(hs, hw); err != nil {
		t.Fatal(err)
	}
	return probe.growth()
}

// The point of #87: the export's heap must not grow with the number of
// events in the window. The same window is exported by the previous
// in-memory builder, which shows the probe sees the problem (several
// times the NDJSON size held at once), and by the streaming exporter,
// which must stay at a small fraction of the NDJSON size.
func TestExportBundle_HeapDoesNotGrowWithWindow(t *testing.T) {
	const (
		events     = 30_000
		eventBytes = 200
	)
	if raceDetector {
		// Pure CPU (JSON, gzip, SHA-256): the race detector makes it
		// some eight times slower, half a minute where the rest of the
		// package takes seconds, for a number it has no bearing on. The
		// export paths themselves run under -race in the tests above.
		t.Skip("heap measurement; run without -race")
	}
	if events%7919 == 0 {
		t.Fatal("syntheticChain.List would not visit every event")
	}
	// 512 KiB of sorter buffer against ~15 MiB of events: the window is
	// some 30 times what the sorter may hold, so it is merged from disk.
	forceSearchSpill(t, 512<<10)
	chain := newSyntheticChain(t, events, eventBytes)
	ctx := context.Background()
	signer := newFixtureSigner()
	opts := fixtureExportOptions()
	opts.IncludeAnchors = false
	opts.SpoolDir = t.TempDir()

	var (
		ndjson int64
		refRes *BundleResult
	)
	refGrowth := measureExportHeap(t, chain, events/10, func(sp storage.StoragePlugin, w io.Writer) error {
		res, canon, err := refExportBundle(ctx, sp, w, signer, opts)
		refRes = res
		ndjson = int64(len(canon)) // events.ndjson plus a few KiB of fixed files
		return err
	})
	var res *BundleResult
	growth := measureExportHeap(t, chain, events/10, func(sp storage.StoragePlugin, w io.Writer) error {
		var err error
		res, err = ExportBundle(ctx, sp, w, signer, opts)
		return err
	})
	assertEmptyDir(t, opts.SpoolDir)
	// The exporter is done with the repository once the scan ends. Were
	// the chain collectable from then on, its megabytes of hashes would
	// leave the heap mid-measurement and be credited to the export.
	runtime.KeepAlive(chain)

	t.Logf("%d events, %.1f MiB of NDJSON: peak live-heap growth %.1f MiB in memory (before), %.1f MiB streaming (after)",
		events, float64(ndjson)/(1<<20), float64(refGrowth)/(1<<20), float64(growth)/(1<<20))
	// The large, merged-from-disk window is the same bundle too.
	if res.EventCount != events || res.SHA256 != refRes.SHA256 || res.BundleBytes != refRes.BundleBytes {
		t.Fatalf("streaming export: %d events, %d bytes, sha256 %s; in-memory builder: %d events, %d bytes, sha256 %s",
			res.EventCount, res.BundleBytes, res.SHA256, refRes.EventCount, refRes.BundleBytes, refRes.SHA256)
	}
	if ndjson < int64(events*eventBytes) {
		t.Fatalf("fixture is only %d bytes of NDJSON; the bound below would be meaningless", ndjson)
	}
	// The in-memory builder holds the NDJSON body and the canonical copy
	// of it (and the events). If the probe does not see at least the
	// NDJSON size there, it is not measuring what this test is about.
	if refGrowth < ndjson {
		t.Errorf("probe saw only %d bytes of growth in the in-memory builder over %d bytes of NDJSON: the measurement is broken",
			refGrowth, ndjson)
	}
	// Streaming holds the sorter buffer (512 KiB here), the spool and
	// compressor buffers and one event at a time: a few MiB whatever the
	// window. A quarter of the NDJSON size is far above that and far
	// below anything proportional to the window.
	if growth > ndjson/4 {
		t.Errorf("heap grew %d bytes exporting %d bytes of NDJSON: the export still holds the window in memory", growth, ndjson)
	}
}
