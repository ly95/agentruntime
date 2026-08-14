## Summary

Describe the runtime problem and the change that solves it.

## Runtime contracts

- [ ] The change remains business-neutral and does not add host-application behavior.
- [ ] Invalid configuration and ambiguous outcomes fail explicitly.
- [ ] Idempotency, session fencing, approval-resume, verification, and execution-transition invariants are preserved where applicable.
- [ ] Public API, persisted-state, and provider-transport compatibility impacts are documented.

## Verification

- [ ] `go test ./...`
- [ ] `go vet ./...`
- [ ] Tests cover relevant failure, cancellation, retry, or reconciliation paths.
- [ ] Public documentation and examples are updated when behavior changes.

## Additional context

Include migration notes, tradeoffs, or follow-up work that reviewers should know.
