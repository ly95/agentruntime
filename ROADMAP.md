# Roadmap and known limits

The v0.1 release establishes a deliberately small, host-owned model/tool
kernel. Roadmap work must preserve that boundary and start from a concrete
runtime contract.

## Adoption roadmap

- **Reference store conformance:** current `main` includes exported `RunStore`
  and `ExecutionStore` conformance suites plus a process-local reference
  implementation. See [ADR 0001](docs/adr/0001-store-adapter-conformance.md),
  the [store adapter guide](docs/store-adapter-guide.md), and
  [tracking issue #3](https://github.com/ly95/agentruntime/issues/3).
- **Remote MCP boundary:** decide whether a remote MCP adapter can be mapped
  into immutable `OperationRegistry` contracts without acquiring execution
  authority or silently refreshing schemas. See
  [ADR 0002](docs/adr/0002-remote-mcp-provider-boundaries.md) and
  [tracking issue #4](https://github.com/ly95/agentruntime/issues/4).
- **Additional model providers:** specify the validation, lifecycle corpus,
  stable error mapping, and immutable run binding required of another `Model`
  adapter. See
  [ADR 0002](docs/adr/0002-remote-mcp-provider-boundaries.md), the
  [provider compatibility guide](docs/provider-compatibility.md), and
  [tracking issue #5](https://github.com/ly95/agentruntime/issues/5).

## Known limits

- OpenAI Responses is the only bundled model adapter.
- Remote MCP discovery and execution are not implemented.
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
