package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRuntimeResumesApprovalWithCanonicalizedDefaultInput(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "power_on", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "started"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(Operation{
		Name: "power_on", ContractVersion: "test-v1", Description: "power on", Effect: OperationEffectWrite,
		InputSchema: json.RawMessage(`{
            "type":"object",
            "properties":{"model_key":{"type":["string","null"]}},
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
	approver := &resumableApprover{}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	executions := 0
	var executionID string
	executor := OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		executions++
		executionID = request.ExecutionID
		arguments, ok := request.Arguments.(map[string]any)
		if !ok || arguments["model_key"] != "ltx" {
			t.Fatalf("executor arguments=%#v, want canonical ltx default", request.Arguments)
		}
		return OperationResult{Output: json.RawMessage(`{"started":true}`)}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, policy, executor, confirmingVerifier(), approver, store)
	input := Input{RunID: "run-default", SessionID: "session-default", User: "start", IdempotencyKey: "approval-default"}

	first, err := rt.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != RunStatusWaitingUser || executions != 0 || approver.request == nil {
		t.Fatalf("first=%+v executions=%d approval=%+v", first, executions, approver.request)
	}
	if string(approver.request.Preview) != `{"model_key":"ltx"}` {
		t.Fatalf("approval preview=%s, want canonical ltx default", approver.request.Preview)
	}
	arguments, ok := approver.request.Operation.Arguments.(map[string]any)
	if !ok || arguments["model_key"] != "ltx" {
		t.Fatalf("approval arguments=%#v, want canonical ltx default", approver.request.Operation.Arguments)
	}

	approver.resolve(true, "approved by user")
	second, err := rt.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != RunStatusCompleted || second.Output != "started" || executions != 1 {
		t.Fatalf("second=%+v executions=%d", second, executions)
	}
	if executionID == "" || executionID != approver.request.Operation.ExecutionID {
		t.Fatalf("execution_id=%q approval_execution_id=%q", executionID, approver.request.Operation.ExecutionID)
	}
}

func TestRuntimeRestoresApprovalResponseIDForTerminalOperation(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-terminal", ToolCall{ID: "call-terminal", Name: "finish_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish_change", OperationEffectWrite)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approver := &resumableApprover{}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, policy, OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"done":true}`), FinalResponse: "已完成。"}, nil
	}), confirmingVerifier(), approver, store)
	input := Input{RunID: "run-terminal", SessionID: "session-terminal", User: "finish", IdempotencyKey: "approval-terminal"}
	first, err := rt.Run(t.Context(), input)
	if err != nil || first.Status != RunStatusWaitingUser {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	approver.resolve(true, "approved")
	second, err := rt.Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != RunStatusCompleted || second.Output != "已完成。" || second.LastResponseID != "resp-terminal" || len(model.requests) != 1 {
		t.Fatalf("second=%+v model_calls=%d", second, len(model.requests))
	}
	if got := store.sessions[input.SessionID].LastResponseID; got != "resp-terminal" {
		t.Fatalf("session last_response_id=%q", got)
	}
}

func TestRuntimeResumesDeniedApprovalAsToolResultWithoutOperationFailure(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "已按你的决定取消修改。"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approver := &resumableApprover{}
	store := &recordingStore{}
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, policy, OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		t.Fatal("denied operation must not execute")
		return OperationResult{}, nil
	}), confirmingVerifier(), approver, store, func(event Event) { events = append(events, event) })
	input := Input{RunID: "run-denied", SessionID: "session-denied", User: "apply", IdempotencyKey: "approval-denied"}
	first, err := rt.Run(context.Background(), input)
	if err != nil || first.Status != RunStatusWaitingUser {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	approver.resolve(false, "user declined")
	second, err := rt.Run(context.Background(), input)
	if err != nil || second.Status != RunStatusCompleted || second.Output != "已按你的决定取消修改。" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	failed, cancelled := 0, 0
	for _, event := range events {
		switch event.Type {
		case EventOperationFailed:
			failed++
		case EventOperationCancelled:
			cancelled++
		}
	}
	if failed != 0 || cancelled != 1 {
		t.Fatalf("operation_failed=%d operation_cancelled=%d", failed, cancelled)
	}
	request := model.requests[1]
	if len(request.Input) != 3 || request.Input[2].Type != ModelInputToolResult || !strings.Contains(string(request.Input[2].Output), `"approved":false`) {
		t.Fatalf("denial resume transcript=%+v", request.Input)
	}
	if got := len(store.sessions[input.SessionID].Transcript); got != 4 {
		t.Fatalf("session transcript items=%d, want 4", got)
	}
}

