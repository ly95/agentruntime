package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type resumableApprover struct {
	request      *ApprovalRequest
	approved     *bool
	reason       string
	calls        int
	mutateResume func(*ApprovalResume)
}

// persistedApprovalResumer simulates a fresh process: it reconstructs resume
// state exclusively from the approval atomically committed by recordingStore.
type persistedApprovalResumer struct {
	store        *recordingStore
	approved     *bool
	reason       string
	mutateResume func(*ApprovalResume)
}

func (a *persistedApprovalResumer) RequestApproval(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{}, errors.New("persisted approval resumer cannot create a new approval")
}

func (a *persistedApprovalResumer) ResumeApproval(_ context.Context, runID string) (*ApprovalResume, error) {
	pending, err := a.store.pendingApprovalForTest(runID)
	if err != nil || pending == nil {
		return nil, err
	}
	request := pending.Request
	resume := &ApprovalResume{
		ID: pending.Decision.ID, ExecutionID: request.Operation.ExecutionID,
		Operation: request.Operation.Operation.Name, ContractID: request.Operation.Operation.ContractID,
		Call: request.Operation.Call, ResponseID: request.ResponseID,
		ModelOutput: cloneModelOutputItems(request.ModelOutput),
		Preview:     append(json.RawMessage(nil), request.Preview...),
		Checkpoint:  cloneApprovalCheckpoint(request.Checkpoint, true),
		Reason:      a.reason,
	}
	if a.approved == nil {
		resume.Pending = true
	} else {
		resume.Approved = *a.approved
	}
	if a.mutateResume != nil {
		a.mutateResume(resume)
	}
	return resume, nil
}

func (a *resumableApprover) RequestApproval(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	a.calls++
	if a.request == nil {
		cloned := request
		cloned.ModelOutput = cloneModelOutputItems(request.ModelOutput)
		cloned.Operation.Call.Input = append(json.RawMessage(nil), request.Operation.Call.Input...)
		cloned.Preview = append(json.RawMessage(nil), request.Preview...)
		cloned.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, true)
		a.request = &cloned
	}
	if a.approved == nil {
		return ApprovalDecision{ID: "approval-1", Pending: true}, nil
	}
	return ApprovalDecision{ID: "approval-1", Approved: *a.approved, Reason: a.reason}, nil
}

func (a *resumableApprover) ResumeApproval(_ context.Context, runID string) (*ApprovalResume, error) {
	if a.request == nil || a.request.Operation.RunID != runID {
		return nil, nil
	}
	resume := &ApprovalResume{
		ID: "approval-1", ExecutionID: a.request.Operation.ExecutionID,
		Operation: a.request.Operation.Operation.Name, ContractID: a.request.Operation.Operation.ContractID, Call: a.request.Operation.Call,
		ResponseID: a.request.ResponseID, ModelOutput: cloneModelOutputItems(a.request.ModelOutput), Reason: a.reason,
		Preview: append(json.RawMessage(nil), a.request.Preview...), Checkpoint: cloneApprovalCheckpoint(a.request.Checkpoint, true),
	}
	if a.approved == nil {
		resume.Pending = true
	} else {
		resume.Approved = *a.approved
	}
	if a.mutateResume != nil {
		a.mutateResume(resume)
	}
	return resume, nil
}

func (a *resumableApprover) resolve(approved bool, reason string) {
	a.approved = &approved
	a.reason = reason
}

func allowPolicy() OperationPolicy {
	return OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyAllow}, nil
	})
}

func messageResponse(id, text string) *ModelResponse {
	itemID := id + "-message"
	return &ModelResponse{ID: id, OutputText: text, Items: []ModelOutputItem{{
		ID: itemID, Type: ModelOutputMessage, Text: text,
		Raw: mustJSON(map[string]any{
			"id": itemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}),
	}}}
}

func refusalResponse(id, refusal string) *ModelResponse {
	itemID := id + "-message"
	return &ModelResponse{ID: id, Refusal: refusal, Items: []ModelOutputItem{{
		ID: itemID, Type: ModelOutputMessage,
		Raw: mustJSON(map[string]any{
			"id": itemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "refusal", "refusal": refusal}},
		}),
	}}}
}

func TestRuntimeReturnsRefusalAsFinalOutput(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{refusalResponse("resp-refusal", "I cannot help with that.")}}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, nil)

	result, err := rt.Run(context.Background(), Input{User: "request"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "I cannot help with that." {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestRuntimeRejectsReasoningOnlyResponseWithoutRetry(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		{ID: "resp-reasoning-only", FinishReason: "length", HadReasoning: true, Items: []ModelOutputItem{}},
		messageResponse("must-not-run", "unexpected retry"),
	}}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, nil)

	_, err := rt.Run(context.Background(), Input{User: "request"})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), `finish_reason="length"`) {
		t.Fatalf("Run error=%v, want reasoning-only ErrInvalidModelOutput", err)
	}
	if len(model.requests) != 1 || len(model.responses) != 1 {
		t.Fatalf("model requests=%d remaining responses=%d, want one call and no retry", len(model.requests), len(model.responses))
	}
	if model.requests[0].DisableReasoning || strings.Contains(model.requests[0].Instructions, "never return reasoning alone") {
		t.Fatalf("Runtime synthesized corrective request options: %+v", model.requests[0])
	}
}

