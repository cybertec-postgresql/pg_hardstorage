//go:build pkcs11
// +build pkcs11

// CGo-backed PKCS#11 client built on github.com/miekg/pkcs11.
// Compiled only with `-tags pkcs11`; the default build uses
// realclient_stub.go which refuses every call with a clear
// "rebuild with -tags pkcs11" remediation.
//
// Every method maps to the corresponding C_* call:
//
//	Encrypt(MechAESGCM)   → C_EncryptInit(CKM_AES_GCM)+C_Encrypt
//	Encrypt(MechRSAOAEP)  → C_EncryptInit(CKM_RSA_PKCS_OAEP)+C_Encrypt
//	Decrypt(MechAESGCM)   → C_DecryptInit(CKM_AES_GCM)+C_Decrypt
//	Decrypt(MechRSAOAEP)  → C_DecryptInit(CKM_RSA_PKCS_OAEP)+C_Decrypt
//	DestroyKey            → C_FindObjectsInit + C_DestroyObject
//	DescribeKey           → C_GetAttributeValue (CKA_KEY_TYPE,
//	                        CKA_LABEL, CKA_MODULUS_BITS, …)
//	Close                 → C_CloseSession; C_Logout / C_Finalize only
//	                        for the last user in the process
//
// The session is a single CK_SESSION_HANDLE; PKCS#11 sessions
// are cheap to keep open and the alternative (open/close per
// call) tanks throughput.  Methods are serialised by the
// Provider's mutex above us; the C library itself is OK with
// multi-threading on most modules but it's a per-vendor
// minefield and the cost of serialising at the Go layer is
// negligible for a KEK-wrap workload.
//
// Tested against SoftHSM2 in CI behind the `pkcs11` tag plus
// the test/scenarios/pkcs11.scenario.yaml integration run.

package pkcs11

import (
	"context"
	"errors"
	"fmt"

	"github.com/miekg/pkcs11"
)

// Built reports that this binary was built with the cgo
// backend.
func Built() bool { return true }

// cgoClient holds this provider's session + key-handle cache. The
// module and the token login are shared process-wide through
// sharedModules (module_share.go); only the session is ours.
type cgoClient struct {
	ctx     *pkcs11.Ctx
	session pkcs11.SessionHandle
	lease   *sessionLease

	// keyCache maps (CKA_LABEL, CKA_CLASS) → object handle so
	// repeated wraps don't FindObjects every time.  The class is
	// part of the key because an RSA-OAEP keypair shares a single
	// CKA_LABEL: the public half (encrypt) and the private half
	// (decrypt) are distinct objects, and caching one handle for
	// both operations would use the wrong half.  Cleared on key
	// destroy.
	keyCache map[cacheKey]pkcs11.ObjectHandle
}

// cacheKey identifies a cached object by its label and PKCS#11
// object class (CKO_SECRET_KEY / CKO_PUBLIC_KEY / CKO_PRIVATE_KEY).
type cacheKey struct {
	label string
	class uint
}

// cgoModule adapts *pkcs11.Ctx to moduleOps, translating the two
// "someone in this process got there first" return codes.
type cgoModule struct{ c *pkcs11.Ctx }

func (m cgoModule) Initialize() error {
	err := m.c.Initialize()
	var perr pkcs11.Error
	if errors.As(err, &perr) && perr == pkcs11.CKR_CRYPTOKI_ALREADY_INITIALIZED {
		return errAlreadyInitialized
	}
	return err
}

func (m cgoModule) OpenSession(slot uint) (uint, error) {
	s, err := m.c.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	return uint(s), err
}

func (m cgoModule) Login(sess uint, pin string) error {
	err := m.c.Login(pkcs11.SessionHandle(sess), pkcs11.CKU_USER, pin)
	var perr pkcs11.Error
	if errors.As(err, &perr) && perr == pkcs11.CKR_USER_ALREADY_LOGGED_IN {
		return errAlreadyLoggedIn
	}
	return err
}

func (m cgoModule) Logout(sess uint) error { return m.c.Logout(pkcs11.SessionHandle(sess)) }

func (m cgoModule) CloseSession(sess uint) error {
	return m.c.CloseSession(pkcs11.SessionHandle(sess))
}

