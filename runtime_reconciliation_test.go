package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRetriesWriteAfterExplicitReconciliation(t *testing.T) {
	sentinel := errors.New("executor connection lost")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-3", "done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		if executions == 1 {
			return OperationResult{}, sentinel
		}
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply", IdempotencyKey: "reconciled-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("first Run error=%v, want sentinel", err)
	}
	var executionID string
	for id := range store.executions {
		executionID = id
	}
	if executionID == "" {
		t.Fatal("missing journal execution")
	}
	unknown, err := store.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if unknown.Status != OperationExecutionUnknown {
		t.Fatalf("execution status=%q, want unknown", unknown.Status)
	}
	if err := rt.ReconcileOperation(context.Background(), ReconcileOperationRequest{
		ExecutionID:       executionID,
		ExpectedAttemptID: unknown.AttemptID,
		Action:            OperationReconciliationRetry,
		Actor:             "test-host",
		Message:           "host confirmed no side effect",
	}); err != nil {
		t.Fatalf("ReconcileOperation: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if executions != 2 {
		t.Fatalf("executor calls=%d, want 2", executions)
	}
	execution, err := store.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetExecution after retry: %v", err)
	}
	if execution.Status != OperationExecutionCompleted {
		t.Fatalf("execution status=%q, want completed", execution.Status)
	}
}

func TestRuntimeReplaysHostReconciledCompletedWrite(t *testing.T) {
	sentinel := errors.New("executor connection lost")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-3", "done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{}, sentinel
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply", IdempotencyKey: "reconciled-completed-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("first Run error=%v, want sentinel", err)
	}
	var executionID string
	for id := range store.executions {
		executionID = id
	}
	unknown, err := store.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if err := rt.ReconcileOperation(context.Background(), ReconcileOperationRequest{
		ExecutionID:       executionID,
		ExpectedAttemptID: unknown.AttemptID,
		Action:            OperationReconciliationComplete,
		Result:            OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":2}`)},
		Actor:             "test-host",
		Message:           "host observed version 2",
	}); err != nil {
		t.Fatalf("ReconcileOperation: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
}

func TestReconcileOperationPersistsPermanentRecoveryFailure(t *testing.T) {
	sentinel := errors.New("executor connection lost")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{}, sentinel
	}), confirmingVerifier(), nil, store)
	if _, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "recovery-failure", IdempotencyScope: "test"}); !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}
	var executionID string
	for id := range store.executions {
		executionID = id
	}
	unknown, err := store.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if err := rt.ReconcileOperation(context.Background(), ReconcileOperationRequest{
		ExecutionID: executionID, ExpectedAttemptID: unknown.AttemptID,
		Action: OperationReconciliationFail, Actor: "automatic-recovery",
		Message:  "durable receipt conflicts with approved request",
		Evidence: json.RawMessage(`{"reason":"receipt_mismatch"}`),
	}); err != nil {
		t.Fatalf("ReconcileOperation: %v", err)
	}
	failed, err := store.GetExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetExecution after reconciliation: %v", err)
	}
	if failed.Status != OperationExecutionRecoveryFailed || hasOperationResult(failed.Result) {
		t.Fatalf("execution=%+v, want recovery_failed without a result", failed)
	}
	transitions, err := store.ListExecutionTransitions(context.Background(), executionID)
	if err != nil {
		t.Fatalf("ListExecutionTransitions: %v", err)
	}
	last := transitions[len(transitions)-1]
	if last.From != OperationExecutionUnknown || last.To != OperationExecutionRecoveryFailed || last.Actor != "automatic-recovery" {
		t.Fatalf("last transition=%+v", last)
	}
}

func TestReconcileOperationRejectsStartedAttempt(t *testing.T) {
	store := &recordingStore{}
	execution := OperationExecutionRecord{
		ID: "execution-active", IdempotencyKey: "request-active", IdempotencyScope: "test",
		RunID: "run-active", CallID: "call-active", AttemptID: "attempt-active",
		Name: "apply_change", Arguments: json.RawMessage(`{}`), Status: OperationExecutionStarted,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	if _, err := store.AcquireExecution(context.Background(), AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-active", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
			Message: "execution acquired", To: OperationExecutionStarted, CreatedAt: time.Unix(1, 0),
		},
	}); err != nil {
		t.Fatalf("AcquireExecution: %v", err)
	}
	rt := &Runtime{executions: store}
	err := rt.ReconcileOperation(context.Background(), ReconcileOperationRequest{
		ExecutionID: execution.ID, ExpectedAttemptID: execution.AttemptID,
		Action: OperationReconciliationRetry, Actor: "operator", Message: "retry active attempt",
	})
	if !errors.Is(err, ErrInvalidReconciliation) {
		t.Fatalf("ReconcileOperation error=%v, want ErrInvalidReconciliation", err)
	}
	got, err := store.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if got.Status != OperationExecutionStarted || got.AttemptID != execution.AttemptID {
		t.Fatalf("execution=%+v, want unchanged started attempt", got)
	}
}

