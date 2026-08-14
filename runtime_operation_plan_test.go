package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type rejectingPlanSealStore struct {
	recordingStore
	sealErr   error
	appendErr error
	drift     bool
}

func (s *rejectingPlanSealStore) SealPlan(_ context.Context, seal OperationPlanSeal) (PlanSealResult, error) {
	if s.sealErr != nil {
		return PlanSealResult{}, s.sealErr
	}
	if s.drift {
		seal.BatchCount++
		return PlanSealResult{Seal: seal}, nil
	}
	panic("unexpected successful plan seal")
}

func (s *rejectingPlanSealStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if s.appendErr != nil && item.Type == ItemTypeOperationPlan && item.Error != "" {
		return s.appendErr
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func TestRuntimeReplaysCompletedWriteOperation(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "first done"),
		callResponse("resp-3", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-4", "second done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":1}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply", IdempotencyKey: "stable-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	if len(store.executions) != 1 {
		t.Fatalf("journal executions=%+v", store.executions)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionCompleted || string(execution.Result.Receipt) != `{"version":1}` {
			t.Fatalf("execution=%+v", execution)
		}
	}
}

func TestRuntimeDistinguishesRepeatedWritesAndReplaysEachPlannedStep(t *testing.T) {
	firstCalls := []ToolCall{
		{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
		{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
	}
	secondCalls := []ToolCall{
		{ID: "call-3", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
		{ID: "call-4", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
	}
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", firstCalls...),
		messageResponse("resp-2", "first done"),
		callResponse("resp-3", secondCalls...),
		messageResponse("resp-4", "second done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	var executionIDs, attemptIDs []string
	executor := OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		executionIDs = append(executionIDs, request.ExecutionID)
		attemptIDs = append(attemptIDs, request.AttemptID)
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply twice", IdempotencyKey: "stable-repeated-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(executionIDs) != 2 || executionIDs[0] == executionIDs[1] {
		t.Fatalf("executor execution IDs=%v, want two distinct executions", executionIDs)
	}
	if len(attemptIDs) != 2 || attemptIDs[0] == "" || attemptIDs[1] == "" || attemptIDs[0] == attemptIDs[1] {
		t.Fatalf("executor attempt IDs=%v, want distinct non-empty attempts", attemptIDs)
	}
	if len(store.executions) != 2 {
		t.Fatalf("journal executions=%d, want 2", len(store.executions))
	}
}

func TestRuntimeRejectsChangedWritePlanBeforeRetryExecution(t *testing.T) {
	sentinel := errors.New("append operation result failed")
	store := &appendFailingStore{failType: ItemTypeOperationResult, err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1",
			ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
			ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)},
		),
		callResponse("resp-2", ToolCall{ID: "call-3", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply twice", IdempotencyKey: "changed-plan-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("first Run error=%v, want sentinel", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrOperationPlanChanged) {
		t.Fatalf("second Run error=%v, want ErrOperationPlanChanged", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	requestID := operationRequestID(input)
	if got := len(store.plans[requestID][0].Steps); got != 2 {
		t.Fatalf("recorded plan steps=%d, want 2", got)
	}
}

func TestRuntimeSealedPlanRejectsAdditionalWriteBatch(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)}),
		messageResponse("resp-2", "first done"),
		callResponse("resp-3", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{"value":1}`)}),
		callResponse("resp-4", ToolCall{ID: "call-3", Name: "apply_change", Input: json.RawMessage(`{"value":2}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store, func(event Event) { events = append(events, event) })
	input := Input{User: "apply then maybe extend", IdempotencyKey: "sealed-plan-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrOperationPlanChanged) {
		t.Fatalf("second Run error=%v, want ErrOperationPlanChanged", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	requestID := operationRequestID(input)
	if seal, ok := store.seals[requestID]; !ok || seal.BatchCount != 1 || len(store.plans[requestID]) != 1 {
		t.Fatalf("seal=%+v plans=%+v", seal, store.plans[requestID])
	}
	var rejectedItem *ItemRecord
	var failedItem *ItemRecord
	for i := range store.items {
		item := &store.items[i]
		if item.Type == ItemTypeOperationPlan && item.Error != "" {
			rejectedItem = item
		}
		if item.Type == ItemTypeError && item.CallID == "call-3" {
			failedItem = item
		}
	}
	if rejectedItem == nil || rejectedItem.CallID != "call-3" || rejectedItem.ExecutionID == "" || failedItem == nil || failedItem.ExecutionID != rejectedItem.ExecutionID {
		t.Fatalf("rejected_item=%+v failed_item=%+v", rejectedItem, failedItem)
	}
	rejectedEvents := 0
	for _, event := range events {
		if event.Type == EventOperationPlanRejected && event.CallID == "call-3" && event.ExecutionID == rejectedItem.ExecutionID {
			rejectedEvents++
		}
	}
	if rejectedEvents != 1 {
		t.Fatalf("correlated plan rejected events=%d, want 1", rejectedEvents)
	}
}

func TestRuntimeRejectsWritePlanWhenSealFailsOrDrifts(t *testing.T) {
	sealFailure := errors.New("seal storage unavailable")
	appendFailure := errors.New("rejected plan audit unavailable")
	tests := []struct {
		name             string
		sealErr          error
		appendErr        error
		drift            bool
		wantErr          error
		wantSecondaryErr error
		wantMessage      string
		wantCode         string
		wantRejectedItem bool
	}{
		{
			name: "store failure", sealErr: sealFailure, wantErr: sealFailure,
			wantMessage: "seal operation plan", wantCode: "internal_error", wantRejectedItem: true,
		},
		{
			name: "store returns different seal", drift: true, wantErr: ErrOperationPlanChanged,
			wantMessage: "operation plan changed", wantCode: "operation_plan_changed", wantRejectedItem: true,
		},
		{
			name: "rejection audit append failure", sealErr: sealFailure, appendErr: appendFailure,
			wantErr: sealFailure, wantSecondaryErr: appendFailure,
			wantMessage: "seal operation plan", wantCode: "internal_error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedModel{responses: []*ModelResponse{
				callResponse("response-1", ToolCall{
					ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`),
				}),
				messageResponse("response-2", "done"),
			}}
			operations := NewOperationRegistry()
			if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
				t.Fatalf("Register: %v", err)
			}
			store := &rejectingPlanSealStore{
				sealErr: test.sealErr, appendErr: test.appendErr, drift: test.drift,
			}
			executions := 0
			var events []Event
			runtime := newTestRuntimeWithEventSink(
				t, model, operations, allowPolicy(),
				OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
					executions++
					return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
				}),
				confirmingVerifier(), nil, store,
				func(event Event) { events = append(events, event) },
			)
			input := Input{
				User: "apply", IdempotencyKey: "seal-rejection-request", IdempotencyScope: "test",
			}

			_, err := runtime.Run(t.Context(), input)
			if !errors.Is(err, test.wantErr) || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Run error=%v, want errors.Is(%v) and message %q", err, test.wantErr, test.wantMessage)
			}
			if test.wantSecondaryErr != nil && !errors.Is(err, test.wantSecondaryErr) {
				t.Fatalf("Run error=%v, want secondary errors.Is(%v)", err, test.wantSecondaryErr)
			}
			if executions != 1 {
				t.Fatalf("executor calls=%d, want 1", executions)
			}
			if len(store.executions) != 1 {
				t.Fatalf("executions=%+v, want one durable execution", store.executions)
			}
			var execution OperationExecutionRecord
			for _, candidate := range store.executions {
				execution = candidate
			}
			if execution.Status != OperationExecutionCompleted {
				t.Fatalf("execution status=%q, want completed", execution.Status)
			}

			requestID := operationRequestID(input)
			if len(store.plans[requestID]) != 1 {
				t.Fatalf("plans=%+v, want one reserved plan", store.plans)
			}
			var rejected *ItemRecord
			for index := range store.items {
				item := &store.items[index]
				if item.Type == ItemTypeOperationPlan && item.Error != "" {
					rejected = item
				}
			}
			if test.wantRejectedItem {
				if rejected == nil || rejected.RequestID != requestID || rejected.PlanBatch != 1 ||
					rejected.CallID != "call-1" || rejected.ExecutionID != execution.ID {
					t.Fatalf("rejected plan item=%+v, execution=%+v", rejected, execution)
				}
			} else if rejected != nil {
				t.Fatalf("rejected plan item unexpectedly persisted: %+v", rejected)
			}
			rejectedEvents := 0
			for _, event := range events {
				if event.Type == EventOperationPlanRejected {
					rejectedEvents++
					if event.RequestID != requestID || event.PlanBatch != 1 ||
						event.CallID != "call-1" || event.ExecutionID != execution.ID ||
						event.ErrorCode != test.wantCode {
						t.Fatalf("plan rejected event=%+v", event)
					}
				}
			}
			if rejectedEvents != 1 {
				t.Fatalf("plan rejected events=%d, want 1", rejectedEvents)
			}
		})
	}
}

func TestRuntimeSealsZeroWritePlan(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("resp-1", "nothing to change"),
		callResponse("resp-2", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
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
	input := Input{User: "inspect request", IdempotencyKey: "sealed-zero-plan", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrOperationPlanChanged) {
		t.Fatalf("second Run error=%v, want ErrOperationPlanChanged", err)
	}
	if executions != 0 {
		t.Fatalf("executor calls=%d, want 0", executions)
	}
	requestID := operationRequestID(input)
	if seal, ok := store.seals[requestID]; !ok || seal.BatchCount != 0 {
		t.Fatalf("zero-write seal=%+v exists=%v", seal, ok)
	}
}

func TestRuntimeBlocksRetryWhenWriteOutcomeIsUnknown(t *testing.T) {
	sentinel := errors.New("executor connection lost")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
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
	input := Input{User: "apply", IdempotencyKey: "stable-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("first Run error=%v, want sentinel", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("second Run error=%v, want ErrOperationOutcomeUnknown", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	if len(store.failed) != 0 {
		t.Fatalf("unknown write was terminally failed: %+v", store.failed)
	}
	for _, run := range store.runs {
		if run.Status != RunStatusInterrupted {
			t.Fatalf("run status=%q, want interrupted for automatic recovery", run.Status)
		}
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionUnknown {
			t.Fatalf("execution=%+v", execution)
		}
	}
}

func TestRuntimeBlocksRetryWhenWriteReturnsInvalidInternalArtifactJSON(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{
			Output: json.RawMessage(`{"applied":true}`),
			Artifacts: []ResultArtifact{{
				Type: "change_set", Data: json.RawMessage(`{"id":"change-1"}`), InternalData: json.RawMessage(`{"storage_key"`),
			}},
		}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply", IdempotencyKey: "invalid-internal-artifact", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); err == nil || !strings.Contains(err.Error(), "internal data must be valid JSON") {
		t.Fatalf("first Run error=%v, want invalid internal artifact error", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("second Run error=%v, want ErrOperationOutcomeUnknown", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionUnknown {
			t.Fatalf("execution=%+v, want unknown", execution)
		}
	}
}

func TestRuntimeDoesNotTreatDefinitelyUnappliedWriteAsUnknown(t *testing.T) {
	sentinel := errors.New("stale approved input")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{}, MarkOperationNotApplied(sentinel)
	}), confirmingVerifier(), nil, store)
	_, err := rt.Run(t.Context(), Input{User: "apply", IdempotencyKey: "stable-request", IdempotencyScope: "test"})
	if !errors.Is(err, sentinel) || !errors.Is(err, ErrOperationNotApplied) || errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("Run error=%v", err)
	}
	if len(store.failed) != 1 || store.failed[0].Status != RunStatusFailed {
		t.Fatalf("failed runs=%+v", store.failed)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionRetryable {
			t.Fatalf("execution=%+v", execution)
		}
	}
}

func TestRuntimeReadOperationFailureDoesNotCreateUnknownExecutionTransition(t *testing.T) {
	sentinel := errors.New("read operation rejected invalid domain input")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "read_context", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{}, sentinel
	}), nil, nil, store)
	_, err := rt.Run(context.Background(), Input{User: "inspect"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}
	if strings.Contains(err.Error(), "invalid operation execution transition") {
		t.Fatalf("read failure was polluted by an execution transition error: %v", err)
	}
	if len(store.executions) != 0 || len(store.transitions) != 0 {
		t.Fatalf("read failure created execution journal state: executions=%+v transitions=%+v", store.executions, store.transitions)
	}
	if len(store.failed) != 1 || store.failed[0].ErrorCode != "internal_error" {
		t.Fatalf("failed runs=%+v", store.failed)
	}
}
