package runner

import (
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

// LocalEncryptionFromKeyring mirrors the CLI's local-custody
// auto-encryption decision for non-interactive callers (the agent's
// schedule engine and the control-plane executor): when a KEK exists
// at keyringDir the backup MUST encrypt with it; when none exists the
// backup is plaintext, exactly like `pg_hardstorage backup` without
// flags on the same host.
//
// This exists because the agent paths used to build TakeOptions with
// no Encryption at all — every scheduled backup was silently
// plaintext even in a repo initialised with --encrypt. Plaintext-hash
// dedup then welds those manifests onto encrypted chunks (and vice
// versa): manifests claiming aes-256-gcm can reference cleartext
// chunks, breaking the crypto-shred guarantee, and unencrypted
// manifests can reference GCM envelopes they can never decrypt.
func LocalEncryptionFromKeyring(keyringDir string) (*EncryptionConfig, error) {
	if !keystore.KEKExists(keyringDir) {
		return nil, nil
	}
	kek, _, err := keystore.LoadOrGenerateKEK(keyringDir)
	if err != nil {
		return nil, fmt.Errorf("load KEK from keyring: %w", err)
	}
	return &EncryptionConfig{KEK: kek, KEKRef: keystore.KEKRefLocal}, nil
}
