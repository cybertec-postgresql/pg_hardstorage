package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

type fakeLeaderAddr struct {
	host string
	port int
	err  error
}

func (f fakeLeaderAddr) LeaderAddr(context.Context) (string, int, error) {
	return f.host, f.port, f.err
}

// patroni_failover observed the slot on the static pg_connection, which
// after the switchover names the OLD leader: the drill measured the
// demoted node while the new leader's slot went unexamined. The DSN
// must follow Patroni's current leader, keeping credentials.
func TestGameDayLeaderDSN_FollowsCurrentLeader(t *testing.T) {
	static := "postgres://rep:pw@old-leader:5432/postgres?sslmode=require"
	got, err := gameDayLeaderDSN(context.Background(), fakeLeaderAddr{host: "new-leader", port: 5433}, static)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "new-leader:5433") || strings.Contains(got, "old-leader") {
		t.Fatalf("DSN does not target the current leader: %q", got)
	}
	if !strings.Contains(got, "rep:pw@") || !strings.Contains(got, "sslmode=require") {
		t.Fatalf("credentials/options lost: %q", got)
	}

	if _, err := gameDayLeaderDSN(context.Background(), fakeLeaderAddr{err: errors.New("503")}, static); err == nil {
		t.Fatal("leader lookup failure must not fall back to the static (old-leader) DSN")
	}
	if _, err := gameDayLeaderDSN(context.Background(), nil, static); err == nil {
		t.Fatal("no Patroni driver must leave the slot unmeasured, not measure the static DSN")
	}
}

// The slot is the one the agent keeps on the leader: patroni.slot, else
// the leader-role entry of patroni.slots, else the default name.
func TestGameDaySlotName_HonoursPatroniSlots(t *testing.T) {
	cases := []struct {
		p    config.PatroniConfig
		want string
		err  bool
	}{
		{config.PatroniConfig{Slot: "explicit"}, "explicit", false},
		{config.PatroniConfig{Slots: []config.PatroniSlot{
			{Name: "db1_replica", Role: "replica"},
			{Name: "db1_primary", Role: "leader"},
		}}, "db1_primary", false},
		{config.PatroniConfig{Slots: []config.PatroniSlot{{Name: "only_replica", Role: "replica"}}}, "", true},
		{config.PatroniConfig{}, "pg_hardstorage_db1", false},
	}
	for i, tc := range cases {
		got, err := gameDaySlotName("db1", tc.p)
		if (err != nil) != tc.err {
			t.Fatalf("case %d: err=%v, want err=%v", i, err, tc.err)
		}
		if got != tc.want {
			t.Errorf("case %d: slot %q, want %q", i, got, tc.want)
		}
	}
}

// The production driver must expose the leader's address.
func TestPatroniGameDayDriver_IsLeaderAddresser(t *testing.T) {
	var d any = patroniGameDayDriver{}
	if _, ok := d.(leaderAddresser); !ok {
		t.Fatal("patroniGameDayDriver does not implement leaderAddresser; observations would never run")
	}
}
