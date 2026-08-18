package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeRejectsOperationOutputThatViolatesSchema(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "read_context", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "read_context", Description: "read", Effect: OperationEffectRead,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"context":{"type":"string"}},"required":["context"]}`),
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatal(err)
	}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"context":42}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, &recordingStore{})
	_, err := rt.Run(context.Background(), Input{User: "read"})
	if err == nil || !strings.Contains(err.Error(), "output does not match schema") {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeConfirmationNoneWriteCompletesWithoutApprovalOrVerification(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "queue_work", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "queue_work", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "queue_work", ContractVersion: "test-v1", Description: "queue", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{
			"type":"object","properties":{"queued":{"type":"boolean"}},"required":["queued"]
		}`),
		Confirmation:           ConfirmationSpec{Mode: ConfirmationNone},
		Terminal:               true,
		ProjectTerminalSession: func(any) ([]TerminalSessionProjection, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	var events []Event
	executions := 0
	rt := newTestRuntimeWithEventSink(
		t, model, ops, allowPolicy(),
		OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			executions++
			return OperationResult{
				Output: json.RawMessage(`{"queued":true}`), FinalResponse: "queued",
			}, nil
		}),
		nil, nil, store, func(event Event) { events = append(events, event) },
	)
	input := Input{User: "queue", IdempotencyKey: "direct-write", IdempotencyScope: "test"}
	for runIndex := 0; runIndex < 2; runIndex++ {
		result, err := rt.Run(context.Background(), input)
		if err != nil {
			t.Fatalf("Run %d: %v", runIndex+1, err)
		}
		if result.Output != "queued" {
			t.Fatalf("Run %d result=%+v", runIndex+1, result)
		}
	}
	if executions != 1 {
		t.Fatalf("direct write executor calls=%d, want 1", executions)
	}
	for _, event := range events {
		if event.Type == EventApprovalRequested || event.Type == EventApprovalCompleted ||
			event.Type == EventApprovalFailed || event.Type == EventVerificationStarted ||
			event.Type == EventVerificationCompleted || event.Type == EventVerificationFailed {
			t.Fatalf("direct write emitted confirmation lifecycle event: %+v", event)
		}
	}
	operationResults := 0
	for _, item := range store.items {
		if item.Type == ItemTypeVerification {
			t.Fatalf("direct write persisted verification item: %+v", item)
		}
		if item.Type == ItemTypeOperationResult {
			operationResults++
			var result struct {
				Confirmation ConfirmationSpec `json:"confirmation"`
			}
			if err := json.Unmarshal(item.Data, &result); err != nil {
				t.Fatalf("decode direct write result: %v", err)
			}
			if result.Confirmation.Mode != ConfirmationNone {
				t.Fatalf("direct write result confirmation=%+v", result.Confirmation)
			}
		}
	}
	if operationResults != 2 {
		t.Fatalf("direct write operation results=%d", operationResults)
	}
	if len(store.executions) != 1 {
		t.Fatalf("direct write executions=%d", len(store.executions))
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionCompleted || execution.Verification != nil {
			t.Fatalf("direct write execution=%+v", execution)
		}
	}
}

func TestRuntimeBlocksFinalWhenVerificationFails(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "must not be reached"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	verifier := ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		return VerificationResult{Confirmed: false, Message: "state did not change", Evidence: json.RawMessage(`{"changed":false}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, verifier, nil, store)
	_, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "verification-failure", IdempotencyScope: "test"})
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("err=%v, want ErrVerificationFailed", err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests=%d, final turn must be blocked", len(model.requests))
	}
	var failedVerification bool
	for _, item := range store.items {
		if item.Type == ItemTypeVerification && item.Error == "state did not change" {
			failedVerification = true
		}
	}
	if !failedVerification {
		t.Fatalf("verification failure was not audited: %+v", store.items)
	}
}

func TestRuntimeInvalidVerificationEvidenceEmitsFailedTerminalEvent(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	verifier := ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		return VerificationResult{Confirmed: true, Evidence: json.RawMessage(`{"invalid"`)}, nil
	})
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), verifier, nil, &recordingStore{}, func(event Event) {
		events = append(events, event)
	})
	if _, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "invalid-evidence", IdempotencyScope: "test"}); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("Run error=%v, want ErrVerificationFailed", err)
	}
	var started, failed Event
	completed := 0
	for _, event := range events {
		switch event.Type {
		case EventVerificationStarted:
			started = event
		case EventVerificationCompleted:
			completed++
		case EventVerificationFailed:
			failed = event
		}
	}
	if completed != 0 || started.ExecutionID == "" || failed.ExecutionID != started.ExecutionID || failed.CallID != started.CallID {
		t.Fatalf("verification lifecycle started=%+v failed=%+v completed=%d", started, failed, completed)
	}
}

func TestRuntimeReturnsConfirmedEvidenceToModel(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "confirmed done"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":2}`)}, nil
	})
	verifier := ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		return VerificationResult{Confirmed: true, Message: "version advanced", Evidence: json.RawMessage(`{"before":1,"after":2}`)}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, verifier, nil, store)
	res, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "confirmed-evidence", IdempotencyScope: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "confirmed done" || len(model.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", res, len(model.requests))
	}
	var toolOutput string
	for _, item := range model.requests[1].Input {
		if item.Type == ModelInputToolResult {
			toolOutput = string(item.Output)
		}
	}
	if !strings.Contains(toolOutput, `"confirmed":true`) || !strings.Contains(toolOutput, `"after":2`) {
		t.Fatalf("confirmed evidence missing from tool output: %s", toolOutput)
	}
	verificationItems := 0
	for _, item := range store.items {
		if item.Type == ItemTypeVerification {
			verificationItems++
			if item.ExecutionID == "" {
				t.Fatalf("verification item is missing execution_id: %+v", item)
			}
		}
	}
	if verificationItems != 1 {
		t.Fatalf("verification items=%d, want 1", verificationItems)
	}
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func int64Pointer(value int64) *int64 {
	return new(value)
}

func BenchmarkRuntimeModelRequestCached(b *testing.B) {
	rt, err := NewRuntime(RuntimeConfig{Model: &scriptedModel{}})
	if err != nil {
		b.Fatal(err)
	}
	state, err := rt.stateFromSession("", "", RunHandle{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	pending := []ModelInputItem{{Type: ModelInputUserMessage, Text: "benchmark"}}
	var request ModelRequest
	b.ReportAllocs()
	for b.Loop() {
		request, _ = rt.modelRequest(state, pending)
	}
	runtime.KeepAlive(request)
}

func BenchmarkOperationDecodeAndValidateInput(b *testing.B) {
	reg := NewOperationRegistry()
	if err := reg.Register(Operation{
		Name: "read_context", Description: "read", Effect: OperationEffectRead,
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"limit":{"type":"integer"}},"required":["id","limit"]}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`), Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		b.Fatal(err)
	}
	if err := reg.Freeze(); err != nil {
		b.Fatal(err)
	}
	input := json.RawMessage(`{"id":"doc1","limit":20}`)
	var decoded any
	b.ReportAllocs()
	for b.Loop() {
		var err error
		decoded, err = reg.DecodeInput("read_context", input)
		if err != nil {
			b.Fatal(err)
		}
	}
	runtime.KeepAlive(decoded)
}
