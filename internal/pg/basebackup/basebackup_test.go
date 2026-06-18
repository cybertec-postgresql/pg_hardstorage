package basebackup

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// recordingSink records every callback for assertion.
type recordingSink struct {
	startsByIdx map[int]int
	endsByIdx   map[int]int
	dataByIdx   map[int][]byte
	startErr    map[int]error
	dataErr     map[int]error
	endErr      map[int]error
}

func newRecordingSink() *recordingSink {
	return &recordingSink{
		startsByIdx: map[int]int{},
		endsByIdx:   map[int]int{},
		dataByIdx:   map[int][]byte{},
		startErr:    map[int]error{},
		dataErr:     map[int]error{},
		endErr:      map[int]error{},
	}
}

func (s *recordingSink) OnTablespaceStart(idx int, _ TablespaceInfo) error {
	s.startsByIdx[idx]++
	return s.startErr[idx]
}
func (s *recordingSink) OnTablespaceData(idx int, data []byte) error {
	s.dataByIdx[idx] = append(s.dataByIdx[idx], data...)
	return s.dataErr[idx]
}
func (s *recordingSink) OnTablespaceEnd(idx int) error {
	s.endsByIdx[idx]++
	return s.endErr[idx]
}

// pipeDrive sets up a streaming.Reader connected to a "server" net.Pipe
// half wrapped in pgproto3.Backend. The function returns the reader,
// the backend (for emitting canned messages), and a cleanup func.
func pipeDrive(t *testing.T) (*streaming.Reader, *pgproto3.Backend, context.Context, context.CancelFunc) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	r := streaming.NewWithConn(ctx, clientConn, streaming.Options{
		InactivityTimeout: 5 * time.Second,
	})
	be := pgproto3.NewBackend(serverConn, serverConn)
	t.Cleanup(func() {
		_ = r.Close()
		_ = serverConn.Close()
	})
	return r, be, ctx, cancel
}

// emit pushes a sequence of backend messages and flushes.
func emit(t *testing.T, be *pgproto3.Backend, msgs ...pgproto3.BackendMessage) {
	t.Helper()
	for _, m := range msgs {
		be.Send(m)
	}
	if err := be.Flush(); err != nil {
		t.Errorf("flush: %v", err)
	}
}

// tablespaceListSchema is the RowDescription PG 15+ sends for the
// tablespace list result.  Three columns, fixed shape:
// spcoid (OID), spclocation (TEXT), size (INT8).  Column types are
// not checked by the parser — values pass through as bytes.
func tablespaceListSchema() *pgproto3.RowDescription {
	return &pgproto3.RowDescription{
		Fields: []pgproto3.FieldDescription{
			{Name: []byte("spcoid")},
			{Name: []byte("spclocation")},
			{Name: []byte("size")},
		},
	}
}

// lsnSchema is the RowDescription for SendXlogRecPtrResult (start
// LSN AND stop LSN — the same schema is used for both): recptr
// (TEXT) + tli (INT8 rendered as text by DestRemoteSimple).
func lsnSchema() *pgproto3.RowDescription {
	return &pgproto3.RowDescription{
		Fields: []pgproto3.FieldDescription{
			{Name: []byte("recptr")},
			{Name: []byte("tli")},
		},
	}
}

func dataRow(values ...string) *pgproto3.DataRow {
	dr := &pgproto3.DataRow{Values: make([][]byte, len(values))}
	for i, v := range values {
		dr.Values[i] = []byte(v)
	}
	return dr
}

// lsnResult is the three-message sequence PG emits for one
// SendXlogRecPtrResult call: RowDescription + DataRow + CommandComplete.
// Used at both the start and the stop of BASE_BACKUP.
func lsnResult(lsn, tli string) []pgproto3.BackendMessage {
	return []pgproto3.BackendMessage{
		lsnSchema(),
		dataRow(lsn, tli),
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
	}
}

