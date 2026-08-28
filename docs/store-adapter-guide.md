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
checks inside the transaction. `RunRunStoreConformance` exercises the V4 model
binding, callback, lease, approval, and terminal-commit authority available
through public calls; a durable adapter is not compatible unless the complete
suite passes.

The conformance factory starts from an empty store, so adapters must also add
backend-specific migration tests that seed already-persisted malformed records.
At minimum, cover empty run/session/checkpoint model bindings, authority versions
zero and 1, unsupported future authority versions, and missing approval
checkpoints. Reads, resume, finish, lease takeover, and recovery must reject those
records before callback or mutation; passing the public suite does not authorize
an adapter to infer or backfill legacy authority.

## Why the V4 start protocol is split

`CreateRunV4` means “this Run ID must not exist.” `ResumeRunV4` means “this exact
Run ID must exist in `waiting_user` and carry the matching approval authority.”
Combining them lets an adapter accidentally turn a create collision into a
resume, or a missing resume into a new run. The synchronous acceptance callback
lets Runtime validate the store-proposed lease/session/approval state while the
transaction can still abort. Invoke it exactly once; never retry it.

The V4 method names also fence older adapters until they implement immutable
model binding. Every request carries a canonical, non-empty
`RunRecord.ModelBindingID`; mismatch checks happen before callback invocation,
lease acquisition, or expired-owner fencing.

## Run transaction pseudocode

```text
CreateRunV4(request, accept):
  validate request, including canonical non-empty ModelBindingID
  begin transaction
  require run ID and all item IDs are absent from the shared identity namespace
  compare ModelBindingID with any session, active run, and approval checkpoint
  require no live lease owns session; fence an expired lease only after binding checks
  for a new stateful session, create/return revision-zero immutable bindings
  propose store-owned deadline and monotonic lease generation
  call accept(proposal) exactly once while transaction remains abortable
  re-check context, clock, lease conflicts, immutable bindings, and identities
  if callback rejected: rollback without visible mutation
  commit run + session binding + lease + identities atomically

ResumeRunV4(request, accept):
  validate request, including canonical non-empty ModelBindingID and InputDigest
  begin transaction
  require exact run exists and status == waiting_user
  require the existing run's SessionState and authority-v2 PendingApprovalCommit rows exist
  call PendingApprovalCommit.ValidateAuthority(ModelBindingID) and match the run digest
  require checkpoint session revision equals current revision
  compare ModelBindingID across run, session, active owner, and approval checkpoint
  require immutable SkillSet/OperationSet bindings match
  acquire a new monotonic lease generation
  call accept(exact run/session/approval proposal) once
  re-check all predicates, then commit running state + lease atomically

FinishRun(request):
  validate live lease ID + generation + current deadline
  reject all lease/session authority on a stateless run
  compare the run, next session, durable approval, and checkpoint ModelBindingID
  require any run digest to have an exact authority row and revalidate its complete digest
  validate next session ID, LastRunID, revision, other immutable bindings, and timestamps
  retain Session.CreatedAt and reject a regressed Session.UpdatedAt
  validate the terminal status payload before any mutation
  for waiting_user: commit Run + Session + authority-v2 PendingApproval + audit atomically
  for failed/interrupted/cancelled: commit Run + FailureItem atomically, or require
    FailureAuditStatus == audit_missing when Runtime could not construct the item
  for completed: commit Run + optional Session atomically
  release the lease in the same transaction
```

Lease validation compares the live store deadline, not only the possibly stale
deadline in the caller's handle. Renewal extends the same lease ID and
generation; it never creates a new owner. Session generations are monotonic and
must not be reused after expiry.

`RunRecord.CreatedAt` and its persistent `Input` are fixed by `CreateRunV4`.
Resuming or finishing that same run retains those values, rejects a different
input identity, and rejects an `UpdatedAt` older than the current durable
record.

## V4 model-binding storage and migration

Runtime derives `ModelBindingID` as a versioned SHA-256 digest of provider,
model, endpoint class, credential principal, and adapter version. Durable
runtime records store only the digest ID: persist it on every `RunRecord`, every
`SessionState`, and every `ApprovalCheckpoint`. Do not copy the five source
fields, endpoint URLs, credentials, API keys, or tokens into these records.

Roll out V4 as a schema and adapter migration before constructing a new Runtime:

1. add non-empty `ModelBindingID` storage to run, session, and approval-checkpoint
   records and preserve it exactly on every read/write path;
2. implement `CreateRunV4`/`ResumeRunV4` and run the current exported
   `storetest` suite against the production adapter;
3. deploy the matching `BoundModel` runtime only after the V4 adapter is active;
4. route new sessions to the new binding while keeping the old runtime/adapter
   deployment available to drain sessions that it owns.

An empty legacy binding is not evidence of the provider, model, endpoint,
principal, or adapter contract that created the record. Do not fill it from the
current default, infer it from a model name, or upgrade it during create,
resume, finish, or lease recovery. V4 returns `ErrModelBindingMismatch` without
mutation or callback invocation. V4 defines no backfill path for an empty
binding. Those records remain non-resumable by V4 and must be drained by the
deployment that created them or retired explicitly by the host.

Changing `AdapterVersion` creates a new binding even when the provider and model
names are unchanged. Credential-secret rotation does not require a new binding
when the stable non-secret principal remains unchanged; changing endpoint class
or credential principal does.

## Approval authority v2

A waiting run stores the complete `PendingApprovalCommit` with
`AuthorityVersion == agentruntime.PendingApprovalAuthorityVersion` (currently
2), its digest, and its approval audit item atomically. Compute the digest with
`PendingApprovalCommit.AuthorityDigest` and validate loaded authority with
`PendingApprovalCommit.ValidateAuthority`; do not duplicate the canonical JSON,
number, or replay-envelope normalization algorithm in an adapter. Version 2
covers the persistent request, decision, audit, normalized arguments, operation
summary, host-owned input identity, checkpoint, and canonical adapter replay
envelopes. `ResumeRunV4` must return a complete defensive copy and compare the
checkpoint `InputDigest`, expected session revision, and `ModelBindingID` before
mutation. Version zero, version 1, and every other non-current authority are not
resumable; never synthesize omitted authority during migration. A run that
references a missing session or approval row is corrupt durable state, not a new
revision-zero session, and must fail without callback invocation or mutation.

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
accepts a step from a currently reserved batch before the final plan seal
because Runtime must execute tools to
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
| Model binding drift or empty legacy binding | `ErrModelBindingMismatch` | none |
| Skill or operation binding drift | `ErrSkillSetMismatch` / `ErrOperationPlanChanged` | none |
| Approval authority missing, malformed, or digest drift | `ErrOperationPlanChanged` | none |
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
