# Store adapter guide

`RunStore` and `ExecutionStore` are authorities, not caches. An adapter must
provide the atomicity and fencing described here even when the backing database
has weaker primitives. `InMemoryStore` is executable reference behavior; it is
not durable and must not be used across process restarts.

## Start with the conformance suites

```go
func TestStores(t *testing.T) {
    storetest.RunRunStoreConformance(t, func() agentruntime.RunStore {
        return newDatabaseRunStore(t)
    })
    storetest.RunExecutionStoreConformance(t, func() agentruntime.ExecutionStore {
        return newDatabaseExecutionStore(t)
    })
}
```

Run `go test ./...` under the race detector for the adapter as well. Call the
exported `Validate` methods on `CreateRunRequest`, `ResumeRunRequest`,
`FinishRunRequest`, `OperationPlanBatch`, `OperationPlanSeal`,
`AcquireExecutionRequest`, and `OperationExecutionTransition` before opening a
transaction. Validation is necessary but does not replace compare-and-swap
checks inside the transaction.

## Why the V3 start protocol is split

`CreateRunV3` means “this Run ID must not exist.” `ResumeRunV3` means “this exact
Run ID must exist in `waiting_user` and carry the matching approval authority.”
Combining them lets an adapter accidentally turn a create collision into a
resume, or a missing resume into a new run. The synchronous acceptance callback
lets Runtime validate the store-proposed lease/session/approval state while the
transaction can still abort. Invoke it exactly once; never retry it.

## Run transaction pseudocode

```text
CreateRunV3(request, accept):
  validate request
  begin transaction
  require run ID and all item IDs are absent from the shared identity namespace
  require no live lease owns session; expired leases may be fenced
  create/bind revision-zero session when immutable SkillSet/OperationSet is present
  propose store-owned deadline and monotonic lease generation
  call accept(proposal) exactly once while transaction remains abortable
  re-check context, clock, lease conflicts, immutable bindings, and identities
  if callback rejected: rollback without visible mutation
  commit run + session binding + lease + identities atomically

ResumeRunV3(request, accept):
  validate request
  begin transaction
  require exact run exists and status == waiting_user
  require pending approval object and digest match each other and requested input
  require checkpoint session revision equals current revision
  require immutable SkillSet/OperationSet bindings match
  acquire a new monotonic lease generation
  call accept(exact run/session/approval proposal) once
  re-check all predicates, then commit running state + lease atomically

FinishRun(request):
  validate live lease ID + generation + current deadline
  reject all lease/session authority on a stateless run
  validate next session ID, LastRunID, revision, immutable bindings, and timestamps
  retain Session.CreatedAt and reject a regressed Session.UpdatedAt
  validate the terminal status payload before any mutation
  for waiting_user: commit Run + Session + PendingApproval + approval audit atomically
  for failed/interrupted/cancelled: commit Run + FailureItem atomically, or require
    FailureAuditStatus == audit_missing when Runtime could not construct the item
  for completed: commit Run + optional Session atomically
  release the lease in the same transaction
```

Lease validation compares the live store deadline, not only the possibly stale
deadline in the caller's handle. Renewal extends the same lease ID and
generation; it never creates a new owner. Session generations are monotonic and
must not be reused after expiry.

`RunRecord.CreatedAt` and its persistent `Input` are fixed by `CreateRunV3`.
Resuming or finishing that same run retains those values, rejects a different
input identity, and rejects an `UpdatedAt` older than the current durable
record.

## Execution state machine

```text
reserved plan ── acquire ──> started
                              ├─ committed journal ──> executed ── verify ──> completed
                              ├─ proved pre-commit failure ────────────────> retryable
                              └─ commit boundary uncertain ────────────────> unknown

unknown ── evidence proves not applied ──> retryable ── new fenced attempt ──> started
unknown ── evidence proves committed ──────────────────────────────────────> completed
started ── evidence proves executor never began ──────────────────────────> retryable
started ── evidence proves exact attempt committed ───────────────────────> completed
executed ── verification recovery ───────────────────────────────> completed/recovery_failed
```

Every transition ID is globally unique. Every attempt ID is unique within an
execution's history. Acquisitions and transitions must reject timestamps that
would move the current execution's `UpdatedAt` backward. `AcquireExecution`
accepts a step from a currently
reserved batch before the final plan seal because Runtime must execute tools to
obtain the model's terminal response. A later `SealPlan` freezes the complete
batch count; it must still match every already acquired execution.

Every planned execution ID is likewise owned by exactly one batch globally.
Reject a second assignment before reserving it so a conflicting plan cannot
make the original plan ambiguous. New batch timestamps are nondecreasing and an
initial seal cannot predate any batch. Idempotent batch/seal retries compare
their semantic authority and contents, return the first stored timestamps, and
do not treat a later retry observation time as plan drift.

The executor must validate `ExecutionID`, `AttemptID`, and `SessionLease`
atomically with its external side effect. A store-side precheck alone leaves a
time-of-check/time-of-use gap.

## Error matrix

| Condition | Required error | Mutation |
| --- | --- | --- |
| Existing run/item identity | `ErrIdentityConflict` | none |
| Missing explicit resume run | exact `ErrRunNotFound` | none |
| Live session owner | `ErrSessionBusy` | none |
| Stale lease or attempt | `ErrSessionLeaseLost` / `ErrOperationAttemptLost` | none |
| Skill or operation binding drift | `ErrSkillSetMismatch` / `ErrOperationPlanChanged` | none |
| Plan, contract, arguments, or verification bit drift | `ErrOperationPlanChanged` | none |
| Invalid status payload or transition | `ErrInvalidExecutionTransition` | none |
| Callback rejection/cancellation | callback/context error | none |
| Missing durable record | `ErrRunNotFound`, `ErrSessionNotFound`, or `ErrOperationExecutionNotFound` | none |

Do not translate these sentinels into a generic not-found/conflict error inside
the adapter; Runtime uses their exact semantics to decide whether an action is
safe.

## Failure cleanup

Runtime keeps lease renewal alive during detached finalization. Store methods
must still honor their contexts and return explicit errors. A terminal error
record has `FailureAuditStatus=committed` only when the `ItemTypeError` record
was included in the same `FinishRun` transaction. `audit_missing` is a visible
degraded state, never an implicit success.
