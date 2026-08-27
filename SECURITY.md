# Security Policy

## Supported versions

`agentruntime` is pre-1.0. Security fixes are developed against `main` and released
in a new tag when necessary. The latest tagged release and the current `main`
branch are the supported versions; older pre-1.0 versions may not receive
backports.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting flow for this repository:

https://github.com/ly95/agentruntime/security/advisories/new

If that flow is unavailable, contact the maintainer privately through the
contact method on the [ly95 GitHub profile](https://github.com/ly95) without
including exploit details in a public message.

Include enough information to reproduce and assess the issue:

- affected version or commit;
- runtime configuration and operation effect;
- minimal reproduction or proof of concept;
- expected and observed behavior;
- impact on authorization, approval, persistence, execution, or data exposure;
- any suggested mitigation.

The maintainer will coordinate validation, remediation, release timing, and
disclosure with the reporter. Please keep details private until a fix or agreed
disclosure plan is available.

## Security-sensitive areas

Reports are especially useful when they involve:

- policy or capability bypass;
- approval-preview or approval-resume integrity;
- duplicate or un-fenced write execution;
- idempotency-key or plan-sealing violations;
- stale session or execution ownership;
- transcript, attachment, artifact, or trusted-context disclosure;
- JSON Schema validation or provider-output confusion;
- MCP transport-binding, schema-snapshot, metadata-header, or read-only
  authority confusion;
- incorrect reconciliation of an ambiguous operation result.

Host application policies, storage adapters, domain tools, and deployment
configuration are outside this repository unless the issue is caused by a
runtime contract or implementation defect.
