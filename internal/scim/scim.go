// Package scim implements the System for Cross-domain Identity
// Management (SCIM) 2.0 user / group provisioning surface
// (RFC 7643 schema, RFC 7644 protocol).  Closes the SPEC
// commitment "SCIM 2.0 user/group provisioning. Auto-provision
// and de-provision human users."
//
// Operationally: an enterprise IdP (Okta, Azure AD, OneLogin,
// JumpCloud, …) pushes user + group lifecycle events to
// pg_hardstorage's SCIM endpoints.  When a new operator joins,
// the IdP auto-creates them; when they leave, the IdP
// deprovisions them.  No manual operator management; no stale
// accounts after team changes.
//
// Scope at+: domain primitive — User, Group, Store, Filter
// parser, and a small subset of the SCIM PATCH operation set.
// HTTP handler wiring into the existing server lands in a
// follow-on commit; the primitive is what the integration needs
// + the harder thing to get right.
//
// What's deliberately NOT here:
//   - Bulk operations (RFC 7644 §3.7).  IdPs that need bulk
//     fall back to per-resource ops, which work.
//   - Search via complex grouped filter expressions.  We
//     implement attribute-equality, attribute-presence, and
//     simple AND chains; the IdP-driven operator-lookup case
//     doesn't need more.
//   - Schema-extension support (custom attributes per
//     enterprise).  The core User + Group schemas cover the
//     90%-case for our RBAC model.
//   - SAML / OIDC binding.  Out of scope for SCIM; ships
//     SAML separately.
package scim

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema URIs from RFC 7643.  These are stable identifiers
// pg_hardstorage emits in every Resource's `schemas` array.
const (
	SchemaUser  = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup = "urn:ietf:params:scim:schemas:core:2.0:Group"

	// SchemaListResponse + SchemaPatchOp + SchemaError are the
	// non-Resource schemas RFC 7644 defines for envelopes.
	SchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
)

// Storage prefixes.
const (
	UsersPrefix  = "scim/users/"
	GroupsPrefix = "scim/groups/"
)

// Sentinel errors.  Map to SCIM error responses at the HTTP
// boundary: NotFound → 404, Conflict → 409, BadFilter → 400.
var (
	ErrNotFound       = errors.New("scim: resource not found")
	ErrConflict       = errors.New("scim: resource conflict (already exists)")
	ErrBadFilter      = errors.New("scim: malformed filter expression")
	ErrUnsupportedOp  = errors.New("scim: unsupported PATCH op")
	ErrInvalidPayload = errors.New("scim: invalid resource payload")
)

// ----- core types -----

// User mirrors the RFC 7643 §4.1 schema.  Fields beyond what an
// IdP typically writes are omitted for clarity; the JSON tags
// match the RFC so a vanilla SCIM IdP serialises directly into
// the type.
type User struct {
	Schemas     []string   `json:"schemas"`
	ID          string     `json:"id"`
	ExternalID  string     `json:"externalId,omitempty"`
	UserName    string     `json:"userName"`
	Name        *Name      `json:"name,omitempty"`
	DisplayName string     `json:"displayName,omitempty"`
	Active      bool       `json:"active"`
	Emails      []Email    `json:"emails,omitempty"`
	Groups      []GroupRef `json:"groups,omitempty"`
	Meta        Meta       `json:"meta"`
}

// Name is the structured user-name attribute.
type Name struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

// Email is one entry in User.Emails.
type Email struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// GroupRef is an inline group reference in User.Groups.
type GroupRef struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Ref     string `json:"$ref,omitempty"`
}

// Group mirrors RFC 7643 §4.2.
type Group struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id"`
	ExternalID  string   `json:"externalId,omitempty"`
	DisplayName string   `json:"displayName"`
	Members     []Member `json:"members,omitempty"`
	Meta        Meta     `json:"meta"`
}

// Member is one entry in Group.Members.
type Member struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Ref     string `json:"$ref,omitempty"`
	Type    string `json:"type,omitempty"`
}

