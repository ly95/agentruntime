package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type commitErrorCompletionStore struct {
	recordingStore
	err error
}

type recordOnlyCompletionStore struct {
	recordingStore
	err error
}

type historyReadFailureCompletionStore struct {
	commitErrorCompletionStore
	historyErr error
}

func (store *historyReadFailureCompletionStore) ListExecutionTransitions(context.Context, string) ([]OperationExecutionTransition, error) {
	return nil, store.historyErr
}

type malformedHistoryCompletionStore struct {
	commitErrorCompletionStore
}

type cancelAfterCommitCompletionStore struct {
	commitErrorCompletionStore
	cancel context.CancelFunc
}

func (store *cancelAfterCommitCompletionStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	record, err := store.commitErrorCompletionStore.TransitionExecution(ctx, transition)
	if transition.From == OperationExecutionStarted && transition.To == OperationExecutionCompleted {
		store.cancel()
	}
	return record, err
}

func (store *malformedHistoryCompletionStore) ListExecutionTransitions(ctx context.Context, executionID string) ([]OperationExecutionTransition, error) {
	history, err := store.recordingStore.ListExecutionTransitions(ctx, executionID)
	if err == nil && len(history) > 1 {
		history[len(history)-1].Evidence = json.RawMessage(`null`)
	}
	return history, err
}

func (store *recordOnlyCompletionStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	record, err := store.recordingStore.TransitionExecution(ctx, transition)
	if err != nil || transition.From != OperationExecutionStarted || transition.To != OperationExecutionCompleted {
		return record, err
	}
	store.mu.Lock()
	history := store.transitions[transition.ExecutionID]
	store.transitions[transition.ExecutionID] = history[:len(history)-1]
	store.mu.Unlock()
	return OperationExecutionRecord{}, store.err
}

type concurrentAmbiguousCompletionStore struct {
	recordingStore
	err       error
	arrived   atomic.Int32
	winner    atomic.Bool
	ready     chan struct{}
	committed chan struct{}
}

func (store *concurrentAmbiguousCompletionStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	if transition.From != OperationExecutionStarted || transition.To != OperationExecutionCompleted {
		return store.recordingStore.TransitionExecution(ctx, transition)
	}
	if store.arrived.Add(1) == 2 {
		close(store.ready)
	}
	select {
	case <-store.ready:
	case <-ctx.Done():
		return OperationExecutionRecord{}, ctx.Err()
	}
	if store.winner.CompareAndSwap(false, true) {
		record, err := store.recordingStore.TransitionExecution(ctx, transition)
		close(store.committed)
		return record, err
	}
	select {
	case <-store.committed:
		return OperationExecutionRecord{}, store.err
	case <-ctx.Done():
		return OperationExecutionRecord{}, ctx.Err()
	}
}

func (store *commitErrorCompletionStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	record, err := store.recordingStore.TransitionExecution(ctx, transition)
	if err != nil {
		return OperationExecutionRecord{}, err
	}
	if transition.From == OperationExecutionStarted && transition.To == OperationExecutionCompleted {
		return OperationExecutionRecord{}, store.err
	}
	return record, nil
}

func seedStartedReconciliationExecution(t *testing.T, operations *OperationRegistry, store ExecutionStore, confirmationRequired bool) OperationExecutionRecord {
	t.Helper()
	registered := operation("apply_change", OperationEffectWrite)
	if !confirmationRequired {
		registered.Confirmation = ConfirmationSpec{Mode: ConfirmationNone}
		registered.ApprovalPreview = nil
	}
	if err := operations.Register(registered); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	execution := OperationExecutionRecord{
		ID: "execution-started", IdempotencyKey: "request-started", IdempotencyScope: "tenant",
		RunID: "run-started", CallID: "call-started", AttemptID: "attempt-started",
		Name: registered.Name, ContractID: operationSummary(registered).ContractID,
		VerificationRequired: confirmationRequired, Arguments: json.RawMessage(`{}`),
		Status: OperationExecutionStarted, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.AcquireExecution(t.Context(), AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-acquired", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime", Message: "execution acquired",
			To: OperationExecutionStarted, VerificationRequired: confirmationRequired, CreatedAt: now,
		},
	}); err != nil {
		t.Fatalf("AcquireExecution: %v", err)
	}
	return execution
}

