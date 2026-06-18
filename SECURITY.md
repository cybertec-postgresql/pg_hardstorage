# Security Policy

## Reporting a vulnerability

**Do not open a public GitHub issue for security findings.**

Use the **Report a vulnerability** button under the repository's
**Security** tab — it opens a private GitHub Security Advisory draft
visible only to the maintainer. Provide:

- A clear description of the issue and the impact you observed.
- A minimal reproducer (preferably a `pg_hardstorage_testkit`
  scenario file, but a plain shell script + DSN works).
- The version (`pg_hardstorage version` output) and environment.
- Any disclosure timeline you'd like us to honour.

We commit to:

- Acknowledging receipt within **5 business days**.
- A first triage assessment within **10 business days**.
- A public advisory and patched release coordinated with the reporter,
  by default no later than **90 days** from the initial report.

## Supported versions

| Version | Status                                |
|---------|---------------------------------------|
| 0.1.x   | actively patched                      |
| < 0.1   | unsupported (pre-release scaffolding) |

The 24-month manifest schema and CLI v1 contract apply across
patched versions: a security fix never breaks operator scripting or
restorability of existing backups.

## Crypto policy

- Manifest signatures: Ed25519, public key embedded.
- Chunk encryption: AES-256-GCM, per-backup DEK, KEK at rest.
- KMS plugins: AWS KMS in v0.1; GCP KMS, Azure Key Vault, Vault
  Transit, PKCS#11/HSM, TPM in v0.5+.
- All release artefacts are cosign-signed (Sigstore keyless via
  GitHub Actions OIDC). Verify before deploying — the README contains
  a worked example.

## Hardening expectations

The systemd units we ship apply the standard hardening set
(`NoNewPrivileges=yes`, `ProtectSystem=strict`, `PrivateTmp=yes`,
`SystemCallFilter=@system-service`, etc.). If you're running outside
systemd, the equivalent posture is documented in
[docs/operator-guide.md](docs/operator-guide.md).
