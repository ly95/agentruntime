# ADR 0001: reference store conformance boundary

Status: accepted roadmap boundary; not implemented in v0.1.0.

## Context

`RunStore` and `ExecutionStore` define durability, identity, lease, approval,
plan, transition, and reconciliation invariants. Hosts need executable evidence
that an adapter preserves those contracts, but this repository must not own a
production database implementation.

## Decision

A future reference package may provide reusable conformance suites and a
process-local in-memory implementation as executable documentation. Production
adapters remain host or ecosystem packages. Conformance must exercise atomic
failure behavior, replay identity, monotonic revisions and timestamps, lease
generation fencing, approval resume, global execution identity, and uncertain
write reconciliation.

The reference implementation must be labeled non-durable and must not introduce
database selection, migrations, connection configuration, retries, or host
application repositories into agentruntime.

## Acceptance boundary

- Every test names the store invariant it proves.
- Failure paths verify that rejected calls do not partially mutate state.
- Adapter documentation maps each callback and atomic transition to the
  backend transaction/isolation model.
- The public API is driven by current runtime behavior, not speculative storage
  extension points.