// Meta is the resource metadata block (RFC 7643 §3.1).
type Meta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location,omitempty"`
	Version      string    `json:"version,omitempty"`
}

// ListResponse wraps a search result (RFC 7644 §3.4.2).
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex,omitempty"`
	ItemsPerPage int      `json:"itemsPerPage,omitempty"`
	Resources    []any    `json:"Resources"`
}

// PatchOp is the RFC 7644 §3.5.2 PATCH operation envelope.
type PatchOp struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

// PatchOperation is one entry in PatchOp.Operations.  Only the
// "add" / "remove" / "replace" ops are supported; "move" / "copy"
// don't exist in the SCIM grammar.
type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Value any    `json:"value,omitempty"`
}

// ErrorResp is the RFC 7644 §3.12 error envelope.
type ErrorResp struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"` // HTTP status code as string
	Detail   string   `json:"detail,omitempty"`
	ScimType string   `json:"scimType,omitempty"`
}

// ----- Store -----

// Store persists Users and Groups under the standard SCIM
// prefixes in a storage.StoragePlugin.  Concurrent CRUD is
// safe; the per-resource files are independently atomic via
// the underlying RenameIfNotExists.
type Store struct {
	sp  storage.StoragePlugin
	mu  sync.Mutex
	now func() time.Time
}

// StoreOption is the functional-option shape.
type StoreOption func(*Store)

// WithClock injects a deterministic clock for tests.
func WithClock(now func() time.Time) StoreOption {
	return func(s *Store) { s.now = now }
}

