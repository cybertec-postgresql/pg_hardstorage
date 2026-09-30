package scim

import (
	"context"
	"testing"
)

// TestCreateUser_IgnoresClientIDAndNeverOverwrites pins that a create
// cannot clobber an existing resource. CreateUser honoured a
// client-supplied id and wrote it with a plain Put, so POSTing a new
// userName with an existing user's id silently replaced that user's
// record (its userName, groups, active flag). RFC 7643 §3.1 makes id
// server-assigned; the client's value is ignored.
func TestCreateUser_IgnoresClientIDAndNeverOverwrites(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	victim, err := st.CreateUser(ctx, &User{UserName: "alice", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := st.CreateUser(ctx, &User{ID: victim.ID, UserName: "mallory"})
	if err != nil {
		t.Fatal(err)
	}
	if intruder.ID == victim.ID {
		t.Errorf("create honoured the client-supplied id %q", victim.ID)
	}
	got, err := st.GetUser(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserName != "alice" {
		t.Errorf("existing user overwritten: userName = %q, want alice", got.UserName)
	}
}

func TestCreateGroup_IgnoresClientIDAndNeverOverwrites(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	victim, err := st.CreateGroup(ctx, &Group{DisplayName: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := st.CreateGroup(ctx, &Group{ID: victim.ID, DisplayName: "admins"})
	if err != nil {
		t.Fatal(err)
	}
	if intruder.ID == victim.ID {
		t.Errorf("create honoured the client-supplied id %q", victim.ID)
	}
	got, err := st.GetGroup(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "ops" {
		t.Errorf("existing group overwritten: displayName = %q, want ops", got.DisplayName)
	}
}
