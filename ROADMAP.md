# Roadmap and known limits

The v0.1 release establishes a deliberately small, host-owned model/tool
kernel. Roadmap work must preserve that boundary and start from a concrete
runtime contract.

## Adoption roadmap

- **Reference store conformance — implemented:** current `main` includes exported
  `RunStore` and `ExecutionStore` conformance suites plus a process-local
  reference implementation. See [ADR 0001](docs/adr/0001-store-adapter-conformance.md),
  the [store adapter guide](docs/store-adapter-guide.md), and
  [tracking issue #3](https://github.com/ly95/agentruntime/issues/3).
- **Constrained remote MCP reads — implemented:** `mcpadapter` performs bounded
  startup discovery against MCP `2026-07-28`, freezes exact host-allowlisted
  read-only tools into immutable operations for execution through Runtime's
  existing policy/executor path. Direct `Snapshot.Execute` calls bypass that
  policy just like direct calls to any executor. Network transport, streaming
  limits, credentials, SSRF controls, snapshot deployment pins, and read-only
  attestation remain host-owned. See
  [ADR 0002](docs/adr/0002-remote-mcp-provider-boundaries.md), the
  [adapter guide](docs/mcp-adapter-guide.md), and
  [tracking issue #4](https://github.com/ly95/agentruntime/issues/4).
- **Additional model provider readiness — next design gate:** freeze the adapter
  contract, durable provider binding, lifecycle/error/usage/cancellation rules,
  and provider-neutral positive and negative conformance corpus before choosing
  or implementing a second provider. See
  [ADR 0002](docs/adr/0002-remote-mcp-provider-boundaries.md), the
  [provider compatibility guide](docs/provider-compatibility.md), and
  [tracking issue #5](https://github.com/ly95/agentruntime/issues/5).

## Known limits

- OpenAI Responses is the only bundled model adapter.
- `mcpadapter` supports only startup-frozen, synchronous, structured read tools.
  It does not persist snapshots, migrate durable catalogs, bundle
  HTTP/SSE/OAuth, execute remote writes, hot-refresh, subscribe, follow
  multi-round-trip input, run asynchronous tasks, perform adapter-level retries,
  or fall back. Host transports must enforce binding, read limits, and no-retry
  behavior at dispatch.
- There is no Codex Plugin runtime; Skills are explicit immutable instruction
  snapshots and supporting files are inert.
- Remote Skill fetch, including GitHub, is not implemented. The built-in source
  loads only explicit local directories.
- v0.1.0 does not include a public store conformance package. Current `main`
  includes a process-local reference implementation, not a durable database
  adapter.
- Tools are function-style read/write operations. Browser control, code
  execution, and asynchronous jobs are not generalized tool classes.
- The runtime does not provide HTTP handlers, queues, databases, approval UI,
  billing, product prompts, deployment infrastructure, or domain tools.

Unsupported behavior fails explicitly instead of being retried, downgraded,
substituted, or inferred.
