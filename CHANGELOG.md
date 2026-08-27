# Changelog

All notable changes are recorded here. The module follows Semantic Versioning;
before v1.0, a minor release may change public contracts when the release notes
include explicit compatibility guidance.

## [Unreleased]

### Added

- A concurrency-safe process-local `InMemoryStore` and exported `storetest`
  conformance suites for `RunStore` and `ExecutionStore`.
- Public result artifacts and approval summaries, outcome and provider-error
  classification, strict argument decoding, trusted input builders,
  reconciliation evidence helpers, context defaults, and an error-returning ID
  factory.
- Safe event dispatch, metrics mapping, OpenTelemetry integration, lease and
  reconciliation events, and provider token usage fields.
- Immutable Skill resources and an offline approval, retry, and reconciliation
  example.
- Atomic `OperationRegistry.RegisterAll` registration and a constrained
  `mcpadapter` for bounded MCP `2026-07-28` discovery of host-allowlisted,
  synchronous read tools with immutable binding/schema digests and structured
  result validation.

### Changed

- Terminal run events (`run_completed`, `run_waiting_user`, `run_failed`,
  `run_interrupted`, `run_cancelled`) are delivered after the session lease
  stops, and lease-renewal events are delivered on a dedicated path, so a
  blocking event observer can no longer stall lease renewal or keep a lease
  alive past a finished run. At most one renewal event may still be in flight
  after a run returns.
- Failed, interrupted, and cancelled runs atomically commit their error audit
  item or explicitly record `audit_missing`.
- OpenAI semantic validation is split by protocol domain and protected by a
  checked-in golden stream corpus.

### Removed

- Built-in GitHub Skill loading (`NewGitHubSource`, `GitHubFetcher`,
  `HTTPGitHubFetcher`, and related types). Mount Skills from explicit local
  directories; a host that needs remote content snapshots it itself.

### Deprecated

- `RuntimeConfig.NewID`; use `IDFactory` so generation failures are explicit.
- `ErrInsufficientCredits`; use `ErrProviderQuotaExceeded`.

### Security

- Added approval UI and log guidance, redaction helpers, strict provider
  authority validation, and immutable resource access rules.
- Remote MCP reads keep transport credentials and SSRF policy host-owned and
  strip untrusted schema annotation text before model exposure. Generated
  operations require explicit host read-only attestation and reject a
  contradictory negative server hint. The adapter rejects hot schema replacement,
  non-local references, MRTR, malformed header annotations, binding drift,
  configured snapshot-pin mismatch, and raw server/transport-error disclosure.
  Injected transports are contractually responsible for atomic binding checks,
  bounded reads, cancellation, and no transparent retry.

## [0.1.0] - 2026-08-25

### Added

- A business-neutral, embeddable model/tool kernel with explicit host-owned
  policy, approval, execution, verification, persistence, and reconciliation
  boundaries.
- JSON Schema-validated operation contracts, durable write plans, execution
  transitions, session leases, fencing, approval resume, context compaction,
  attachments, and immutable Skill snapshots.
- Strict OpenAI Responses lifecycle validation and provider-neutral model,
  result, artifact, error, and event contracts.

### Changed

- Operations are exposed directly as function tools from `OperationRegistry`;
  execution remains behind the host policy and executor interfaces.
- `Result` carries artifacts, stable error data, and pending-approval identity
  needed by host integrations.
- Go 1.26 is the minimum supported version.

### Removed

- The pre-release in-process MCP execution hop and Codex Plugin loader. The
  release does not claim to be a remote MCP client or plugin runtime.
- The unused billing-specific insufficient-credits error from the
  business-neutral runtime contract.

[Unreleased]: https://github.com/ly95/agentruntime/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ly95/agentruntime/releases/tag/v0.1.0