func (m cgoModule) Finalize() error {
	err := m.c.Finalize()
	var perr pkcs11.Error
	if errors.As(err, &perr) && perr == pkcs11.CKR_CRYPTOKI_NOT_INITIALIZED {
		return nil
	}
	return err
}

func (m cgoModule) Destroy() { m.c.Destroy() }

// sharedModules is the process-wide module registry.
var sharedModules = newModuleRegistry(func(path string) (moduleOps, error) {
	c := pkcs11.New(path)
	if c == nil {
		return nil, fmt.Errorf("pkcs11: load module %q: returned nil context", path)
	}
	return cgoModule{c: c}, nil
})

// newRealClient opens a session on the named token, logged in with the
// supplied PIN, through the shared module registry: the module is
// initialised and the token logged in only by the first user in the
// process. The session is held open for the lifetime of the Provider.
func newRealClient(_ context.Context, cfg realClientConfig) (Client, error) {
	lease, err := sharedModules.open(cfg.ModulePath, cfg.PIN, func(m moduleOps) (uint, error) {
		return resolveSlot(m.(cgoModule).c, cfg)
	})
	if err != nil {
		return nil, err
	}
	return &cgoClient{
		ctx:      lease.ops.(cgoModule).c,
		session:  pkcs11.SessionHandle(lease.session),
		lease:    lease,
		keyCache: map[cacheKey]pkcs11.ObjectHandle{},
	}, nil
}

// resolveSlot finds the slot containing the named token.
// If the operator supplied an explicit slot id, we use that
// directly.  Otherwise we walk GetSlotList and match on the
// CK_TOKEN_INFO label.
func resolveSlot(c *pkcs11.Ctx, cfg realClientConfig) (uint, error) {
	if cfg.SlotSet {
		return uint(cfg.Slot), nil
	}
	slots, err := c.GetSlotList(true)
	if err != nil {
		return 0, fmt.Errorf("pkcs11: GetSlotList: %w", err)
	}
	for _, s := range slots {
		info, err := c.GetTokenInfo(s)
		if err != nil {
			continue
		}
		// Token labels are space-padded fixed-width fields;
		// trim before comparing.
		label := trimNullsSpaces(info.Label)
		if label == cfg.TokenLabel {
			return s, nil
		}
	}
	return 0, fmt.Errorf("pkcs11: no slot with token label %q (saw %d slots)", cfg.TokenLabel, len(slots))
}

// keyClassFor returns the PKCS#11 object class to search for
// given the wrap mechanism and the operation direction.
//
//   - AES-GCM is symmetric: a single CKO_SECRET_KEY object serves
//     both encrypt and decrypt.
//   - RSA-OAEP is asymmetric: the public half (CKO_PUBLIC_KEY)
//     encrypts and the private half (CKO_PRIVATE_KEY) decrypts.
//     Both halves share the same CKA_LABEL, so the class is the
//     only thing that disambiguates them.
func keyClassFor(mech Mechanism, encrypt bool) uint {
	if mech == MechRSAOAEP {
		if encrypt {
			return pkcs11.CKO_PUBLIC_KEY
		}
		return pkcs11.CKO_PRIVATE_KEY
	}
	return pkcs11.CKO_SECRET_KEY
}

// findKey returns the object handle for the named CKA_LABEL that
// also matches the requested CKA_CLASS.  Constraining on class is
// essential for RSA-OAEP, where the encrypt (public) and decrypt
// (private) halves share a label; searching label-only would
// return whichever object the token happens to enumerate first
// and cache the wrong half for the opposite operation.  Caches
// per (label, class) so encrypt and decrypt keep independent
// handles.
func (cc *cgoClient) findKey(label string, class uint) (pkcs11.ObjectHandle, error) {
	ck := cacheKey{label: label, class: class}
	if h, ok := cc.keyCache[ck]; ok {
		return h, nil
	}
	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, []byte(label)),
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, class),
	}
	if err := cc.ctx.FindObjectsInit(cc.session, template); err != nil {
		return 0, fmt.Errorf("FindObjectsInit: %w", err)
	}
	defer func() { _ = cc.ctx.FindObjectsFinal(cc.session) }()
	objs, _, err := cc.ctx.FindObjects(cc.session, 1)
	if err != nil {
		return 0, fmt.Errorf("FindObjects: %w", err)
	}
	if len(objs) == 0 {
		return 0, fmt.Errorf("no key with label %q and class %d", label, class)
	}
	cc.keyCache[ck] = objs[0]
	return objs[0], nil
}

