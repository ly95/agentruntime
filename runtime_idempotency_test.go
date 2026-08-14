package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRuntimeRejectsReusedCallIDAcrossModelTurns(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
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
	_, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "reused-call", IdempotencyScope: "test",
	})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "reused function call id") {
		t.Fatalf("Run error=%v", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
}

func TestRuntimeRejectsReusedCallIDFromCommittedSession(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "done"),
		callResponse("resp-3", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
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
	if _, err := rt.Run(context.Background(), Input{
		User: "first", SessionID: "session-call-id", IdempotencyKey: "request-1",
	}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	_, err := rt.Run(context.Background(), Input{
		User: "second", SessionID: "session-call-id", IdempotencyKey: "request-2",
	})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "reused function call id") {
		t.Fatalf("second Run error=%v", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
}

func TestRuntimeRequiresScopeForStatelessWrite(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
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
	_, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "unscoped"})
	if !errors.Is(err, ErrIdempotencyScopeRequired) {
		t.Fatalf("Run error=%v, want ErrIdempotencyScopeRequired", err)
	}
	if executions != 0 || len(store.plans) != 0 || len(store.executions) != 0 {
		t.Fatalf("side effects occurred: executor=%d plans=%d executions=%d", executions, len(store.plans), len(store.executions))
	}
}

func TestRuntimeIsolatesStatelessIdempotencyByScope(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "done"),
		callResponse("resp-3", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-4", "done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	var executionIDs []string
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(_ context.Context, req OperationRequest) (OperationResult, error) {
		executionIDs = append(executionIDs, req.ExecutionID)
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store)
	for _, scope := range []string{"tenant-a", "tenant-b"} {
		if _, err := rt.Run(context.Background(), Input{
			User: "apply", IdempotencyKey: "shared-key", IdempotencyScope: scope,
		}); err != nil {
			t.Fatalf("Run scope %q: %v", scope, err)
		}
	}
	if len(executionIDs) != 2 || executionIDs[0] == executionIDs[1] {
		t.Fatalf("execution IDs=%v, want two scope-isolated IDs", executionIDs)
	}
	if len(store.executions) != 2 {
		t.Fatalf("stored executions=%d, want 2", len(store.executions))
	}
}

func TestOperationIDsUseUnambiguousFieldEncoding(t *testing.T) {
	leftRequest := operationRequestID(Input{
		IdempotencyScope: "a", SessionID: "b\x00c", IdempotencyKey: "d",
	})
	rightRequest := operationRequestID(Input{
		IdempotencyScope: "a\x00b", SessionID: "c", IdempotencyKey: "d",
	})
	if leftRequest == rightRequest {
		t.Fatalf("distinct request fields produced the same ID: %s", leftRequest)
	}
	leftExecution := operationExecutionID("request", 0, 0, "a", json.RawMessage("b\x00c"))
	rightExecution := operationExecutionID("request", 0, 0, "a\x00b", json.RawMessage("c"))
	if leftExecution == rightExecution {
		t.Fatalf("distinct execution fields produced the same ID: %s", leftExecution)
	}
}

func TestOperationRequestIsDeepClonedAcrossPolicyApprovalAndExecutor(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{"value":"original"}`)}),
		messageResponse("resp-2", "done"),
	}}
	ops := NewOperationRegistry()
	op := operation("apply_change", OperationEffectWrite)
	op.Capabilities = []string{"write_state"}
	if err := ops.Register(op); err != nil {
		t.Fatal(err)
	}
	assertOriginal := func(boundary string, req OperationRequest) {
		t.Helper()
		args, ok := req.Arguments.(map[string]any)
		if !ok || args["value"] != "original" {
			t.Fatalf("%s arguments=%#v", boundary, req.Arguments)
		}
		if string(req.Call.Input) != `{"value":"original"}` || len(req.Operation.Capabilities) != 1 || req.Operation.Capabilities[0] != "write_state" {
			t.Fatalf("%s request mutated: call=%s operation=%+v", boundary, req.Call.Input, req.Operation)
		}
		nested, _ := req.Input.Metadata["nested"].(map[string]any)
		if nested["value"] != "original" {
			t.Fatalf("%s metadata=%#v", boundary, req.Input.Metadata)
		}
	}
	policy := OperationPolicyFunc(func(_ context.Context, req OperationRequest) (PolicyDecision, error) {
		assertOriginal("policy", req)
		req.Arguments.(map[string]any)["value"] = "policy-mutated"
		req.Call.Input[0] = '['
		req.Operation.Capabilities[0] = "policy-mutated"
		req.Input.Metadata["nested"].(map[string]any)["value"] = "policy-mutated"
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "test mutation isolation"}, nil
	})
	approver := ApproverFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
		assertOriginal("approver", request.Operation)
		request.Operation.Arguments.(map[string]any)["value"] = "approver-mutated"
		return ApprovalDecision{Approved: true}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, policy, OperationExecutorFunc(func(_ context.Context, req OperationRequest) (OperationResult, error) {
		assertOriginal("executor", req)
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), approver, store)
	if _, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "immutable-request", IdempotencyScope: "test",
		Metadata: map[string]any{"nested": map[string]any{"value": "original"}},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRuntimeProtectsToolResultFromAuditObservers(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &mutatingAppendStore{}
	eventSink := EventSink(func(event Event) {
		if event.Type == EventOperationCompleted && len(event.Data) > 0 {
			event.Data[0] = '['
		}
	})
	rt := newTestRuntimeWithEventSink(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store, eventSink)
	result, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "observer-boundary", IdempotencyScope: "test",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("output=%q, want done", result.Output)
	}
}

func TestRuntimeRejectsInvalidAuditItemData(t *testing.T) {
	rt := &Runtime{runStore: &recordingStore{}}
	for _, data := range []json.RawMessage{nil, json.RawMessage(`{`)} {
		err := rt.appendItem(context.Background(), ItemRecord{Type: ItemTypeOperationResult, Data: data})
		if err == nil || !strings.Contains(err.Error(), "data must be valid JSON") {
			t.Fatalf("data=%q err=%v", data, err)
		}
	}
}

func TestRuntimeProtectsInputFromRunStoreMutation(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &mutatingBeginStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		nested, _ := request.Input.Metadata["nested"].(map[string]any)
		if nested["value"] != "original" {
			t.Fatalf("executor metadata=%#v, want original", request.Input.Metadata)
		}
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store)
	if _, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "store-input-boundary", IdempotencyScope: "test",
		Metadata: map[string]any{"nested": map[string]any{"value": "original"}},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var userData struct {
		Metadata map[string]any `json:"metadata"`
	}
	for _, item := range store.items {
		if item.Type == ItemTypeUserMessage {
			if err := json.Unmarshal(item.Data, &userData); err != nil {
				t.Fatalf("unmarshal user audit: %v", err)
			}
			break
		}
	}
	nested, _ := userData.Metadata["nested"].(map[string]any)
	if nested["value"] != "original" {
		t.Fatalf("user audit metadata=%#v, want original", userData.Metadata)
	}
}

func TestRuntimeRejectsMismatchedRunHandleBeforeCallingModel(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "must not run")}}
	store := &mismatchedRunHandleStore{}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, store)
	_, err := rt.Run(context.Background(), Input{User: "hello", SessionID: "session-1"})
	if err == nil || !strings.Contains(err.Error(), "run store returned handle for run") {
		t.Fatalf("Run error=%v, want mismatched run handle", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests=%d, want none", len(model.requests))
	}
}

func TestRuntimeReplaysExecutedWriteForVerificationWithoutRepeatingSideEffect(t *testing.T) {
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
	verifications := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":1}`)}, nil
	}), ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		verifications++
		return VerificationResult{Confirmed: verifications > 1, Message: "not observable yet"}, nil
	}), nil, store)
	input := Input{User: "apply", SessionID: "session-verification", IdempotencyKey: "pending-verification"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("first Run error=%v, want ErrVerificationFailed", err)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionExecuted {
			t.Fatalf("execution after failed verification=%+v", execution)
		}
	}
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if executions != 1 || verifications != 2 {
		t.Fatalf("executor calls=%d verification calls=%d", executions, verifications)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionCompleted || execution.Verification == nil || !execution.Verification.Confirmed {
			t.Fatalf("execution after confirmed replay=%+v", execution)
		}
	}
}
