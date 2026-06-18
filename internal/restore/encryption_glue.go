// encryption_glue.go — buildEncryptedCAS: wires the manifest's KEK/DEK envelope into a decrypting CAS.
package restore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// buildEncryptedCAS constructs a CAS that can decrypt the chunks described by
// info. The DEK is obtained one of two ways, by KEKRef scheme:
//
//   - Cloud KMS (aws-kms / gcp-kms / azure-kv / vault-transit / pkcs11):
//     the DEK is unwrapped server-side via unwrapDEK — the KEK never leaves
//     the HSM, so the local-bytes kekForRef cannot resolve it.
//   - Local custody ("" / "local:default"): the proven path — kekForRef
//     resolves the 32-byte KEK from the keyring and we AES-GCM-unwrap the
//     DEK in-process.
//
// Error codes:
//   - nil resolver for the relevant scheme → config.no_kek_resolver
//     (ExitMisuse — an invocation/config error)
//   - cloud KMS unreachable          → kms.unreachable (ExitUnreachable)
//   - cloud unwrap auth failure      → restore.kek_mismatch
//   - cloud resolve failure (other)  → restore.kek_resolve_failed
//   - local resolve / unwrap failure → restore.kek_resolve_failed / kek_mismatch
func buildEncryptedCAS(
	ctx context.Context,
	sp storage.StoragePlugin,
	info *backup.EncryptionInfo,
	kekForRef func(string) ([encryption.KeyLen]byte, error),
	unwrapDEK func(context.Context, string, []byte) ([]byte, error),
) (*repo.CAS, error) {
	if info == nil {
		return nil, errors.New("restore: buildEncryptedCAS called with nil EncryptionInfo")
	}
	if info.Scheme != "aes-256-gcm" {
		return nil, output.NewError("restore.unknown_scheme",
			fmt.Sprintf("restore: unsupported encryption scheme %q", info.Scheme)).
			WithSuggestion(&output.Suggestion{
				Human: "this backup may have been written by a future pg_hardstorage version with a new encryption codec; upgrade and try again",
			})
	}
	wrapped, err := base64.StdEncoding.DecodeString(info.WrappedDEK)
	if err != nil {
		return nil, output.NewError("restore.bad_wrapped_dek",
			fmt.Sprintf("restore: wrapped_dek not valid base64: %v", err)).Wrap(err)
	}

	// Cloud KMS branch: the DEK is unwrapped server-side.
	if scheme := kms.SchemeOf(info.KEKRef); scheme != "" && scheme != "local" {
		if unwrapDEK == nil {
			// Reachable on the control-plane / agent path when a host with no
			// cloud-KMS resolver claims a cloud-KMS-encrypted restore. ErrUsage
			// → ExitMisuse, like the local no-resolver case.
			return nil, output.NewError("config.no_kek_resolver",
				fmt.Sprintf("restore: backup is wrapped with a cloud KMS KEK (%q) but no cloud-KMS unwrap resolver was provided", info.KEKRef)).
				WithSuggestion(&output.Suggestion{
					Human: "restore from a host with the matching KMS plugin + credentials; the CLI wires this automatically (use --kms-config for endpoint/region overrides)",
				}).Wrap(output.ErrUsage)
		}
		dek, err := unwrapDEK(ctx, info.KEKRef, wrapped)
		if err != nil {
			switch {
			case kms.IsUnreachable(err):
				return nil, output.NewError("kms.unreachable",
					fmt.Sprintf("restore: cloud KMS for %q unreachable: %v", info.KEKRef, err)).
					WithSuggestion(&output.Suggestion{
						Human: "verify network reachability + credentials for the KMS provider, then retry — exit 8 signals a transient infrastructure failure.",
					}).Wrap(err)
			case errors.Is(err, kms.ErrUnwrap):
				return nil, output.NewError("restore.kek_mismatch",
					fmt.Sprintf("restore: cloud KMS could not unwrap the DEK for %q: %v", info.KEKRef, err)).
					WithSuggestion(&output.Suggestion{
						Human: "the KEK referenced by this backup may have been rotated, disabled, or scheduled for deletion; confirm the key is still active",
					}).Wrap(err)
			default:
				return nil, output.NewError("restore.kek_resolve_failed",
					fmt.Sprintf("restore: resolve DEK via cloud KMS %q: %v", info.KEKRef, err)).Wrap(err)
			}
		}
		enc, err := aesgcm.New(dek)
		if err != nil {
			return nil, output.NewError("internal",
				fmt.Sprintf("restore: build aes-gcm encryptor: %v", err)).Wrap(err)
		}
		return casdefault.NewEncrypted(sp, enc), nil
	}

	// Local-custody path.
	if kekForRef == nil {
		// "encrypted backup, no key supplied" is an invocation/config error →
		// ExitMisuse. config.* alone falls through to ExitError; the ErrUsage
		// wrap is what makes ExitMisuse real (ExitCodeFor checks it first).
		return nil, output.NewError("config.no_kek_resolver",
			"restore: this backup is encrypted but no KEK resolver was provided").
			WithSuggestion(&output.Suggestion{
				Human: "set Options.KEKForRef (or pass --kek-file via the CLI) so we can resolve the manifest's KEKRef to the actual key",
			}).Wrap(output.ErrUsage)
	}
	kek, err := kekForRef(info.KEKRef)
	if err != nil {
		return nil, output.NewError("restore.kek_resolve_failed",
			fmt.Sprintf("restore: resolve KEK %q: %v", info.KEKRef, err)).Wrap(err)
	}
	dek, err := encryption.Unwrap(kek, wrapped)
	if err != nil {
		return nil, output.NewError("restore.kek_mismatch",
			fmt.Sprintf("restore: unwrap DEK with KEK %q: %v", info.KEKRef, err)).
			WithSuggestion(&output.Suggestion{
				Human: "the supplied KEK doesn't match the one that wrapped this backup's DEK; check the keyring path and KEKRef",
			}).Wrap(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		return nil, output.NewError("internal",
			fmt.Sprintf("restore: build aes-gcm encryptor: %v", err)).Wrap(err)
	}
	return casdefault.NewEncrypted(sp, enc), nil
}
