package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func modelLifecycleEvents(events []Event) (started, completed, failed, runFailed Event, terminalCount int) {
	for _, event := range events {
		switch event.Type {
		case EventModelStarted:
			started = event
		case EventModelCompleted:
			completed = event
			terminalCount++
		case EventModelFailed:
			failed = event
			terminalCount++
		case EventRunFailed:
			runFailed = event
		}
	}
	return started, completed, failed, runFailed, terminalCount
}

func assertStoredErrorModelCallID(t *testing.T, items []ItemRecord, modelCallID string) {
	t.Helper()
	for _, item := range items {
		if item.Type == ItemTypeError {
			if item.ModelCallID != modelCallID {
				t.Fatalf("error item=%+v, want model_call_id=%q", item, modelCallID)
			}
			return
		}
	}
	t.Fatal("missing stored error item")
}

func TestRuntimeUsesDetachedContextsForCancellationCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &cancellationAwareStore{}
	executor := OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		if request.SessionLease.Generation == 0 || request.SessionLease.SessionID != "session-cancel" {
			t.Fatalf("write executor received invalid session fence: %+v", request.SessionLease)
		}
		cancel()
		return OperationResult{}, context.Canceled
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	_, err := rt.Run(ctx, Input{User: "apply", SessionID: "session-cancel", IdempotencyKey: "cancel-request"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v, want context.Canceled", err)
	}
	if store.unknownContextErr != nil || store.errorItemContextErr != nil || store.finishContextErr != nil {
		t.Fatalf("cleanup saw canceled context: unknown=%v error_item=%v finish=%v", store.unknownContextErr, store.errorItemContextErr, store.finishContextErr)
	}
	if _, leased := store.leases["session-cancel"]; leased {
		t.Fatal("canceled run retained its session lease")
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionUnknown {
			t.Fatalf("execution status=%q, want unknown", execution.Status)
		}
	}
}

func TestRuntimeRenewsSessionLeaseWhileModelIsRunning(t *testing.T) {
	release := make(chan struct{})
	store := &renewalSignalStore{renewed: make(chan struct{})}
	rt, err := NewRuntime(RuntimeConfig{
		Model: blockingModel{release: release}, RunStore: store,
		SessionLeaseTTL: 100 * time.Millisecond, LeaseRenewalInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	type runResult struct {
		result *Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, runErr := rt.Run(context.Background(), Input{User: "wait", SessionID: "session-renew"})
		done <- runResult{result: result, err: runErr}
	}()
	select {
	case <-store.renewed:
		close(release)
	case <-time.After(2 * time.Second):
		t.Fatal("session lease was not renewed")
	}
	select {
	case got := <-done:
		if got.err != nil || got.result == nil || got.result.Output != "done" {
			t.Fatalf("Run result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish after model release")
	}
}

func TestLeaseRenewalStopsAfterCancellationGrace(t *testing.T) {
	store := &graceBlockingRenewStore{
		firstRenewed:   make(chan struct{}),
		secondStarted:  make(chan struct{}),
		secondCanceled: make(chan struct{}),
	}
	begun, err := createRunForTest(t.Context(), store, CreateRunRequest{
		Run:     RunRecord{ID: "run-cancel-grace", SessionID: "session-cancel-grace", Status: RunStatusRunning},
		LeaseID: "lease-cancel-grace", LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	parent, cancelParent := context.WithCancel(context.Background())
	guard := newLeaseGuard(store, begun.Handle, time.Minute, 10*time.Millisecond, 80*time.Millisecond)
	runCtx, _, stop := guard.Start(parent)
	defer func() { _ = stop() }()

	select {
	case <-store.firstRenewed:
	case <-time.After(time.Second):
		t.Fatal("first renewal did not run")
	}
	cancelParent()
	<-runCtx.Done()
	select {
	case <-store.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("renewal did not remain active during cancellation grace")
	}
	select {
	case <-store.secondCanceled:
	case <-time.After(time.Second):
		t.Fatal("renewal context outlived cancellation grace")
	}
	if err := stop(); err != nil {
		t.Fatalf("stop renewal: %v", err)
	}
}

func TestRuntimePreservesLeaseRenewalFailureFromEarlyRunStep(t *testing.T) {
	sentinel := errors.New("lease store unavailable")
	store := &renewalFailingAppendStore{err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "unused")}}
	runtime := newTestRuntime(t, model, nil, nil, nil, nil, nil, store)
	runtime.sessionLeaseTTL = time.Minute
	runtime.leaseRenewalInterval = 5 * time.Millisecond

	result, err := runtime.Run(t.Context(), Input{User: "start", SessionID: "session-renewal-failure"})
	if result != nil || !errors.Is(err, ErrSessionLeaseLost) || !errors.Is(err, sentinel) || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%+v err=%v, want joined lease loss, store error, and context cancellation", result, err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests=%d, want 0", len(model.requests))
	}
	if len(store.failed) != 1 || store.failed[0].Status != RunStatusFailed {
		t.Fatalf("failed runs=%+v", store.failed)
	}
}

func TestRuntimeKeepsSessionLeaseRenewalActiveThroughFinishRun(t *testing.T) {
	const (
		leaseTTL        = time.Minute
		renewalInterval = 5 * time.Millisecond
	)
	configureLease := func(runtime *Runtime) {
		runtime.sessionLeaseTTL = leaseTTL
		runtime.leaseRenewalInterval = renewalInterval
	}

	t.Run("completed", func(t *testing.T) {
		store := newDeadlineCrossingFinishStore(nil)
		runtime := newTestRuntime(
			t,
			&scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}},
			nil, nil, nil, nil, nil, store,
		)
		configureLease(runtime)

		result, err := runtime.Run(t.Context(), Input{User: "finish", SessionID: "session-finish-completed"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result == nil || result.Status != RunStatusCompleted || result.Output != "done" {
			t.Fatalf("result=%+v", result)
		}
		if statuses := store.statuses(); len(statuses) != 1 || statuses[0] != RunStatusCompleted {
			t.Fatalf("FinishRun statuses=%v, want [completed]", statuses)
		}
	})

	t.Run("waiting user", func(t *testing.T) {
		model := &scriptedModel{responses: []*ModelResponse{
			callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		}}
		operations := NewOperationRegistry()
		if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
			t.Fatal(err)
		}
		policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
			return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
		})
		store := newDeadlineCrossingFinishStore(nil)
		runtime := newTestRuntime(
			t, model, operations, policy,
			OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
				return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
			}),
			confirmingVerifier(), &resumableApprover{}, store,
		)
		configureLease(runtime)

		result, err := runtime.Run(t.Context(), Input{
			RunID: "run-finish-waiting", SessionID: "session-finish-waiting", User: "apply",
			IdempotencyKey: "finish-waiting",
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result == nil || result.Status != RunStatusWaitingUser {
			t.Fatalf("result=%+v", result)
		}
		if statuses := store.statuses(); len(statuses) != 1 || statuses[0] != RunStatusWaitingUser {
			t.Fatalf("FinishRun statuses=%v, want [waiting_user]", statuses)
		}
	})
}

