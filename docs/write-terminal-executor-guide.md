# Write and terminal executor guide

## Commit boundary

Validate policy inputs and external preconditions before the side-effect commit
boundary. At the boundary, atomically verify all three runtime fences:

- `ExecutionID`: idempotency identity for this planned write;
- `AttemptID`: current execution owner;
- `SessionLease`: current session generation and revision.

Persist the external result under `ExecutionID` in the same transaction as the
side effect. Returning a successful result without that idempotency record can
duplicate the effect after a lost response.

When failure definitely occurred before the commit boundary, return:

```go
return agentruntime.OperationResult{},
    agentruntime.MarkOperationNotApplied(cause)
```

Runtime may transition that execution to `retryable`. Once the commit boundary
may have been crossed, return an error wrapping `ErrOperationOutcomeUnknown`.
That state requires reconciliation and must not be retried blindly.

## Confirmation and verification

`ApprovalPreview` receives normalized, schema-validated arguments and returns a
small safe JSON object. Never forward raw arguments to a browser. Policy routes
the operation to approval, while `ConfirmationSpec` determines whether the
successful write also needs independent positive verification. Verification
evidence must be non-null exact JSON.

## Terminal writes

A terminal write returns `FinalResponse` and one or more `ResultArtifact`
records. Before execution, `ProjectTerminalSession` declares the exact public
historical projection. After execution Runtime rejects missing, extra, or
changed projections.

```go
return agentruntime.OperationResult{
    Output:        outputJSON,
    FinalResponse: "The requested operation completed.",
    Artifacts: []agentruntime.ResultArtifact{{
        Type:           "host_record",
        Data:           publicJSON,
        InternalData:   privateMaterializationJSON,
        SessionSummary: boundedHistoricalProjectionJSON,
    }},
}, nil
```

`Data` may enter the model-visible tool result and public `Result`.
`InternalData` is available only on the terminal durable run record.
`SessionSummary` is retained only as a bounded, explicitly untrusted historical
projection. Neither private field appears in ordinary events or public results.

A terminal batch must be homogeneous, may contain at most the operation's
`TerminalBatchLimit`, and can never exceed `MaxTerminalBatchLimit`. Runtime
plans, fences, executes, validates, and persists each call independently before
combining artifacts.

Executable success and failure paths are in
[`examples/approval`](../examples/approval/main.go),
[`runtime_terminal_test.go`](../runtime_terminal_test.go), and
[`runtime_reconciliation_test.go`](../runtime_reconciliation_test.go).

## Reconciliation evidence

Use `BuildAbandonmentEvidence` only when an authoritative source proves the
exact started attempt never entered its executor. Use `BuildCompletionEvidence`
when an authoritative source proves that exact attempt committed. The proof
object should contain durable record identifiers, not screenshots or operator
assertions alone. Both `Runtime.ReconcileOperation` and `OperationReconciler`
delegate to the same validation and transition engine and emit the same event
path.