// NewStore constructs a Store rooted at sp.  Default clock is
// time.Now.
func NewStore(sp storage.StoragePlugin, opts ...StoreOption) *Store {
	s := &Store{sp: sp, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ----- User CRUD -----

// CreateUser persists a new user.  Generates the ID + Meta if
// the caller didn't.  Refuses duplicates (matches by userName,
// case-insensitive per RFC 7643 §3.4.1).
func (s *Store) CreateUser(ctx context.Context, u *User) (*User, error) {
	if u == nil || u.UserName == "" {
		return nil, fmt.Errorf("%w: userName required", ErrInvalidPayload)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refuse duplicates by userName (case-insensitive).
	existing, err := s.listUsersLocked(ctx)
	if err != nil {
		return nil, err
	}
	for _, ex := range existing {
		if strings.EqualFold(ex.UserName, u.UserName) {
			return nil, fmt.Errorf("%w: userName %q", ErrConflict, u.UserName)
		}
	}

	now := s.now().UTC()
	if u.ID == "" {
		u.ID = newID(now, u.UserName)
	}
	u.Schemas = []string{SchemaUser}
	u.Meta = Meta{
		ResourceType: "User",
		Created:      now,
		LastModified: now,
		Location:     "/scim/v2/Users/" + u.ID,
	}
	if err := s.putUserLocked(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// GetUser reads one user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	key, err := userKeyForID(id)
	if err != nil {
		return nil, err
	}
	body, err := s.read(ctx, key)
	if err != nil {
		return nil, err
	}
	var u User
	if err := stdjson.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("%w: decode user: %v", ErrInvalidPayload, err)
	}
	return &u, nil
}

// ReplaceUser is the SCIM PUT semantic — the body fully replaces
// the existing resource, except for ID + Meta.Created which are
// preserved.
func (s *Store) ReplaceUser(ctx context.Context, id string, u *User) (*User, error) {
	if u == nil || u.UserName == "" {
		return nil, fmt.Errorf("%w: userName required", ErrInvalidPayload)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.getUserLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	u.ID = existing.ID
	u.Schemas = []string{SchemaUser}
	u.Meta = Meta{
		ResourceType: "User",
		Created:      existing.Meta.Created,
		LastModified: now,
		Location:     existing.Meta.Location,
	}
	if err := s.putUserLocked(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// PatchUser applies a SCIM PATCH operation (RFC 7644 §3.5.2).
// Supports add / remove / replace on top-level scalar attributes
// (active, displayName, externalId) + add/remove on emails.
// Other paths return ErrUnsupportedOp; the caller is expected
// to fall back to PUT for richer changes.
func (s *Store) PatchUser(ctx context.Context, id string, ops []PatchOperation) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, err := s.getUserLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if err := applyUserPatch(u, op); err != nil {
			return nil, err
		}
	}
	now := s.now().UTC()
	u.Meta.LastModified = now
	if err := s.putUserLocked(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// DeleteUser removes the user.  Idempotent: deleting a missing
// user is not an error (matches IdP retry-on-failure behaviour).
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	key, err := userKeyForID(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sp.Delete(ctx, key); err != nil {
		// Some storage plugins return ErrNotFound; treat as no-op.
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("scim: delete user %q: %w", id, err)
	}
	return nil
}

// ListUsers enumerates users matching the optional filter
// expression.  Empty filter returns every user.  count <= 0
// disables paging (returns everything matching).  startIndex
// is 1-based per RFC 7644 §3.4.2.4.
func (s *Store) ListUsers(ctx context.Context, filter string, startIndex, count int) (*ListResponse, error) {
	users, err := s.listUsersLocked(ctx)
	if err != nil {
		return nil, err
	}
	matched := users
	if filter != "" {
		f, err := ParseFilter(filter)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadFilter, err)
		}
		matched = matched[:0]
		for _, u := range users {
			if f.MatchUser(u) {
				matched = append(matched, u)
			}
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].UserName < matched[j].UserName
	})
	total := len(matched)
	if startIndex < 1 {
		startIndex = 1
	}
	if startIndex > total {
		matched = nil
	} else {
		matched = matched[startIndex-1:]
	}
	if count > 0 && count < len(matched) {
		matched = matched[:count]
	}
	resources := make([]any, len(matched))
	for i := range matched {
		resources[i] = matched[i]
	}
	return &ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(matched),
		Resources:    resources,
	}, nil
}

// ----- Group CRUD (mirror shape of User CRUD) -----

// CreateGroup persists a new group.  Generates the ID + Meta if
// the caller didn't.  Refuses duplicates (matches by displayName,
// case-insensitive).
func (s *Store) CreateGroup(ctx context.Context, g *Group) (*Group, error) {
	if g == nil || g.DisplayName == "" {
		return nil, fmt.Errorf("%w: displayName required", ErrInvalidPayload)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.listGroupsLocked(ctx)
	if err != nil {
		return nil, err
	}
	for _, ex := range existing {
		if strings.EqualFold(ex.DisplayName, g.DisplayName) {
			return nil, fmt.Errorf("%w: displayName %q", ErrConflict, g.DisplayName)
		}
	}
	now := s.now().UTC()
	if g.ID == "" {
		g.ID = newID(now, g.DisplayName)
	}
	g.Schemas = []string{SchemaGroup}
	g.Meta = Meta{
		ResourceType: "Group",
		Created:      now,
		LastModified: now,
		Location:     "/scim/v2/Groups/" + g.ID,
	}
	if err := s.putGroupLocked(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// GetGroup reads one group by ID.
func (s *Store) GetGroup(ctx context.Context, id string) (*Group, error) {
	key, err := groupKeyForID(id)
	if err != nil {
		return nil, err
	}
	body, err := s.read(ctx, key)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := stdjson.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("%w: decode group: %v", ErrInvalidPayload, err)
	}
	return &g, nil
}

// ReplaceGroup is the SCIM PUT semantic — the body fully replaces
// the existing resource, except for ID + Meta.Created which are
// preserved.
func (s *Store) ReplaceGroup(ctx context.Context, id string, g *Group) (*Group, error) {
	if g == nil || g.DisplayName == "" {
		return nil, fmt.Errorf("%w: displayName required", ErrInvalidPayload)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.getGroupLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	g.ID = existing.ID
	g.Schemas = []string{SchemaGroup}
	g.Meta = Meta{
		ResourceType: "Group",
		Created:      existing.Meta.Created,
		LastModified: now,
		Location:     existing.Meta.Location,
	}
	if err := s.putGroupLocked(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// PatchGroup applies a SCIM PATCH operation to a group.  Supports
// add / remove / replace on displayName, externalId, and members.
func (s *Store) PatchGroup(ctx context.Context, id string, ops []PatchOperation) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.getGroupLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if err := applyGroupPatch(g, op); err != nil {
			return nil, err
		}
	}
	g.Meta.LastModified = s.now().UTC()
	if err := s.putGroupLocked(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// DeleteGroup removes the group.  Idempotent: deleting a missing
// group is not an error (matches IdP retry-on-failure behaviour).
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	key, err := groupKeyForID(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sp.Delete(ctx, key); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("scim: delete group %q: %w", id, err)
	}
	return nil
}

// ListGroups enumerates groups matching the optional filter
// expression.  Empty filter returns every group.  count <= 0
// disables paging.  startIndex is 1-based per RFC 7644 §3.4.2.4.
func (s *Store) ListGroups(ctx context.Context, filter string, startIndex, count int) (*ListResponse, error) {
	groups, err := s.listGroupsLocked(ctx)
	if err != nil {
		return nil, err
	}
	matched := groups
	if filter != "" {
		f, err := ParseFilter(filter)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadFilter, err)
		}
		matched = matched[:0]
		for _, g := range groups {
			if f.MatchGroup(g) {
				matched = append(matched, g)
			}
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].DisplayName < matched[j].DisplayName
	})
	total := len(matched)
	if startIndex < 1 {
		startIndex = 1
	}
	if startIndex > total {
		matched = nil
	} else {
		matched = matched[startIndex-1:]
	}
	if count > 0 && count < len(matched) {
		matched = matched[:count]
	}
	resources := make([]any, len(matched))
	for i := range matched {
		resources[i] = matched[i]
	}
	return &ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(matched),
		Resources:    resources,
	}, nil
}

// ----- internals -----

func (s *Store) read(ctx context.Context, key string) ([]byte, error) {
	rd, err := s.sp.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

func (s *Store) putUserLocked(ctx context.Context, u *User) error {
	key, err := userKeyForID(u.ID)
	if err != nil {
		return err
	}
	body, err := stdjson.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.sp.Put(ctx, key,
		strings.NewReader(string(body)),
		storage.PutOptions{ContentLength: int64(len(body))})
	if err != nil {
		return fmt.Errorf("scim: put user: %w", err)
	}
	return nil
}

func (s *Store) putGroupLocked(ctx context.Context, g *Group) error {
	key, err := groupKeyForID(g.ID)
	if err != nil {
		return err
	}
	body, err := stdjson.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.sp.Put(ctx, key,
		strings.NewReader(string(body)),
		storage.PutOptions{ContentLength: int64(len(body))})
	if err != nil {
		return fmt.Errorf("scim: put group: %w", err)
	}
	return nil
}

// validateResourceID rejects a SCIM resource id that would break out of
// its namespace storage key (scim/users/<id>.json, scim/groups/<id>.json):
// empty ids, path separators, and control characters. The ids we mint
// are URL-safe tokens; a client passing "a/b" or "../groups/x" in a
// Users/<id> path would otherwise resolve into a different prefix and
// read/delete across namespaces (the storage layer blocks leaving the
// repo root, but not this cross-prefix confusion). A bare ".." is
// harmless here because the prefix+suffix wrap makes it the filename
// "...json", but blocking separators outright is the clean invariant.
func validateResourceID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty resource id", ErrInvalidPayload)
	}
	if strings.IndexFunc(id, func(r rune) bool {
		return r == '/' || r == '\\' || r < 0x20 || r == 0x7f
	}) >= 0 {
		return fmt.Errorf("%w: resource id %q contains an illegal character", ErrInvalidPayload, id)
	}
	return nil
}

func userKeyForID(id string) (string, error) {
	if err := validateResourceID(id); err != nil {
		return "", err
	}
	return UsersPrefix + id + ".json", nil
}

func groupKeyForID(id string) (string, error) {
	if err := validateResourceID(id); err != nil {
		return "", err
	}
	return GroupsPrefix + id + ".json", nil
}

func (s *Store) getUserLocked(ctx context.Context, id string) (*User, error) {
	key, err := userKeyForID(id)
	if err != nil {
		return nil, err
	}
	body, err := s.read(ctx, key)
	if err != nil {
		return nil, err
	}
	var u User
	if err := stdjson.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("%w: decode user: %v", ErrInvalidPayload, err)
	}
	return &u, nil
}

