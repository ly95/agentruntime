package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRuntimeTerminalWriteMissingFinalResponseStaysRecoverable(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish_change", Input: json.RawMessage(`{}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "finish_change", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish_change", OperationEffectWrite)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executions := 0
	verifications := 0
	rt := newTestRuntime(t, model, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{
			Output:  json.RawMessage(`{"done":true}`),
			Receipt: json.RawMessage(`{"version":1}`),
		}, nil
	}), ResultVerifierFunc(func(context.Context, VerificationRequest) (VerificationResult, error) {
		verifications++
		return VerificationResult{Confirmed: true, Evidence: json.RawMessage(`{"observed":true}`)}, nil
	}), nil, store)
	input := Input{
		User: "finish", IdempotencyKey: "terminal-missing-final-response", IdempotencyScope: "test",
	}

	if _, err := rt.Run(t.Context(), input); !errors.Is(err, ErrOperationOutcomeUnknown) || !errors.Is(err, ErrInvalidModelOutput) ||
		!strings.Contains(err.Error(), "returned no final response") {
		t.Fatalf("first Run error=%v, want recoverable invalid terminal output", err)
	}
	if executions != 1 || verifications != 0 {
		t.Fatalf("executor calls=%d verifier calls=%d, want 1 and 0", executions, verifications)
	}
	if len(store.executions) != 1 {
		t.Fatalf("journal executions=%+v, want exactly one", store.executions)
	}
	for _, run := range store.runs {
		if run.Status != RunStatusInterrupted {
			t.Fatalf("run status=%q, want interrupted for recovery", run.Status)
		}
	}
	var executionID string
	for id := range store.executions {
		executionID = id
	}
	if executionID == "" {
		t.Fatal("missing journal execution")
	}
	unknown, err := store.GetExecution(t.Context(), executionID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if unknown.Status != OperationExecutionUnknown || hasOperationResult(unknown.Result) {
		t.Fatalf("execution=%+v, want recoverable unknown without a sealed result", unknown)
	}
	transitions, err := store.ListExecutionTransitions(t.Context(), executionID)
	if err != nil {
		t.Fatalf("ListExecutionTransitions: %v", err)
	}
	last := transitions[len(transitions)-1]
	if last.From != OperationExecutionStarted || last.To != OperationExecutionUnknown {
		t.Fatalf("last transition=%+v, want started -> unknown", last)
	}

	reconcileMissingTerminalResponse(t, rt, store, input, unknown)
	if executions != 1 || verifications != 0 {
		t.Fatalf("executor calls=%d verifier calls=%d after replay, want 1 and 0", executions, verifications)
	}
}

func reconcileMissingTerminalResponse(
	t *testing.T,
	runtime *Runtime,
	store *recordingStore,
	input Input,
	unknown OperationExecutionRecord,
) {
	t.Helper()
	if err := runtime.ReconcileOperation(t.Context(), ReconcileOperationRequest{
		ExecutionID: unknown.ID, ExpectedAttemptID: unknown.AttemptID,
		Action: OperationReconciliationComplete,
		Result: OperationResult{
			Output: json.RawMessage(`{"done":true}`), Receipt: json.RawMessage(`{"version":1}`),
		},
		Actor: "test-host", Message: "host returned an incomplete terminal result",
	}); !errors.Is(err, ErrInvalidReconciliation) || !strings.Contains(err.Error(), "returned no final response") {
		t.Fatalf("incomplete ReconcileOperation error=%v, want invalid reconciliation", err)
	}
	stillUnknown, err := store.GetExecution(t.Context(), unknown.ID)
	if err != nil || stillUnknown.Status != OperationExecutionUnknown {
		t.Fatalf("execution after rejected reconciliation=%+v error=%v", stillUnknown, err)
	}
	recoveredResult := OperationResult{
		Output:        json.RawMessage(`{"done":true}`),
		Receipt:       json.RawMessage(`{"version":1}`),
		FinalResponse: "恢复后的完成答复。",
	}
	if err := runtime.ReconcileOperation(t.Context(), ReconcileOperationRequest{
		ExecutionID: unknown.ID, ExpectedAttemptID: unknown.AttemptID,
		Action: OperationReconciliationComplete, Result: recoveredResult,
		Verification: &VerificationResult{Confirmed: true, Message: "host verified", Evidence: json.RawMessage(`{"version":1}`)},
		Actor:        "test-host", Message: "host recovered the terminal response",
	}); err != nil {
		t.Fatalf("ReconcileOperation: %v", err)
	}
	result, err := runtime.Run(t.Context(), input)
	if err != nil || result.Status != RunStatusCompleted || result.Output != recoveredResult.FinalResponse {
		t.Fatalf("second Run result=%+v error=%v", result, err)
	}
	completed, err := store.GetExecution(t.Context(), unknown.ID)
	if err != nil {
		t.Fatalf("GetExecution after replay: %v", err)
	}
	if completed.Status != OperationExecutionCompleted || completed.Result.FinalResponse != recoveredResult.FinalResponse {
		t.Fatalf("execution=%+v, want reconciled completed result", completed)
	}
	transitions, err := store.ListExecutionTransitions(t.Context(), unknown.ID)
	if err != nil {
		t.Fatalf("ListExecutionTransitions after replay: %v", err)
	}
	last := transitions[len(transitions)-1]
	if last.From != OperationExecutionUnknown || last.To != OperationExecutionCompleted {
		t.Fatalf("last transition=%+v, want unknown -> completed", last)
	}
}