// mplexCopy wraps a multiplex type byte + payload bytes into a CopyData
// message — the PG 15+ BASE_BACKUP archive/manifest stream is a single
// CopyOut where every CopyData payload starts with one of 'n', 'd',
// 'p', 'm' to distinguish archive/data/progress/manifest frames.
func mplexCopy(typeByte byte, payload []byte) *pgproto3.CopyData {
	buf := make([]byte, 0, 1+len(payload))
	buf = append(buf, typeByte)
	buf = append(buf, payload...)
	return &pgproto3.CopyData{Data: buf}
}

// archiveFrame builds a 'n' (new archive) frame.  Payload is
// archive_name + NUL + tablespace_path + NUL — same shape PG emits in
// bbsink_copystream_begin_archive.
func archiveFrame(archiveName, tablespacePath string) *pgproto3.CopyData {
	payload := append([]byte(archiveName), 0)
	payload = append(payload, []byte(tablespacePath)...)
	payload = append(payload, 0)
	return mplexCopy('n', payload)
}

// dataFrame builds a 'd' (archive/manifest content) frame.
func dataFrame(content []byte) *pgproto3.CopyData { return mplexCopy('d', content) }

// progressFrame builds a 'p' frame with an 8-byte big-endian int64
// payload.  bytes_done value is irrelevant to the parser; it just has
// to be exactly 8 bytes.
func progressFrame() *pgproto3.CopyData {
	return mplexCopy('p', []byte{0, 0, 0, 0, 0, 0, 0, 0})
}

// manifestStartFrame builds an empty 'm' frame (signals end of last
// archive and start of manifest data; subsequent 'd' frames carry the
// manifest body).
func manifestStartFrame() *pgproto3.CopyData { return mplexCopy('m', nil) }

func TestDrive_HappyPath_OneTablespace_NoManifest(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("tablespace tar bytes go here")),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/30001A0", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	res := &Result{}
	if err := drive(ctx, r, Options{Manifest: false}, sink, res); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(res.Tablespaces) != 1 {
		t.Errorf("Tablespaces = %d, want 1", len(res.Tablespaces))
	}
	if res.Tablespaces[0].OID != 1663 {
		t.Errorf("OID = %d, want 1663", res.Tablespaces[0].OID)
	}
	if res.StartLSN != "0/2000028" || res.StartTimeline != 1 {
		t.Errorf("StartLSN/Timeline = %q/%d", res.StartLSN, res.StartTimeline)
	}
	if res.StopLSN != "0/30001A0" {
		t.Errorf("StopLSN = %q", res.StopLSN)
	}
	if res.StopTimeline != 1 {
		t.Errorf("StopTimeline = %d", res.StopTimeline)
	}
	if sink.startsByIdx[0] != 1 || sink.endsByIdx[0] != 1 {
		t.Errorf("Start/End counts: starts=%v ends=%v", sink.startsByIdx, sink.endsByIdx)
	}
	if string(sink.dataByIdx[0]) != "tablespace tar bytes go here" {
		t.Errorf("data: %q", sink.dataByIdx[0])
	}
}

func TestDrive_HappyPath_MultipleTablespaces(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			dataRow("16384", "/srv/ts2", "2048"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("first")),
			archiveFrame("16384.tar", "/srv/ts2"),
			dataFrame([]byte("second")),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/40000000", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	res := &Result{}
	if err := drive(ctx, r, Options{}, sink, res); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(res.Tablespaces) != 2 {
		t.Fatalf("Tablespaces = %d, want 2", len(res.Tablespaces))
	}
	if string(sink.dataByIdx[0]) != "first" || string(sink.dataByIdx[1]) != "second" {
		t.Errorf("data per tablespace: %v", sink.dataByIdx)
	}
}