func TestRuntimeReasoningOnlyFailureDoesNotAdvanceDurableTranscript(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{{
		ID: "resp-durable-reasoning-only", FinishReason: "stop", HadReasoning: true, Items: []ModelOutputItem{},
	}}}
	store := &recordingStore{}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, store)

	_, err := rt.Run(context.Background(), Input{User: "request", SessionID: "reasoning-only-session"})
	if !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatalf("Run error=%v, want ErrInvalidModelOutput", err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests=%d, want exactly one", len(model.requests))
	}
	store.mu.Lock()
	session := store.sessions["reasoning-only-session"]
	responseAudits := 0
	for _, item := range store.items {
		if item.Type == ItemTypeModelResponse {
			responseAudits++
		}
	}
	store.mu.Unlock()
	if session.LastResponseID != "" || len(session.Transcript) != 0 {
		t.Fatalf("durable transcript advanced after invalid response: %+v", session)
	}
	if session.Revision != 1 || session.LastRunID == "" || !strings.Contains(session.LastError, ErrInvalidModelOutput.Error()) {
		t.Fatalf("durable failure metadata was not committed: %+v", session)
	}
	if responseAudits != 1 {
		t.Fatalf("model response audits=%d, want one accepted response audit", responseAudits)
	}
}

func TestRuntimeUsesHostAssignedRunID(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-host-run", "done")}}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, &recordingStore{})

	result, err := rt.Run(context.Background(), Input{RunID: "durable-run-42", User: "request"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RunID != "durable-run-42" {
		t.Fatalf("run id=%q, want host-assigned id", result.RunID)
	}
}

func TestRuntimeOmitsStreamSinkWithoutEventConsumer(t *testing.T) {
	model := &streamSinkObservingModel{}
	rt := newTestRuntime(t, model, nil, nil, nil, nil, nil, nil)

	if _, err := rt.Run(context.Background(), Input{User: "hello"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if model.sawStreamSink {
		t.Fatal("runtime installed a stream sink without an event consumer")
	}
}

func TestRuntimeProtectsTranscriptAndToolsFromModelMutation(t *testing.T) {
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	rt := newTestRuntime(t, &mutatingRequestModel{}, ops, allowPolicy(), OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), nil, &recordingStore{})
	result, err := rt.Run(context.Background(), Input{
		User: "apply", IdempotencyKey: "model-boundary", IdempotencyScope: "test",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("output=%q, want done", result.Output)
	}
}

func TestNewRuntimeDoesNotFreezeRegistryWhenConfigurationIsInvalid(t *testing.T) {
	ops := NewOperationRegistry()
	if err := ops.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	_, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Operations: ops,
	})
	if err == nil || !strings.Contains(err.Error(), "operation policy is required") {
		t.Fatalf("NewRuntime error=%v, want missing policy", err)
	}
	if err := ops.Register(operation("read_more", OperationEffectRead)); err != nil {
		t.Fatalf("registry was frozen by failed constructor: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RuntimeConfig)
	}{
		{name: "negative max iterations", mutate: func(cfg *RuntimeConfig) { cfg.MaxIterations = -1 }},
		{name: "negative session lease", mutate: func(cfg *RuntimeConfig) { cfg.SessionLeaseTTL = -1 }},
		{name: "invalid renewal interval", mutate: func(cfg *RuntimeConfig) {
			cfg.SessionLeaseTTL = time.Second
			cfg.LeaseRenewalInterval = time.Second
		}},
		{name: "negative cleanup timeout", mutate: func(cfg *RuntimeConfig) { cfg.CleanupTimeout = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewOperationRegistry()
			cfg := RuntimeConfig{Model: &scriptedModel{}, Operations: registry}
			test.mutate(&cfg)
			if _, err := NewRuntime(cfg); err == nil {
				t.Fatal("NewRuntime unexpectedly succeeded")
			}
			if err := registry.Register(operation("read_after_failure", OperationEffectRead)); err != nil {
				t.Fatalf("registry was frozen by failed constructor: %v", err)
			}
		})
	}
}

func TestNewRuntimeRejectsTypedNilDependencies(t *testing.T) {
	var nilModel *scriptedModel
	var nilPolicy OperationPolicyFunc
	var nilExecutor OperationExecutorFunc
	var nilVerifier ResultVerifierFunc
	var nilApprover ApproverFunc
	var nilStore *recordingStore

	tests := []struct {
		name   string
		mutate func(*RuntimeConfig)
		want   string
	}{
		{name: "model", mutate: func(cfg *RuntimeConfig) { cfg.Model = nilModel }, want: "model is required"},
		{name: "operation policy", mutate: func(cfg *RuntimeConfig) { cfg.Policy = nilPolicy }, want: "configured operation policy is nil"},
		{name: "operation executor", mutate: func(cfg *RuntimeConfig) { cfg.Executor = nilExecutor }, want: "configured operation executor is nil"},
		{name: "result verifier", mutate: func(cfg *RuntimeConfig) { cfg.Verifier = nilVerifier }, want: "configured result verifier is nil"},
		{name: "approver", mutate: func(cfg *RuntimeConfig) { cfg.Approver = nilApprover }, want: "configured approver is nil"},
		{name: "run store", mutate: func(cfg *RuntimeConfig) { cfg.RunStore = nilStore }, want: "configured run store is nil"},
		{name: "execution store", mutate: func(cfg *RuntimeConfig) { cfg.Executions = nilStore }, want: "configured execution store is nil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := RuntimeConfig{Model: &scriptedModel{}}
			test.mutate(&cfg)
			_, err := NewRuntime(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewRuntime error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeCorrelatesModelFailure(t *testing.T) {
	sentinel := errors.New("provider unavailable")
	store := &recordingStore{}
	var events []Event
	nextID := 0
	rt, err := NewRuntime(RuntimeConfig{
		Model: failingModel{err: sentinel}, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
		NewID: func() string {
			nextID++
			return fmt.Sprintf("id-%d", nextID)
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "hello"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}

	var started, failed, runFailed Event
	for _, event := range events {
		switch event.Type {
		case EventModelStarted:
			started = event
		case EventModelFailed:
			failed = event
		case EventRunFailed:
			runFailed = event
		}
	}
	if started.ModelCallID == "" || failed.ModelCallID != started.ModelCallID || runFailed.ModelCallID != started.ModelCallID {
		t.Fatalf("uncorrelated failure events: started=%+v failed=%+v run_failed=%+v", started, failed, runFailed)
	}
	foundErrorItem := false
	for _, item := range store.items {
		if item.Type == ItemTypeError {
			foundErrorItem = true
			if item.ModelCallID != started.ModelCallID {
				t.Fatalf("error item=%+v, want model_call_id=%q", item, started.ModelCallID)
			}
		}
	}
	if !foundErrorItem {
		t.Fatal("missing stored error item")
	}
}

func TestRuntimeFailsModelLifecycleWhenResponseAuditFails(t *testing.T) {
	sentinel := errors.New("append model response failed")
	store := &appendFailingStore{failType: ItemTypeModelResponse, err: sentinel}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}}
	var events []Event
	nextID := 0
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
		NewID: func() string {
			nextID++
			return fmt.Sprintf("id-%d", nextID)
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "hello"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error=%v, want sentinel", err)
	}

	started, completed, failed, runFailed, terminalCount := modelLifecycleEvents(events)
	if terminalCount != 1 || started.ModelCallID == "" || completed.ModelCallID != "" || failed.ModelCallID != started.ModelCallID || failed.ResponseID != "resp-1" || runFailed.ModelCallID != started.ModelCallID {
		t.Fatalf("lifecycle events: started=%+v completed=%+v failed=%+v run_failed=%+v", started, completed, failed, runFailed)
	}
	assertStoredErrorModelCallID(t, store.items, started.ModelCallID)
}

func TestRuntimeClosesModelLifecycleWhenResponseMarshalFails(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{{
		ID:    "resp-invalid",
		Items: []ModelOutputItem{{Type: ModelOutputMessage, Raw: json.RawMessage(`{`)}},
	}}}
	store := &recordingStore{}
	var events []Event
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
		NewID:     func() string { return fmt.Sprintf("id-%d", len(events)+len(store.items)+1) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "hello"})
	if err == nil || !strings.Contains(err.Error(), "cannot be replayed") {
		t.Fatalf("Run error=%v", err)
	}

	started, completed, failed, runFailed, terminalCount := modelLifecycleEvents(events)
	if terminalCount != 1 || started.ModelCallID == "" || completed.ModelCallID != "" || failed.ModelCallID != started.ModelCallID || runFailed.ModelCallID != started.ModelCallID {
		t.Fatalf("lifecycle events: started=%+v completed=%+v failed=%+v run_failed=%+v", started, completed, failed, runFailed)
	}
	assertStoredErrorModelCallID(t, store.items, started.ModelCallID)
}

func TestRuntimeCorrelatesInvalidModelOutput(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("", "done")}}
	store := &recordingStore{}
	var events []Event
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
		NewID:     func() string { return fmt.Sprintf("id-%d", len(events)+len(store.items)+1) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = rt.Run(context.Background(), Input{User: "hello"})
	if !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatalf("Run error=%v, want ErrInvalidModelOutput", err)
	}

	started, completed, failed, runFailed, terminalCount := modelLifecycleEvents(events)
	if terminalCount != 1 || started.ModelCallID == "" || completed.ModelCallID != "" || failed.ModelCallID != started.ModelCallID || runFailed.ModelCallID != started.ModelCallID {
		t.Fatalf("lifecycle events: started=%+v completed=%+v failed=%+v run_failed=%+v", started, completed, failed, runFailed)
	}
	assertStoredErrorModelCallID(t, store.items, started.ModelCallID)
}