func TestRuntimeTerminalOperationReturnsFinalResponseWithoutAnotherModelCall(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	var executed OperationRequest
	executor := OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		executed = request
		return OperationResult{Output: json.RawMessage(`{"done":true}`), FinalResponse: "任务已提交。"}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, &recordingStore{})
	result, err := rt.Run(context.Background(), Input{User: "finish"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "任务已提交。" || len(model.requests) != 1 {
		t.Fatalf("result=%+v model_requests=%d", result, len(model.requests))
	}
	if executed.ExecutionID == "" || !strings.HasPrefix(executed.ExecutionID, "terminal_op_") {
		t.Fatalf("terminal read execution id=%q", executed.ExecutionID)
	}
	if executed.AttemptID != "" {
		t.Fatalf("terminal read attempt id=%q, want empty", executed.AttemptID)
	}
}

func TestRuntimeBatchesHomogeneousTerminalWriteOperations(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse(
			"resp-1",
			ToolCall{ID: "call-1", Name: "save_proposal", Input: json.RawMessage(`{"target":"frame-1"}`)},
			ToolCall{ID: "call-2", Name: "save_proposal", Input: json.RawMessage(`{"target":"frame-4"}`)},
		),
	}}
	ops := NewOperationRegistry()
	terminal := operation("save_proposal", OperationEffectWrite)
	terminal.Terminal = true
	terminal.TerminalBatchLimit = 4
	terminal.ProjectTerminalSession = func(arguments any) ([]TerminalSessionProjection, error) {
		values, ok := arguments.(map[string]any)
		if !ok {
			return nil, errors.New("arguments are not an object")
		}
		summary, err := json.Marshal(map[string]any{"target": values["target"]})
		if err != nil {
			return nil, err
		}
		return []TerminalSessionProjection{{Type: "proposal", SessionSummary: summary}}, nil
	}
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executed := make([]string, 0, 2)
	executor := OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
		executed = append(executed, request.Call.ID)
		artifact, err := json.Marshal(map[string]string{"call_id": request.Call.ID})
		if err != nil {
			return OperationResult{}, err
		}
		summary, err := json.Marshal(map[string]any{"target": request.Arguments.(map[string]any)["target"]})
		if err != nil {
			return OperationResult{}, err
		}
		return OperationResult{
			Output: json.RawMessage(`{"saved":true}`), Receipt: json.RawMessage(`{"persisted":true}`),
			FinalResponse: "视频生成方案已保存。",
			Artifacts: []ResultArtifact{{
				Type: "proposal", Data: artifact, SessionSummary: summary,
			}},
		}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, confirmingVerifier(), nil, store)
	result, err := rt.Run(t.Context(), Input{
		RunID: "run-terminal-batch", SessionID: "session-terminal-batch", User: "保存两个方案",
		IdempotencyKey: "terminal-batch", IdempotencyScope: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusCompleted || result.Output != "视频生成方案已保存。" || len(model.requests) != 1 {
		t.Fatalf("result=%+v model_requests=%d", result, len(model.requests))
	}
	if !slices.Equal(executed, []string{"call-1", "call-2"}) {
		t.Fatalf("executed calls=%v", executed)
	}
	if len(store.completed) != 1 || len(store.completed[0].Artifacts) != 2 {
		t.Fatalf("completed runs=%+v", store.completed)
	}
	if string(store.completed[0].Artifacts[0].Data) != `{"call_id":"call-1"}` ||
		string(store.completed[0].Artifacts[1].Data) != `{"call_id":"call-2"}` {
		t.Fatalf("artifacts=%+v", store.completed[0].Artifacts)
	}
	if len(store.executions) != 2 {
		t.Fatalf("execution count=%d, want 2", len(store.executions))
	}
	session := store.sessions["session-terminal-batch"]
	if len(session.Transcript) != 2 || session.Transcript[0].Type != ModelInputUserMessage ||
		session.Transcript[1].Type != ModelInputAssistantOutput || session.Transcript[1].OutputType != ModelOutputMessage {
		t.Fatalf("projected transcript=%+v", session.Transcript)
	}
	for _, item := range session.Transcript {
		if item.Type == ModelInputToolResult || item.OutputType == ModelOutputFunctionCall {
			t.Fatalf("terminal batch function-call pair survived projection: %+v", session.Transcript)
		}
	}
	if !bytes.Contains(session.Transcript[1].Raw, []byte(`\"target\":\"frame-1\"`)) ||
		!bytes.Contains(session.Transcript[1].Raw, []byte(`\"target\":\"frame-4\"`)) {
		t.Fatalf("projected batch history=%s", session.Transcript[1].Raw)
	}
}

func TestRuntimeRejectsMultipleNonBatchableTerminalOperationsBeforeExecution(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse(
			"resp-1",
			ToolCall{ID: "call-1", Name: "finish", Input: json.RawMessage(`{}`)},
			ToolCall{ID: "call-2", Name: "finish", Input: json.RawMessage(`{}`)},
		),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish", OperationEffectWrite)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	executions := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{}, nil
	}), confirmingVerifier(), nil, &recordingStore{})
	_, err := rt.Run(t.Context(), Input{
		RunID: "run-terminal-rejected", SessionID: "session-terminal-rejected", User: "finish twice",
		IdempotencyKey: "terminal-rejected", IdempotencyScope: "test",
	})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "must be the only operation") {
		t.Fatalf("Run() error=%v", err)
	}
	if executions != 0 {
		t.Fatalf("executor calls=%d, want 0", executions)
	}
}
