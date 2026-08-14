package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRuntimeDoesNotRepeatWriteWhenResultAuditFails(t *testing.T) {
	sentinel := errors.New("append operation result failed")
	store := &appendFailingStore{failType: ItemTypeOperationResult, err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`), Receipt: json.RawMessage(`{"version":1}`)}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	input := Input{User: "apply", IdempotencyKey: "stable-request", IdempotencyScope: "test"}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("first Run error=%v, want sentinel", err)
	}
	if _, err := rt.Run(context.Background(), input); !errors.Is(err, sentinel) {
		t.Fatalf("second Run error=%v, want sentinel", err)
	}
	if executions != 1 {
		t.Fatalf("executor calls=%d, want 1", executions)
	}
	for _, execution := range store.executions {
		if execution.Status != OperationExecutionCompleted {
			t.Fatalf("execution=%+v", execution)
		}
	}
}

func TestRuntimeDoesNotPublishOperationCompletedWhenResultAuditFails(t *testing.T) {
	sentinel := errors.New("append operation result failed")
	store := &appendFailingStore{failType: ItemTypeOperationResult, err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, store, func(event Event) { events = append(events, event) })
	if _, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "audit-failure-event-order", IdempotencyScope: "test",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}
	for _, event := range events {
		if event.Type == EventOperationCompleted {
			t.Fatalf("events=%#v, result audit failure must not publish operation completed", events)
		}
	}
}

func TestRuntimeOperationLifecycleOnPolicyDenial(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-denied", Name: "read_context", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	var events []Event
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: ops, RunStore: store,
		Policy: OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
			return PolicyDecision{Action: PolicyDeny, Reason: "blocked"}, nil
		}),
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("executor must not run")
			return OperationResult{}, nil
		}),
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "read"})
	if !errors.Is(err, ErrOperationDenied) {
		t.Fatalf("Run error=%v, want ErrOperationDenied", err)
	}
	requested, started, completed, failed := 0, 0, 0, 0
	var runFailed Event
	for _, event := range events {
		switch event.Type {
		case EventOperationRequested:
			requested++
		case EventOperationStarted:
			started++
		case EventOperationCompleted:
			completed++
		case EventOperationFailed:
			failed++
		case EventRunFailed:
			runFailed = event
		}
	}
	if requested != 1 || started != 0 || completed != 0 || failed != 1 || runFailed.CallID != "call-denied" {
		t.Fatalf("operation lifecycle requested=%d started=%d completed=%d failed=%d run_failed=%+v", requested, started, completed, failed, runFailed)
	}
}

func TestRuntimeFailsRunWhenCompletionPersistenceFails(t *testing.T) {
	sentinel := errors.New("complete run failed")
	store := &completeFailingStore{err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}}
	var events []Event
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "hello", SessionID: "thread-finalize-failure"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}
	completed, failed := 0, 0
	for _, event := range events {
		switch event.Type {
		case EventRunCompleted:
			completed++
		case EventRunFailed:
			failed++
		}
	}
	if completed != 0 || failed != 1 || len(store.failed) != 1 || store.failed[0].Status != RunStatusFailed {
		t.Fatalf("run lifecycle completed=%d failed=%d stored_failed=%+v", completed, failed, store.failed)
	}
	session := store.sessions["thread-finalize-failure"]
	if session.Revision != 1 || session.LastResponseID != "" || len(session.Transcript) != 0 || session.LastError == "" {
		t.Fatalf("session advanced after completion persistence failure: %+v", session)
	}
}

func TestRuntimeForwardsModelStreamEventsWithRunIdentity(t *testing.T) {
	var events []Event
	rt, err := NewRuntime(RuntimeConfig{
		Model: streamCallbackModel{},
		EventSink: func(event Event) {
			events = append(events, event)
		},
		NewID: func() string { return "run-stream" },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	result, err := rt.Run(context.Background(), Input{User: "hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "hello" {
		t.Fatalf("output=%q, want hello", result.Output)
	}

	var reasoning, commentary, deltas []string
	for _, event := range events {
		if event.Type != EventModelStreamChunk {
			continue
		}
		if event.Chunk == nil {
			t.Fatal("stream event has nil chunk")
		}
		switch event.Chunk.Type {
		case ModelStreamReasoningSummaryDelta:
			reasoning = append(reasoning, event.Chunk.Delta)
			if event.Chunk.SequenceNumber == nil || *event.Chunk.SequenceNumber != 4 || event.Chunk.ItemID != "rs-1" || event.Chunk.ProviderType != "response.reasoning_summary_text.delta" || event.Chunk.RawJSON != `{"type":"reasoning"}` {
				t.Fatalf("reasoning chunk=%+v", event.Chunk)
			}
		case ModelStreamCommentaryDelta:
			commentary = append(commentary, event.Chunk.Delta)
		case ModelStreamTextDelta:
			deltas = append(deltas, event.Chunk.Delta)
		}
		if event.RunID != "run-stream" {
			t.Fatalf("stream event run_id=%q, want run-stream", event.RunID)
		}
	}
	if got := strings.Join(reasoning, ""); got != "checking" {
		t.Fatalf("reasoning=%q, want checking", got)
	}
	if got := strings.Join(commentary, ""); got != "using tool" {
		t.Fatalf("commentary=%q, want using tool", got)
	}
	if got := strings.Join(deltas, ""); got != "hello" {
		t.Fatalf("deltas=%q, want hello", got)
	}
}

func TestRuntimeDisambiguatesChunksAcrossModelCalls(t *testing.T) {
	model := &multiTurnChunkModel{}
	store := &recordingStore{}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	var events []Event
	nextID := 0
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: ops, RunStore: store,
		Policy: allowPolicy(),
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{Output: json.RawMessage(`{"context":"loaded"}`)}, nil
		}),
		EventSink: func(event Event) { events = append(events, event) },
		NewID: func() string {
			nextID++
			return fmt.Sprintf("id-%d", nextID)
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if _, err := rt.Run(context.Background(), Input{User: "read"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	modelCalls := make(map[string]string)
	operationCallID := ""
	for _, event := range events {
		switch event.Type {
		case EventModelStreamChunk:
			if event.Chunk == nil || event.ModelCallID == "" || event.Chunk.ModelCallID != event.ModelCallID || event.ResponseID != event.Chunk.ResponseID {
				t.Fatalf("uncorrelated stream event=%+v", event)
			}
			if event.Chunk.SequenceNumber == nil || *event.Chunk.SequenceNumber != 0 {
				t.Fatalf("chunk sequence=%+v", event.Chunk.SequenceNumber)
			}
			modelCalls[event.ModelCallID] = event.ResponseID
		case EventOperationStarted:
			operationCallID = event.CallID
		}
	}
	if len(modelCalls) != 2 || operationCallID != "call-1" {
		t.Fatalf("model calls=%+v operation call_id=%q", modelCalls, operationCallID)
	}
	storedResponses := make(map[string]string)
	for _, item := range store.items {
		if item.Type == ItemTypeModelRequest && (item.ModelCallID == "" || item.ID != item.ModelCallID) {
			t.Fatalf("uncorrelated model request item=%+v", item)
		}
		if item.Type == ItemTypeModelResponse {
			storedResponses[item.ModelCallID] = item.ResponseID
		}
	}
	if len(storedResponses) != 2 {
		t.Fatalf("stored model responses=%+v", storedResponses)
	}
	for modelCallID, responseID := range modelCalls {
		if storedResponses[modelCallID] != responseID {
			t.Fatalf("stored response for %s=%q, want %q", modelCallID, storedResponses[modelCallID], responseID)
		}
	}
}

func callResponse(id string, calls ...ToolCall) *ModelResponse {
	items := make([]ModelOutputItem, 0, len(calls))
	for i := range calls {
		call := calls[i]
		itemID := fmt.Sprintf("%s-call-%d", id, i)
		items = append(items, ModelOutputItem{
			ID: itemID, Type: ModelOutputFunctionCall, Call: &call,
			Raw: mustJSON(map[string]any{
				"id": itemID, "type": "function_call", "status": "completed",
				"call_id": call.ID, "name": call.Name, "arguments": string(call.Input),
			}),
		})
	}
	return &ModelResponse{ID: id, Items: items}
}

func operation(name string, effect OperationEffect) Operation {
	confirmation := ConfirmationSpec{Mode: ConfirmationNone}
	var preview func(any) (json.RawMessage, error)
	if effect == OperationEffectWrite {
		confirmation = ConfirmationSpec{Mode: ConfirmationRequired, Description: "Confirm the requested state change is observable."}
		preview = func(any) (json.RawMessage, error) { return json.RawMessage(`{"change":"test"}`), nil }
	}
	return Operation{
		Name: name, Description: name, Effect: effect, Confirmation: confirmation,
		ApprovalPreview: preview,
		InputSchema:     json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func confirmingVerifier() ResultVerifier {
	return ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		return VerificationResult{Confirmed: true, Message: "confirmed", Evidence: json.RawMessage(`{"confirmed":true}`)}, nil
	})
}

func TestDurableOperationComparisonsUseJSONSemantics(t *testing.T) {
	leftBatch := OperationPlanBatch{
		RequestID: "request-1", SessionID: "session-1", IdempotencyKey: "key-1", Index: 0,
		Steps: []OperationPlanStep{{ExecutionID: "execution-1", Name: "write", Arguments: json.RawMessage(`{"a":1,"b":[true]}`)}},
	}
	rightBatch := leftBatch
	rightBatch.Steps = []OperationPlanStep{{ExecutionID: "execution-1", Name: "write", Arguments: json.RawMessage(`{ "b": [true], "a": 1.0 }`)}}
	if !equalOperationPlanBatch(leftBatch, rightBatch) {
		t.Fatal("semantically equal JSONB plan arguments must replay")
	}
	if !equalOperationResult(
		OperationResult{Output: json.RawMessage(`{"ok":true}`), Receipt: json.RawMessage(`{"version":1}`)},
		OperationResult{Output: json.RawMessage(`{ "ok": true }`), Receipt: json.RawMessage(`{"version":1.0}`)},
	) {
		t.Fatal("semantically equal JSONB operation results must replay")
	}
}

func TestOperationRegistryRemainsImmutableAfterFreeze(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)
	outputSchema := json.RawMessage(`{"type":"object"}`)
	capabilities := []string{"write_state"}
	registry := NewOperationRegistry()
	if err := registry.Register(Operation{
		Name: "immutable", InputSchema: inputSchema, OutputSchema: outputSchema,
		Effect: OperationEffectRead, Capabilities: capabilities,
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	inputSchema[0] = '['
	outputSchema[0] = '['
	capabilities[0] = "mutated"
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	first, ok := registry.Get("immutable")
	if !ok || !json.Valid(first.InputSchema) || !json.Valid(first.OutputSchema) || first.Capabilities[0] != "write_state" {
		t.Fatalf("registered operation was externally mutated: %+v", first)
	}
	first.InputSchema[0] = '['
	first.OutputSchema[0] = '['
	first.Capabilities[0] = "mutated-through-get"
	second, ok := registry.Get("immutable")
	if !ok || !json.Valid(second.InputSchema) || !json.Valid(second.OutputSchema) || second.Capabilities[0] != "write_state" {
		t.Fatalf("Get exposed registry internals: %+v", second)
	}
	if err := registry.ValidateInput("immutable", json.RawMessage(`{"value":"ok"}`)); err != nil {
		t.Fatalf("compiled validator changed after external mutation: %v", err)
	}
}
