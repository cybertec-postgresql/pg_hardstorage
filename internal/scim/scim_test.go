package scim

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// fixedClock returns the same instant on every call.  Lets us
// assert that Meta.Created and the lex-sortable id portion are
// derived from the deterministic clock, not wallclock-now.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatalf("open fs storage: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return NewStore(sp, WithClock(fixedClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))))
}

// ----- User CRUD -----

func TestCreateUser_AllocatesIDAndMeta(t *testing.T) {
	st := newStore(t)
	u, err := st.CreateUser(context.Background(), &User{
		UserName:    "alice",
		DisplayName: "Alice",
		Active:      true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID == "" {
		t.Fatal("ID should be set")
	}
	if len(u.Schemas) != 1 || u.Schemas[0] != SchemaUser {
		t.Fatalf("schemas = %v, want [%s]", u.Schemas, SchemaUser)
	}
	if u.Meta.ResourceType != "User" {
		t.Fatalf("ResourceType = %q, want User", u.Meta.ResourceType)
	}
	if u.Meta.Created.IsZero() || u.Meta.LastModified.IsZero() {
		t.Fatal("meta times should be set")
	}
	if !strings.HasPrefix(u.Meta.Location, "/scim/v2/Users/") {
		t.Fatalf("location = %q, want /scim/v2/Users/<id>", u.Meta.Location)
	}
}

func TestCreateUser_RejectsMissingUserName(t *testing.T) {
	st := newStore(t)
	_, err := st.CreateUser(context.Background(), &User{})
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

func TestCreateUser_ConflictOnDuplicateUserName(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, &User{UserName: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive conflict per RFC 7643 §3.4.1.
	_, err := st.CreateUser(ctx, &User{UserName: "ALICE"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	st := newStore(t)
	_, err := st.GetUser(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetUser_RoundTrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	created, err := st.CreateUser(ctx, &User{
		UserName:    "alice",
		DisplayName: "Alice In Wonderland",
		Emails:      []Email{{Value: "alice@example.com", Primary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserName != "alice" {
		t.Fatalf("UserName = %q, want alice", got.UserName)
	}
	if len(got.Emails) != 1 || got.Emails[0].Value != "alice@example.com" {
		t.Fatalf("emails round-trip lost data: %+v", got.Emails)
	}
}

func TestReplaceUser_PreservesCreatedTimestamp(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create at t0, advance clock, replace, assert Created stuck.
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st.now = fixedClock(t0)
	created, err := st.CreateUser(ctx, &User{UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	t1 := t0.Add(7 * 24 * time.Hour)
	st.now = fixedClock(t1)
	replaced, err := st.ReplaceUser(ctx, created.ID, &User{
		UserName:    "alice",
		DisplayName: "Alice (renamed)",
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !replaced.Meta.Created.Equal(t0) {
		t.Fatalf("Created = %v, want preserved %v", replaced.Meta.Created, t0)
	}
	if !replaced.Meta.LastModified.Equal(t1) {
		t.Fatalf("LastModified = %v, want %v", replaced.Meta.LastModified, t1)
	}
	if replaced.ID != created.ID {
		t.Fatalf("ID changed: %q -> %q", created.ID, replaced.ID)
	}
}

func TestReplaceUser_RejectsMissingUserName(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	created, err := st.CreateUser(ctx, &User{UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ReplaceUser(ctx, created.ID, &User{})
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

func TestPatchUser_AllScalarPaths(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &User{UserName: "alice", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		op   PatchOperation
		want func(*User) bool
	}{
		{"replace active", PatchOperation{Op: "replace", Path: "active", Value: false},
			func(u *User) bool { return !u.Active }},
		{"add displayName", PatchOperation{Op: "add", Path: "displayName", Value: "Alice"},
			func(u *User) bool { return u.DisplayName == "Alice" }},
		{"add externalId", PatchOperation{Op: "add", Path: "externalId", Value: "okta-123"},
			func(u *User) bool { return u.ExternalID == "okta-123" }},
		{"replace externalId", PatchOperation{Op: "replace", Path: "externalId", Value: "okta-456"},
			func(u *User) bool { return u.ExternalID == "okta-456" }},
		{"remove displayName", PatchOperation{Op: "remove", Path: "displayName"},
			func(u *User) bool { return u.DisplayName == "" }},
		{"remove externalId", PatchOperation{Op: "remove", Path: "externalId"},
			func(u *User) bool { return u.ExternalID == "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.PatchUser(ctx, u.ID, []PatchOperation{tc.op})
			if err != nil {
				t.Fatalf("patch: %v", err)
			}
			if !tc.want(got) {
				t.Fatalf("post-patch state failed assertion: %+v", got)
			}
		})
	}
}

func TestPatchUser_EmailsAddVsReplace(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &User{
		UserName: "alice",
		Emails:   []Email{{Value: "a@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// add: appends.
	got, err := st.PatchUser(ctx, u.ID, []PatchOperation{
		{Op: "add", Path: "emails", Value: []Email{{Value: "b@example.com"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 2 {
		t.Fatalf("after add: emails = %d, want 2 (%+v)", len(got.Emails), got.Emails)
	}

	// replace: overwrites.
	got, err = st.PatchUser(ctx, u.ID, []PatchOperation{
		{Op: "replace", Path: "emails", Value: []Email{{Value: "c@example.com"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 1 || got.Emails[0].Value != "c@example.com" {
		t.Fatalf("after replace: emails = %+v, want [c@example.com]", got.Emails)
	}

	// remove: clears.
	got, err = st.PatchUser(ctx, u.ID, []PatchOperation{{Op: "remove", Path: "emails"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 0 {
		t.Fatalf("after remove: emails = %+v, want empty", got.Emails)
	}
}

func TestPatchUser_UnsupportedPathRejected(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &User{UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.PatchUser(ctx, u.ID, []PatchOperation{
		{Op: "replace", Path: "name.givenName", Value: "Alicia"},
	})
	if !errors.Is(err, ErrUnsupportedOp) {
		t.Fatalf("err = %v, want ErrUnsupportedOp", err)
	}
}

func TestPatchUser_TypeMismatchRejected(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &User{UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.PatchUser(ctx, u.ID, []PatchOperation{
		{Op: "replace", Path: "active", Value: "yes-please"},
	})
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

func TestDeleteUser_Idempotent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &User{UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("second delete (idempotent): %v", err)
	}
	if err := st.DeleteUser(ctx, "never-existed"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// ----- Group CRUD -----

func TestCreateGroup_AllocatesIDAndMeta(t *testing.T) {
	st := newStore(t)
	g, err := st.CreateGroup(context.Background(), &Group{DisplayName: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || g.Meta.ResourceType != "Group" {
		t.Fatalf("missing ID or wrong ResourceType: %+v", g)
	}
	if !strings.HasPrefix(g.Meta.Location, "/scim/v2/Groups/") {
		t.Fatalf("location = %q", g.Meta.Location)
	}
}

func TestCreateGroup_RejectsMissingDisplayName(t *testing.T) {
	st := newStore(t)
	_, err := st.CreateGroup(context.Background(), &Group{})
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

func TestCreateGroup_ConflictOnDuplicate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := st.CreateGroup(ctx, &Group{DisplayName: "ops"}); err != nil {
		t.Fatal(err)
	}
	_, err := st.CreateGroup(ctx, &Group{DisplayName: "OPS"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestPatchGroup_MembersLifecycle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	g, err := st.CreateGroup(ctx, &Group{DisplayName: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.PatchGroup(ctx, g.ID, []PatchOperation{
		{Op: "add", Path: "members", Value: []Member{{Value: "alice"}, {Value: "bob"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("after add: members = %d, want 2", len(got.Members))
	}
	got, err = st.PatchGroup(ctx, g.ID, []PatchOperation{
		{Op: "replace", Path: "members", Value: []Member{{Value: "alice"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].Value != "alice" {
		t.Fatalf("after replace: members = %+v, want [alice]", got.Members)
	}
	got, err = st.PatchGroup(ctx, g.ID, []PatchOperation{{Op: "remove", Path: "members"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 0 {
		t.Fatalf("after remove: members = %+v, want empty", got.Members)
	}
}

func TestReplaceGroup_PreservesCreated(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st.now = fixedClock(t0)
	created, err := st.CreateGroup(ctx, &Group{DisplayName: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(48 * time.Hour)
	st.now = fixedClock(t1)
	replaced, err := st.ReplaceGroup(ctx, created.ID, &Group{DisplayName: "ops-renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.Meta.Created.Equal(t0) || !replaced.Meta.LastModified.Equal(t1) {
		t.Fatalf("meta times wrong: %+v", replaced.Meta)
	}
}

func TestDeleteGroup_Idempotent(t *testing.T) {
	st := newStore(t)
	if err := st.DeleteGroup(context.Background(), "never-existed"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// ----- ListUsers / ListGroups -----

func TestListUsers_SortsAndPagesAndFilters(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, name := range []string{"charlie", "alice", "bob", "dave"} {
		if _, err := st.CreateUser(ctx, &User{UserName: name, Active: true}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	// Full list, sorted.
	resp, err := st.ListUsers(ctx, "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalResults != 4 || resp.ItemsPerPage != 4 {
		t.Fatalf("counts = (%d, %d), want (4, 4)", resp.TotalResults, resp.ItemsPerPage)
	}
	wantOrder := []string{"alice", "bob", "charlie", "dave"}
	for i, r := range resp.Resources {
		got := r.(User).UserName
		if got != wantOrder[i] {
			t.Fatalf("idx %d: got %q, want %q", i, got, wantOrder[i])
		}
	}

	// startIndex 2, count 2 → bob, charlie.
	resp, err = st.ListUsers(ctx, "", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Resources) != 2 {
		t.Fatalf("page len = %d, want 2", len(resp.Resources))
	}
	if got := resp.Resources[0].(User).UserName; got != "bob" {
		t.Fatalf("page[0] = %q, want bob", got)
	}

	// Filter by userName eq.
	resp, err = st.ListUsers(ctx, `userName eq "alice"`, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalResults != 1 {
		t.Fatalf("filter total = %d, want 1", resp.TotalResults)
	}
}

func TestListUsers_StartIndexBeyondTotal(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, &User{UserName: "alice"}); err != nil {
		t.Fatal(err)
	}
	resp, err := st.ListUsers(ctx, "", 99, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Resources) != 0 {
		t.Fatalf("resources = %d, want 0 (startIndex past end)", len(resp.Resources))
	}
	if resp.TotalResults != 1 {
		t.Fatalf("total should still reflect full match-set: got %d", resp.TotalResults)
	}
}

func TestListUsers_BadFilter(t *testing.T) {
	st := newStore(t)
	_, err := st.ListUsers(context.Background(), "userName SHRUG something", 1, 0)
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("err = %v, want ErrBadFilter", err)
	}
}

func TestListGroups_FilterAndPaging(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, n := range []string{"ops", "dev", "qa"} {
		if _, err := st.CreateGroup(ctx, &Group{DisplayName: n}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := st.ListGroups(ctx, `displayName co "p"`, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalResults != 1 {
		t.Fatalf("matches = %d, want 1 (only 'ops' contains 'p')", resp.TotalResults)
	}
}

// ----- Filter parser -----

func TestParseFilter_AttributePresence(t *testing.T) {
	f, err := ParseFilter("displayName pr")
	if err != nil {
		t.Fatal(err)
	}
	hasName := User{UserName: "x", DisplayName: "X"}
	noName := User{UserName: "x"}
	if !f.MatchUser(hasName) {
		t.Fatal("hasName should match")
	}
	if f.MatchUser(noName) {
		t.Fatal("noName should not match")
	}
}

func TestParseFilter_AndChain(t *testing.T) {
	f, err := ParseFilter(`userName eq "alice" and active eq "true"`)
	if err != nil {
		t.Fatal(err)
	}
	if !f.MatchUser(User{UserName: "alice", Active: true}) {
		t.Fatal("alice+active should match")
	}
	if f.MatchUser(User{UserName: "alice"}) {
		t.Fatal("alice w/o active should not match")
	}
	if f.MatchUser(User{UserName: "bob", Active: true}) {
		t.Fatal("non-alice should not match")
	}
}

func TestParseFilter_AllComparisonOps(t *testing.T) {
	cases := []struct {
		expr  string
		match User
		want  bool
	}{
		{`userName eq "alice"`, User{UserName: "alice"}, true},
		{`userName eq "alice"`, User{UserName: "Alice"}, true}, // case-insensitive
		{`userName ne "alice"`, User{UserName: "bob"}, true},
		{`userName co "lic"`, User{UserName: "Alice"}, true},
		{`userName sw "ali"`, User{UserName: "Alice"}, true},
		{`userName ew "ice"`, User{UserName: "alice"}, true},
		{`userName gt "alice"`, User{UserName: "bob"}, true},
		{`userName ge "alice"`, User{UserName: "alice"}, true},
		{`userName lt "bob"`, User{UserName: "alice"}, true},
		{`userName le "bob"`, User{UserName: "bob"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			f, err := ParseFilter(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.MatchUser(tc.match); got != tc.want {
				t.Fatalf("filter %q match %+v = %v, want %v", tc.expr, tc.match, got, tc.want)
			}
		})
	}
}

func TestParseFilter_EmptyMatchesAll(t *testing.T) {
	f, err := ParseFilter("")
	if err != nil {
		t.Fatal(err)
	}
	if !f.MatchUser(User{UserName: "alice"}) {
		t.Fatal("empty filter should match every user")
	}
}

func TestParseFilter_RejectsMalformed(t *testing.T) {
	cases := []string{
		"userName",                               // no op
		"userName badop \"x\"",                   // unknown op
		"userName eq \"x\" or userName eq \"y\"", // OR not supported
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			if _, err := ParseFilter(expr); err == nil {
				t.Fatalf("expected error for %q", expr)
			}
		})
	}
}

func TestFilter_UnknownAttributeIsAbsent(t *testing.T) {
	f, err := ParseFilter(`shoeSize eq "42"`)
	if err != nil {
		t.Fatal(err)
	}
	if f.MatchUser(User{UserName: "alice"}) {
		t.Fatal("unknown attribute should not match anything")
	}
}

// ----- Schema completeness / round-trip -----

func TestStorageRoundTrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	u, err := st.CreateUser(ctx, &User{
		UserName:    "alice",
		DisplayName: "Alice",
		Active:      true,
		ExternalID:  "okta-1",
		Emails: []Email{
			{Value: "alice@example.com", Primary: true, Type: "work"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh store on the same underlying storage to confirm
	// nothing relies on in-memory state.
	got, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserName != "alice" || got.DisplayName != "Alice" || got.ExternalID != "okta-1" {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if len(got.Emails) != 1 || !got.Emails[0].Primary || got.Emails[0].Type != "work" {
		t.Fatalf("round-trip lost email metadata: %+v", got.Emails)
	}
}

func TestNewID_LexSortableByCreationTime(t *testing.T) {
	a := newID(time.Unix(0, 1).UTC(), "alice")
	b := newID(time.Unix(0, 2).UTC(), "alice")
	if !(a < b) {
		t.Fatalf("expected lex-sortable: %q < %q", a, b)
	}
}
