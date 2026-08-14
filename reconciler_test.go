package agentruntime

import (
	"errors"
	"testing"
	"time"
)

func TestNewOperationReconcilerRejectsTypedNilExecutionStore(t *testing.T) {
	var executions *recordingStore

	reconciler, err := NewOperationReconciler(NewOperationRegistry(), executions)
	if err == nil {
		t.Fatal("NewOperationReconciler accepted a typed nil execution store")
	}
	if reconciler != nil {
		t.Fatalf("NewOperationReconciler returned reconciler %#v after rejecting a typed nil execution store", reconciler)
	}
}

func TestOperationReconcilerRejectsNilReceiver(t *testing.T) {
	var reconciler *OperationReconciler

	err := reconciler.ReconcileOperation(t.Context(), ReconcileOperationRequest{})
	if !errors.Is(err, ErrExecutionStoreRequired) {
		t.Fatalf("ReconcileOperation error=%v, want ErrExecutionStoreRequired", err)
	}
}

func TestOperationReconcilerSettlesUnknownExecutionWithoutRuntimeDependencies(t *testing.T) {
	now := time.Unix(10, 0)
	store := &recordingStore{}
	execution := OperationExecutionRecord{
		ID: "execution-1", IdempotencyKey: "request-1", IdempotencyScope: "test",
		RunID: "run-1", CallID: "call-1", AttemptID: "attempt-1",
		Name: "apply_change", Arguments: []byte(`{}`), Status: OperationExecutionStarted,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.AcquireExecution(t.Context(), AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-1", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
			Message: "execution acquired", To: OperationExecutionStarted, CreatedAt: now,
		},
	}); err != nil {
		t.Fatalf("AcquireExecution: %v", err)
	}
	if _, err := store.TransitionExecution(t.Context(), OperationExecutionTransition{
		ID: "transition-2", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
		Message: "outcome unknown", From: OperationExecutionStarted,
		To: OperationExecutionUnknown, CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	reconciler, err := NewOperationReconciler(NewOperationRegistry(), store)
	if err != nil {
		t.Fatalf("NewOperationReconciler: %v", err)
	}
	if err := reconciler.ReconcileOperation(t.Context(), ReconcileOperationRequest{
		ExecutionID: execution.ID, ExpectedAttemptID: execution.AttemptID,
		Action: OperationReconciliationRetry, Actor: "operator",
		Message: "host confirmed no side effect",
	}); err != nil {
		t.Fatalf("ReconcileOperation: %v", err)
	}

	settled, err := store.GetExecution(t.Context(), execution.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if settled.Status != OperationExecutionRetryable {
		t.Fatalf("execution status=%q, want retryable", settled.Status)
	}
}