func (s *Store) getGroupLocked(ctx context.Context, id string) (*Group, error) {
	key, err := groupKeyForID(id)
	if err != nil {
		return nil, err
	}
	body, err := s.read(ctx, key)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := stdjson.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("%w: decode group: %v", ErrInvalidPayload, err)
	}
	return &g, nil
}

func (s *Store) listUsersLocked(ctx context.Context) ([]User, error) {
	var out []User
	for obj, err := range s.sp.List(ctx, UsersPrefix) {
		if err != nil {
			return nil, fmt.Errorf("scim: list users: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		body, err := s.read(ctx, obj.Key)
		if err != nil {
			continue
		}
		var u User
		if err := stdjson.Unmarshal(body, &u); err != nil {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Store) listGroupsLocked(ctx context.Context) ([]Group, error) {
	var out []Group
	for obj, err := range s.sp.List(ctx, GroupsPrefix) {
		if err != nil {
			return nil, fmt.Errorf("scim: list groups: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		body, err := s.read(ctx, obj.Key)
		if err != nil {
			continue
		}
		var g Group
		if err := stdjson.Unmarshal(body, &g); err != nil {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

// ----- PATCH application -----

func applyUserPatch(u *User, op PatchOperation) error {
	switch strings.ToLower(op.Op) {
	case "add", "replace":
		switch op.Path {
		case "active":
			b, ok := op.Value.(bool)
			if !ok {
				return fmt.Errorf("%w: active wants bool", ErrInvalidPayload)
			}
			u.Active = b
		case "displayName":
			s, ok := op.Value.(string)
			if !ok {
				return fmt.Errorf("%w: displayName wants string", ErrInvalidPayload)
			}
			u.DisplayName = s
		case "externalId":
			s, ok := op.Value.(string)
			if !ok {
				return fmt.Errorf("%w: externalId wants string", ErrInvalidPayload)
			}
			u.ExternalID = s
		case "emails":
			emails, err := decodeEmails(op.Value)
			if err != nil {
				return err
			}
			if strings.EqualFold(op.Op, "replace") {
				u.Emails = emails
			} else {
				u.Emails = append(u.Emails, emails...)
			}
		default:
			return fmt.Errorf("%w: path %q", ErrUnsupportedOp, op.Path)
		}
	case "remove":
		switch op.Path {
		case "active":
			u.Active = false
		case "displayName":
			u.DisplayName = ""
		case "externalId":
			u.ExternalID = ""
		case "emails":
			u.Emails = nil
		default:
			return fmt.Errorf("%w: path %q", ErrUnsupportedOp, op.Path)
		}
	default:
		return fmt.Errorf("%w: op %q", ErrUnsupportedOp, op.Op)
	}
	return nil
}

func applyGroupPatch(g *Group, op PatchOperation) error {
	switch strings.ToLower(op.Op) {
	case "add", "replace":
		switch op.Path {
		case "displayName":
			s, ok := op.Value.(string)
			if !ok {
				return fmt.Errorf("%w: displayName wants string", ErrInvalidPayload)
			}
			g.DisplayName = s
		case "externalId":
			s, ok := op.Value.(string)
			if !ok {
				return fmt.Errorf("%w: externalId wants string", ErrInvalidPayload)
			}
			g.ExternalID = s
		case "members":
			members, err := decodeMembers(op.Value)
			if err != nil {
				return err
			}
			if strings.EqualFold(op.Op, "replace") {
				g.Members = members
			} else {
				g.Members = append(g.Members, members...)
			}
		default:
			return fmt.Errorf("%w: path %q", ErrUnsupportedOp, op.Path)
		}
	case "remove":
		switch op.Path {
		case "displayName":
			g.DisplayName = ""
		case "externalId":
			g.ExternalID = ""
		case "members":
			g.Members = nil
		default:
			return fmt.Errorf("%w: path %q", ErrUnsupportedOp, op.Path)
		}
	default:
		return fmt.Errorf("%w: op %q", ErrUnsupportedOp, op.Op)
	}
	return nil
}

func decodeEmails(v any) ([]Email, error) {
	bs, err := stdjson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: emails encode: %v", ErrInvalidPayload, err)
	}
	// Either a single Email object or an array; both legal per SCIM.
	var arr []Email
	if err := stdjson.Unmarshal(bs, &arr); err == nil {
		return arr, nil
	}
	var single Email
	if err := stdjson.Unmarshal(bs, &single); err != nil {
		return nil, fmt.Errorf("%w: emails decode: %v", ErrInvalidPayload, err)
	}
	return []Email{single}, nil
}

func decodeMembers(v any) ([]Member, error) {
	bs, err := stdjson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: members encode: %v", ErrInvalidPayload, err)
	}
	var arr []Member
	if err := stdjson.Unmarshal(bs, &arr); err == nil {
		return arr, nil
	}
	var single Member
	if err := stdjson.Unmarshal(bs, &single); err != nil {
		return nil, fmt.Errorf("%w: members decode: %v", ErrInvalidPayload, err)
	}
	return []Member{single}, nil
}

// ----- ID generation -----

func newID(at time.Time, hint string) string {
	// Lex-sortable, low-collision: <unix-nanos>-<short-hash>.
	// Scoped to test predictability when WithClock is set.
	h := uint32(2166136261)
	for i := 0; i < len(hint); i++ {
		h ^= uint32(hint[i])
		h *= 16777619
	}
	return fmt.Sprintf("%020d-%08x", at.UTC().UnixNano(), h)
}

// ----- Filter parser -----

// Filter is the parsed form of a SCIM filter expression.  The
// grammar we implement is a simplified subset of RFC 7644 §3.4.2.2
// covering the IdP-driven operator-lookup case:
//
//	<attr> <op> <literal>          one comparison
//	<expr> and <expr>              binary AND chain
//	<attr> pr                      attribute-presence
//
// Operators: eq | ne | co | sw | ew | gt | ge | lt | le | pr.
// Literals are either double-quoted strings or unquoted numbers.
// Attribute names are case-insensitive per RFC 7643 §3.4.1.
//
// Anything more complex (parenthesised groups, OR chains, NOT,
// nested attribute paths like emails[type eq "work"]) returns
// ErrBadFilter at parse time.  Real-world IdPs almost never
// emit the heavier forms; if they do, the operator can fall
// back to client-side filtering on the unfiltered list.
type Filter struct {
	terms []filterTerm
}

type filterTerm struct {
	attr     string
	op       string
	literal  string
	presence bool
}

// ParseFilter compiles the SCIM filter expression.
func ParseFilter(expr string) (*Filter, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return &Filter{}, nil
	}
	parts := splitAnd(expr)
	terms := make([]filterTerm, 0, len(parts))
	for _, p := range parts {
		t, err := parseTerm(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
	return &Filter{terms: terms}, nil
}

// splitAnd splits on " and " (case-insensitive) outside quoted
// strings.  Tiny tokeniser; sufficient for the supported grammar.
func splitAnd(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			cur.WriteByte(c)
			i++
			continue
		}
		if !inQuote && i+5 <= len(s) &&
			(s[i] == 'a' || s[i] == 'A') &&
			(s[i+1] == 'n' || s[i+1] == 'N') &&
			(s[i+2] == 'd' || s[i+2] == 'D') &&
			(s[i+3] == ' ' || s[i+3] == '\t') {
			// Boundary check: the char before "and" must also be
			// whitespace (or start-of-string).
			if cur.Len() == 0 ||
				cur.String()[cur.Len()-1] == ' ' ||
				cur.String()[cur.Len()-1] == '\t' {
				out = append(out, strings.TrimSpace(cur.String()))
				cur.Reset()
				i += 4
				continue
			}
		}
		cur.WriteByte(c)
		i++
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

func parseTerm(s string) (filterTerm, error) {
	tokens := tokenise(s)
	if len(tokens) == 2 && strings.EqualFold(tokens[1], "pr") {
		return filterTerm{attr: tokens[0], presence: true}, nil
	}
	if len(tokens) != 3 {
		return filterTerm{}, fmt.Errorf("term %q: want <attr> <op> <literal> or <attr> pr", s)
	}
	op := strings.ToLower(tokens[1])
	switch op {
	case "eq", "ne", "co", "sw", "ew", "gt", "ge", "lt", "le":
	default:
		return filterTerm{}, fmt.Errorf("unknown op %q", tokens[1])
	}
	literal := tokens[2]
	if strings.HasPrefix(literal, `"`) && strings.HasSuffix(literal, `"`) {
		literal = strings.TrimPrefix(strings.TrimSuffix(literal, `"`), `"`)
	}
	return filterTerm{attr: tokens[0], op: op, literal: literal}, nil
}

// tokenise splits a term into (attr, op, literal) preserving
// quoted strings.  Tiny implementation; replace with a proper
// tokeniser if the grammar grows.
func tokenise(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			cur.WriteByte(c)
			continue
		}
		if !inQuote && (c == ' ' || c == '\t') {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// MatchUser reports whether the filter accepts u.  Empty filter
// matches everything.
func (f *Filter) MatchUser(u User) bool {
	if f == nil || len(f.terms) == 0 {
		return true
	}
	for _, t := range f.terms {
		if !t.matchUser(u) {
			return false
		}
	}
	return true
}

// MatchGroup is the Group equivalent.
func (f *Filter) MatchGroup(g Group) bool {
	if f == nil || len(f.terms) == 0 {
		return true
	}
	for _, t := range f.terms {
		if !t.matchGroup(g) {
			return false
		}
	}
	return true
}

func (t filterTerm) matchUser(u User) bool {
	val, present := userAttr(u, t.attr)
	return t.eval(val, present)
}

func (t filterTerm) matchGroup(g Group) bool {
	val, present := groupAttr(g, t.attr)
	return t.eval(val, present)
}

func (t filterTerm) eval(val string, present bool) bool {
	if t.presence {
		return present && val != ""
	}
	if !present {
		return false
	}
	a := strings.ToLower(val)
	b := strings.ToLower(t.literal)
	switch t.op {
	case "eq":
		return a == b
	case "ne":
		return a != b
	case "co":
		return strings.Contains(a, b)
	case "sw":
		return strings.HasPrefix(a, b)
	case "ew":
		return strings.HasSuffix(a, b)
	case "gt":
		return a > b
	case "ge":
		return a >= b
	case "lt":
		return a < b
	case "le":
		return a <= b
	}
	return false
}

// userAttr resolves an attribute path on a user.  Supported:
// userName, displayName, externalId, active (rendered as "true"/
// "false" for string ops), id.  Unknown attribute → not present.
func userAttr(u User, attr string) (string, bool) {
	switch strings.ToLower(attr) {
	case "id":
		return u.ID, true
	case "username":
		return u.UserName, true
	case "externalid":
		return u.ExternalID, true
	case "displayname":
		return u.DisplayName, true
	case "active":
		if u.Active {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func groupAttr(g Group, attr string) (string, bool) {
	switch strings.ToLower(attr) {
	case "id":
		return g.ID, true
	case "displayname":
		return g.DisplayName, true
	case "externalid":
		return g.ExternalID, true
	}
	return "", false
}
