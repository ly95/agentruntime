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
`RunStore` validate the full authority digest and expected session revision
before the waiting run becomes active.

## Result handling

Use `ClassifyRunOutcome(result, err)` as the common HTTP/queue/UI decision. A
waiting result includes a safe `PendingApproval` summary. Completed terminal
artifacts expose only public `Data`; `InternalData` and `SessionSummary` never
cross the `Result` boundary.

The complete offline reference is
[`examples/approval`](../examples/approval/main.go). It exercises pending
approval, resume, a proved not-applied write, and evidence reconciliation with
`InMemoryStore`.