// findAnyKey returns the first object handle matching CKA_LABEL
// regardless of class.  Used by DestroyKey/DescribeKey, which
// operate on the key object by label alone (they don't wrap or
// unwrap, so the encrypt/decrypt class distinction doesn't apply).
// Not cached: these are one-shot operations and DestroyKey must
// not leave a stale handle behind.
func (cc *cgoClient) findAnyKey(label string) (pkcs11.ObjectHandle, error) {
	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, []byte(label)),
	}
	if err := cc.ctx.FindObjectsInit(cc.session, template); err != nil {
		return 0, fmt.Errorf("FindObjectsInit: %w", err)
	}
	defer func() { _ = cc.ctx.FindObjectsFinal(cc.session) }()
	objs, _, err := cc.ctx.FindObjects(cc.session, 1)
	if err != nil {
		return 0, fmt.Errorf("FindObjects: %w", err)
	}
	if len(objs) == 0 {
		return 0, fmt.Errorf("no key with label %q", label)
	}
	return objs[0], nil
}

// Encrypt wraps `plaintext` under `keyLabel`.  IV (12 bytes)
// is supplied by the caller for aes-gcm and ignored for
// rsa-oaep.
func (cc *cgoClient) Encrypt(_ context.Context, mech Mechanism, keyLabel string, iv, plaintext []byte) ([]byte, error) {
	key, err := cc.findKey(keyLabel, keyClassFor(mech, true))
	if err != nil {
		return nil, err
	}
	mechanism, cleanup, err := buildMech(mech, iv)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// miekg/pkcs11 ≥ v1.1.0 takes a slice of *Mechanism for the
	// *Init calls (PKCS#11 spec allows multi-step mechanisms;
	// the older single-pointer form was deprecated).  Every
	// mechanism this client uses is single-step, so we wrap.
	if err := cc.ctx.EncryptInit(cc.session, []*pkcs11.Mechanism{mechanism}, key); err != nil {
		return nil, fmt.Errorf("EncryptInit: %w", err)
	}
	out, err := cc.ctx.Encrypt(cc.session, plaintext)
	if err != nil {
		return nil, fmt.Errorf("Encrypt: %w", err)
	}
	return out, nil
}

// Decrypt unwraps `ciphertext` under `keyLabel`.
func (cc *cgoClient) Decrypt(_ context.Context, mech Mechanism, keyLabel string, iv, ciphertext []byte) ([]byte, error) {
	key, err := cc.findKey(keyLabel, keyClassFor(mech, false))
	if err != nil {
		return nil, err
	}
	mechanism, cleanup, err := buildMech(mech, iv)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// Same slice-wrap as EncryptInit above; see the comment there.
	if err := cc.ctx.DecryptInit(cc.session, []*pkcs11.Mechanism{mechanism}, key); err != nil {
		return nil, fmt.Errorf("DecryptInit: %w", err)
	}
	out, err := cc.ctx.Decrypt(cc.session, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: %w", err)
	}
	return out, nil
}

// DestroyKey removes the named key object.  Drops the cache
// entry on success so a subsequent operation triggers a fresh
// FindObjects (which will fail — by design, Shred is final).
func (cc *cgoClient) DestroyKey(_ context.Context, keyLabel string) error {
	key, err := cc.findAnyKey(keyLabel)
	if err != nil {
		return err
	}
	if err := cc.ctx.DestroyObject(cc.session, key); err != nil {
		return fmt.Errorf("DestroyObject: %w", err)
	}
	// Drop every cached handle for this label (all classes) so a
	// subsequent operation triggers a fresh FindObjects (which
	// will fail — by design, Shred is final).
	for k := range cc.keyCache {
		if k.label == keyLabel {
			delete(cc.keyCache, k)
		}
	}
	return nil
}

