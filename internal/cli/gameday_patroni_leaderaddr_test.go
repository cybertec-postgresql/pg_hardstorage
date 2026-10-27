package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

func patroniDriverFor(t *testing.T, body string, status int) patroniGameDayDriver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return patroniGameDayDriver{c: c}
}

func TestPatroniGameDayDriver_LeaderAddrNamesTheCurrentLeader(t *testing.T) {
	d := patroniDriverFor(t, `{"scope":"s","members":[
		{"name":"a","role":"replica","host":"10.0.0.1","port":5432,"timeline":3},
		{"name":"b","role":"leader","host":"10.0.0.2","port":5433,"timeline":3}]}`, 200)
	host, port, err := d.LeaderAddr(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if host != "10.0.0.2" || port != 5433 {
		t.Fatalf("LeaderAddr = %s:%d, want the leader 10.0.0.2:5433", host, port)
	}
	name, tli, err := d.Leader(context.Background())
	if err != nil || name != "b" || tli != 3 {
		t.Fatalf("Leader = %q %d %v", name, tli, err)
	}
}

func TestPatroniGameDayDriver_LeaderAddrErrorsWithoutLeader(t *testing.T) {
	d := patroniDriverFor(t, `{"members":[{"name":"a","role":"replica","host":"h","port":1}]}`, 200)
	if _, _, err := d.LeaderAddr(context.Background()); err == nil {
		t.Fatal("LeaderAddr with no leader must fail, not return an empty address")
	}
	if _, _, err := d.Leader(context.Background()); err == nil {
		t.Fatal("Leader with no leader must fail")
	}
}
