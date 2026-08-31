# Safety guarantees and failure model

This document states the safety boundary of the current runtime. The guarantees
below are conditional on conforming host implementations; they do not turn an
arbitrary remote API into an exactly-once transaction.

The public contracts that define this boundary are
[`OperationExecutor`](../operation_contracts.go#L208),
[`OperationResult`](../operation_contracts.go#L212),
[`ExecutionStore`](../store.go#L791), and
[`ReconcileOperationRequest`](../store.go#L812). The sentinel errors are declared
in [`errors.go`](../errors.go#L8).

## Required assumptions

| Component | Required host behavior | Why it is part of the safety boundary |
| --- | --- | --- |
| `RunStore` | Implement the documented V4 transactions, lease generations, immutable bindings, approval authority, and no-mutation-on-error rules. Pass `storetest.RunRunStoreConformance`. | Runtime cannot fence a session or resume an approval safely if the store acknowledges state that it did not atomically commit. |
| `ExecutionStore` | Atomically reserve/seal plans, acquire attempts, compare-and-swap transitions, append immutable transition history, preserve globally unique transition IDs and per-execution attempt IDs, and reject stale attempts. Pass `storetest.RunExecutionStoreConformance`. | The execution state machine is an authority, not a cache. A permissive or lossy adapter can allow duplicate or stale execution. |
| Write executor | Treat `ExecutionID` as the idempotency identity and `AttemptID` plus `SessionLease` as owner fences. Validate those fences atomically with the domain mutation and persist a receipt under `ExecutionID` in that same transaction whenever the resource permits it. | Runtime's pre-executor `ValidateRunLease` and `ValidateExecutionAttempt` checks are intentionally only early rejection. A separate check leaves a time-of-check/time-of-use gap. |
| Executor error classification | Return `MarkOperationNotApplied(cause)` only when authoritative evidence proves the commit boundary was not crossed. Return an error wrapping `ErrOperationOutcomeUnknown` (or another ordinary error) once the boundary may have been crossed. | Runtime may make only the first class retryable. Misclassifying an ambiguous result as not applied authorizes a later attempt that can duplicate the effect. |
| Reconciler | Bind every decision to the exact current `ExecutionID` and `ExpectedAttemptID`, use an authoritative source, and persist non-null exact-JSON evidence for abandonment or unresolved completion/retry. | Human assertion, a screenshot, or absence from an eventually consistent read is not proof of commit or non-application. |
| Verifier | For confirmation-required writes, independently confirm the committed result and return positive, non-null exact-JSON evidence. | A successful executor response is not confirmation. Runtime completes such a write only after positive verification. |
| Registry and runtime configuration | Keep operation contracts, normalizers, approval previews, model binding, skill set, and operation set compatible with the durable run. | Resume and reconciliation deliberately fail closed when durable authority can no longer be reproduced. |
| Persistence | Use durable stores for restart recovery. | `InMemoryStore` is reference behavior only; process exit loses its records and therefore its fences and evidence. |

`MarkOperationNotApplied` is a semantic assertion, not a convenience wrapper;
see its implementation in [`errors.go`](../errors.go#L57). The write-side atomic
fence requirement is also part of the normative
[`ExecutionStore` contract](../store.go#L757).

## What the runtime guarantees

Subject to the assumptions above, the runtime provides these properties:

| Property | Guarantee | Limit |
| --- | --- | --- |
| Immutable write plan | Each write batch is durably reserved before acquisition. Reuse must match request authority, batch position, `ExecutionID`, operation name, contract ID, and canonical arguments. A final seal fixes the observed batch count. Drift fails with `ErrOperationPlanChanged`. | A batch may execute before the final seal because later model turns can add batches. The seal does not roll back an already committed external effect. |
| Single current attempt | Acquisition changes one planned execution to `started` with a unique `AttemptID`. A stale or reconciled attempt is rejected with `ErrOperationAttemptLost` or a blocked disposition. | This prevents a conforming executor from committing after it loses ownership; it cannot stop an executor that ignores the fence. |
| Durable replay suppression | An acquisition that finds `executed` or `completed` returns `ExecutionReplay`; Runtime reuses the durable `OperationResult` and does not call the executor again. | `started` and `unknown` are deliberately blocked, not replayed. `retryable` may acquire a new attempt because non-application was proved. |
| Fail-closed ambiguity | An executor error other than proven `ErrOperationNotApplied`, an invalid success payload, or failure to journal a returned success is treated as an unresolved write. Runtime attempts to persist `unknown` and joins `ErrOperationOutcomeUnknown` into the error. | A hard process crash can leave `started` because Runtime has no opportunity to write `unknown`. Both states require evidence, not a blind retry. |
| Positive verification | A confirmation-required write reaches `completed` only with a normalized positive [`VerificationResult`](../operation_contracts.go#L384). `executed` retains the immutable result while verification is retried or reconciled. | Verification proves only what the host verifier and evidence source actually establish. Runtime does not inspect the external system itself. |
| Approval-resume binding | A pending approval, its complete authority digest, audit item, waiting run, and applicable session revision are committed together. Resume compares the exact call, execution, contract, normalized input, preview, transcript/checkpoint, model binding, and session revision before execution. | Runtime does not decide whether the human or approval service should approve; it prevents a decision from being applied to changed durable authority. |
| Session fencing | Lease ID, monotonically increasing generation, deadline, and session revision identify the current stateful owner. Runtime validates the lease before a write and passes the fence to the executor. | The host must repeat the validation atomically at its mutation boundary. Deadline checks made only in Runtime are insufficient. |
| Evidence-gated reconciliation | `started` can be abandoned only with proof the executor never began, and can be completed only with proof the exact attempt committed. `unknown` can become `retryable` or `completed` only with evidence. The expected attempt and current operation contract must still match. | Runtime validates the shape and binding of evidence, not its truth. Only a trusted reconciler may assert what the evidence proves. |
| Ambiguous store acknowledgement | When a reconciliation transition returns an error, Runtime accepts it as committed only if a detached read proves the exact resulting record and exactly one identical transition-history entry. Otherwise it returns the acknowledgement error plus the proof failure. | This proof is only as trustworthy and durable as the `ExecutionStore`. |

## Durable execution states

The state names describe durable knowledge, not merely the last function call:

| State | Durable meaning | Safe next action |
| --- | --- | --- |
| `started` | One attempt owns the execution. The executor may not have begun, may be running, or may have committed before the process died. There is no durable result. | Do not execute again. Prove that the executor never began and reconcile `abandon`, or prove that this exact attempt committed and reconcile `complete`. |
| `executed` | Runtime durably recorded the immutable `OperationResult`; independent verification may still be pending. | Replay the result. If confirmation is required, verify it or reconcile `complete`/`fail`; never repeat the side effect. |
| `completed` | The result is durable. If confirmation was required, positive verification is durable too. | Replay the result and continue/finalize the run without calling the executor. |
| `unknown` | The commit boundary may have been crossed and no trusted terminal fact is durable in the execution journal. | Inspect the authoritative side-effect system. Reconcile `retry` only with proof of non-application, `complete` only with proof of commit and the full result, or explicitly `fail` into `recovery_failed` to end recovery without authorizing another execution. |
| `retryable` | Non-application was proved for the prior attempt, or a trusted abandonment proved its executor never began. | Let Runtime acquire a fresh, unique attempt and revalidate every fence at commit. |
| `recovery_failed` | Reconciliation or verification recovery was deliberately ended without a successful completion. A prior `executed` result, if any, remains immutable. | Escalate according to host policy; this state is not permission to execute again. |

The legal payloads and transitions are enforced by
[`OperationExecutionTransition.Validate`](../store.go#L668) and revalidated on
store acknowledgements in
[`runtime_reconciliation.go`](../runtime_reconciliation.go#L196).

## Failure-window matrix

“Persisted” below means persisted by a conforming durable adapter. An event or
in-memory value alone is not recovery authority.

| Failure window | Persisted fact after the failure | Runtime behavior on the failing call or next replay | Host recovery obligation | Residual risk |
| --- | --- | --- | --- | --- |
| Before `ReservePlanBatch` commits | No plan batch and no execution for this write. | Returns the preparation/store error; the executor is not called. A later call may reserve the plan. | Repair the dependency and retry the same run/input identity. | A host that performed a side effect during normalization, policy, preview, or another preflight callback has violated the contract; Runtime cannot fence it. |
| Batch reservation commits, then audit append or process fails before acquisition | The immutable plan batch may exist; no execution attempt exists. | A retry must reproduce the same batch. Different contract, arguments, position, or authority fails with `ErrOperationPlanChanged`; matching reservation is idempotent. | Preserve the same operation registry and input authority, then retry. | The plan-audit item may be missing even though the authoritative batch exists. The batch, not the item stream, governs execution. |
| `AcquireExecution` commits, then the process dies before entering the executor | Execution is `started` for the exact attempt; the plan batch exists. | Replay is blocked because absence of an executor response does not prove non-execution. | Use durable evidence to reconcile `abandon`, or prove commit and reconcile `complete`. | If the host cannot prove whether the executor began, the execution can remain unresolved indefinitely. This favors safety over liveness. |
| Runtime lease/attempt prechecks pass, then ownership changes before the mutation | Usually still `started`; the prechecks alone record no domain outcome. | Runtime passes `ExecutionID`, `AttemptID`, and `SessionLease` to the executor. | Revalidate all fences inside the same transaction/conditional write as the side effect. Reject a stale owner before mutation. | A non-atomic host check permits a stale process to commit after lease expiry or reconciliation. Runtime alone cannot close this race. |
| Executor proves failure before its commit boundary and returns `MarkOperationNotApplied` | Runtime attempts `started -> retryable`. | If the transition commits, a later call acquires a new attempt. If acknowledgement/inspection cannot prove fencing, Runtime returns the original error plus transition/proof errors and leaves retry unauthorized. | Treat “not applied” as a proof obligation. If the release was not durably proved, reconcile `abandon` with evidence before retrying. | Incorrectly returning `ErrOperationNotApplied` after an ambiguous timeout can duplicate the effect. |
| Executor returns an ordinary error after the commit boundary may have been crossed | Runtime attempts `started -> unknown`; a hard crash may leave `started`. | Returns an error matching `ErrOperationOutcomeUnknown` and does not authorize automatic retry. | Inspect the authoritative resource and reconcile the exact attempt. | An external system without idempotency records or reliable read-after-write evidence may make the outcome permanently unknowable. |
| Domain mutation commits, but the executor response is lost or the process dies before returning | The runtime journal is normally still `started`; the domain system should contain the mutation and receipt under `ExecutionID`. | The execution remains blocked. If an error reaches Runtime, it attempts `unknown`; a process crash cannot do that cleanup. | Recover the receipt from the authoritative domain store and reconcile `complete` with exact-attempt evidence and the full validated result. | Without atomic mutation-plus-receipt persistence, commit can be real but unprovable. |
| Executor returns success, but result/protocol validation fails | Runtime has no trusted result and attempts `unknown`. | It does not journal or expose the malformed success as completed, and it does not rerun the write automatically. | Determine the actual domain outcome; reconcile with a schema-valid result or mark recovery failed. | A domain mutation can succeed even though the executor violated the output, receipt, terminal-artifact, or UTF-8 protocol. |
| Executor returns valid success, then `started -> executed` acknowledgement fails | Depending on the store, the durable state can be `executed`, `unknown`, or the original `started`; Runtime inspects/fences but does not assume success. | Returns an unresolved error. A later acquisition replays only a valid `executed`/`completed` record and blocks other states. | Inspect execution history and the domain receipt; reconcile if the state is still unresolved. | Runtime and the external side effect are not one general distributed transaction. |
| `executed` is durable, then the process dies before verification | The immutable result and receipt are in the execution journal. | Replay skips the executor and runs only the required verifier. | Restore verifier availability or reconcile using independent positive evidence. | The write may be committed while the run remains incomplete for an arbitrarily long time. |
| Verifier errors, denies confirmation, or emits invalid/missing evidence | Execution remains `executed`; a verification audit may record failure. | Returns `ErrVerificationFailed`; it does not convert the write to `retryable` and does not call the executor again on replay. | Re-run a trustworthy verifier, reconcile `complete` with positive evidence, or reconcile `fail`. | Negative verification is not proof that the write was not applied. Compensation, if desired, is a separate host operation. |
| Positive verification is obtained, but `executed -> completed` acknowledgement fails | Store may contain either `executed` or `completed`. | Returns the transition error. Replay of `executed` verifies again; replay of `completed` uses the durable verification. The executor is not called in either case. | Inspect the execution state/history if operational certainty is required. | Verification work may repeat, so verifiers must tolerate duplicate reads; the side effect does not repeat through Runtime. |
| `completed` commits, then operation-result item append, run finalization, or process exit fails | The execution result (and required verification) is durable, but the run transcript/status may lag. | A same-plan replay receives `ExecutionReplay` and rebuilds/persists the tool result without invoking the executor. | Retry the run under its normal run/session fences and repair any run-store failure. | Execution completion and whole-run completion are separate transactions; consumers must not infer one from the other. |
| Writes finish, but the final `SealPlan` is absent or conflicts | Reserved batches and their execution records remain; no final batch-count authority exists, or a different seal exists. | Matching replay may finish and seal the same count. A mismatched batch or seal fails closed with `ErrOperationPlanChanged`; no prior external effect is rolled back. | Investigate plan drift and keep the original registry/input authority available. Do not reinterpret the idempotency key as a new plan. | A successfully committed write can coexist with a failed run caused by later plan-seal persistence. |
| Process fails before the waiting-user `FinishRun` transaction commits | A conforming `RunStore` exposes none of the waiting run, pending authority, approval audit, session revision, or lease release from that failed transaction. | Resume is unavailable because no committed pending approval exists. | Retry/fail the run through normal lease recovery; do not manufacture approval authority. | The external approval service may have created a request that Runtime never durably linked to the run. Host cleanup may be needed. |
| Waiting-user transaction commits, then the process dies | Waiting run, complete [`PendingApprovalCommit`](../store.go#L325), digest, audit, checkpoint, and expected session revision are durable together. | Resume of the exact Run ID reloads that authority; a pure pending poll does not advance the session revision. | Ask the approval service for the decision and return an [`ApprovalResume`](../operation_contracts.go#L437) that exactly matches the durable authority. | Availability depends on retaining both the runtime record and the approval service's decision. |
| Approval resume changes input, call, contract, preview, transcript, model binding, operation set, or session revision | Original waiting authority remains the reference; conforming resume transactions leave it unchanged on rejection. | Fails before executor acquisition, normally with `ErrOperationPlanChanged` or `ErrModelBindingMismatch`. | Route the approval back to the compatible deployment or start a new run/new approval explicitly. | Runtime intentionally provides no implicit migration or “close enough” resume path. |
| Reconciliation transition returns an ambiguous store error | The requested transition may or may not have committed. | Runtime reads the record and full history using a detached cleanup context. It reports success only when the exact record and exactly one identical transition prove the commit. | Repair store reads/history if proof is unavailable; retry reconciliation with the same durable facts only after inspecting current state. | A corrupt or incomplete history prevents proof even if the state row appears terminal. |

## Explicit non-guarantees

The runtime does **not** guarantee any of the following:

- External exactly-once execution across an arbitrary API and the runtime's
  `ExecutionStore`. Exactly-once-like behavior requires resource-specific
  atomic mutation, fencing, and receipt persistence, or an external API with
  equivalent idempotency semantics.
- Automatic resolution of `started` or `unknown`. Unknown is a durable safety
  outcome, not retry permission.
- Correctness of a host's database adapter, executor, verifier, policy,
  approver, or reconciliation evidence. Conformance tests check observable
  protocol behavior; they cannot prove a production storage engine or external
  authority is truthful.
- Rollback or compensation of a committed side effect. Compensation must be a
  separately modeled, fenced operation with its own idempotency identity.
- Availability or bounded recovery time. A safe execution can remain blocked
  forever when authoritative evidence is unavailable.
- Semantic correctness of model output, operation business rules, approval
  decisions, or verifier predicates. Runtime validates registered schemas and
  immutable authority, not domain truth.
- Protection for a mutation registered as `OperationEffectRead`, or a side
  effect performed from normalizers, preview builders, policies, verifiers,
  event observers, or other callbacks outside the write executor boundary.
- Atomicity between execution completion and the complete run transcript,
  session snapshot, or host response. The durable execution journal is the
  replay authority when later run persistence fails.
- Recovery after process exit when `InMemoryStore` is used, or after durable
  records/history are deleted, rewritten, or silently backfilled.

## Operator rule

Never retry from an exception string alone. Load the current
[`OperationExecutionRecord`](../store.go#L560) and transition history, inspect the authoritative
domain receipt keyed by `ExecutionID`, and bind any reconciliation to the
current `AttemptID`. Use `retry`/`abandon` only for proved non-application and
`complete` only for proved commit. If neither can be proved, retain
`started`/`unknown` and escalate rather than guessing.
