# ADR 0002: remote MCP and additional provider boundaries

Status: deferred design boundary; no capability is enabled by this ADR.

## Remote MCP

A remote MCP server combines transport identity, discovery, credentials,
availability, and potentially side-effect authority. Any proposal must pin
server identity and schemas, bound discovery, define reconnect and cancellation
semantics, preserve approval-safe operation contracts, and keep execution
behind `OperationPolicy`, `OperationExecutor`, plans, fencing, verification, and
reconciliation. Remote tools must not be injected directly as ambient runtime
authority.

## Additional model providers

Another provider implements `Model` only after closed provider-owned validation
maps its complete response lifecycle into the existing provider-neutral types.
Each adapter needs a golden lifecycle corpus, strict unknown-output rejection,
stable error categories, usage mapping, cancellation behavior, and an immutable
provider/adapter binding for a run. Runtime-wide fallback or automatic provider
downgrade is not acceptable.

## Tool classes

Browser control, code execution, remote MCP delegation, and asynchronous jobs
have different authority and completion models. They must not be represented by
speculative flags on `Operation`; each needs a concrete host behavior, commit
boundary, persistence state machine, cancellation model, and reconciliation
design before a public runtime abstraction is added.
