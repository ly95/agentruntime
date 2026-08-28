# Host integration guide

Transport payloads may populate only JSON-visible `Input` fields. Authenticated
host state is applied after decoding with `ApplyTrustedInput` and before queueing
or invoking Runtime.

```go
var public agentruntime.Input
decoder := json.NewDecoder(request.Body)
decoder.DisallowUnknownFields()
if err := decoder.Decode(&public); err != nil { return err }

input, err := agentruntime.ApplyTrustedInput(public, agentruntime.TrustedInputFields{
    RunID:            authenticatedRequestID,
    IdempotencyScope: tenantID,
    TrustedContext:   currentStateJSON,
})
if err != nil { return err }
if routeMayWrite {
    if err := agentruntime.ValidateWriteInput(input); err != nil { return err }
}
queue.Publish(input)
```

`TrustedContext` must be non-null, unambiguous JSON. Runtime puts it in the
current request instructions; it is not persisted as a user transcript item.
`RunID`, `IdempotencyScope`, and attachment resolvers are excluded from request
JSON, so a client cannot claim those authorities through ordinary unmarshalling.

## Durable model binding and OpenAI client policy

A Runtime configured with `RunStore` accepts only a `BoundModel`. For the
bundled OpenAI adapter, configure the full binding and the injected client
explicitly:

```go
client := openai.NewClient(
    option.WithAPIKey(secretFromHostVault),
    option.WithMaxRetries(0), // explicit host-owned transport policy
)
model, err := agentruntime.NewOpenAIModel(client, agentruntime.OpenAIModelConfig{
    Model:               selectedModel,
    EndpointClass:       "openai-public-api",
    CredentialPrincipal: "agent-runtime-production",
})
if err != nil { return err }

runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
    Model:    model,
    RunStore: durableRunStore,
})
```

`EndpointClass` and `CredentialPrincipal` are stable, non-secret identifiers.
Use a deployment class and authenticated account/service principal, not a URL,
API key, token, or secret fingerprint. Runtime hashes them with provider, model,
and adapter version into a SHA-256 `ModelBindingID`; durable records contain only
that ID.

The host owns the injected client's authentication, endpoint, middleware,
timeout, transport, and retry policy. A deliberately configured bounded retry
before streaming is allowed, but Runtime does not silently retry a model turn
after response evidence. The OpenAI adapter requires the returned
`response.model` to equal the requested model. Do not implement host fallback,
downgrade, or provider/model substitution around a failed call.

Treat a binding change as a deployment boundary. An adapter-version change
always creates a new binding. Endpoint class, credential principal, provider, or
model changes do too. Keep the old runtime binary, adapter, and client policy
available and route its existing sessions to it until they drain; route only new
sessions to the new binding. Rotating a secret may preserve the binding when the
stable credential principal is unchanged.

`RunStore` V4 rejects a session, run, or approval checkpoint with a different or
empty `ModelBindingID` before callback, lease acquisition, or fencing. Do not
infer or backfill legacy empty bindings from current configuration. V4 defines
no upgrade path for them; keep them with the deployment that created them until
they drain, or retire them explicitly.

## Attachment resolver precedence

Set `RuntimeConfig.ImageAttachmentResolver` for the common host resolver. A
trusted queue worker may set `Input.ImageAttachmentResolver` to override it for
one run. A typed-nil resolver is rejected. Current user attachments fail fast;
historical attachments are re-resolved from their durable storage key or
explicitly reported unavailable.

## Approval endpoint

Persist an operator decision in the host approval system, then call the narrow
façade with the same immutable public input and trusted fields:

```go
result, err := runtime.ResumeApproval(ctx, inputWithExactWaitingRunID)
```

`ApprovalResumer` reconstructs `ApprovalResume` from the durable
`PendingApprovalCommit`; it must not trust browser-posted operation names,
arguments, previews, checkpoints, execution IDs, or contract IDs. Runtime and
`RunStore` V4 require approval authority v2 and validate its complete digest,
input digest, model binding, and expected session revision before the waiting
run becomes active. Older subset authority is not resumable and must not be
upgraded by inference.

## Result handling

Use `ClassifyRunOutcome(result, err)` as the common HTTP/queue/UI decision. A
waiting result includes a safe `PendingApproval` summary. Completed terminal
artifacts expose only public `Data`; `InternalData` and `SessionSummary` never
cross the `Result` boundary.

The complete offline reference is
[`examples/approval`](../examples/approval/main.go). It exercises pending
approval, resume, a proved not-applied write, and evidence reconciliation with
`InMemoryStore`.
