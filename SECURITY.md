# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
— the **Security** tab on this repository, then **Report a vulnerability**.

Please do not open a public issue for a suspected vulnerability.

This is a best-effort project with no support commitment or paid maintenance.
Reports are read and acted on as time allows; there is no response-time
guarantee. If a report is urgent and unanswered, say so in a follow-up.

## Supported versions

Only the latest release. There are no maintained release branches.

## What is in scope

The plugin holds a privileged position, so the interesting surfaces are:

- **The admission webhook.** It mutates Clusters at creation. A path that let a
  restore write `spec.plugins`, `spec.backup`, `spec.externalClusters` or
  `spec.bootstrap` would be a real finding — those are refused by design,
  because restoring them would point two clusters at one WAL archive.
- **Snapshot handling.** Snapshots are read from an object store and applied to
  a Cluster spec. Anything that turns a malformed or hostile snapshot into
  unexpected writes, a panic in the webhook, or resource exhaustion is in scope.
- **CEL transforms.** Expressions come from a `RestorePolicy` and run inside
  the webhook. They are cost-limited; a way to exceed that bound, or to reach
  outside the expression environment, is in scope.
- **Credential handling.** The plugin can read Secrets cluster-wide (see
  [Permissions](README.md#permissions)). Anything that logs, stores or
  transmits credentials beyond the object-store request is in scope.

## What is not

- The RBAC breadth itself. That the `ClusterRole` can read Secrets cluster-wide
  is a documented design trade, not a vulnerability; `README.md` describes how
  to narrow it.
- Anything requiring an attacker who can already create or edit `RestorePolicy`
  resources or Clusters in the target namespace. That is an operator, and an
  operator can already set the spec directly.
- Vulnerabilities in CloudNativePG, cert-manager or the object store. Report
  those upstream.
