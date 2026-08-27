# Constrained remote MCP adapter

`mcpadapter` maps an explicitly allowlisted startup snapshot of remote MCP tools
into ordinary `agentruntime.Operation` contracts. It is not a network client:
the host owns endpoint selection, authentication, redirects and SSRF controls,
HTTP/SSE or stdio framing, timeouts, and cancellation through
`mcpadapter.Transport`.

The adapter is pinned to MCP `2026-07-28`. It does not negotiate down to an older
revision or use the legacy `initialize` lifecycle. Every request carries the
required per-request `_meta` fields. `Request.MetadataHeaders` contains the
header-ready `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`, and validated
`Mcp-Param-*` values. Credentials never enter that map; a transport adds them
separately.

## Authority model

A mapping is accepted only when the host supplies:

- the exact remote tool name and local operation name;
- a host-authored model description;
- a host contract-version token that changes with the host's understanding of
  the remote read semantics;
- an explicit `ReadOnly: true` attestation;
- any host capability labels used by `OperationPolicy`.

Server instructions, tool descriptions, schema annotation text, identities, and
approval hints are untrusted. Natural-language schema annotations and
`x-mcp-header` extensions are removed from the model-facing operation schema;
functional validation keywords remain. A positive server `readOnlyHint` does
not grant read authority, while an explicit negative hint contradicts the host
attestation and fails discovery.

Selected operations are non-terminal reads with `ConfirmationNone`. Registering
a snapshot does not authorize it, and calling `Snapshot.Execute` directly has
the same policy-bypass risk as calling any `OperationExecutor` directly. Use it
through `Runtime`, where `OperationPolicy` runs before dispatch.

A policy for these reads must resolve to allow or deny. Runtime's per-operation
approval-preview protocol is for writes, so `PolicyRequireApproval` cannot be
used for an MCP read. A host that needs consent before sending arguments to a
remote server must establish that consent before the run or deny the operation.

Remote writes are not supported. MCP has no commit boundary at which the runtime
can atomically enforce `ExecutionID`, `AttemptID`, and `SessionLease`, so adapting
a remote write would violate fencing and idempotency guarantees.

## Constructing and pinning a snapshot

```go
func discoverRemoteReads(
    ctx context.Context,
    transport mcpadapter.Transport,
    bindingID string,
    expectedSnapshotID string,
) (*agentruntime.OperationRegistry, *mcpadapter.Snapshot, error) {
    snapshot, err := mcpadapter.Discover(ctx, mcpadapter.Config{
        Transport:          transport,
        BindingID:          bindingID,
        ExpectedSnapshotID: expectedSnapshotID,
        Mappings: []mcpadapter.Mapping{
            {
                RemoteName:    "documents.lookup",
                OperationName: "remote_documents_lookup",
                Description:   "Look up documents visible to the current principal.",
                Capabilities:  []string{"documents.read"},
                HostVersion:   "documents-read-v1",
                ReadOnly:      true,
            },
        },
    })
    if err != nil {
        return nil, nil, err
    }

    registry := agentruntime.NewOperationRegistry()
    if err := snapshot.Register(registry); err != nil {
        return nil, nil, err
    }
    return registry, snapshot, nil
}
```

An empty `ExpectedSnapshotID` is useful only during controlled provisioning.
Review the captured selected contracts and header mappings, record
`Snapshot.ID()`, then deploy that value as the expected pin. A selected sanitized
schema, mapping, binding, protocol, host version, header mapping, or execution
response-limit change then fails discovery before registration. Changes only to
stripped annotations, unselected tools, or discovery cache metadata do not alter
the pin.

Runtime binds operation contracts to durable sessions and waiting approvals.
After a restart, the host must reconstruct the same pinned snapshot before
resuming them. Keep the matching runtime/adapter binary and old catalog/config
versions available, drain sessions before retiring a pin, or make the host
transport replay the retained, cursor-complete discovery results while routing
`tools/call` to the exact immutable server deployment represented by the binding.
Retained results must be re-enveloped with each current JSON-RPC request ID; a raw
old response cannot be replayed. An adapter contract-version change intentionally
changes the pin. Remote availability is required if discovery is not retained.
The adapter does not persist snapshots or migrate durable sessions automatically.

If a runtime also has local operations, the host composes an executor that routes
each frozen operation name to the correct implementation; the adapter does not
acquire authority over unrelated operations.

## Transport contract

`BindingID` is a non-secret identity for the endpoint, credential principal,
authorization scope, and transport semantics that affect execution. The adapter
samples it before and after every logical RPC. `Request.ExpectedBindingID` lets
the transport atomically reject a different binding at the actual dispatch
boundary; sampling alone cannot provide that guarantee.

`BindingID` and `RoundTrip` may be called concurrently; their binding,
credential, endpoint, and connection state must therefore be concurrency-safe. A
conforming Streamable HTTP transport must, for each `RoundTrip`:

