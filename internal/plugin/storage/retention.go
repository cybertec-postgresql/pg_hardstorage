package storage

// retention.go — reading back an object's WORM lock.
//
// SetRetention is write-only, and Object Lock refuses to SHORTEN a
// lock. A caller that asks for a deadline earlier than the one already
// on the object gets a refusal that is indistinguishable from a missing
// permission — although the object is protected at least as long as
// the caller needs. Reading the lock back lets the caller tell the two
// apart instead of failing.

import (
	"context"
	"time"
)

// RetentionAware is an OPTIONAL interface: Retention reports the WORM
// lock currently on key. A zero until means the object carries no lock.
type RetentionAware interface {
	Retention(ctx context.Context, key string) (until time.Time, mode WORMMode, err error)
}

// RetentionOf reads key's lock when sp implements RetentionAware, and
// returns ErrUnsupported otherwise.
func RetentionOf(ctx context.Context, sp StoragePlugin, key string) (time.Time, WORMMode, error) {
	if r, ok := sp.(RetentionAware); ok {
		return r.Retention(ctx, key)
	}
	return time.Time{}, WORMNone, ErrUnsupported
}

// LockCovers reports whether the lock already on key holds it until
// need in a mode at least as strong as mode, and returns that lock's
// deadline. It is the question to ask after a SetRetention was refused:
// "refused because the object is held longer than I asked" needs no
// action, every other refusal is a failure.
//
// ok is false with a nil error when the lock was read and is too short
// or too weak; err reports that it could not be read (ErrUnsupported on
// a backend that cannot report locks).
func LockCovers(ctx context.Context, sp StoragePlugin, key string, need time.Time, mode WORMMode) (until time.Time, ok bool, err error) {
	cur, curMode, err := RetentionOf(ctx, sp, key)
	if err != nil {
		return time.Time{}, false, err
	}
	return cur, !cur.IsZero() && !cur.Before(need) && curMode.Covers(mode), nil
}

// Covers reports whether a lock in mode m holds an object at least as
// firmly as want: compliance covers both modes, governance only
// governance (a governance lock can be lifted by a privileged
// principal, so it does not satisfy a compliance promise).
func (m WORMMode) Covers(want WORMMode) bool {
	switch want {
	case WORMNone:
		return true
	case WORMGovernance:
		return m == WORMGovernance || m == WORMCompliance
	default:
		return m == WORMCompliance
	}
}