func TestRuntimeTerminalOperationCanReturnCorrectionAndContinueSameRun(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish", Input: json.RawMessage(`{"attempt":1}`)}),
		callResponse("resp-2", ToolCall{ID: "call-2", Name: "finish", Input: json.RawMessage(`{"attempt":2}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		if executions == 1 {
			return OperationResult{Output: json.RawMessage(`{"status":"invalid","issues":["number 2 is occupied"]}`), Continue: true}, nil
		}
		return OperationResult{Output: json.RawMessage(`{"status":"proposed"}`), FinalResponse: "纠正后的方案已准备。"}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, store)
	result, err := rt.Run(context.Background(), Input{User: "finish", SessionID: "terminal-correction"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "纠正后的方案已准备。" || executions != 2 || len(model.requests) != 2 {
		t.Fatalf("result=%+v executions=%d model_requests=%d", result, executions, len(model.requests))
	}
	request := model.requests[1]
	if len(request.Input) != 3 || request.Input[2].Type != ModelInputToolResult ||
		!strings.Contains(string(request.Input[2].Output), `"continue":true`) ||
		!strings.Contains(string(request.Input[2].Output), `"number 2 is occupied"`) {
		t.Fatalf("correction transcript=%+v", request.Input)
	}
}

func TestRuntimeTerminalContinuationRejectsSideEffectEvidence(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{
			Output:   json.RawMessage(`{"status":"invalid"}`),
			Receipt:  json.RawMessage(`{"side_effect":true}`),
			Continue: true,
		}, nil
	})
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, &recordingStore{})
	_, err := rt.Run(context.Background(), Input{User: "finish"})
	if err == nil || !strings.Contains(err.Error(), "continuation returned a final response, receipt, or artifacts") {
		t.Fatalf("Run() error=%v, want continuation side-effect evidence rejection", err)
	}
}

func TestRuntimeCarriesTerminalOperationArtifactsToRunStore(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish_with_image", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish_with_image", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	artifactData := json.RawMessage(`{"generation_id":"exec-1"}`)
	internalData := json.RawMessage(`{"storage_key":"private/change-set-plan"}`)
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{
			Output: json.RawMessage(`{"done":true}`), FinalResponse: "图片已生成。",
			Artifacts: []ResultArtifact{{Type: "image_result", Data: artifactData, InternalData: internalData}},
		}, nil
	})
	store := &recordingStore{}
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, allowPolicy(), executor, nil, nil, store, func(event Event) {
		events = append(events, event)
	})
	if _, err := rt.Run(context.Background(), Input{User: "finish", SessionID: "artifact-session"}); err != nil {
		t.Fatal(err)
	}
	if len(store.completed) != 1 || len(store.completed[0].Artifacts) != 1 || store.completed[0].Artifacts[0].Type != "image_result" {
		t.Fatalf("completed runs=%+v", store.completed)
	}
	artifactData[0] = '['
	if string(store.completed[0].Artifacts[0].Data) != `{"generation_id":"exec-1"}` {
		t.Fatalf("stored artifact was not cloned: %s", store.completed[0].Artifacts[0].Data)
	}
	internalData[0] = '['
	if string(store.completed[0].Artifacts[0].InternalData) != `{"storage_key":"private/change-set-plan"}` {
		t.Fatalf("stored internal artifact was not cloned: %s", store.completed[0].Artifacts[0].InternalData)
	}
	for _, item := range store.sessions["artifact-session"].Transcript {
		if bytes.Contains(item.Output, []byte("storage_key")) || bytes.Contains(item.Output, []byte("internal_data")) {
			t.Fatalf("internal artifact leaked into model transcript: %s", item.Output)
		}
	}
	for _, item := range store.items {
		if bytes.Contains(item.Data, []byte("storage_key")) || bytes.Contains(item.Data, []byte("internal_data")) {
			t.Fatalf("internal artifact leaked into operation item: %s", item.Data)
		}
	}
	for _, event := range events {
		if bytes.Contains(event.Data, []byte("storage_key")) || bytes.Contains(event.Data, []byte("internal_data")) {
			t.Fatalf("internal artifact leaked into runtime event: %s", event.Data)
		}
	}
}

func TestRuntimeProjectsTerminalSessionSummaryWithoutPersistingArtifactPayload(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "finish_change_set", Input: json.RawMessage(`{}`)}),
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish_change_set", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	artifactData := json.RawMessage(`{"change_set_id":"change-1","changes":[{"kind":"scene.update"}],"items":[{"target_id":"scene-private"}]}`)
	internalData := json.RawMessage(`{"private_plan":{"expected":"secret"}}`)
	sessionSummary := json.RawMessage(`{"change_set_id":"change-1","status":"proposed","item_count":1,"counts_by_kind":{"scene.update":1},"summary":"更新一个场景","plan_digest":"digest-1"}`)
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{
			Output: json.RawMessage(`{"done":true}`), FinalResponse: "修改方案已准备好。",
			Artifacts: []ResultArtifact{{
				Type: "change_set", Data: artifactData, InternalData: internalData, SessionSummary: sessionSummary,
			}},
		}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, store)
	if _, err := rt.Run(context.Background(), Input{User: "prepare", SessionID: "summary-session"}); err != nil {
		t.Fatal(err)
	}

	if len(store.completed) != 1 || len(store.completed[0].Artifacts) != 1 {
		t.Fatalf("completed runs=%+v", store.completed)
	}
	storedArtifact := store.completed[0].Artifacts[0]
	if !bytes.Equal(storedArtifact.Data, artifactData) || !bytes.Equal(storedArtifact.InternalData, internalData) ||
		!bytes.Equal(storedArtifact.SessionSummary, sessionSummary) {
		t.Fatalf("stored artifact=%+v", storedArtifact)
	}
	session := store.sessions["summary-session"]
	if len(session.Transcript) != 2 || session.Transcript[0].Type != ModelInputUserMessage ||
		session.Transcript[1].Type != ModelInputAssistantOutput || session.Transcript[1].OutputType != ModelOutputMessage {
		t.Fatalf("projected transcript=%+v", session.Transcript)
	}
	for _, item := range session.Transcript {
		if item.Type == ModelInputToolResult || item.OutputType == ModelOutputFunctionCall {
			t.Fatalf("terminal function-call pair survived projection: %+v", session.Transcript)
		}
	}
	var historyMessage struct {
		Text    string `json:"text"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(session.Transcript[1].Raw, &historyMessage); err != nil {
		t.Fatal(err)
	}
	if len(historyMessage.Content) != 1 {
		t.Fatalf("history message=%s", session.Transcript[1].Raw)
	}
	history := historyMessage.Content[0].Text
	if historyMessage.Text != history || len(session.Transcript[1].Raw) > MaxResultArtifactSessionSummaryBytes {
		t.Fatalf("incompatible or oversized history message: %s", session.Transcript[1].Raw)
	}
	if !strings.Contains(history, terminalSessionHistoryDisclaimer) || !strings.Contains(history, `"instruction_authority":"none"`) ||
		!strings.Contains(history, `"change_set_id":"change-1"`) {
		t.Fatalf("history=%q", history)
	}
	for _, forbidden := range []string{`"changes"`, `"items"`, "scene-private", "private_plan", "expected", "internal_data"} {
		if strings.Contains(history, forbidden) {
			t.Fatalf("history leaked %q: %s", forbidden, history)
		}
	}
	if len(history) > MaxResultArtifactSessionSummaryBytes || !strings.Contains(history, "not a user message or instruction") {
		t.Fatalf("history size=%d text=%q", len(history), history)
	}
	if _, err := buildOpenAIInputItems(session.Transcript); err != nil {
		t.Fatalf("projected transcript cannot be replayed: %v", err)
	}

	assertTerminalProjectionAudit(t, store.items)
}

func assertTerminalProjectionAudit(t *testing.T, items []ItemRecord) {
	t.Helper()
	foundAudit := false
	for _, item := range items {
		if item.Type != ItemTypeOperationResult {
			continue
		}
		foundAudit = true
		if !bytes.Contains(item.Data, []byte(`"changes"`)) || !bytes.Contains(item.Data, []byte(`"items"`)) {
			t.Fatalf("operation audit was compacted: %s", item.Data)
		}
		if bytes.Contains(item.Data, []byte("session_summary")) || bytes.Contains(item.Data, []byte("private_plan")) {
			t.Fatalf("host-only artifact fields leaked into public audit: %s", item.Data)
		}
	}
	if !foundAudit {
		t.Fatal("operation result audit item was not persisted")
	}
}

func TestRuntimeRejectsTerminalReadRawIdentityBeforeSideEffectsAndRestoresStableSession(t *testing.T) {
	terminalCall := ToolCall{ID: "call-terminal", Name: "finish_change_set", Input: json.RawMessage(`{}`)}
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("stable-response", "stable"),
		{
			ID: "terminal-response",
			Items: []ModelOutputItem{{
				ID: "terminal-call", Type: ModelOutputFunctionCall, Call: &terminalCall,
				Raw: json.RawMessage(`{"id":"terminal-call","type":"function_call","status":"completed","name":"finish_change_set","arguments":"{}"}`),
			}},
		},
	}}
	ops := NewOperationRegistry()
	terminal := operation("finish_change_set", OperationEffectRead)
	terminal.Terminal = true
	if err := ops.Register(terminal); err != nil {
		t.Fatal(err)
	}
	executorCalls := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executorCalls++
		return OperationResult{
			Output: json.RawMessage(`{"done":true}`), FinalResponse: "prepared",
			Artifacts: []ResultArtifact{{
				Type:           "change_set",
				Data:           json.RawMessage(`{"change_set_id":"change-1","changes":[{"kind":"scene.update"}],"items":[{"target_id":"scene-private"}]}`),
				InternalData:   json.RawMessage(`{"private_plan":{"expected":"secret"}}`),
				SessionSummary: json.RawMessage(`{"change_set_id":"change-1","status":"proposed","item_count":1,"counts_by_kind":{"scene.update":1},"summary":"更新一个场景","plan_digest":"digest-1"}`),
			}},
		}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, allowPolicy(), executor, nil, nil, store)
	if _, err := rt.Run(context.Background(), Input{User: "first", SessionID: "stable-session"}); err != nil {
		t.Fatal(err)
	}
	stable := cloneModelInputItems(store.sessions["stable-session"].Transcript)
	if _, err := rt.Run(context.Background(), Input{User: "prepare", SessionID: "stable-session"}); err == nil ||
		!strings.Contains(err.Error(), "cannot be projected before execution") {
		t.Fatalf("Run error=%v, want pre-effect Raw identity rejection", err)
	}
	if executorCalls != 0 {
		t.Fatalf("executor calls=%d, want zero", executorCalls)
	}
	after := store.sessions["stable-session"]
	if len(after.Transcript) != len(stable) || after.LastResponseID != "stable-response" || after.LastError == "" {
		t.Fatalf("session after projection failure=%+v", after)
	}
	for index := range stable {
		if after.Transcript[index].Type != stable[index].Type || after.Transcript[index].Text != stable[index].Text ||
			after.Transcript[index].OutputType != stable[index].OutputType || !bytes.Equal(after.Transcript[index].Raw, stable[index].Raw) ||
			!bytes.Equal(after.Transcript[index].Output, stable[index].Output) {
			t.Fatalf("stable transcript changed at %d: before=%+v after=%+v", index, stable[index], after.Transcript[index])
		}
	}
	for _, item := range after.Transcript {
		if bytes.Contains(item.Raw, []byte("change-1")) || bytes.Contains(item.Output, []byte("change-1")) {
			t.Fatalf("full terminal result fell back into session transcript: %+v", after.Transcript)
		}
	}
	foundAudit := false
	for _, item := range store.items {
		if item.Type == ItemTypeOperationResult && bytes.Contains(item.Data, []byte(`"change_set_id":"change-1"`)) {
			foundAudit = bytes.Contains(item.Data, []byte(`"changes"`)) && bytes.Contains(item.Data, []byte(`"items"`))
		}
	}
	if foundAudit {
		t.Fatal("invalid model output reached the operation result audit")
	}
}