func startedCompletionRequest(execution OperationExecutionRecord) ReconcileOperationRequest {
	request := ReconcileOperationRequest{
		ExecutionID: execution.ID, ExpectedAttemptID: execution.AttemptID,
		Action: OperationReconciliationComplete,
		Result: OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":2}`)},
		Actor:  "recovery-worker", Message: "durable receipt proves the exact attempt committed",
		Evidence: json.RawMessage(`{"attempt":"attempt-started","receipt":"version-2"}`),
	}
	if execution.VerificationRequired {
		request.Verification = &VerificationResult{
			Confirmed: true, Message: "host verified the committed state", Evidence: json.RawMessage(`{"version":2}`),
		}
	}
	return request
}

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
	operations := NewOperationRegistry()
	registered := operation("apply_change", OperationEffectWrite)
	if err := operations.Register(registered); err != nil {
		t.Fatal(err)
	}
	verificationRequired := registered.Confirmation.Mode == ConfirmationRequired
	execution := OperationExecutionRecord{
		ID: "execution-1", IdempotencyKey: "request-1", IdempotencyScope: "test",
		RunID: "run-1", CallID: "call-1", AttemptID: "attempt-1",
		Name: "apply_change", ContractID: operationSummary(registered).ContractID,
		VerificationRequired: verificationRequired, Arguments: []byte(`{}`), Status: OperationExecutionStarted,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.AcquireExecution(t.Context(), AcquireExecutionRequest{
		Execution: execution,
		Transition: OperationExecutionTransition{
			ID: "transition-1", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
			RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
			Message: "execution acquired", To: OperationExecutionStarted,
			VerificationRequired: verificationRequired, CreatedAt: now,
		},
	}); err != nil {
		t.Fatalf("AcquireExecution: %v", err)
	}
	if _, err := store.TransitionExecution(t.Context(), OperationExecutionTransition{
		ID: "transition-2", ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime",
		Message: "outcome unknown", From: OperationExecutionStarted,
		To: OperationExecutionUnknown, VerificationRequired: verificationRequired, CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	reconciler, err := NewOperationReconciler(operations, store)
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

func TestOperationReconcilerCompletesEvidenceBackedStartedAttempt(t *testing.T) {
	for _, test := range []struct {
		name                 string
		confirmationRequired bool
	}{
		{name: "direct write"},
		{name: "confirmation-required write", confirmationRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := NewOperationRegistry()
			store := &recordingStore{}
			execution := seedStartedReconciliationExecution(t, operations, store, test.confirmationRequired)

			// Constructing a fresh reconciler models recovery after the runtime that
			// invoked the executor has exited without settling its journal entry.
			reconciler, err := NewOperationReconciler(operations, store)
			if err != nil {
				t.Fatal(err)
			}
			request := startedCompletionRequest(execution)
			if err := reconciler.ReconcileOperation(t.Context(), request); err != nil {
				t.Fatalf("ReconcileOperation: %v", err)
			}

			settled, err := store.GetExecution(t.Context(), execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if settled.Status != OperationExecutionCompleted || !equalOperationResult(settled.Result, request.Result) {
				t.Fatalf("settled execution=%+v, want completed exact result", settled)
			}
			if test.confirmationRequired {
				if settled.Verification == nil || !settled.Verification.Confirmed {
					t.Fatalf("settled verification=%+v, want positive durable verification", settled.Verification)
				}
			} else if settled.Verification != nil {
				t.Fatalf("direct write retained unexpected verification: %+v", settled.Verification)
			}
			transitions, err := store.ListExecutionTransitions(t.Context(), execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			last := transitions[len(transitions)-1]
			if last.From != OperationExecutionStarted || last.To != OperationExecutionCompleted || !jsonSemanticallyEqual(last.Evidence, request.Evidence) {
				t.Fatalf("completion transition=%+v, want evidence-backed started -> completed", last)
			}
		})
	}
}

func TestOperationReconcilerRejectsEvidenceFreeStartedCompletion(t *testing.T) {
	operations := NewOperationRegistry()
	store := &recordingStore{}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	reconciler, err := NewOperationReconciler(operations, store)
	if err != nil {
		t.Fatal(err)
	}
	request := startedCompletionRequest(execution)
	request.Evidence = nil
	if err := reconciler.ReconcileOperation(t.Context(), request); !errors.Is(err, ErrInvalidReconciliation) {
		t.Fatalf("ReconcileOperation error=%v, want ErrInvalidReconciliation", err)
	}
	unchanged, err := store.GetExecution(t.Context(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != OperationExecutionStarted || hasOperationResult(unchanged.Result) {
		t.Fatalf("execution mutated after rejected completion: %+v", unchanged)
	}
}

func TestOperationReconcilerRejectsStaleStartedAttemptCompletion(t *testing.T) {
	operations := NewOperationRegistry()
	store := &recordingStore{}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	reconciler, err := NewOperationReconciler(operations, store)
	if err != nil {
		t.Fatal(err)
	}
	request := startedCompletionRequest(execution)
	request.ExpectedAttemptID = "stale-attempt"
	if err := reconciler.ReconcileOperation(t.Context(), request); !errors.Is(err, ErrOperationAttemptLost) {
		t.Fatalf("ReconcileOperation error=%v, want ErrOperationAttemptLost", err)
	}
	unchanged, err := store.GetExecution(t.Context(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != OperationExecutionStarted || unchanged.AttemptID != execution.AttemptID {
		t.Fatalf("execution mutated after stale reconciliation: %+v", unchanged)
	}
}

func TestOperationReconcilerProvesStartedCompletionCommittedAfterAcknowledgementError(t *testing.T) {
	for _, test := range []struct {
		name                 string
		confirmationRequired bool
	}{
		{name: "direct write"},
		{name: "confirmation-required write", confirmationRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("transition acknowledgement lost")
			operations := NewOperationRegistry()
			store := &commitErrorCompletionStore{err: sentinel}
			execution := seedStartedReconciliationExecution(t, operations, store, test.confirmationRequired)
			reconciler, err := NewOperationReconciler(operations, store)
			if err != nil {
				t.Fatal(err)
			}
			if err := reconciler.ReconcileOperation(t.Context(), startedCompletionRequest(execution)); err != nil {
				t.Fatalf("ReconcileOperation returned an error after proving the exact durable commit: %v", err)
			}
			settled, err := store.GetExecution(t.Context(), execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if settled.Status != OperationExecutionCompleted || settled.VerificationRequired != test.confirmationRequired {
				t.Fatalf("execution=%+v, want completed with verificationRequired=%t", settled, test.confirmationRequired)
			}
		})
	}
}

func TestOperationReconcilerUsesDetachedContextToProveCommittedTransition(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	acknowledgementErr := errors.New("transition acknowledgement lost")
	operations := NewOperationRegistry()
	store := &cancelAfterCommitCompletionStore{
		commitErrorCompletionStore: commitErrorCompletionStore{err: acknowledgementErr},
		cancel:                     cancel,
	}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	reconciler, err := NewOperationReconciler(operations, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileOperation(ctx, startedCompletionRequest(execution)); err != nil {
		t.Fatalf("ReconcileOperation did not prove exact commit after parent cancellation: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("parent context error=%v, want canceled", ctx.Err())
	}
}

func TestOperationReconcilerRejectsRecordOnlyAmbiguousCompletion(t *testing.T) {
	sentinel := errors.New("transition acknowledgement lost")
	operations := NewOperationRegistry()
	store := &recordOnlyCompletionStore{err: sentinel}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	reconciler, err := NewOperationReconciler(operations, store)
	if err != nil {
		t.Fatal(err)
	}
	err = reconciler.ReconcileOperation(t.Context(), startedCompletionRequest(execution))
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "transition history contains 0 exact entries") {
		t.Fatalf("ReconcileOperation error=%v, want acknowledgement error plus missing-history proof failure", err)
	}
	settled, getErr := store.GetExecution(t.Context(), execution.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if settled.Status != OperationExecutionCompleted {
		t.Fatalf("execution status=%q, want record-only completed fixture state", settled.Status)
	}
	history, historyErr := store.ListExecutionTransitions(t.Context(), execution.ID)
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	if len(history) != 1 {
		t.Fatalf("transition count=%d, want acquisition only", len(history))
	}
}

func TestOperationReconcilerRejectsUnreadableOrMalformedCommitHistory(t *testing.T) {
	t.Run("history read failure", func(t *testing.T) {
		acknowledgementErr := errors.New("transition acknowledgement lost")
		historyErr := errors.New("transition history unavailable")
		operations := NewOperationRegistry()
		store := &historyReadFailureCompletionStore{
			commitErrorCompletionStore: commitErrorCompletionStore{err: acknowledgementErr},
			historyErr:                 historyErr,
		}
		execution := seedStartedReconciliationExecution(t, operations, store, false)
		reconciler, err := NewOperationReconciler(operations, store)
		if err != nil {
			t.Fatal(err)
		}
		err = reconciler.ReconcileOperation(t.Context(), startedCompletionRequest(execution))
		if !errors.Is(err, acknowledgementErr) || !errors.Is(err, historyErr) {
			t.Fatalf("ReconcileOperation error=%v, want acknowledgement and history errors", err)
		}
	})

	t.Run("malformed matching history", func(t *testing.T) {
		acknowledgementErr := errors.New("transition acknowledgement lost")
		operations := NewOperationRegistry()
		store := &malformedHistoryCompletionStore{
			commitErrorCompletionStore: commitErrorCompletionStore{err: acknowledgementErr},
		}
		execution := seedStartedReconciliationExecution(t, operations, store, false)
		reconciler, err := NewOperationReconciler(operations, store)
		if err != nil {
			t.Fatal(err)
		}
		err = reconciler.ReconcileOperation(t.Context(), startedCompletionRequest(execution))
		if !errors.Is(err, acknowledgementErr) || !errors.Is(err, ErrInvalidExecutionTransition) {
			t.Fatalf("ReconcileOperation error=%v, want acknowledgement and malformed-history errors", err)
		}
	})
}

func TestRuntimeAmbiguousCompletionProofRequiresExactConcurrentTransition(t *testing.T) {
	sentinel := errors.New("competing transition acknowledgement")
	operations := NewOperationRegistry()
	store := &concurrentAmbiguousCompletionStore{
		err: sentinel, ready: make(chan struct{}), committed: make(chan struct{}),
	}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	var identity atomic.Uint64
	runtime := &Runtime{
		operations: operations, executions: store,
		now: func() time.Time { return time.Unix(200, 0) },
		newID: func() string {
			return fmt.Sprintf("concurrent-transition-%d", identity.Add(1))
		},
		cleanupTimeout: defaultDetachedCleanupTimeout,
	}
	requests := []ReconcileOperationRequest{startedCompletionRequest(execution), startedCompletionRequest(execution)}
	requests[0].Actor = "reconciler-a"
	requests[0].Message = "receipt a proves commit"
	requests[0].Evidence = json.RawMessage(`{"receipt":"a"}`)
	requests[1].Actor = "reconciler-b"
	requests[1].Message = "receipt b proves commit"
	requests[1].Evidence = json.RawMessage(`{"receipt":"b"}`)
	errorsByWorker := make([]error, len(requests))
	var workers sync.WaitGroup
	for index := range requests {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			errorsByWorker[index] = runtime.ReconcileOperation(t.Context(), requests[index])
		}(index)
	}
	workers.Wait()
	successes := 0
	ambiguousFailures := 0
	for _, err := range errorsByWorker {
		if err == nil {
			successes++
		}
		if errors.Is(err, sentinel) {
			ambiguousFailures++
		}
	}
	if successes != 1 || ambiguousFailures != 1 {
		t.Fatalf("concurrent reconciliation errors=%v, want one success and one exact-history failure", errorsByWorker)
	}
	history, err := store.ListExecutionTransitions(t.Context(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("transition count=%d, want acquisition plus one exact completion", len(history))
	}
}

func TestOperationReconcilerSerializesConcurrentStartedCompletions(t *testing.T) {
	operations := NewOperationRegistry()
	store := &recordingStore{}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	reconciler, err := NewOperationReconciler(operations, store)
	if err != nil {
		t.Fatal(err)
	}
	request := startedCompletionRequest(execution)
	start := make(chan struct{})
	errorsByWorker := make([]error, 2)
	var workers sync.WaitGroup
	for index := range errorsByWorker {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			errorsByWorker[index] = reconciler.ReconcileOperation(t.Context(), request)
		}(index)
	}
	close(start)
	workers.Wait()
	successes := 0
	for _, err := range errorsByWorker {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent reconciliation errors=%v, want exactly one success", errorsByWorker)
	}
	settled, err := store.GetExecution(t.Context(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != OperationExecutionCompleted {
		t.Fatalf("execution status=%q, want completed", settled.Status)
	}
	transitions, err := store.ListExecutionTransitions(t.Context(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 {
		t.Fatalf("transition count=%d, want acquisition plus one completion", len(transitions))
	}
}

func TestExecutionTransitionRequiresEvidenceForStartedCompletion(t *testing.T) {
	transition := OperationExecutionTransition{
		ID: "transition-complete", ExecutionID: "execution-started", AttemptID: "attempt-started",
		RunID: "run-started", CallID: "call-started", Actor: "recovery-worker", Message: "receipt proves commit",
		From: OperationExecutionStarted, To: OperationExecutionCompleted,
		Result: OperationResult{Output: json.RawMessage(`{"applied":true}`)}, CreatedAt: time.Unix(101, 0),
	}
	if err := transition.Validate(); !errors.Is(err, ErrInvalidExecutionTransition) {
		t.Fatalf("Validate without evidence error=%v, want ErrInvalidExecutionTransition", err)
	}
	transition.Evidence = json.RawMessage(`{"receipt":"version-2"}`)
	if err := transition.Validate(); err != nil {
		t.Fatalf("Validate evidence-backed started completion: %v", err)
	}
}