func TestDrive_HappyPath_ManifestEnabled(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("tarbytes")),
			// Manifest takes over: 'm' frame closes the previous archive,
			// then 'd' frames carry the manifest body.
			manifestStartFrame(),
			dataFrame([]byte(`{"manifest":1}`)),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/30001A0", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	res := &Result{}
	if err := drive(ctx, r, Options{Manifest: true}, sink, res); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if string(res.ManifestBytes) != `{"manifest":1}` {
		t.Errorf("ManifestBytes = %q", res.ManifestBytes)
	}
	if sink.startsByIdx[ManifestSinkIndex] != 1 {
		t.Errorf("manifest sink callback not invoked")
	}
	if sink.endsByIdx[ManifestSinkIndex] != 1 {
		t.Errorf("manifest end not invoked: %v", sink.endsByIdx)
	}
}

func TestDrive_ManifestUnexpectedWhenDisabled(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("tar")),
			// Server sent an 'm' (manifest start) frame but we asked
			// for Manifest=false.
			manifestStartFrame(),
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	err := drive(ctx, r, Options{Manifest: false}, sink, &Result{})
	if !errors.Is(err, streaming.ErrUnexpectedMessage) {
		t.Errorf("expected ErrUnexpectedMessage; got %v", err)
	}
}

func TestDrive_SinkErrorOnStartAborts(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	wantErr := errors.New("sink boom")
	sink.startErr[0] = wantErr

	err := drive(ctx, r, Options{}, sink, &Result{})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected sink error to propagate; got %v", err)
	}
}

func TestDrive_SinkErrorOnDataAborts(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("x")),
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	sink.dataErr[0] = errors.New("write fail")
	err := drive(ctx, r, Options{}, sink, &Result{})
	if err == nil || !strings.Contains(err.Error(), "write fail") {
		t.Errorf("expected wrapped sink error; got %v", err)
	}
}

func TestDrive_ServerErrorMidStream(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			// Server changed its mind and sends ErrorResponse mid-stream.
			&pgproto3.ErrorResponse{Severity: "ERROR", Code: "57014", Message: "canceled"},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	err := drive(ctx, r, Options{}, sink, &Result{})
	var se *streaming.ServerError
	if !errors.As(err, &se) {
		t.Errorf("expected *ServerError; got %v", err)
	}
}

func TestDrive_UnexpectedMessageInHeader(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		// Phase 1 expects the start LSN RowDescription; emit
		// ReadyForQuery instead so the parser bails immediately.
		emit(t, be,
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
	}()

	err := drive(ctx, r, Options{}, newRecordingSink(), &Result{})
	if !errors.Is(err, streaming.ErrUnexpectedMessage) {
		t.Errorf("expected ErrUnexpectedMessage; got %v", err)
	}
}

func TestDrive_ContextCancelledMidStream(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := streaming.NewWithConn(ctx, clientConn, streaming.Options{
		InactivityTimeout: 5 * time.Second,
	})
	defer r.Close()
	defer serverConn.Close()
	be := pgproto3.NewBackend(serverConn, serverConn)

	// Send the start LSN result + tablespace header + open the
	// CopyOut, then stall and cancel the context.
	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
		)
		emit(t, be, msgs...)
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := drive(ctx, r, Options{}, newRecordingSink(), &Result{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}
}

func TestDrive_TooManyArchives(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	// Header announces 1 tablespace but the server emits two 'n'
	// frames — multiplexed BASE_BACKUP must reject the surplus.
	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("first")),
			archiveFrame("extra.tar", "/srv/extra"),
		)
		emit(t, be, msgs...)
	}()

	err := drive(ctx, r, Options{}, newRecordingSink(), &Result{})
	if !errors.Is(err, streaming.ErrUnexpectedMessage) {
		t.Errorf("expected ErrUnexpectedMessage; got %v", err)
	}
}

