package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRuntimeApprovalBoundary(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "applied"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approvals := 0
	approver := ApproverFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
		approvals++
		if string(request.Preview) != `{"change":"test"}` {
			t.Fatalf("approval preview=%s", request.Preview)
		}
		return ApprovalDecision{Approved: true, Reason: "approved in test"}, nil
	})
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	store := &recordingStore{}
	var events []Event
	rt := newTestRuntimeWithEventSink(t, model, ops, policy, executor, confirmingVerifier(), approver, store, func(event Event) {
		events = append(events, event)
	})
	if _, err := rt.Run(context.Background(), Input{User: "apply", IdempotencyKey: "approval-boundary", IdempotencyScope: "test"}); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 || executions != 1 {
		t.Fatalf("approvals=%d executions=%d", approvals, executions)
	}
	var requested, completed Event
	for _, event := range events {
		switch event.Type {
		case EventApprovalRequested:
			requested = event
		case EventApprovalCompleted:
			completed = event
		}
	}
	if requested.ExecutionID == "" || completed.ExecutionID != requested.ExecutionID || string(requested.ApprovalPreview) != `{"change":"test"}` {
		t.Fatalf("approval events requested=%+v completed=%+v", requested, completed)
	}
	approvalItems := 0
	for _, item := range store.items {
		if item.Type == ItemTypeApproval {
			approvalItems++
			if item.ExecutionID != requested.ExecutionID {
				t.Fatalf("approval item execution_id=%q, want %q", item.ExecutionID, requested.ExecutionID)
			}
		}
	}
	if approvalItems != 1 {
		t.Fatalf("approval items=%d, want 1", approvalItems)
	}
}

func TestRuntimeCommitsPendingApprovalAuditThroughFinishRun(t *testing.T) {
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
	approver := &resumableApprover{}
	store := &appendFailingStore{failType: ItemTypeApproval, err: errors.New("approval audit must not use AppendItem")}
	rt := newTestRuntime(t, model, ops, policy, OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		t.Fatal("pending operation must not execute")
		return OperationResult{}, nil
	}), confirmingVerifier(), approver, store)

	result, err := rt.Run(t.Context(), Input{
		RunID: "run-atomic-approval", SessionID: "session-atomic-approval",
		User: "apply", IdempotencyKey: "atomic-approval",
	})
	if err != nil || result.Status != RunStatusWaitingUser {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	approvalItems := 0
	for _, item := range store.items {
		if item.Type == ItemTypeApproval {
			approvalItems++
		}
	}
	if approvalItems != 1 {
		t.Fatalf("approval items=%d, want one atomically committed by FinishRun", approvalItems)
	}
}

func TestRuntimeWaitsForApprovalAndResumesSameRun(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "applied"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approver := &resumableApprover{}
	executions := 0
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		executions++
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	store := &recordingStore{}
	rt := newTestRuntime(t, model, ops, policy, executor, confirmingVerifier(), approver, store)
	input := Input{RunID: "run-1", SessionID: "session-1", User: "apply", IdempotencyKey: "approval-resume"}
	first, err := rt.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != RunStatusWaitingUser || executions != 0 {
		t.Fatalf("first=%+v executions=%d", first, executions)
	}
	recoveredPending, err := rt.Run(context.Background(), input)
	if err != nil || recoveredPending.Status != RunStatusWaitingUser || len(model.requests) != 1 || approver.calls != 1 {
		t.Fatalf("pending recovery=%+v err=%v model_calls=%d approvals=%d", recoveredPending, err, len(model.requests), approver.calls)
	}
	approver.resolve(true, "approved by user")
	second, err := rt.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != RunStatusCompleted || second.Output != "applied" || executions != 1 || approver.calls != 1 || len(model.requests) != 2 {
		t.Fatalf("second=%+v executions=%d approvals=%d model_calls=%d", second, executions, approver.calls, len(model.requests))
	}
	if second.LastResponseID != "resp-2" || approver.request == nil || approver.request.ResponseID != "resp-1" {
		t.Fatalf("last_response_id=%q approval_response_id=%+v", second.LastResponseID, approver.request)
	}
	if len(store.runs) != 1 || store.runs[0].ID != "run-1" || store.runs[0].Status != RunStatusCompleted {
		t.Fatalf("runs=%+v", store.runs)
	}
	userItems, operationCalls := 0, 0
	for _, item := range store.items {
		if item.Type == ItemTypeUserMessage {
			userItems++
		}
		if item.Type == ItemTypeOperationCall {
			operationCalls++
		}
	}
	if userItems != 1 || operationCalls != 1 {
		t.Fatalf("user_items=%d operation_calls=%d, want one durable audit item each", userItems, operationCalls)
	}
}

