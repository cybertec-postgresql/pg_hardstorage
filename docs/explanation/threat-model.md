---
title: Threat model
description: Attacker capabilities the design defends against, and what is explicitly out of scope.
tags:
  - security
  - threat-model
  - compliance
---

# Threat model

`pg_hardstorage` is a backup system that holds your most
sensitive durable state — every row in your production database,
plus enough history to reconstruct it.  This page is an honest
articulation of **what the design defends against**, **what it
doesn't**, and where the operator's own controls have to take
over.

The threat model is structured by attacker capability.  A defence
that holds against a stronger attacker also holds against weaker
ones.

---

## Attacker capabilities (in increasing strength)

| # | Capability | Defended? |
| --- | --- | --- |
| 1 | Network-only attacker (passive sniff, MITM) | Yes |
| 2 | Cloud-storage misconfiguration (bucket made public) | Yes |
| 3 | Stolen backup tape / object-store dump | Yes |
| 4 | Compromised storage backend (full read of the bucket) | Yes |
| 5 | Compromised storage backend with write access | Mostly |
| 6 | Single tenant compromise (one customer's KMS principal) | Yes (containment) |
| 7 | Compromised LLM provider | Yes |
| 8 | Compromised agent process at runtime | Partial |
| 9 | Compromised KMS principal (full access to one tenant's KEK) | No (this *is* the access) |
| 10 | Compromised binary build / supply chain | Out of scope (cosign / SLSA helps) |
| 11 | Quantum cryptanalysis | Out of scope (revisit on PQC standardisation) |
| 12 | Insider with full RBAC + KMS + bucket | No (audit log is the response) |

Each row is a separate attacker model.  The design defends each
row independently — defeating one capability doesn't unlock the
next.

---

## Capability 1 — network-only attacker

**What they can do:** Passive sniffing of agent ↔ PG, agent ↔
storage, agent ↔ KMS.  Active MITM of any of those.

**Defences:**

- All connections use TLS 1.2+.  PG connections require
  `sslmode=verify-full` by default in the `init` wizard; the
  binary refuses to start with `sslmode=disable` unless
  `--insecure` is passed (and that flag is audited).
- mTLS on the control-plane ↔ agent path.  Identity is
  `(host_fqdn, agent_uuid)`, with both sides validated.
- Agent ↔ KMS uses cloud-IAM-signed requests (SigV4 / GCP IAM /
  Azure RBAC) over TLS.  No long-lived secrets in transit.
- WAL on the wire is the streaming replication protocol's binary
  framing, encrypted by TLS.  No unencrypted variants exist.

The network-only attacker sees encrypted bytes, learns connection
endpoints and timing, and that's it.

---

## Capability 2 — cloud-storage misconfiguration

**What they can do:** A misconfigured bucket policy makes the
backup bucket world-readable.  An attacker with no other access
walks in.

**Defences:**

- Every chunk is individually encrypted with a per-chunk key
  derived from the BDEK.  See [envelope encryption]
  (envelope-encryption.md).
- The BDEK is wrapped by the per-tenant RKEK in the KMS.  The
  bucket alone yields no key material.
- Chunk keys are derived from plaintext SHA-256, which means the
  bucket's chunk-name distribution is *also* a function of the
  plaintext — but the per-tenant FastCDC salt makes this useless
  for cross-tenant fingerprinting.

The attacker walks out with ciphertext.  Without KMS access, they
have no path to plaintext.  See [content-addressed storage]
(content-addressed-storage.md) for the chunk-key derivation.

---

## Capability 3 — stolen backup tape / object-store dump

Same as Capability 2 with the additional twist that the attacker
has *all* the data, including the manifests.  The manifests
contain the wrapped BDEKs.

**Defences:** Same as Capability 2.  Manifests reveal:

- Backup metadata (deployment name, sizes, timestamps).  Not
  plaintext.
- Wrapped BDEKs.  Useless without the corresponding RKEK in the
  KMS.
- Cosign signatures.  Useless to an attacker except to verify the
  manifests came from a binary holding the signing key — which
  *they don't have*.

This is the model the encryption posture is sized against.  An
attacker who walks off with last night's S3 export gets nothing
they can decrypt.

---

## Capability 4 — compromised storage backend (read)

**What they can do:** Read every object in the bucket on demand.
Watch new objects arrive.

**Defences:** Same as Capability 3, plus the audit chain
**inside the bucket** is encrypted at rest the same way as
chunks; the events themselves are JSON but the WORM protections
on the audit prefix prevent attacker rewrites.

The attacker can correlate timing (a backup committed at 09:00
UTC) and sizes (this deployment's chunks took up 14 GB), but no
content.

---

## Capability 5 — compromised storage backend (write)

**What they can do:** Read every object, plus modify or delete
any object.

**Defences:**

- **Per-chunk authentication.**  AES-256-GCM is an AEAD
  cipher.  Substituting a chunk's ciphertext for a different
  ciphertext fails authentication on read — the agent rejects
  the modified chunk with a structured error.  Restore aborts.
- **Manifest signatures.**  Every manifest is signed with
  Ed25519; the signing key never leaves the agent.  Forging a
  manifest requires the signing key.
- **Manifest replicas.**  Every manifest is written *twice* —
  once at the canonical path, once at a replica prefix.  An
  attacker who modifies the canonical copy has to also modify the
  replica, and the verifier reads both.
- **Audit chain.**  Every modification (legitimate or otherwise)
  appends to the hash-chained audit log.  An attacker who
  modifies in-bucket events has to recompute every hash from the
  modification point to the head, and once a transparency-log
  anchor has gone out, even that doesn't work.

What this *doesn't* defend against: the attacker can **delete**
any backup outright.  WORM (S3 Object Lock, Azure immutable blob,
NetApp SnapLock) is the defence — it makes deletion physically
impossible at the storage layer until the retention date passes.
Production deployments should configure WORM for the audit prefix
at minimum, and for the manifest + chunk prefixes when retention
policy permits.

---

## Capability 6 — single tenant compromise

**What they can do:** An attacker compromises one customer's
KMS principal.  They can decrypt that tenant's BDEKs.

**Defences:** This is *containment*, not prevention.  The
per-tenant KEK design means:

- Other tenants are unaffected — their RKEKs live under different
  KMS principals.
- The compromise is bounded: revoke the compromised principal,
  rotate the affected RKEK, the attacker loses access to *future*
  backups immediately and to in-progress restores within the KMS
  cache TTL.
- The audit log shows every BDEK unwrap during the compromise
  window; you know exactly which backups were exposed.

This is what the per-tenant architecture is *for*: a multi-
tenant SaaS doesn't experience a one-tenant compromise as a
whole-system breach.

---

## Capability 7 — compromised LLM provider

**What they can do:** The LLM provider returns crafted responses
designed to convince the operator to run destructive commands,
or includes prompt injections in its responses to other tools.

**Defences:** The full [LLM safety stack](llm-safety-stack.md) —
five gates plus anomaly refusal:

- The LLM cannot construct a command string that bypasses
  `preview_command`.
- Destructive operations require typed confirmation; the LLM
  cannot type for the operator.
- The LLM never bypasses RBAC.
- An LLM proposing a command wildly inconsistent with the
  just-prior context is refused by the anomaly layer.
- Local-only providers (Ollama, llama-cpp) are an option for
  operators who don't want to trust any external provider.

A maliciously cooperative LLM provider can at most *suggest*
harmful commands; the gates prevent execution.

---

## Capability 8 — compromised agent process

**What they can do:** Arbitrary code execution inside the
agent process.  They see whatever the agent sees in memory,
including BDEKs in flight.

**Defences:**

- **Crash-only design** keeps the in-memory window small.  No
  long-lived secrets in process memory.
- **mlock'd buffers** for BDEKs and per-chunk keys where the OS
  supports it.  Disk swap can't expose them.
- **cgroup self-limits** keep the agent below a configured memory
  ceiling, reducing the surface for memory-sniffing attacks.
- **Self-supervised parent + worker** isolates the small parent
  process from the busy worker; a worker compromise doesn't
  immediately escalate to the parent.
- **Race detector + static analysis gates** in CI raise the cost
  of getting an exploitable bug into the binary.
- **SLSA Level 3 build provenance** lets a paranoid operator
  verify the binary came from the published source tree.

What this *doesn't* defend against: an attacker with persistent
code execution in the agent will eventually see plaintext data
flowing through.  This is fundamental — the agent has to handle
plaintext to encrypt and store it.  The defence is to *minimise
the window*, *make compromise visible* (panic capture, audit
events, anomalous behaviour metrics), and *segregate trust*
(separate KMS principal per tenant; FIPS build for regulated
workloads).

---

## Capability 9 — compromised KMS principal

**What they can do:** Full access to one tenant's RKEK.

This is **the access**.  An authorised principal acting
maliciously is by definition not stoppable by access controls —
they have the controls.

**Defences:** Detection and audit.

- Every unwrap is logged with the principal making the call.
- Anomalous patterns (off-hours bulk reads, novel principals,
  unusual download volume) trigger alerts via the configured
  Sinks.
- The hash-chained, transparency-log-anchored (self-hosted) audit
  chain means the attacker cannot silently rewrite the evidence
  after the fact.

The customer's external IAM and threat-detection systems are
expected to cover this case.  We provide the evidence trail.

---

## Capability 10 — compromised binary build / supply chain

Out of scope for the binary's defences in any meaningful
mathematical sense — a compromised build is a compromised build.

**What helps:**

- Cosign-signed releases.  Operators verify the binary signature
  before deployment.
- SLSA Level 3 build provenance.  The build is reproducible from
  the published source tree.
- Reproducible builds (`-trimpath -buildvcs=false`, pinned
  toolchain).  Independent third parties can verify.

The threat model shifts from "trust the binary" to "trust the
signing key + the build chain", which is a smaller and more
auditable surface.

---

## Capability 11 — quantum cryptanalysis

Out of scope.  AES-256 is widely believed to retain ~128 bits of
post-quantum security.  Ed25519 manifest signatures are not
post-quantum safe in the long-term sense.

We will revisit when NIST PQC standardisation is mature enough
that production deployments are practical.  The chunk envelope
versioning lets us add a new cipher mode without breaking older
readers.

---

## Capability 12 — full insider

An insider with RBAC, KMS access, and bucket access can do
anything they want.  Defending against this with
software-architectural controls is a category error.

**What we do provide:**

- **Hash-chained audit log.**  Every action is recorded.
- **Transparency-log anchoring** (on-disk envelope shipped
  in v1.0; periodic publish loop is post-v1.0 work tracked in
  [SPEC_DRIFT.md](../SPEC_DRIFT.md)).  Once anchors go out, an
  insider with full bucket access cannot silently rewrite
  history.
- **n-of-m approval for destructive operations.**  A single
  insider cannot `kms shred` or `repo gc --delete` alone.
- **JIT access.**  Time-bound elevated tokens for break-glass
  operations; auto-expire; audit-stamped.
- **Insider-threat anomaly detection.**  Unusual download
  patterns, novel IAM principals, off-hours bulk reads → alert.

The audit log is what catches the misuse.  The customer's HR /
legal / compliance processes are what respond to it.

---

## What the threat model assumes the operator does

The defences above only work if the operator does their part:

- **Configure WORM** for the audit prefix at minimum.  Without
  WORM, an attacker with bucket-write capability can delete the
  evidence of their own actions.
- **Use cloud-IAM-based KMS access** (or HSM in regulated
  environments).  Long-lived API keys for the KMS undercut the
  per-tenant containment story.
- **Verify the binary signature** on every install.  Running an
  unverified binary is opting into Capability 10.
- **Run `audit verify-chain` periodically.**  The verifier exists
  precisely so an operator can confirm the chain hasn't been
  tampered with.  Compliance teams should automate this.
- **Use the principle of least privilege for RBAC.**  The LLM
  helper inherits the operator's permissions; if every operator
  has `admin:*`, every LLM session has `admin:*`.

These are the operator's load-bearing actions.  The
documentation surfaces them in [the operator guide]
(../operations/operator-guide.md) and the [compliance pages]
(../compliance/index.md).

---

## Further reading

- [Design principles](design-principles.md) — the choices that
  shaped this threat model.
- [Envelope encryption](envelope-encryption.md) — Capabilities 2-6
  in cipher form.
- [Audit chain](audit-chain.md) — Capability 5 and 12 in
  evidence form.
- [LLM safety stack](llm-safety-stack.md) — Capability 7 in gate
  form.
- [Architecture tour](architecture-tour.md) — the broader system
  the threat model defends.