func TestRuntimeKeepsSessionLeaseRenewalActiveThroughFailureCleanup(t *testing.T) {
	t.Run("after completed transaction error", func(t *testing.T) {
		sentinel := errors.New("complete transaction failed")
		store := newDeadlineCrossingFinishStore(sentinel)
		runtime := newTestRuntime(
			t,
			&scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}},
			nil, nil, nil, nil, nil, store,
		)
		runtime.sessionLeaseTTL = time.Minute
		runtime.leaseRenewalInterval = 5 * time.Millisecond

		result, err := runtime.Run(t.Context(), Input{User: "finish", SessionID: "session-finish-failure"})
		if result != nil || !errors.Is(err, sentinel) {
			t.Fatalf("result=%+v err=%v, want complete transaction failure", result, err)
		}
		statuses := store.statuses()
		if len(statuses) != 2 || statuses[0] != RunStatusCompleted || statuses[1] != RunStatusFailed {
			t.Fatalf("FinishRun statuses=%v, want [completed failed]", statuses)
		}
		if len(store.failed) != 1 || store.failed[0].Status != RunStatusFailed {
			t.Fatalf("failed runs=%+v", store.failed)
		}
		if _, leased := store.leases["session-finish-failure"]; leased {
			t.Fatal("failed run retained its session lease")
		}
	})

	t.Run("after parent cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		store := newDeadlineCrossingFinishStore(nil)
		runtime := newTestRuntime(t, cancelingModel{cancel: cancel}, nil, nil, nil, nil, nil, store)
		runtime.sessionLeaseTTL = time.Minute
		runtime.leaseRenewalInterval = 5 * time.Millisecond

		result, err := runtime.Run(ctx, Input{User: "cancel", SessionID: "session-finish-cancelled"})
		if result != nil || !errors.Is(err, context.Canceled) || errors.Is(err, ErrFinishRenewalNotObserved) {
			t.Fatalf("result=%+v err=%v, want context.Canceled", result, err)
		}
		if statuses := store.statuses(); len(statuses) != 1 || statuses[0] != RunStatusFailed {
			t.Fatalf("FinishRun statuses=%v, want [failed]", statuses)
		}
		if len(store.failed) != 1 || store.failed[0].Status != RunStatusFailed {
			t.Fatalf("failed runs=%+v", store.failed)
		}
		if _, leased := store.leases["session-finish-cancelled"]; leased {
			t.Fatal("cancelled run retained its session lease")
		}
	})
}

