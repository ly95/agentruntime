package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResultArtifactSessionSummaryRequiresBoundedJSONObject(t *testing.T) {
	tests := []struct {
		name    string
		summary json.RawMessage
		want    string
	}{
		{name: "array", summary: json.RawMessage(`[]`), want: "JSON object"},
		{name: "null", summary: json.RawMessage(`null`), want: "JSON object"},
		{name: "oversized", summary: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxResultArtifactSessionSummaryBytes) + `"}`), want: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResultArtifacts([]ResultArtifact{{Type: "change_set", Data: json.RawMessage(`{}`), SessionSummary: test.summary}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateResultArtifacts error=%v, want %q", err, test.want)
			}
		})
	}
	t.Run("history record envelope", func(t *testing.T) {
		summary := json.RawMessage(`{"value":"` + strings.Repeat("x", MaxResultArtifactSessionSummaryBytes-100) + `"}`)
		if len(summary) > MaxResultArtifactSessionSummaryBytes {
			t.Fatalf("test summary size=%d", len(summary))
		}
		_, err := terminalSessionHistoryItem("call-1", []terminalSessionArtifactProjection{{
			Type: "change_set", SessionSummary: summary,
		}})
		if err == nil || !strings.Contains(err.Error(), "history exceeds") {
			t.Fatalf("terminalSessionHistoryItem error=%v", err)
		}
	})
}