func TestExecutionTransitionRejectsPayloadForNonResultStates(t *testing.T) {
	now := time.Unix(10, 0)
	tests := []struct {
		name       string
		transition OperationExecutionTransition
	}{
		{
			name: "unknown with result",
			transition: OperationExecutionTransition{
				ID: "transition-unknown", ExecutionID: "execution-1", AttemptID: "attempt-1",
				Actor: "runtime", Message: "outcome unknown", From: OperationExecutionStarted,
				To: OperationExecutionUnknown, Result: OperationResult{Output: json.RawMessage(`{}`)}, CreatedAt: now,
			},
		},
		{
			name: "retryable with verification",
			transition: OperationExecutionTransition{
				ID: "transition-retry", ExecutionID: "execution-1", AttemptID: "attempt-1",
				Actor: "operator", Message: "safe to retry", From: OperationExecutionUnknown,
				To: OperationExecutionRetryable, Verification: &VerificationResult{Confirmed: true}, CreatedAt: now,
			},
		},
		{
			name: "started with result",
			transition: OperationExecutionTransition{
				ID: "transition-start", ExecutionID: "execution-1", AttemptID: "attempt-2",
				Actor: "runtime", Message: "retry acquired", From: OperationExecutionRetryable,
				To: OperationExecutionStarted, Result: OperationResult{Output: json.RawMessage(`{}`)}, CreatedAt: now,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.transition.Validate(); !errors.Is(err, ErrInvalidExecutionTransition) {
				t.Fatalf("Validate error=%v, want ErrInvalidExecutionTransition", err)
			}
		})
	}
}

func TestExecutionTransitionRejectsInvalidInternalArtifactJSON(t *testing.T) {
	transition := OperationExecutionTransition{
		ID: "transition-1", ExecutionID: "execution-1", AttemptID: "attempt-1",
		RunID: "run-1", CallID: "call-1", Actor: "runtime", Message: "executed",
		From: OperationExecutionStarted, To: OperationExecutionExecuted, CreatedAt: time.Unix(10, 0),
		Result: OperationResult{
			Output: json.RawMessage(`{"applied":true}`),
			Artifacts: []ResultArtifact{{
				Type: "change_set", Data: json.RawMessage(`{"id":"change-1"}`), InternalData: json.RawMessage(`{"storage_key"`),
			}},
		},
	}
	err := transition.Validate()
	if !errors.Is(err, ErrInvalidExecutionTransition) || !strings.Contains(err.Error(), "internal data must be valid JSON") {
		t.Fatalf("Validate error=%v, want invalid internal artifact transition", err)
	}
}

func TestExecutionTransitionRequiresAuditCorrelationFields(t *testing.T) {
	base := OperationExecutionTransition{
		ID: "transition-1", ExecutionID: "execution-1", AttemptID: "attempt-1",
		RunID: "run-1", CallID: "call-1", Actor: "runtime", Message: "outcome unknown",
		From: OperationExecutionStarted, To: OperationExecutionUnknown, CreatedAt: time.Unix(10, 0),
	}
	tests := []struct {
		name   string
		mutate func(*OperationExecutionTransition)
	}{
		{name: "missing run id", mutate: func(transition *OperationExecutionTransition) { transition.RunID = "" }},
		{name: "missing call id", mutate: func(transition *OperationExecutionTransition) { transition.CallID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transition := base
			test.mutate(&transition)
			if err := transition.Validate(); !errors.Is(err, ErrInvalidExecutionTransition) {
				t.Fatalf("Validate error=%v, want ErrInvalidExecutionTransition", err)
			}
		})
	}
}

func TestAcquireExecutionRejectsPollutedStartedRecord(t *testing.T) {
	now := time.Unix(10, 0)
	execution := OperationExecutionRecord{
		ID: "execution-1", IdempotencyKey: "request-1", IdempotencyScope: "test",
		RunID: "run-1", CallID: "call-1", AttemptID: "attempt-1", Name: "apply_change",
		Arguments: json.RawMessage(`{}`), Status: OperationExecutionStarted,
		Result:    OperationResult{Output: json.RawMessage(`{"applied":true}`)},
		CreatedAt: now, UpdatedAt: now,
	}
	err := (AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-1", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
			Message: "execution acquired", To: OperationExecutionStarted, CreatedAt: now,
		},
	}).Validate()
	if !errors.Is(err, ErrInvalidExecutionTransition) {
		t.Fatalf("Validate error=%v, want ErrInvalidExecutionTransition", err)
	}
}

