package audit

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// lyingAuditSP models a backend whose IfNotExists is last-writer-wins
// and which HONESTLY reports ConditionalPut=false (e.g. sftp without
// the hardlink extension). For one chosen slot key it simulates a
// concurrent winner: the caller's put "succeeds" but the stored bytes
// end up being the OTHER writer's.
type lyingAuditSP struct {
	storage.StoragePlugin
	stompKey  string
	stompWith []byte
	stomped   bool
}

func (l *lyingAuditSP) Capabilities() storage.Capabilities {
	return storage.Capabilities{ConditionalPut: false}
}

func (l *lyingAuditSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return storage.PutResult{}, err
	}
	opts.IfNotExists = false // the lie: overwrite, never ErrAlreadyExists
	if key == l.stompKey && !l.stomped {
		// The concurrent winner's bytes land "after" ours.
		l.stomped = true
		body = l.stompWith
	}
	opts.ContentLength = int64(len(body))
	return l.StoragePlugin.Put(ctx, key, bytes.NewReader(body), opts)
}

// Regression (concurrency audit): on a no-ConditionalPut backend, a
// "won" Append whose slot was actually taken by a concurrent writer
// must DETECT the loss via read-back and relink onto the next slot —
// the old behavior returned success while the event bytes were silently
// replaced, an invisible loss in a tamper-evident log.
func TestAppend_NoConditionalPut_ReadBackDetectsLostSlot(t *testing.T) {
	dir := t.TempDir()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dir}}); err != nil {
		t.Fatal(err)
	}
	defer inner.Close()

	// First, on a scratch prefix... simpler: produce the WINNER event
	// bytes by running a normal Append on a sibling store rooted at a
	// different dir with the same (empty) chain state, then capture the
	// slot-0 object it wrote.
	wdir := t.TempDir()
	winner := &fs.Plugin{}
	if err := winner.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: wdir}}); err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	ws := NewStore(winner)
	if err := ws.Append(context.Background(), &Event{Action: "winner.event", Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("winner append: %v", err)
	}
	var winnerKey string
	var winnerBytes []byte
	for info, err := range winner.List(context.Background(), "audit/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, ".json") && info.Key != HeadKey {
			winnerKey = info.Key
			rc, _ := winner.Get(context.Background(), info.Key)
			winnerBytes, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if winnerKey == "" || len(winnerBytes) == 0 {
		t.Fatal("could not capture winner slot object")
	}

	sp := &lyingAuditSP{StoragePlugin: inner, stompKey: winnerKey, stompWith: winnerBytes}
	s := NewStore(sp)
	if err := s.Append(context.Background(), &Event{Action: "mine.event", Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// BOTH events must exist: the winner at slot 0 and ours relinked
	// at slot 1. The pre-fix behavior left only the winner (our event
	// silently lost) while Append reported success.
	var actions []string
	for info, err := range inner.List(context.Background(), "audit/") {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(info.Key, ".json") || info.Key == HeadKey {
			continue
		}
		rc, _ := inner.Get(context.Background(), info.Key)
		b, _ := io.ReadAll(rc)
		rc.Close()
		actions = append(actions, string(b))
	}
	joined := strings.Join(actions, "\n")
	if !strings.Contains(joined, "winner.event") {
		t.Error("winner event missing")
	}
	if !strings.Contains(joined, "mine.event") {
		t.Error("OUR event was silently lost (pre-fix behavior): read-back did not detect the stomped slot")
	}
}