func TestValidateProjectedFunctionCallSupportsRuntimeModelEnvelopes(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "native", raw: json.RawMessage(`{"type":"function_call","call_id":"call-1"}`)},
		{name: "host adapter", raw: json.RawMessage(`{"type":"function_call","call":{"id":"call-1","name":"finish","arguments":{}}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateProjectedFunctionCall(ModelInputItem{
				Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call-1", Raw: test.raw,
			}, "call-1"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeApprovalErrorEmitsFailedTerminalEvent(t *testing.T) {
	sentinel := errors.New("approval service unavailable")
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approver := ApproverFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{}, sentinel
	})
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, policy, OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		t.Fatal("executor must not run when approval fails")
		return OperationResult{}, nil
	}), confirmingVerifier(), approver, &recordingStore{}, func(event Event) {
		events = append(events, event)
	})
	if _, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "approval-error", IdempotencyScope: "test"}); !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}
	var requested, failed Event
	completed := 0
	for _, event := range events {
		switch event.Type {
		case EventApprovalRequested:
			requested = event
		case EventApprovalCompleted:
			completed++
		case EventApprovalFailed:
			failed = event
		}
	}
	if completed != 0 || requested.ExecutionID == "" || failed.ExecutionID != requested.ExecutionID || failed.CallID != requested.CallID {
		t.Fatalf("approval lifecycle requested=%+v failed=%+v completed=%d", requested, failed, completed)
	}
}

func TestRuntimeRejectsUnknownOperation(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "missing", Input: json.RawMessage(`{}`)}),
	}}
	rt := newTestRuntime(t, model, NewOperationRegistry(), nil, nil, nil, nil, &recordingStore{})
	_, err := rt.Run(context.Background(), Input{User: "do it"})
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestFailedRunDoesNotPersistUnresolvedResponse(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("stable-response", "first"),
		callResponse("failed-response", ToolCall{ID: "call-1", Name: "missing", Input: json.RawMessage(`{}`)}),
	}}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, NewOperationRegistry(), nil, nil, nil, nil, store)
	if _, err := rt.Run(context.Background(), Input{User: "first", SessionID: "thread-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), Input{User: "fail", SessionID: "thread-1"}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("err=%v", err)
	}
	if got := store.sessions["thread-1"].LastResponseID; got != "stable-response" {
		t.Fatalf("last response=%q, want stable-response", got)
	}
	if got := len(store.sessions["thread-1"].Transcript); got != 2 {
		t.Fatalf("failed run changed stable transcript length to %d", got)
	}
}

func TestOperationRegistryUsesCapabilitiesInsteadOfNameBlacklist(t *testing.T) {
	reg := NewOperationRegistry()
	err := reg.Register(Operation{
		Name: "execute_sql", Description: "Run a host-scoped query",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Effect: OperationEffectRead, Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
		Capabilities: []string{"database.read"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !reg.Provides("database.read") || !reg.Provides("execute_sql") {
		t.Fatalf("registered capabilities were not indexed")
	}
}

func TestOperationRegistryFreezeCachesMetadataAndRejectsMutation(t *testing.T) {
	reg := NewOperationRegistry()
	if err := reg.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Freeze(); err != nil {
		t.Fatal(err)
	}
	first := reg.Summaries()
	first[0].Name = "mutated"
	if got := reg.Summaries()[0].Name; got != "read_context" {
		t.Fatalf("cached summary was mutated: %q", got)
	}
	if err := reg.Register(operation("late_operation", OperationEffectRead)); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("late Register err=%v", err)
	}
}

func TestRuntimeRejectsOperationInputThatViolatesSchema(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "read_context", Input: json.RawMessage(`{"id":42}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "read_context", Description: "read", Effect: OperationEffectRead,
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`), Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatal(err)
	}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		t.Fatal("executor must not receive schema-invalid input")
		return OperationResult{}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, &recordingStore{})
	_, err := rt.Run(context.Background(), Input{User: "read"})
	if err == nil || !strings.Contains(err.Error(), "does not match schema") {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeRejectsNormalizedOperationInputThatViolatesSchema(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "read_context", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "read_context", Description: "read", Effect: OperationEffectRead,
		InputSchema: json.RawMessage(`{
            "type":"object",
            "properties":{"id":{"type":"string"}},
            "additionalProperties":false
        }`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		NormalizeInput: func(any) (any, error) {
			return map[string]any{"id": 42}, nil
		},
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatal(err)
	}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		t.Fatal("executor must not receive schema-invalid normalized input")
		return OperationResult{}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, &recordingStore{})
	_, err := rt.Run(context.Background(), Input{User: "read"})
	if err == nil || !strings.Contains(err.Error(), "normalized operation") || !strings.Contains(err.Error(), "does not match schema") {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeCanonicalizesOperationInputBeforePlanIdentity(t *testing.T) {
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "apply_default", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{
            "type":"object",
            "properties":{"model_key":{"type":["string","null"]}},
            "required":[],
            "additionalProperties":false
        }`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		NormalizeInput: func(arguments any) (any, error) {
			input, ok := arguments.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("unexpected arguments %T", arguments)
			}
			modelKey, _ := input["model_key"].(string)
			if strings.TrimSpace(modelKey) == "" {
				modelKey = "ltx"
			}
			return map[string]any{"model_key": modelKey}, nil
		},
		Confirmation: ConfirmationSpec{Mode: ConfirmationRequired, Description: "confirm"},
		ApprovalPreview: func(arguments any) (json.RawMessage, error) {
			return json.Marshal(arguments)
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{operations: ops}
	input := Input{SessionID: "session-1", IdempotencyKey: "request-1"}
	omitted, err := runtime.prepareOperation(input, ToolCall{
		ID: "call-1", Name: "apply_default", Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := runtime.prepareOperation(input, ToolCall{
		ID: "call-2", Name: "apply_default", Input: json.RawMessage(`{"model_key":"ltx"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonSemanticallyEqual(omitted.normalizedArguments, explicit.normalizedArguments) ||
		!bytes.Contains(omitted.normalizedArguments, []byte(`"model_key":"ltx"`)) {
		t.Fatalf("arguments were not canonicalized before planning: omitted=%s explicit=%s", omitted.normalizedArguments, explicit.normalizedArguments)
	}
	requestID := operationRequestID(input)
	omittedID := operationExecutionID(requestID, 0, 0, omitted.call.Name, omitted.normalizedArguments)
	explicitID := operationExecutionID(requestID, 0, 0, explicit.call.Name, explicit.normalizedArguments)
	if omittedID != explicitID {
		t.Fatalf("semantic defaults produced different execution IDs: omitted=%s explicit=%s", omittedID, explicitID)
	}
}

func TestOperationRegistryRejectsWriteWithoutConfirmationMode(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "apply_change", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "confirmation mode must be none or required") {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryAllowsConfirmationNoneWriteWithoutApprovalPreview(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "apply_change", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	})
	if err != nil {
		t.Fatalf("Register confirmation-none write: %v", err)
	}
}

func TestOperationRegistryRejectsApprovalPreviewOnDirectWrite(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "apply_change", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
		ApprovalPreview: func(any) (json.RawMessage, error) {
			return json.RawMessage(`{"change":"test"}`), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot declare an approval preview") {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryRejectsWriteWithoutSafeApprovalPreview(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "apply_change", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Confirmation: ConfirmationSpec{Mode: ConfirmationRequired, Description: "verify the persisted change"},
	})
	if err == nil || !strings.Contains(err.Error(), "safe approval preview") {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryRejectsEmptyApprovalPreview(t *testing.T) {
	registry := NewOperationRegistry()
	err := registry.Register(Operation{
		Name: "apply_change", Description: "write", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Confirmation:    ConfirmationSpec{Mode: ConfirmationRequired, Description: "verify the persisted change"},
		ApprovalPreview: func(any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.BuildApprovalPreview("apply_change", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "at least one change") {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryRejectsNonObjectInputSchema(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "read_value", Description: "read", Effect: OperationEffectRead,
		InputSchema: json.RawMessage(`{"type":"string"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil || !strings.Contains(err.Error(), `top-level type must be "object"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryRejectsEmptyCapability(t *testing.T) {
	err := NewOperationRegistry().Register(Operation{
		Name: "read_value", Description: "read", Effect: OperationEffectRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"read_value", "  "}, Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	})
	if err == nil || !strings.Contains(err.Error(), "capability 1 is empty") {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationRegistryRejectsTrailingJSONValues(t *testing.T) {
	registry := NewOperationRegistry()
	if err := registry.Register(Operation{
		Name: "read_value", Description: "read", Effect: OperationEffectRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecodeInput("read_value", json.RawMessage(`{} {}`)); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("DecodeInput err=%v", err)
	}
	if _, err := registry.DecodeOutput("read_value", json.RawMessage(`{} {}`)); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("DecodeOutput err=%v", err)
	}
}

func TestRuntimeRequiresVerifierForConfirmedOperation(t *testing.T) {
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	_, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Operations: ops, Policy: allowPolicy(),
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{Output: json.RawMessage(`{}`)}, nil
		}),
	})
	if !errors.Is(err, ErrVerifierRequired) {
		t.Fatalf("err=%v, want ErrVerifierRequired", err)
	}
}
