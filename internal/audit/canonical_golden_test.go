package audit

// The audit chain is the tamper-evidence mechanism: every event's hash
// covers its own canonical JSON plus the previous event's hash, so a
// verified chain is the compliance artefact SOC 2 / ISO 27001 / HIPAA
// reporting rests on.
//
// Those canonical bytes are produced by json.Marshal of the Event
// struct, which means the STRUCT'S FIELD DECLARATION ORDER is part of
// the on-disk format. Nothing said so, and the comment on
// canonicalForHash said the opposite:
//
//	the encoder's key order is alphabetical for marshalled structs
//
// encoding/json sorts MAP keys; struct fields are emitted in
// declaration order. Reading that comment, reordering the Event struct
// looks like a cosmetic change. It is not: it rewrites the canonical
// bytes of every event ever written, so `audit verify` then reports
// every historical chain as broken — indistinguishable, to the operator
// looking at the output, from someone having actually edited the audit
// log.
//
// The only prior coverage hashed one event twice in the same process
// and compared. That passes under any field order, any added field, and
// any encoding change; it proves determinism WITHIN a build, which is
// not the property the on-disk format needs.
//
// So: pin the bytes. A change that alters them must be deliberate
// enough to update this test, and the failure message says what the
// consequence is.

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenEvent is fixed for all time. Do not "tidy" it.
func goldenEvent() *Event {
	return &Event{
		Schema:    "pg_hardstorage.audit.v1",
		ID:        "01H7K8ZZZZZZZZZZZZZZZZZZZZ",
		Sequence:  42,
		Timestamp: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		Actor:     "alice",
		Tenant:    "acme",
		Action:    "backup.create",
		Subject: Subject{
			Deployment: "db1",
			BackupID:   "db1.full.001",
			Tenant:     "acme",
			Repo:       "file:///srv/repo",
		},
		Body:     map[string]any{"bytes": float64(1024), "alpha": "z"},
		PrevHash: "0000000000000000000000000000000000000000000000000000000000000000",
		Hash:     "this value must not influence the hash",
	}
}

const goldenCanonicalJSON = `{"schema":"pg_hardstorage.audit.v1","id":"01H7K8ZZZZZZZZZZZZZZZZZZZZ","sequence":42,"timestamp":"2026-04-29T12:00:00Z","actor":"alice","tenant":"acme","action":"backup.create","subject":{"deployment":"db1","backup_id":"db1.full.001","tenant":"acme","repo":"file:///srv/repo"},"body":{"alpha":"z","bytes":1024},"prev_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":""}`

func TestCanonicalForHash_GoldenBytes(t *testing.T) {
	got, err := canonicalForHash(goldenEvent())
	if err != nil {
		t.Fatalf("canonicalForHash: %v", err)
	}
	if string(got) != goldenCanonicalJSON {
		t.Fatalf("the audit chain's canonical bytes changed.\n got: %s\nwant: %s\n\n"+
			"These bytes ARE the on-disk format: every persisted event's hash was computed "+
			"over them. Reordering Event's fields, renaming a json tag, adding a field without "+
			"omitempty, or changing the encoder all land here — and all of them make `audit "+
			"verify` report every historical chain as broken, which an operator cannot "+
			"distinguish from real tampering. If the change is intended, it needs a schema "+
			"version bump and a dual-verify path, not a new golden string.",
			got, goldenCanonicalJSON)
	}
}

// The hash itself, so a change to ComputeHash's construction (not just
// the bytes) is caught too.
func TestComputeHash_GoldenValue(t *testing.T) {
	const want = "4cdb04c6bf5769855eb2d9681fa2f44e804f9f97a2b01e45a672a7a43a8513cc"
	got, err := ComputeHash(goldenEvent())
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("hash is %d chars, want 64 hex", len(got))
	}
	if got != want {
		t.Errorf("golden hash changed: got %s, want %s — the canonical bytes or the hash "+
			"construction moved; see TestCanonicalForHash_GoldenBytes", got, want)
	}
}

// The Hash field must not feed its own hash, or the value could never
// be verified.
func TestCanonicalForHash_ExcludesTheHashFieldItself(t *testing.T) {
	a := goldenEvent()
	b := goldenEvent()
	b.Hash = "a completely different value"

	ha, err := ComputeHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := ComputeHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("the Hash field influenced its own hash (%s != %s) — no event could verify",
			ha, hb)
	}
}

