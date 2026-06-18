---
title: How-to guides
description: Task-oriented recipes for steady-state operators.
---

# How-to guides

Task-oriented, problem-solving recipes.  Pick the page that
matches the question you're trying to answer right now.

How-to guides differ from
[tutorials](../tutorials/getting-started.md) in scope: a
tutorial walks an entire end-to-end flow; a how-to assumes
you already understand the system and tells you the
**specific steps** for one task.

---

## Adding a resource

Wire a new repository, KMS provider, sink, or deployment.

### Deployments

- [Add a deployment](adding/deployment.md)

### Repositories

- [S3](adding/repository-s3.md)
- [Azure Blob](adding/repository-azblob.md)
- [GCS](adding/repository-gcs.md)
- [SFTP](adding/repository-sftp.md)
- [SCP (ssh-exec)](adding/repository-scp.md)

### KMS providers

- [AWS KMS](adding/kms-aws.md)
- [GCP KMS](adding/kms-gcp.md)
- [Azure Key Vault](adding/kms-azure.md)
- [HashiCorp Vault Transit](adding/kms-vault.md)
- [PKCS#11 / HSM](adding/kms-hsm-pkcs11.md)

### Sinks

- [Slack](adding/sink-slack.md)
- [Jira](adding/sink-jira.md)
- [PagerDuty](adding/sink-pagerduty.md)
- [Webhook](adding/sink-webhook.md)
- [syslog](adding/sink-syslog.md)
- [Other sinks (CEF / Datadog / Email / Teams / Discord / OpsGenie / ServiceNow / SplunkHEC / OTel events)](adding/sink-other.md)

---

## Operating a deployment

Day-2 mutations on a running deployment.

- [Configure pg_hardstorage with a file](operating/configuration-file.md)
- [Set retention policy](operating/set-retention.md)
- [Schedule backups](operating/schedule-backups.md)
- [Verify — fast vs full](operating/verify-fast-vs-full.md)
- [Scrub the repo for bit-rot](operating/scrub-and-heal.md)
- [Rotate the KEK](operating/rotate-kek.md)
- [Crypto-shred a tenant (GDPR Art. 17)](operating/crypto-shred.md)
- [Apply a legal hold](operating/legal-hold.md)
- [Pin data to a region (residency)](operating/data-residency.md)
- [n-of-m approvals for destructive ops](operating/n-of-m-approvals.md)
- [Configure the LLM helper](configure-llm.md)
- [Install on Windows](windows-install.md)

---

## Kubernetes

- [Local drop-in QA run (kind / minikube)](k8s-quickstart.md)
- [CNPG-I provider](kubernetes/cnpg-i-provider.md)
- [WAL-G shim (Zalando)](kubernetes/walg-shim.md)
- [pgBackRest shim (Crunchy)](kubernetes/pgbackrest-shim.md)
- [Helm: control-plane chart](kubernetes/helm-server-chart.md)
- [Helm: sidecar chart](kubernetes/helm-sidecar-chart.md)

---

## Air-gapped operation

- [Overview](air-gapped/index.md)
- [Enable air-gap policy](air-gapped/enable-policy.md)
- [Export a repo bundle](air-gapped/repo-bundle-export.md)
- [Import a repo bundle on the destination side](air-gapped/transport-bundle-import.md)

---

## Packaging

- [Overview](packaging/index.md)
- [Build from source](packaging/build-from-source.md)
- [Debian / RPM](packaging/debian-rpm.md)
- [FIPS variant](packaging/fips-variant.md)
- [PKCS#11 / HSM variant](packaging/pkcs11-variant.md)
- [Firecracker microVM-sandbox variant](packaging/firecracker-variant.md)

---

## Migration

- [From pgBackRest](migration/from-pgbackrest.md)
- [From WAL-G](migration/from-walg.md)
- [From Barman](migration/from-barman.md)

---

## Verifier sandbox

- [Docker sandbox (default)](verify/docker-sandbox.md)
- [Firecracker microVM sandbox](verify/firecracker-sandbox.md)