// DescribeKey reads a curated set of object attributes for
// the inspector surface.  We deliberately don't read sensitive
// attributes (CKA_VALUE / CKA_PRIVATE_EXPONENT) — those are
// either non-extractable or wouldn't fit a JSON dump.
func (cc *cgoClient) DescribeKey(_ context.Context, keyLabel string) (map[string]any, error) {
	key, err := cc.findAnyKey(keyLabel)
	if err != nil {
		return nil, err
	}
	wanted := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, nil),
		pkcs11.NewAttribute(pkcs11.CKA_ID, nil),
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS_BITS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE_LEN, nil),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, nil),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, nil),
	}
	got, err := cc.ctx.GetAttributeValue(cc.session, key, wanted)
	if err != nil {
		return nil, fmt.Errorf("GetAttributeValue: %w", err)
	}
	out := map[string]any{}
	for _, a := range got {
		switch a.Type {
		case pkcs11.CKA_CLASS:
			out["class"] = uintFromAttr(a)
		case pkcs11.CKA_KEY_TYPE:
			out["key_type"] = uintFromAttr(a)
		case pkcs11.CKA_LABEL:
			out["label"] = string(a.Value)
		case pkcs11.CKA_ID:
			out["id_hex"] = fmt.Sprintf("%x", a.Value)
		case pkcs11.CKA_MODULUS_BITS:
			out["modulus_bits"] = uintFromAttr(a)
		case pkcs11.CKA_VALUE_LEN:
			out["value_len"] = uintFromAttr(a)
		case pkcs11.CKA_TOKEN:
			out["token"] = boolFromAttr(a)
		case pkcs11.CKA_EXTRACTABLE:
			out["extractable"] = boolFromAttr(a)
		case pkcs11.CKA_SENSITIVE:
			out["sensitive"] = boolFromAttr(a)
		}
	}
	return out, nil
}

// Close releases this provider's session. The token is logged out and
// the module finalised only when this was the last user in the process
// (see module_share.go). Idempotent.
func (cc *cgoClient) Close() error {
	if cc.lease == nil {
		return nil
	}
	err := cc.lease.close()
	cc.lease = nil
	cc.ctx = nil
	return err
}

// buildMech builds a pkcs11.Mechanism for the requested wrap
// algorithm.  Returns a cleanup func the caller must defer
// (only material for AES-GCM, where pkcs11.GCMParams holds
// C-allocated memory that must be Free'd after Encrypt/
// Decrypt completes).
func buildMech(mech Mechanism, iv []byte) (*pkcs11.Mechanism, func(), error) {
	switch mech {
	case MechAESGCM:
		// CK_GCM_PARAMS: pIv + ulIvBits + pAAD(0) + ulTagBits.
		// Vendor compatibility note: SoftHSM2, NSS, and most
		// HSM vendors interpret this as PKCS#11 v2.40
		// specifies; AWS CloudHSM had a deprecated v1 layout
		// (callers needing CloudHSM should update firmware
		// to a v2.40-compliant version).
		gcm := pkcs11.NewGCMParams(iv, nil, aesGCMTagLen*8)
		return pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, gcm), func() { gcm.Free() }, nil
	case MechRSAOAEP:
		// SHA-256 + MGF1-SHA-256 + empty source data.
		oaep := pkcs11.NewOAEPParams(pkcs11.CKM_SHA256, pkcs11.CKG_MGF1_SHA256,
			pkcs11.CKZ_DATA_SPECIFIED, nil)
		return pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS_OAEP, oaep), func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("pkcs11: unsupported mech %q", mech)
	}
}

// uintFromAttr decodes a CKA_* numeric attribute.  PKCS#11
// returns these as little-endian native CK_ULONGs.
func uintFromAttr(a *pkcs11.Attribute) uint64 {
	var v uint64
	for i, b := range a.Value {
		if i >= 8 {
			break
		}
		v |= uint64(b) << (8 * i)
	}
	return v
}

// boolFromAttr decodes a CKA_* boolean attribute.
func boolFromAttr(a *pkcs11.Attribute) bool {
	for _, b := range a.Value {
		if b != 0 {
			return true
		}
	}
	return false
}

// trimNullsSpaces trims null bytes and trailing spaces from
// a fixed-width PKCS#11 label string.
func trimNullsSpaces(s string) string {
	for len(s) > 0 && (s[len(s)-1] == 0 || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