// PrevHash must feed the hash, or the chain does not link and tampering
// with history goes undetected.
func TestCanonicalForHash_IncludesPrevHash(t *testing.T) {
	a := goldenEvent()
	b := goldenEvent()
	b.PrevHash = "1111111111111111111111111111111111111111111111111111111111111111"

	ha, _ := ComputeHash(a)
	hb, _ := ComputeHash(b)
	if ha == hb {
		t.Error("PrevHash does not influence the hash — the chain does not link, so editing " +
			"a historical event would not cascade and tampering becomes undetectable")
	}
}

// Every field must be covered. A field that does not influence the hash
// can be edited on disk without breaking the chain.
func TestCanonicalForHash_EveryFieldInfluencesTheHash(t *testing.T) {
	base, err := ComputeHash(goldenEvent())
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Event){
		"Schema":             func(e *Event) { e.Schema = "other" },
		"ID":                 func(e *Event) { e.ID = "other" },
		"Sequence":           func(e *Event) { e.Sequence = 43 },
		"Timestamp":          func(e *Event) { e.Timestamp = e.Timestamp.Add(time.Second) },
		"Actor":              func(e *Event) { e.Actor = "mallory" },
		"Tenant":             func(e *Event) { e.Tenant = "other" },
		"Action":             func(e *Event) { e.Action = "backup.delete" },
		"Subject.Deployment": func(e *Event) { e.Subject.Deployment = "db2" },
		"Subject.BackupID":   func(e *Event) { e.Subject.BackupID = "db1.full.002" },
		"Subject.Tenant":     func(e *Event) { e.Subject.Tenant = "other" },
		"Subject.Repo":       func(e *Event) { e.Subject.Repo = "s3://elsewhere" },
		"Body":               func(e *Event) { e.Body = map[string]any{"alpha": "different"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			ev := goldenEvent()
			mutate(ev)
			got, err := ComputeHash(ev)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Errorf("changing %s does not change the event hash — that field can be "+
					"edited on disk without breaking the chain, so the audit log is not "+
					"tamper-evident for it", name)
			}
		})
	}
}

// omitempty behaviour is part of the on-disk format, including where it
// does NOT work.
//
// Subject is declared `json:"subject,omitempty"`, but encoding/json
// ignores omitempty for structs — it applies to empty values of basic
// types, slices, maps, pointers and interfaces, not to a zero struct.
// So every subject-less event carries a literal `"subject":{}` and
// always has, and that `{}` is inside the bytes their hashes were
// computed over.
//
// The trap this pins: a developer noticing the redundant `{}` and
// "fixing" it — switching Subject to *Subject so omitempty finally
// bites — would drop those four bytes from the canonical form of every
// subject-less event and silently invalidate every audit chain in the
// field. The correct behaviour is the current one, and it has to be
// asserted rather than left as a thing that looks like a mistake.
//
// The inner fields DO omit (they are strings), so adding another
// optional string to Subject stays backward-compatible.
func TestCanonicalForHash_EmptySubjectIsEmittedAndMustStayThatWay(t *testing.T) {
	ev := &Event{
		Schema:    "pg_hardstorage.audit.v1",
		ID:        "x",
		Sequence:  1,
		Timestamp: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		Action:    "a.b",
		PrevHash:  "p",
	}
	got, err := canonicalForHash(ev)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema":"pg_hardstorage.audit.v1","id":"x","sequence":1,` +
		`"timestamp":"2026-04-29T12:00:00Z","action":"a.b","subject":{},` +
		`"prev_hash":"p","hash":""}`
	if string(got) != want {
		t.Fatalf("canonical bytes for a subject-less event changed.\n got: %s\nwant: %s\n\n"+
			"If `subject` disappeared, Subject was probably changed to a pointer so its "+
			"omitempty finally applies. That drops four bytes from the canonical form of "+
			"every subject-less event already on disk and invalidates their hashes — `audit "+
			"verify` would report the whole history as tampered.", got, want)
	}

	// The genuinely-optional scalars must still omit, so a new optional
	// string can be added without rewriting anything.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"actor", "tenant", "body"} {
		if _, present := decoded[absent]; present {
			t.Errorf("optional field %q is emitted when empty — omitempty on the scalars is "+
				"what lets a NEW optional field be added without rewriting the canonical "+
				"bytes of every event already on disk", absent)
		}
	}
	for _, required := range []string{"schema", "id", "sequence", "timestamp", "action", "prev_hash", "hash"} {
		if _, present := decoded[required]; !present {
			t.Errorf("required field %q is missing from the canonical bytes", required)
		}
	}
}