1. atomically verify that the dispatch binding equals `ExpectedBindingID`;
2. send exactly one POST and perform no transparent retry or protocol fallback;
3. serialize `Request` as one JSON-RPC body and copy all `MetadataHeaders`;
4. add credentials separately and apply host redirect/origin/SSRF policy;
5. advertise both `application/json` and `text/event-stream` in `Accept`;
6. enforce `MaxResponseBytes` while reading, before allocating a full response;
7. validate HTTP status and content type, and for SSE return only the final
   request-correlated JSON-RPC response after safely handling request-scoped
   notifications;
8. stop the request when `ctx` is cancelled.

The adapter calls `RoundTrip` once and checks the returned size again, but cannot
detect an internal transport retry or undo a dispatch performed under the wrong
binding. Those are violations of the injected transport contract.
`MaxResponseBytes` does not limit outbound data: transports must reject oversized
serialized requests, header counts, individual header names/values, and total
header bytes before dispatch. Hosts should also avoid logging `Mcp-Param-*`
values because they originate from untrusted operation arguments.

## Discovery, caching, and schemas

Discovery is all-or-nothing:

1. call `server/discover` and require `2026-07-28`, cache metadata, and the tools
   capability;
2. exhaust `tools/list` using opaque cursors, treating a present empty cursor as
   valid;
3. require `ttlMs` and one `cacheScope`, with the same scope on every page;
4. detect cursor cycles, duplicate names, malformed tools, and exceeded page,
   tool, response, schema-byte, depth, and node limits;
5. select exact host mappings and require an `outputSchema` for each;
6. compile every discovered schema, sanitize selected schemas, and atomically
   register the complete operation batch.

`listChanged` only advertises notification support; either value is accepted.
The adapter does not subscribe. `ttlMs` is validated as protocol evidence but
is not a mutable-runtime refresh timer: a returned `Snapshot` is an immutable
local contract. Rediscover between runtime/catalog constructions and compare the
host pin; never replace a running catalog in place.

Limits are inclusive; exceeding one fails instead of truncating. Schemas default
to JSON Schema 2020-12; explicit 2020-12 and draft-07 identifiers are accepted.
Each selected schema must use only keywords from its declared dialect: 2020-12
uses `$defs`, while draft-07 uses `definitions`; legacy/cross-dialect assertions
and nested dialect changes fail. References are restricted to `#` or one direct
entry in the dialect's local definition map; network, arbitrary JSON-Pointer,
anchor, and nested-definition references are rejected. Draft-07 input schemas
cannot use a root `$ref`. Selected input schemas also reject tuple `items` and
`prefixItems`, whose omission semantics cannot be represented by the strict tool
profile; output schemas may retain dialect-valid tuple constraints. Custom
vocabularies/identifiers and unsupported selected-schema keywords also fail, so
references cannot bypass sanitization or turn compilation into an SSRF boundary.
JSON numbers whose exact validation would materialize more than 4,096 decimal
digits are rejected before schema compilation or result validation.

For Streamable HTTP, selected `x-mcp-header` annotations must be reachable only
through a static chain of `properties`, use a case-insensitively unique HTTP
token, and target a string, integer, or boolean. A draft-07 annotated parameter
cannot use `$ref`, because that dialect ignores sibling type and bound keywords.
Annotated integer operation schemas are narrowed to the IEEE-754 safe range.
Missing and `null` values are
omitted from derived headers. Unsafe/non-ASCII values and the sentinel pattern
are encoded as `=?base64?...?=`.

OpenAI strict tool schemas represent an omitted optional property as explicit
`null`. Before execution, the snapshot's operation input normalizer removes such
object members only when the frozen declared property is optional and rejects
`null`. Declared-valid `null` values, required nullable properties, and `null`
array elements are preserved. The normalized object is revalidated against the
declared remote schema before dispatch, and direct `Snapshot.Execute` calls apply
the same normalization.

## Execution and failures

`Snapshot.Execute` validates the exact frozen operation summary, normalized
arguments, output schema, and transport binding. It issues one `tools/call` and
never rediscovers, follows MRTR, falls back, or performs an adapter-level retry.

A current-protocol call result must contain `resultType: "complete"`, the required
`content` array, and `structuredContent`. Content blocks are structurally
validated but ignored; only structured content enters Runtime. Results are
extension-open, but unknown `resultType` values fail because no extensions are
negotiated. JSON-RPC errors, `isError: true`, `input_required`, malformed content,
binding drift, and output-schema drift fail explicitly.

Raw server and transport errors are replaced with stable local errors and are not
retained in the error chain. Context cancellation/deadline classification is
preserved through `errors.Is`. The adapter never places credentials,
authorization headers, raw server errors, or unstructured content into snapshot
digests or runtime events.
