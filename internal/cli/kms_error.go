package cli

import (
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// kmsOpError codes an error returned by a KMS provider operation. A
// network-reachability failure against the KMS endpoint is coded
// `kms.unreachable` so the CLI exits with ExitUnreachable (8) — the
// documented contract (see docs/reference/exit-codes.md) and the mirror of
// `storage.unreachable` for a failed PostgreSQL/storage connection.
// Everything else (wrong key, permission denied, key pending deletion)
// keeps the operation's fallback code.
//
//   - op is the human prefix ("kms verify", "backup: open cloud KMS for X").
//   - fallbackCode is the structured code used when the error is not a
//     reachability failure ("kms.verify_failed", "backup.kms_open_failed").
//   - fallbackSuggestion, when non-nil, is attached to the fallback (non-
//     reachability) error — e.g. the backup path's "check the KEKRef +
//     --kms-config" hint. The reachability case carries its own retry hint.
func kmsOpError(err error, op, fallbackCode string, fallbackSuggestion *output.Suggestion) error {
	if kms.IsUnreachable(err) {
		return output.NewError("kms.unreachable",
			fmt.Sprintf("%s: KMS provider unreachable: %v", op, err)).
			WithSuggestion(&output.Suggestion{
				Human: "verify network reachability, endpoint, and credentials for the KMS provider, then retry — this exit code (8) signals a transient infrastructure failure, not a misconfiguration.",
			}).Wrap(err)
	}
	e := output.NewError(fallbackCode, fmt.Sprintf("%s: %v", op, err))
	if fallbackSuggestion != nil {
		e = e.WithSuggestion(fallbackSuggestion)
	}
	return e.Wrap(err)
}
