package pkcs11

// module_share.go — one PKCS#11 module and one login per process.
//
// C_Initialize / C_Finalize are PROCESS-global per module, and a
// PKCS#11 login is per application per token: every session the
// process has on that token shares it, and C_Logout on any of them
// logs all of them out. Each Provider used to C_Initialize + C_Login
// on open and C_Logout + C_Finalize on Close, so two providers in one
// process — an agent running a backup while a restore or verify
// opens the same KEK — broke each other: the second Login failed with
// CKR_USER_ALREADY_LOGGED_IN (treated as fatal), and the first Close
// logged out and finalised the module under the other provider's
// feet.
//
// This registry reference-counts both levels. The module is
// initialised by the first user and finalised by the last; the token
// is logged in by the first user of a (module, slot) and logged out by
// the last. Each provider still gets its own session, so their
// C_*Init/C_* operation pairs never interleave on one session.
//
// The registry is build-tag free and talks to the module through
// moduleOps, so its bookkeeping is testable without cgo or an HSM.

import (
	"errors"
	"fmt"
	"sync"
)

// errAlreadyInitialized / errAlreadyLoggedIn are what a moduleOps
// returns for CKR_CRYPTOKI_ALREADY_INITIALIZED and
// CKR_USER_ALREADY_LOGGED_IN. Both mean "someone else in this process
// got there first" and are success for our purposes.
var (
	errAlreadyInitialized = errors.New("pkcs11: CKR_CRYPTOKI_ALREADY_INITIALIZED")
	errAlreadyLoggedIn    = errors.New("pkcs11: CKR_USER_ALREADY_LOGGED_IN")
)

// moduleOps is the module-lifecycle subset of the PKCS#11 API.
type moduleOps interface {
	Initialize() error
	OpenSession(slot uint) (uint, error)
	Login(session uint, pin string) error
	Logout(session uint) error
	CloseSession(session uint) error
	Finalize() error
	// Destroy unloads the module (dlclose); called after Finalize.
	Destroy()
}

// moduleEntry is one loaded module and its per-slot login counts.
type moduleEntry struct {
	ops    moduleOps
	refs   int
	logins map[uint]int
}

// moduleRegistry hands out shared modules keyed by path.
type moduleRegistry struct {
	mu   sync.Mutex
	load func(path string) (moduleOps, error)
	mods map[string]*moduleEntry
}

func newModuleRegistry(load func(path string) (moduleOps, error)) *moduleRegistry {
	return &moduleRegistry{load: load, mods: map[string]*moduleEntry{}}
}

// sessionLease is one provider's session on a shared, logged-in token.
type sessionLease struct {
	reg     *moduleRegistry
	path    string
	slot    uint
	session uint
	ops     moduleOps
	closed  bool
}

// open returns a logged-in session on slot of the module at path,
// loading/initialising the module and logging in only when this is
// the first user. resolveSlot runs against the (initialised) module.
func (r *moduleRegistry) open(path, pin string, resolveSlot func(moduleOps) (uint, error)) (*sessionLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e := r.mods[path]
	if e == nil {
		ops, err := r.load(path)
		if err != nil {
			return nil, err
		}
		if err := ops.Initialize(); err != nil && !errors.Is(err, errAlreadyInitialized) {
			ops.Destroy()
			return nil, fmt.Errorf("pkcs11: Initialize: %w", err)
		}
		e = &moduleEntry{ops: ops, logins: map[uint]int{}}
		r.mods[path] = e
	}
	e.refs++
	fail := func(err error) (*sessionLease, error) {
		r.releaseModuleLocked(path, e)
		return nil, err
	}

	slot, err := resolveSlot(e.ops)
	if err != nil {
		return fail(err)
	}
	sess, err := e.ops.OpenSession(slot)
	if err != nil {
		return fail(fmt.Errorf("pkcs11: OpenSession slot=%d: %w", slot, err))
	}
	if e.logins[slot] == 0 {
		if err := e.ops.Login(sess, pin); err != nil && !errors.Is(err, errAlreadyLoggedIn) {
			_ = e.ops.CloseSession(sess)
			return fail(fmt.Errorf("pkcs11: Login (CKU_USER) on slot=%d: %w", slot, err))
		}
	}
	e.logins[slot]++
	return &sessionLease{reg: r, path: path, slot: slot, session: sess, ops: e.ops}, nil
}

// close ends this lease: the last user of the slot logs out, the last
// user of the module finalises it. Idempotent.
func (l *sessionLease) close() error {
	r := l.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	e := r.mods[l.path]
	if e == nil {
		return nil
	}
	e.logins[l.slot]--
	if e.logins[l.slot] <= 0 {
		delete(e.logins, l.slot)
		// Logout is token-wide; only the last user may do it.
		_ = e.ops.Logout(l.session)
	}
	_ = e.ops.CloseSession(l.session)
	return r.releaseModuleLocked(l.path, e)
}

// releaseModuleLocked drops one module reference and finalises the
// module when it was the last. r.mu must be held.
func (r *moduleRegistry) releaseModuleLocked(path string, e *moduleEntry) error {
	e.refs--
	if e.refs > 0 {
		return nil
	}
	delete(r.mods, path)
	err := e.ops.Finalize()
	e.ops.Destroy()
	if err != nil {
		return fmt.Errorf("Finalize: %w", err)
	}
	return nil
}