func TestExecutionStoreRejectsStaleAttemptsAndPreservesTransitionHistory(t *testing.T) {
	store := &recordingStore{}
	execution := OperationExecutionRecord{
		ID: "execution-1", IdempotencyKey: "request-1", IdempotencyScope: "test", RunID: "run-1", CallID: "call-1",
		AttemptID: "attempt-1", Name: "apply_change", Arguments: json.RawMessage(`{}`),
		Status: OperationExecutionStarted, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	acquired, err := store.AcquireExecution(context.Background(), AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-1", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime", Message: "execution acquired",
			To: OperationExecutionStarted, CreatedAt: time.Unix(1, 0),
		},
	})
	if err != nil || acquired.Disposition != ExecutionAcquired {
		t.Fatalf("first AcquireExecution result=%+v err=%v", acquired, err)
	}
	if _, err := store.TransitionExecution(context.Background(), OperationExecutionTransition{
		ID: "transition-2", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime", Message: "outcome unknown",
		From: OperationExecutionStarted, To: OperationExecutionUnknown, CreatedAt: time.Unix(2, 0),
	}); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	evidence := json.RawMessage(`{"ticket":"INC-1"}`)
	if _, err := store.TransitionExecution(context.Background(), OperationExecutionTransition{
		ID: "transition-3", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "operator@example.com",
		Message: "host confirmed no side effect", Evidence: evidence,
		From: OperationExecutionUnknown, To: OperationExecutionRetryable, CreatedAt: time.Unix(3, 0),
	}); err != nil {
		t.Fatalf("reconcile retry: %v", err)
	}
	retry := execution
	retry.RunID = "run-2"
	retry.CallID = "call-2"
	retry.AttemptID = "attempt-2"
	retry.UpdatedAt = time.Unix(4, 0)
	acquired, err = store.AcquireExecution(context.Background(), AcquireExecutionRequest{
		Execution: retry,
		Transition: OperationExecutionTransition{
			ID: "transition-4", ExecutionID: retry.ID, AttemptID: retry.AttemptID,
			RunID: retry.RunID, CallID: retry.CallID, Actor: "runtime", Message: "execution acquired",
			To: OperationExecutionStarted, CreatedAt: time.Unix(4, 0),
		},
	})
	if err != nil || acquired.Disposition != ExecutionAcquired {
		t.Fatalf("retry AcquireExecution result=%+v err=%v", acquired, err)
	}
	result := OperationResult{Output: json.RawMessage(`{"applied":true}`)}
	if _, err := store.TransitionExecution(context.Background(), OperationExecutionTransition{
		ID: "transition-stale", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "stale-observer",
		Message: "stale execution", From: OperationExecutionStarted,
		To: OperationExecutionExecuted, Result: result, CreatedAt: time.Unix(5, 0),
	}); !errors.Is(err, ErrOperationAttemptLost) {
		t.Fatalf("stale transition error=%v, want ErrOperationAttemptLost", err)
	}
	if _, err := store.TransitionExecution(context.Background(), OperationExecutionTransition{
		ID: "transition-5", ExecutionID: execution.ID, AttemptID: retry.AttemptID,
		RunID: retry.RunID, CallID: retry.CallID, Actor: "runtime", Message: "operation executed",
		From: OperationExecutionStarted, To: OperationExecutionExecuted, Result: result, CreatedAt: time.Unix(6, 0),
	}); err != nil {
		t.Fatalf("current execution: %v", err)
	}
	if _, err := store.TransitionExecution(context.Background(), OperationExecutionTransition{
		ID: "transition-6", ExecutionID: execution.ID, AttemptID: retry.AttemptID,
		RunID: retry.RunID, CallID: retry.CallID, Actor: "runtime", Message: "operation completed",
		From: OperationExecutionExecuted, To: OperationExecutionCompleted, Result: result, CreatedAt: time.Unix(7, 0),
	}); err != nil {
		t.Fatalf("current completion: %v", err)
	}
	history, err := store.ListExecutionTransitions(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("ListExecutionTransitions: %v", err)
	}
	assertExecutionTransitionHistory(t, history, evidence)
}

func assertExecutionTransitionHistory(
	t *testing.T,
	history []OperationExecutionTransition,
	evidence json.RawMessage,
) {
	t.Helper()
	if len(history) != 6 {
		t.Fatalf("transition history=%d, want 6: %+v", len(history), history)
	}
	if history[1].AttemptID != "attempt-1" || history[2].Actor != "operator@example.com" || string(history[2].Evidence) != string(evidence) || history[3].AttemptID != "attempt-2" {
		t.Fatalf("transition history lost immutable audit fields: %+v", history)
	}
}
