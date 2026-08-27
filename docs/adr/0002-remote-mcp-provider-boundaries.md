# ADR 0002: remote MCP and additional provider boundaries

Status: accepted for the constrained remote MCP read adapter; additional model
provider work remains behind a design and conformance gate.

## Context

A remote MCP server combines transport identity, discovery, credentials,
availability, untrusted schemas and text, and potentially side-effect authority.
Injecting discovered tools directly would bypass immutable operation contracts
and could let server metadata influence policy or approval. Remote writes also
cannot prove that `ExecutionID`, `AttemptID`, and `SessionLease` were checked
atomically at the side-effect commit boundary.

A second model provider has a separate risk: provider-specific lifecycle and
union shapes must map into the closed provider-neutral `Model` contract without
silently ignoring authority fields or changing a durable run's provider.

## Decision: constrained remote MCP reads

The repository includes `mcpadapter`, outside the root runtime package, with the
following fixed boundary:

- MCP revision `2026-07-28` is pinned. There is no downgrade, `initialize`
  fallback, protocol session, GET stream, or legacy HTTP+SSE path.
- The host injects a `Transport` and immutable, non-secret `BindingID`. The host
  owns HTTP/SSE or stdio framing, endpoint and redirect policy, credentials,
  SSRF controls, streaming response limits, cancellation, and the prohibition on
  transparent retries. Each request carries `ExpectedBindingID` so the transport
  can validate the exact binding atomically at dispatch.
- Startup discovery calls `server/discover`, then exhausts bounded `tools/list`
  pagination. Current-protocol `ttlMs`/`cacheScope` fields are required and page
  scopes must agree. Empty cursors remain valid; cycles, duplicates, malformed
  schemas, and exceeded bounds fail without truncation.
- `listChanged` is treated only as notification capability evidence. The adapter
  does not subscribe or hot-refresh. Once constructed, a snapshot is an
  immutable local runtime contract; hosts rediscover only when constructing a
  new catalog.
- The host supplies an exact allowlist, one-to-one operation mapping,
  host-authored description, contract-version token, capability labels, and an
  explicit read-only attestation. Server instructions, descriptions,
  annotations, and self-reported identity do not grant authority. Remote schema
  annotation text and transport extensions are removed before schemas reach the
  model, while functional validation remains.
- A selected tool must declare `outputSchema`. Every discovered schema is
  bounded and compiled; references are limited to the root or one direct local
  definition in the declared dialect. Cross-dialect/legacy assertions, nested
  dialect changes, draft-07 input root references, and selected-input tuple
  arrays are rejected. Numeric conversion is bounded before schema compilation.
  Registration is all-or-nothing through `OperationRegistry.RegisterAll`.
- Every generated operation is a synchronous, non-terminal
  `OperationEffectRead` with `ConfirmationNone`. Runtime evaluates host
  `OperationPolicy` before executor dispatch. Policy may allow or deny these
  reads; runtime's write approval-preview flow is not a consent mechanism for
  sending read arguments to a remote server.
- The contract digest includes the adapter contract and protocol versions,
  hashed binding, host mapping/version, canonical sanitized schemas, header
  mappings, execution response limit, and snapshot/tool digests. A host may set
  `ExpectedSnapshotID` to reject startup drift before registration. Credentials
  and authorization headers are never included.
- Valid statically reachable `x-mcp-header` annotations are supported with the
  required type, uniqueness, safe-integer, omission, and Base64 rules. The
  generated operation schema narrows annotated integers to the encodable range;
  draft-07 annotated parameters cannot use `$ref`, whose sibling bounds would be
  ignored by that dialect.
- Execution validates the frozen summary and binding before one adapter call.
  A current call result must have complete `resultType`, required `content`, and
  `structuredContent`; only the structured value enters Runtime and is checked
  against the frozen output schema. MRTR, unknown result types, protocol/tool
  errors, malformed content, and drift fail explicitly. Results remain
  extension-open for harmless additional members.
- Raw server and transport errors are not retained in runtime-visible strings or
  error chains. Cancellation/deadline classification remains available through
  `errors.Is` without retaining private transport text.

## Threat model and transport obligations

The server and network are malicious or compromised. Exact JSON decoding rejects
ambiguous members and invalid Unicode. Schema-aware traversal, supported-keyword
sanitization, local references, compilation, and resource limits reduce prompt,
SSRF, parser-confusion, and CPU/memory attacks. A conforming transport must apply
`MaxResponseBytes` before buffering; the adapter's post-return length check
cannot undo an oversized allocation. Likewise, pre/post binding sampling cannot
replace the transport's atomic `ExpectedBindingID` check, and one adapter call
cannot detect retries hidden inside a transport.

The first version excludes remote writes, asynchronous tasks, MRTR,
server-owned approval, subscriptions and hot refresh, negotiated extension
behavior, automatic fallback, browser/code-execution classes, and a bundled
HTTP/OAuth client. A host that mis-attests a side-effecting tool as read-only or
violates the transport contract remains responsible for that authority error.

## Durable catalog consequence

Runtime persists operation-set identity in sessions and waiting runs. A host
must reconstruct the same pinned MCP snapshot after restart before resume. It
must retain the matching runtime/adapter binary and old catalog/config versions,
drain affected sessions before retiring a pin, or replay retained discovery
through its transport. Adapter contract-version changes intentionally change the
pin. Remote discovery availability is otherwise a startup dependency. The
adapter deliberately does not persist snapshots or migrate durable sessions
because those stores and deployment policies are host-owned.

## Decision: additional model providers remain deferred

Another provider may implement `Model` only after the adapter contract and a
provider-neutral conformance corpus are frozen. The gate covers complete
response lifecycle mapping, closed unknown-output rejection, stable error and
usage mapping, cancellation, positive and negative protocol evidence, and an
immutable durable provider/adapter binding. Runtime-wide fallback, automatic
provider downgrade, or implementation before this gate is not accepted.

## Consequences

Hosts can use remote read tools without making network or credential choices
part of the runtime. When the snapshot is installed as Runtime's executor,
normal operation dispatch remains behind host policy; directly calling
`Snapshot.Execute` bypasses that policy like any direct executor call. The
profile is intentionally narrower than a general MCP client and adds deployment
obligations for pinned durable catalogs. Remote writes or another model provider require a new concrete state
machine and conformance decision, not flags on `Operation`.
