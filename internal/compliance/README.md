# compliance/

Control-framework mappings: SOC 2, ISO 27001, HIPAA, PCI DSS, FedRAMP. Renders
both JSON and Markdown reports against a window of audit-log evidence.

## What lives here

A `Control` is one assessed item ("CC6.1: logical-access controls"). A `Report`
is a window-of-time rollup of audit events into pass / fail / not-applicable
verdicts for each control. Framework strings are stable (`soc2`, `iso27001`,
`hipaa`, `pci_dss`, `fedramp`) — downstream consumers can pivot reports by
framework with a 24-month BC guarantee.

## Key files

- `controls.go` — `Framework`, `Control`, `ControlStatus`, evaluation
  primitives
- `report.go` — `Report` builder; walks the audit chain over a window
- `markdown.go` — `Render` to Markdown for human evidence packets
- `controls_test.go` / `controls_integration_test.go` — control evaluation +
  end-to-end window rollup
- `report_test.go` — report shape + framework pivots

## Read next

- `../dsa/README.md` — GDPR-specific evidence path (Article 15 / 17)
- `../audit/README.md` — the source of truth this package rolls up
- `../recovery/README.md` — recovery-readiness scorecards feed into BCP / DR
  controls
- `../../docs/compliance/` — user-facing framework guides
  (`soc2-control-mapping.md`, `iso-27001-control-mapping.md`, `hipaa.md`,
  `pci-dss.md`, `fedramp.md`)
- `../README.md` — parent index

## Don't put X here

- Audit-event production — that's `internal/audit/`; this package only
  consumes.
- Live policy enforcement — controls are reported, not enforced; enforcement
  lives at the call site (`approval/`, `jit/`, `threshold/`).