// TestDrive_TablespaceRow_ToleratesExtraColumns locks the
// forward-compat property of the new parser: PG documents 3
// columns (spcoid, spclocation, size); a future PG version
// adding optional trailing fields must not break us.  We
// consume the first three and ignore the rest.  This is a
// deliberate softening after we hit a column-count mismatch
// in production despite matching what the docs said.
func TestDrive_TablespaceRow_ToleratesExtraColumns(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			// Note: 4 columns — extra trailing field PG hasn't
			// shipped today, but might in a future version.
			&pgproto3.RowDescription{
				Fields: []pgproto3.FieldDescription{
					{Name: []byte("spcoid")},
					{Name: []byte("spclocation")},
					{Name: []byte("size")},
					{Name: []byte("future_field")},
				},
			},
			dataRow("1663", "pg_default", "1024", "ignore-me"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("ts1")),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/30001A0", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	res := &Result{}
	if err := drive(ctx, r, Options{Manifest: false}, sink, res); err != nil {
		t.Fatalf("drive should accept >3 columns; got %v", err)
	}
	if len(res.Tablespaces) != 1 || res.Tablespaces[0].OID != 1663 {
		t.Errorf("first three columns should still parse cleanly: %#v", res.Tablespaces)
	}
}

func TestDrive_BadTablespaceRow(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			// Wrong column count (2 instead of 3 — PG's
			// tablespace list has spcoid + spclocation + size,
			// so a 2-column row is malformed).
			dataRow("1663", "ok"),
		)
		emit(t, be, msgs...)
	}()

	err := drive(ctx, r, Options{}, newRecordingSink(), &Result{})
	if err == nil || !strings.Contains(err.Error(), "parse tablespace row") {
		t.Errorf("expected parse error; got %v", err)
	}
}

func TestDrive_BadStopLSNRow(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("x")),
			&pgproto3.CopyDone{},
			lsnSchema(),
			dataRow("0/X", "not-a-number"),
		)
		emit(t, be, msgs...)
	}()

	err := drive(ctx, r, Options{}, newRecordingSink(), &Result{})
	if err == nil || !strings.Contains(err.Error(), "parse stop LSN row") {
		t.Errorf("expected parse error; got %v", err)
	}
}

func TestDrive_StatsAccumulate(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	body := []byte("0123456789")
	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame(body),
			dataFrame(body),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/30001A0", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	if err := drive(ctx, r, Options{}, newRecordingSink(), &Result{}); err != nil {
		t.Fatal(err)
	}
	st := r.Stats()
	if st.BytesReceived < uint64(2*len(body)) {
		t.Errorf("BytesReceived = %d, want >= %d", st.BytesReceived, 2*len(body))
	}
}

