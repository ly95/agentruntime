# Repository Rules

## Scope

This repository owns only the business-neutral Go agent runtime. It must not
contain application HTTP handlers, database implementations, queue workers,
billing rules, product prompts, or domain tools.

## Design

- Keep the model loop, operation contracts, MCP integration, approvals,
  persistence ports, reconciliation, context management, and provider
  transports independent of any host application.
- Host-owned behavior enters only through explicit interfaces and immutable
  constructor configuration.
- Fail explicitly on invalid configuration, unsupported provider output,
  missing resources, or ambiguous operation outcomes.
- Do not silently retry, downgrade, truncate, or substitute a caller choice.
- Preserve idempotency, session fencing, approval-resume, and execution
  transition invariants when changing the runtime.
- Add abstractions only for current runtime behavior; do not add business
  extension points speculatively.
- Do not leave TODO, FIXME, placeholder packages, or empty commands.

## Verification

Run `go test ./...` and `go vet ./...` before handing off changes.
