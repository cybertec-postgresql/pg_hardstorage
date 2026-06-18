// plugins_register.go — single side-effect-import point for Tier-1 KMS plugins (AWS, GCP, Azure, Vault, PKCS#11).
package cli

// This file is the single registration point for Tier-1 KMS
// plugins.  Importing for side-effect: each plugin's init()
// calls kms.DefaultRegistry.Register on its scheme.
//
// Tier-2 plugins are discovered at runtime via the
// $HSPLUGIN_PATH walk in installDispatcher; they don't need
// to be listed here.
//
// Adding a new Tier-1 KMS provider is a one-line change:
// drop the side-effect import in this file alongside the
// existing entries.  The dispatcher in
// resolveBackupEncryption auto-picks up any registered
// scheme.
import (
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/awskms"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/azurekv"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/gcpkms"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/pkcs11"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/vaulttransit"
)