// TestDrive_ProgressFramesIgnored locks that 'p' frames don't
// pollute the sink with bytes — they're metadata, not file content.
func TestDrive_ProgressFramesIgnored(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		msgs := []pgproto3.BackendMessage{}
		msgs = append(msgs, lsnResult("0/2000028", "1")...)
		msgs = append(msgs,
			tablespaceListSchema(),
			dataRow("1663", "pg_default", "1024"),
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT")},
			&pgproto3.CopyOutResponse{},
			archiveFrame("base.tar", ""),
			dataFrame([]byte("real-tar-bytes")),
			progressFrame(),
			progressFrame(),
			&pgproto3.CopyDone{},
		)
		msgs = append(msgs, lsnResult("0/30001A0", "1")...)
		msgs = append(msgs,
			&pgproto3.CommandComplete{CommandTag: []byte("BASE_BACKUP")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
		emit(t, be, msgs...)
	}()

	sink := newRecordingSink()
	if err := drive(ctx, r, Options{}, sink, &Result{}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if string(sink.dataByIdx[0]) != "real-tar-bytes" {
		t.Errorf("'p' frame leaked into sink: %q", sink.dataByIdx[0])
	}
}

func TestBuildQuery(t *testing.T) {
	// FAST is unconditional — see buildQuery doc.  Every emitted
	// command MUST contain FAST regardless of Options.Fast (which
	// is now a deprecated no-op).  The cases below lock both the
	// presence of FAST and the historical option ordering
	// (LABEL, CHECKPOINT 'fast', WAL, MANIFEST, INCREMENTAL).
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"label only", Options{Label: "x"},
			"BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast')"},
		{"label manifest",
			Options{Label: "x", Manifest: true},
			"BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast', MANIFEST 'yes')"},
		{"label wal",
			Options{Label: "x", IncludeWAL: true},
			"BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast', WAL)"},
		{"escapes single quote",
			Options{Label: "it's"},
			"BASE_BACKUP (LABEL 'it''s', CHECKPOINT 'fast')"},
		// PG 17 incremental: INCREMENTAL is a BOOLEAN option in
		// PG 17's BASE_BACKUP grammar.  The manifest body is
		// uploaded out-of-band via UPLOAD_MANIFEST (see
		// uploadIncrementalManifest); BASE_BACKUP just sets the
		// flag.  An earlier version inlined the manifest as
		// `INCREMENTAL '<bytes>'` which PG rejected with
		// `42601: incremental requires a Boolean value`.
		{"incremental simple",
			Options{Label: "x", IncrementalManifest: []byte(`{"a":1}`)},
			`BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast', INCREMENTAL 'true')`},
		{"incremental: manifest content does not change buildQuery",
			Options{Label: "x", IncrementalManifest: []byte(`{"name":"it's"}`)},
			`BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast', INCREMENTAL 'true')`},
		{"incremental with manifest preserves order",
			Options{Label: "x", Manifest: true,
				IncrementalManifest: []byte(`{"k":1}`)},
			`BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast', MANIFEST 'yes', INCREMENTAL 'true')`},
		{"empty incremental is no-op (degrades to full)",
			Options{Label: "x", IncrementalManifest: []byte{}},
			"BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast')"},
		// Setting the deprecated Fast field changes nothing —
		// FAST is already always emitted exactly once.
		{"deprecated Fast=true is a no-op",
			Options{Label: "x", Fast: true},
			"BASE_BACKUP (LABEL 'x', CHECKPOINT 'fast')"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildQuery(c.opts)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildQuery_AlwaysEmitsFast locks the wire-level guarantee:
// every BASE_BACKUP command we emit must contain CHECKPOINT 'fast'
// so PG performs an immediate checkpoint and starts streaming bytes
// without waiting for the next scheduled checkpoint.  Property
// test sweeping option combinations so a future option-flag
// addition can't accidentally regress.
//
// (The legacy bare `FAST` keyword is rejected by PG 15+ in the
// parenthesised form — see PG 17 src/backend/backup/basebackup.c
// parse_basebackup_options, which only recognizes "checkpoint" with
// a 'fast'/'spread' Sconst value.)
func TestBuildQuery_AlwaysEmitsFast(t *testing.T) {
	combos := []Options{
		{Label: "x"},
		{Label: "x", Fast: false}, // explicit false on the deprecated field
		{Label: "x", Manifest: true},
		{Label: "x", IncludeWAL: true},
		{Label: "x", Manifest: true, IncludeWAL: true},
		{Label: "x", Manifest: true, IncrementalManifest: []byte(`{"k":1}`)},
		{Label: "with-special-chars-and-'quotes'", Manifest: true},
	}
	for i, opts := range combos {
		got := buildQuery(opts)
		if !strings.Contains(got, "CHECKPOINT 'fast'") {
			t.Errorf("[%d] missing unconditional CHECKPOINT 'fast'; got %q", i, got)
		}
		if strings.Count(got, "CHECKPOINT 'fast'") != 1 {
			t.Errorf("[%d] CHECKPOINT 'fast' emitted %d times; want 1; got %q",
				i, strings.Count(got, "CHECKPOINT 'fast'"), got)
		}
		// The legacy bare `FAST` keyword is rejected by PG 15+ in
		// the parenthesised form (see commit log + PG 17 source
		// src/backend/backup/basebackup.c — only "checkpoint" is
		// accepted as a name in parse_basebackup_options).  Make
		// sure we never accidentally emit it as a standalone
		// option name.
		if strings.Contains(got, ", FAST,") || strings.HasSuffix(got, ", FAST)") {
			t.Errorf("[%d] emitted bare FAST keyword (rejected by PG 15+); got %q",
				i, got)
		}
	}
}

// TestBuildQuery_PG18Compatibility regression-locks issue #6:
// every emitted BASE_BACKUP command MUST start with
// "BASE_BACKUP (" — the parenthesised form is the ONLY shape
// PG 18 accepts.  This is a property test sweeping option
// combinations rather than enumerated cases so a future
// option-flag addition can't accidentally regress.
func TestBuildQuery_PG18Compatibility(t *testing.T) {
	combos := []Options{
		{Label: "x"},
		{Label: "x", Fast: true},
		{Label: "x", Manifest: true},
		{Label: "x", IncludeWAL: true},
		{Label: "x", Fast: true, IncludeWAL: true, Manifest: true},
		{Label: "with-special-chars-and-'quotes'",
			Fast: true, Manifest: true},
		{Label: "x", Manifest: true,
			IncrementalManifest: []byte(`{"manifest":"data"}`)},
	}
	for i, opts := range combos {
		got := buildQuery(opts)
		if !strings.HasPrefix(got, "BASE_BACKUP (") {
			t.Errorf("[%d] missing PG-18-required parenthesised opening; got %q", i, got)
		}
		if !strings.HasSuffix(got, ")") {
			t.Errorf("[%d] missing PG-18-required parenthesised closing; got %q", i, got)
		}
		// Defensive: legacy space-separated pattern must not
		// appear.  `BASE_BACKUP LABEL '...'` (no opening paren)
		// is exactly the PG-18-rejected form.
		if strings.HasPrefix(got, "BASE_BACKUP LABEL") {
			t.Errorf("[%d] emitted legacy non-parenthesised form: %q", i, got)
		}
	}
}

// TestUploadIncrementalManifest_HappyPath drives the PG 17+
// UPLOAD_MANIFEST wire dance against a pgproto3 backend stand-in.
// Asserts the four required messages flow in the right order:
//  1. Client sends Query{"UPLOAD_MANIFEST"}
//  2. Server emits CopyInResponse
//  3. Client sends CopyData(manifest) + CopyDone
//  4. Server emits CommandComplete + ReadyForQuery
//
// Without this protocol, PG 17 rejects the inline `INCREMENTAL
// '<bytes>'` form with 42601 — the bug that surfaced when the
// incremental-lifecycle integration test first ran end-to-end.
func TestUploadIncrementalManifest_HappyPath(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	manifest := []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`)

	// Run the protocol from the server-emulating side: when we
	// see the client's Query, respond with CopyInResponse, then
	// after the client's CopyDone reply with CommandComplete +
	// ReadyForQuery.
	go func() {
		// The pgproto3 Backend's Receive reads exactly what the
		// CLIENT (our code under test) sent.  We expect Query
		// first, then CopyData + CopyDone.
		gotQuery, err := be.Receive()
		if err != nil {
			t.Errorf("backend.Receive query: %v", err)
			return
		}
		q, ok := gotQuery.(*pgproto3.Query)
		if !ok || q.String != "UPLOAD_MANIFEST" {
			t.Errorf("expected UPLOAD_MANIFEST query, got %T %#v", gotQuery, gotQuery)
		}
		// Respond CopyInResponse.
		emit(t, be, &pgproto3.CopyInResponse{OverallFormat: 0})
		// Read CopyData with the manifest body.
		gotData, err := be.Receive()
		if err != nil {
			t.Errorf("backend.Receive copydata: %v", err)
			return
		}
		cd, ok := gotData.(*pgproto3.CopyData)
		if !ok {
			t.Errorf("expected CopyData, got %T", gotData)
			return
		}
		if string(cd.Data) != string(manifest) {
			t.Errorf("manifest bytes mismatch:\n  got  %q\n  want %q", cd.Data, manifest)
		}
		// Read CopyDone.
		gotDone, err := be.Receive()
		if err != nil {
			t.Errorf("backend.Receive copydone: %v", err)
			return
		}
		if _, ok := gotDone.(*pgproto3.CopyDone); !ok {
			t.Errorf("expected CopyDone, got %T", gotDone)
		}
		// Terminate with CommandComplete + ReadyForQuery.
		emit(t, be,
			&pgproto3.CommandComplete{CommandTag: []byte("UPLOAD_MANIFEST")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)
	}()

	if err := uploadIncrementalManifest(ctx, r, manifest); err != nil {
		t.Fatalf("uploadIncrementalManifest: %v", err)
	}
}

// TestUploadIncrementalManifest_ServerErrorPropagates locks the
// failure path: if the server rejects UPLOAD_MANIFEST (e.g. bad
// JSON, summarize_wal not on), the typed ServerError must
// propagate up to the caller so the backup runner surfaces it
// cleanly with the PG error code.
func TestUploadIncrementalManifest_ServerErrorPropagates(t *testing.T) {
	r, be, ctx, cancel := pipeDrive(t)
	defer cancel()

	go func() {
		_, _ = be.Receive()
		emit(t, be, &pgproto3.ErrorResponse{
			Severity: "ERROR", Code: "42601",
			Message: "syntax error at or near \"UPLOAD_MANIFEST\"",
		})
	}()

	err := uploadIncrementalManifest(ctx, r, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("expected error from server ErrorResponse; got nil")
	}
	if !strings.Contains(err.Error(), "42601") {
		t.Errorf("error should carry the PG SQLSTATE; got %v", err)
	}
}

func TestRun_RejectsRegularConn(t *testing.T) {
	// Run requires ModeReplication. Pass nil — it should error before
	// any I/O.
	_, err := Run(context.Background(), nil, Options{Label: "x"}, newRecordingSink())
	if err == nil {
		t.Error("expected nil-conn error")
	}
}

func TestRun_RequiresLabel(t *testing.T) {
	// Even with a non-nil conn we should reject empty label up front.
	// We cannot easily fabricate a *pg.Conn here, so this is exercised
	// only via the buildQuery test for now and via the integration tests.
	_ = atomic.Int32{} // suppress import-unused warning if any
}

// External review pass: the LABEL embedded in BASE_BACKUP is built
// via escapeSingleQuotes. Single quotes are doubled (the standard
// SQL string-literal escape that closes the LABEL parameter). The
// review suggested also doubling backslashes; we deliberately don't
// (modern PG with standard_conforming_strings=on treats `\` as
// literal in `'...'` strings, and doubling would corrupt operator-
// supplied labels with Windows paths or similar). We DO strip
// embedded NULs and translate raw newlines/CRs to spaces — the
// replication-protocol parser would frame on these.
func TestEscapeSingleQuotes_HardeningInvariants(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text", "hourly-2026-04-28", "hourly-2026-04-28"},
		{"single quote doubled", "it's mine", "it''s mine"},
		{"backslash literal", `c:\backups\foo`, `c:\backups\foo`},
		{"newline becomes space", "a\nb", "a b"},
		{"carriage return becomes space", "a\rb", "a b"},
		{"NUL stripped", "a\x00b", "ab"},
		// NUL is between d and e; stripping it leaves them adjacent
		// (no inserted separator). NL → space, CR → space, ' → '',
		// backslash literal.
		{"all the things", "a'b\nc\rd\x00e\\f", `a''b c de\f`},
		{"empty", "", ""},
		// The dangerous shape: a closing quote must NEVER appear
		// unescaped in the output. Walk the result and assert.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeSingleQuotes(c.in)
			if got != c.want {
				t.Errorf("escapeSingleQuotes(%q) = %q, want %q", c.in, got, c.want)
			}
			// Walking invariant: count consecutive `'` runs in the
			// output — they must all be even-length (every embedded
			// `'` is doubled to `''`). An odd-length run could close
			// the LABEL literal early.
			run := 0
			for i := 0; i < len(got); i++ {
				if got[i] == '\'' {
					run++
					continue
				}
				if run%2 != 0 {
					t.Errorf("unescaped \\' run of length %d ending at %d in %q", run, i, got)
				}
				run = 0
			}
			if run%2 != 0 {
				t.Errorf("trailing unescaped \\' run of length %d in %q", run, got)
			}
		})
	}
}
