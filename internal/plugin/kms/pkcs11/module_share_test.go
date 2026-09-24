package pkcs11

import (
	"sync"
	"testing"
)

// fakeModule models the process-global state of one PKCS#11 module:
// initialised or not, and a token-wide login per slot.
type fakeModule struct {
	mu          sync.Mutex
	initialized bool
	loggedIn    map[uint]bool
	nextSess    uint
	sessSlot    map[uint]uint
	inits       int
	logins      int
	logouts     int
	finalizes   int
}

func newFakeModule() *fakeModule {
	return &fakeModule{loggedIn: map[uint]bool{}, sessSlot: map[uint]uint{}}
}

func (f *fakeModule) Initialize() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inits++
	if f.initialized {
		return errAlreadyInitialized
	}
	f.initialized = true
	return nil
}
func (f *fakeModule) OpenSession(slot uint) (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSess++
	f.sessSlot[f.nextSess] = slot
	return f.nextSess, nil
}
func (f *fakeModule) Login(s uint, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	if f.loggedIn[f.sessSlot[s]] {
		return errAlreadyLoggedIn
	}
	f.loggedIn[f.sessSlot[s]] = true
	return nil
}
func (f *fakeModule) Logout(s uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	f.loggedIn[f.sessSlot[s]] = false
	return nil
}
func (f *fakeModule) CloseSession(s uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessSlot, s)
	return nil
}
func (f *fakeModule) Finalize() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizes++
	f.initialized = false
	return nil
}
func (f *fakeModule) Destroy() {}

// usable reports whether an operation on the slot would succeed right
// now: module initialised and token logged in.
func (f *fakeModule) usable(slot uint) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initialized && f.loggedIn[slot]
}

// TestModuleRegistry_TwoProvidersDoNotBreakEachOther pins H16: two
// providers on the same module+slot in one process (an agent's backup
// plus a restore/verify). Closing the first must neither log the token
// out nor finalise the module under the second.
func TestModuleRegistry_TwoProvidersDoNotBreakEachOther(t *testing.T) {
	mod := newFakeModule()
	reg := newModuleRegistry(func(string) (moduleOps, error) { return mod, nil })
	slot0 := func(moduleOps) (uint, error) { return 0, nil }

	a, err := reg.open("/usr/lib/softhsm/libsofthsm2.so", "1234", slot0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	b, err := reg.open("/usr/lib/softhsm/libsofthsm2.so", "1234", slot0)
	if err != nil {
		t.Fatalf("second open (CKR_USER_ALREADY_LOGGED_IN must not be fatal): %v", err)
	}
	if a.session == b.session {
		t.Error("providers share one session; their C_*Init/C_* pairs would interleave")
	}
	if mod.logins != 1 {
		t.Errorf("C_Login called %d times, want 1", mod.logins)
	}

	if err := a.close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	if !mod.usable(0) {
		t.Fatal("closing one provider logged out / finalised the module under the other")
	}
	if err := b.close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	if mod.logouts != 1 || mod.finalizes != 1 {
		t.Errorf("logouts=%d finalizes=%d, want 1/1 once the last user closes", mod.logouts, mod.finalizes)
	}
	if err := b.close(); err != nil {
		t.Errorf("second close not idempotent: %v", err)
	}

	// A fresh open after full release re-initialises and logs in again.
	c, err := reg.open("/usr/lib/softhsm/libsofthsm2.so", "1234", slot0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !mod.usable(0) {
		t.Error("reopen left the token unusable")
	}
	_ = c.close()
}

// TestModuleRegistry_AlreadyLoggedInIsTolerated: another component of
// the process (or a leaked login) already holds the token's login.
func TestModuleRegistry_AlreadyLoggedInIsTolerated(t *testing.T) {
	mod := newFakeModule()
	mod.loggedIn[3] = true
	reg := newModuleRegistry(func(string) (moduleOps, error) { return mod, nil })
	l, err := reg.open("/m.so", "1234", func(moduleOps) (uint, error) { return 3, nil })
	if err != nil {
		t.Fatalf("open with the token already logged in: %v", err)
	}
	_ = l.close()
}