func TestRuntimeApprovalResumePreservesAttachments(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-2", "applied"),
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
	rt := newTestRuntime(t, model, ops, policy, OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	}), confirmingVerifier(), approver, store)
	expiresAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	attachments := []ModelInputAttachment{
		{
			Kind: ModelInputAttachmentImage,
			ID:   "attachment-image", Filename: "reference.png", MIMEType: "image/png",
			StorageKey: "temp/agent/user/reference.png", ExpiresAt: expiresAt,
			URL: "https://cdn.example.com/reference.png",
		},
		{
			Kind: ModelInputAttachmentText,
			ID:   "attachment-document", Filename: "brief.md", MIMEType: "text/markdown",
			Text: "Keep the original composition.",
		},
	}
	input := Input{
		RunID: "run-attachments", SessionID: "session-attachments-resume", User: "apply",
		IdempotencyKey: "approval-attachments", Attachments: attachments,
		ImageAttachmentResolver: ImageAttachmentResolverFunc(func(_ context.Context, attachment ModelInputAttachment) (ModelInputAttachment, error) {
			attachment.URL = "https://cdn.example.com/reference.png"
			return attachment, nil
		}),
	}

	first, err := rt.Run(t.Context(), input)
	if err != nil || first.Status != RunStatusWaitingUser {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	approver.resolve(true, "approved by user")
	second, err := rt.Run(t.Context(), input)
	if err != nil || second.Status != RunStatusCompleted {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if len(model.requests) != 2 || len(model.requests[1].Input) == 0 {
		t.Fatalf("model requests=%+v", model.requests)
	}
	resumedUser := model.requests[1].Input[0]
	wantModelAttachments := append([]ModelInputAttachment(nil), attachments...)
	for index := range wantModelAttachments {
		wantModelAttachments[index].CurrentRun = true
	}
	if resumedUser.Type != ModelInputUserMessage || resumedUser.Text != input.User ||
		len(resumedUser.Attachments) != len(wantModelAttachments) ||
		resumedUser.Attachments[0] != wantModelAttachments[0] || resumedUser.Attachments[1] != wantModelAttachments[1] {
		t.Fatalf("resumed user input=%+v, want attachments=%+v", resumedUser, wantModelAttachments)
	}

	store.mu.Lock()
	session := store.sessions[input.SessionID]
	store.mu.Unlock()
	if len(session.Transcript) == 0 {
		t.Fatal("completed session transcript is empty")
	}
	savedUser := session.Transcript[0]
	wantSavedAttachments := append([]ModelInputAttachment(nil), attachments...)
	wantSavedAttachments[0].URL = ""
	if savedUser.Type != ModelInputUserMessage || savedUser.Text != input.User ||
		len(savedUser.Attachments) != len(wantSavedAttachments) ||
		savedUser.Attachments[0] != wantSavedAttachments[0] || savedUser.Attachments[1] != wantSavedAttachments[1] {
		t.Fatalf("saved user input=%+v, want attachments=%+v", savedUser, wantSavedAttachments)
	}
}