func TestRunStoreExpiresAndFencesAbandonedSessionLease(t *testing.T) {
	now := time.Unix(100, 0)
	store := &recordingStore{now: func() time.Time { return now }}
	first, err := createRunForTest(context.Background(), store, CreateRunRequest{
		Run:     RunRecord{ID: "run-1", SessionID: "session-lease", Status: RunStatusRunning},
		LeaseID: "lease-1", LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("first BeginRun: %v", err)
	}
	now = now.Add(2 * time.Minute)
	second, err := createRunForTest(context.Background(), store, CreateRunRequest{
		Run:     RunRecord{ID: "run-2", SessionID: "session-lease", Status: RunStatusRunning},
		LeaseID: "lease-2", LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("second BeginRun: %v", err)
	}
	if second.Handle.LeaseGeneration <= first.Handle.LeaseGeneration {
		t.Fatalf("lease generations first=%d second=%d", first.Handle.LeaseGeneration, second.Handle.LeaseGeneration)
	}
	if _, err := store.ValidateRunLease(context.Background(), first.Handle); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("stale lease validation error=%v, want ErrSessionLeaseLost", err)
	}
	if len(store.failed) != 1 || store.failed[0].ID != "run-1" {
		t.Fatalf("abandoned runs=%+v", store.failed)
	}
}

func TestRunStoreRejectsSkillSetSwitchBeforeFencingExpiredLease(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	store := &recordingStore{now: func() time.Time { return now }}
	first, err := createRunForTest(context.Background(), store, CreateRunRequest{
		Run: RunRecord{
			ID: "bound-run-1", SessionID: "bound-session", SkillSetID: "set-a",
			Status: RunStatusRunning, CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "bound-lease-1", LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("first BeginRun: %v", err)
	}
	now = now.Add(2 * time.Minute)
	_, err = createRunForTest(context.Background(), store, CreateRunRequest{
		Run: RunRecord{
			ID: "bound-run-2", SessionID: "bound-session", SkillSetID: "set-b",
			Status: RunStatusRunning, CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "bound-lease-2", LeaseTTL: time.Minute,
	})
	if !errors.Is(err, ErrSkillSetMismatch) {
		t.Fatalf("mismatched BeginRun error=%v", err)
	}
	if len(store.runs) != 1 || store.runs[0].Status != RunStatusRunning || len(store.failed) != 0 {
		t.Fatalf("mismatch changed runs=%+v failed=%+v", store.runs, store.failed)
	}
	if active := store.leases["bound-session"]; !sameTestLeaseOwner(active, first.Handle) {
		t.Fatalf("mismatch changed lease: before=%+v after=%+v", first.Handle, active)
	}
	if binding := store.sessions["bound-session"]; binding.SkillSetID != "set-a" || binding.Revision != 0 {
		t.Fatalf("binding=%+v", binding)
	}

	second, err := createRunForTest(context.Background(), store, CreateRunRequest{
		Run: RunRecord{
			ID: "bound-run-2", SessionID: "bound-session", SkillSetID: "set-a",
			Status: RunStatusRunning, CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "bound-lease-2", LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("same-set fencing BeginRun: %v", err)
	}
	if second.Handle.LeaseGeneration <= first.Handle.LeaseGeneration || len(store.failed) != 1 || store.failed[0].ID != "bound-run-1" {
		t.Fatalf("second=%+v failed=%+v", second, store.failed)
	}
	if binding := store.sessions["bound-session"]; binding.SkillSetID != "set-a" || binding.Revision != 0 {
		t.Fatalf("binding after fencing=%+v", binding)
	}
}

func TestRuntimeRejectsLostLeaseBeforeWriteSideEffect(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &rejectingLeaseStore{}
	executions := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store)
	_, err := rt.Run(context.Background(), Input{
		User: "apply", SessionID: "session-fenced", IdempotencyKey: "lost-lease",
	})
	if !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("Run error=%v, want ErrSessionLeaseLost", err)
	}
	if executions != 0 {
		t.Fatalf("executor calls=%d, want 0", executions)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionRetryable {
			t.Fatalf("execution status=%q, want retryable", execution.Status)
		}
	}
}

func TestRuntimeRejectsLostAttemptBeforeWriteSideEffect(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &rejectingAttemptStore{}
	executions := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store)
	_, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "lost-attempt", IdempotencyScope: "test",
	})
	if !errors.Is(err, ErrOperationAttemptLost) {
		t.Fatalf("Run error=%v, want ErrOperationAttemptLost", err)
	}
	if executions != 0 {
		t.Fatalf("executor calls=%d, want 0", executions)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionRetryable {
			t.Fatalf("execution status=%q, want retryable after contradictory lost-attempt response", execution.Status)
		}
	}
}

func TestRuntimeRejectsDuplicateCallIDBeforePlanningOrExecution(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1",
			ToolCall{ID: "duplicate", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
			ToolCall{ID: "duplicate", Name: "apply_change", Input: json.RawMessage(`{"value":2}`)},
		),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store)
	_, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "duplicate-call"})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "duplicate function call id") {
		t.Fatalf("Run error=%v", err)
	}
	if executions != 0 || len(store.plans) != 0 || len(store.executions) != 0 {
		t.Fatalf("side effects occurred: executor=%d plans=%d executions=%d", executions, len(store.plans), len(store.executions))
	}
}
