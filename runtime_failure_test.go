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
	nextID := 0
	store := &recordingStore{}
	rt, err := NewRuntime(RuntimeConfig{
		Model: streamCallbackModel{}, RunStore: store,
		EventSink: func(event Event) {
			events = append(events, event)
		},
		NewID: func() string {
			nextID++
			if nextID == 1 {
				return "run-stream"
			}
			return fmt.Sprintf("stream-id-%d", nextID)
		},
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

func TestRuntimeRejectsMalformedGeneratedRunIdentityBeforeSideEffects(t *testing.T) {
	for name, generated := range map[string]string{
		"empty":        "",
		"padded":       " generated-run ",
		"invalid-utf8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			store := &recordingStore{}
			var events []Event
			runtime, err := NewRuntime(RuntimeConfig{
				Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
				NewID: func() string { return generated },
			})
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			if _, err := runtime.Run(t.Context(), Input{User: "hello"}); err == nil {
				t.Fatal("Run accepted malformed generated identity")
			}
			store.mu.Lock()
			runs, items := len(store.runs), len(store.items)
			store.mu.Unlock()
			if runs != 0 || items != 0 || len(model.requests) != 0 || len(events) != 0 {
				t.Fatalf("invalid identity crossed a side-effect boundary: runs=%d items=%d requests=%d events=%d", runs, items, len(model.requests), len(events))
			}
		})
	}
}

func TestRuntimeRejectsGeneratedIdentityCollisionBeforeDuplicateAuditOrModelCall(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	store := &recordingStore{}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		NewID: func() string { return "reused-generated-id" },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = runtime.Run(t.Context(), Input{User: "hello"})
	if err == nil || !strings.Contains(err.Error(), "already assigned") {
		t.Fatalf("Run error=%v, want generated identity collision", err)
	}
	store.mu.Lock()
	runs := append([]RunRecord(nil), store.runs...)
	items := append([]ItemRecord(nil), store.items...)
	store.mu.Unlock()
	if len(runs) != 1 || runs[0].ID != "reused-generated-id" || len(items) != 0 || len(model.requests) != 0 {
		t.Fatalf("collision crossed audit/model boundary: runs=%+v items=%+v requests=%d", runs, items, len(model.requests))
	}
}

func TestRuntimeRejectsGeneratedIdentityThatAliasesExplicitRunID(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	store := &recordingStore{}
	ids := []string{"host-run-id", "failure-item-id"}
	next := 0
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		NewID: func() string {
			id := ids[next]
			next++
			return id
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = runtime.Run(t.Context(), Input{RunID: "host-run-id", User: "hello"})
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("Run error=%v, want ErrIdentityConflict", err)
	}
	store.mu.Lock()
	items := append([]ItemRecord(nil), store.items...)
	store.mu.Unlock()
	for _, item := range items {
		if item.ID == "host-run-id" || item.Type == ItemTypeUserMessage {
			t.Fatalf("aliased identity crossed the audit boundary: %+v", items)
		}
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests=%d, want zero", len(model.requests))
	}
}

func TestRuntimeRejectsExplicitRunIdentityWithoutDurableAuthority(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{RunID: "host-run", User: "hello"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("Run error=%v, want ErrIdentityConflict", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests=%d, want zero", len(model.requests))
	}
}

func TestNewRuntimeRejectsCustomIdentityFactoryWithoutDurableAuthority(t *testing.T) {
	runtime, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{},
		NewID: func() string { return "custom-id" },
	})
	if err == nil || !strings.Contains(err.Error(), "custom NewID requires RunStore") {
		t.Fatalf("NewRuntime runtime=%+v error=%v, want explicit durable-authority rejection", runtime, err)
	}
}

func TestRunStoreRejectsGeneratedCreateCollisionWithWaitingRun(t *testing.T) {
	operations := NewOperationRegistry()
	if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "confirm"}, nil
	})
	store := &recordingStore{}
	firstIndex := 0
	first, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{responses: []*ModelResponse{
			callResponse("waiting-response", ToolCall{ID: "waiting-call", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		}},
		Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("executor must not run before approval")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), Approver: &resumableApprover{},
		RunStore: store, Executions: store,
		NewID: func() string {
			firstIndex++
			if firstIndex == 1 {
				return "waiting-generated-run"
			}
			return fmt.Sprintf("waiting-generated-record-%d", firstIndex)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := first.Run(t.Context(), Input{User: "apply", IdempotencyKey: "waiting-request", IdempotencyScope: "tenant"})
	if err != nil || waiting.Status != RunStatusWaitingUser || waiting.RunID != "waiting-generated-run" {
		t.Fatalf("first Run result=%+v error=%v", waiting, err)
	}

	secondModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	legacyAdapter := &legacyBeginRunAdapter{recordingStore: store}
	second, err := NewRuntime(RuntimeConfig{
		Model: secondModel, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("generated create collision must not resume or execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: legacyAdapter, Executions: legacyAdapter,
		NewID: func() string { return "waiting-generated-run" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(t.Context(), Input{User: "unrelated"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("second Run error=%v, want generated create ErrIdentityConflict", err)
	}
	if len(secondModel.requests) != 0 {
		t.Fatalf("second model requests=%d, want zero", len(secondModel.requests))
	}
	if legacyAdapter.legacyCalls != 0 {
		t.Fatalf("legacy BeginRunV2 override calls=%d, want zero because RunStore requires split V4 methods", legacyAdapter.legacyCalls)
	}
}

func TestResumePrecommitRejectionPreservesWaitingAuthorityAndLease(t *testing.T) {
	operations := NewOperationRegistry()
	normalizeCalls := 0
	previewCalls := 0
	writeOperation := operation("apply_change", OperationEffectWrite)
	writeOperation.NormalizeInput = func(arguments any) (any, error) {
		normalizeCalls++
		return arguments, nil
	}
	writeOperation.ApprovalPreview = func(any) (json.RawMessage, error) {
		previewCalls++
		return json.RawMessage(`{"change":"test"}`), nil
	}
	if err := operations.Register(writeOperation); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "confirm"}, nil
	})
	store := &recordingStore{}
	input := Input{
		RunID: "precommit-waiting-run", SessionID: "precommit-waiting-session",
		User: "apply", IdempotencyKey: "precommit-waiting-request",
	}
	first, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{responses: []*ModelResponse{
			callResponse("precommit-waiting-response", ToolCall{ID: "precommit-waiting-call", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		}},
		Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("executor must not run before approval")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), Approver: &resumableApprover{},
		RunStore: store, Executions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := first.Run(t.Context(), input)
	if err != nil || waiting.Status != RunStatusWaitingUser {
		t.Fatalf("waiting result=%+v error=%v", waiting, err)
	}
	normalizeCalls = 0
	previewCalls = 0
	store.mu.Lock()
	beforeRun := store.runs[0]
	beforeSession := store.sessions[input.SessionID]
	beforeGeneration := store.leaseGenerations[input.SessionID]
	beforeApproval := store.pendingApprovals[input.RunID]
	_, beforeLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if beforeLeased {
		t.Fatal("waiting run retained a lease before resume probe")
	}

	adapter := &corruptingResumeStartAdapter{
		recordingStore: store,
		corrupt: func(resumed *ResumedRun) {
			resumed.PendingApprovalDigest = "approval_" + strings.Repeat("0", 64)
		},
	}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	second, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("rejected resume must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: adapter, Executions: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(t.Context(), input); !errors.Is(err, ErrOperationPlanChanged) {
		t.Fatalf("resume error=%v, want ErrOperationPlanChanged", err)
	}
	store.mu.Lock()
	afterRun := store.runs[0]
	afterSession := store.sessions[input.SessionID]
	afterGeneration := store.leaseGenerations[input.SessionID]
	afterApproval := store.pendingApprovals[input.RunID]
	_, afterLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if len(model.requests) != 0 || afterRun.Status != beforeRun.Status ||
		afterRun.PendingApprovalDigest != beforeRun.PendingApprovalDigest ||
		afterSession.Revision != beforeSession.Revision || afterGeneration != beforeGeneration ||
		afterApproval.Digest != beforeApproval.Digest || afterLeased {
		t.Fatalf("pre-commit rejection mutated authority: requests=%d beforeRun=%+v afterRun=%+v beforeSession=%+v afterSession=%+v generations=%d/%d approvals=%q/%q leased=%v",
			len(model.requests), beforeRun, afterRun, beforeSession, afterSession,
			beforeGeneration, afterGeneration, beforeApproval.Digest, afterApproval.Digest, afterLeased)
	}

	retrying := &retryingStartAcceptanceAdapter{recordingStore: store, retryResume: true}
	retryModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run-retry", "bad")}}
	retryRuntime, err := NewRuntime(RuntimeConfig{
		Model: retryModel, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("retried resume acceptance must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: retrying, Executions: retrying,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retryRuntime.Run(t.Context(), input); !errors.Is(err, ErrRunStoreProtocol) {
		t.Fatalf("retried resume acceptance error=%v, want ErrRunStoreProtocol", err)
	}
	store.mu.Lock()
	retriedRun := store.runs[0]
	retriedGeneration := store.leaseGenerations[input.SessionID]
	_, retriedLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if len(retryModel.requests) != 0 || retriedRun.Status != RunStatusWaitingUser ||
		retriedRun.PendingApprovalDigest != beforeRun.PendingApprovalDigest ||
		retriedGeneration != beforeGeneration || retriedLeased {
		t.Fatalf("retried acceptance mutated waiting authority: requests=%d run=%+v generation=%d leased=%v",
			len(retryModel.requests), retriedRun, retriedGeneration, retriedLeased)
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	canceling := &cancelingStartAcceptanceAdapter{recordingStore: store, cancelResume: cancel}
	cancelModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run-cancel", "bad")}}
	var cancelEvents []Event
	cancelRuntime, err := NewRuntime(RuntimeConfig{
		Model: cancelModel, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("cancelled resume acceptance must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: canceling, Executions: canceling,
		EventSink: func(event Event) { cancelEvents = append(cancelEvents, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cancelRuntime.Run(cancelCtx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resume error=%v, want context.Canceled", err)
	}
	store.mu.Lock()
	cancelledRun := store.runs[0]
	cancelledSession := store.sessions[input.SessionID]
	cancelledGeneration := store.leaseGenerations[input.SessionID]
	cancelledApproval := store.pendingApprovals[input.RunID]
	_, cancelledLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if len(cancelModel.requests) != 0 || len(cancelEvents) != 0 || cancelledRun.Status != RunStatusWaitingUser ||
		cancelledRun.PendingApprovalDigest != beforeRun.PendingApprovalDigest ||
		cancelledSession.Revision != beforeSession.Revision || cancelledGeneration != beforeGeneration ||
		cancelledApproval.Digest != beforeApproval.Digest || cancelledLeased {
		t.Fatalf("cancelled acceptance mutated waiting authority: requests=%d events=%d run=%+v session=%+v generation=%d approval=%q leased=%v",
			len(cancelModel.requests), len(cancelEvents), cancelledRun, cancelledSession, cancelledGeneration, cancelledApproval.Digest, cancelledLeased)
	}

	afterCtx, cancelAfter := context.WithCancel(t.Context())
	afterAdapter := &cancelingStartAcceptanceAdapter{
		recordingStore: store, cancelResume: cancelAfter, cancelResumeAfter: true,
	}
	afterRuntime, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("post-callback cancelled resume must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: afterAdapter, Executions: afterAdapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := afterRuntime.Run(afterCtx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-callback cancelled resume error=%v, want context.Canceled", err)
	}
	store.mu.Lock()
	afterCancelledRun := store.runs[0]
	afterCancelledGeneration := store.leaseGenerations[input.SessionID]
	_, afterCancelledLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if afterCancelledRun.Status != RunStatusWaitingUser ||
		afterCancelledRun.PendingApprovalDigest != beforeRun.PendingApprovalDigest ||
		afterCancelledGeneration != beforeGeneration || afterCancelledLeased {
		t.Fatalf("post-callback cancellation mutated waiting authority: run=%+v generation=%d leased=%v",
			afterCancelledRun, afterCancelledGeneration, afterCancelledLeased)
	}

	concurrent := &concurrentStartAcceptanceAdapter{recordingStore: store, concurrentResume: true}
	concurrentModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run-concurrent", "bad")}}
	concurrentRuntime, err := NewRuntime(RuntimeConfig{
		Model: concurrentModel, Operations: operations, Policy: policy,
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("concurrent resume acceptance must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
		RunStore: concurrent, Executions: concurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := concurrentRuntime.Run(t.Context(), input); !errors.Is(err, ErrRunStoreProtocol) {
		t.Fatalf("concurrent resume acceptance error=%v, want ErrRunStoreProtocol", err)
	}
	store.mu.Lock()
	concurrentRun := store.runs[0]
	concurrentSession := store.sessions[input.SessionID]
	concurrentGeneration := store.leaseGenerations[input.SessionID]
	concurrentApproval := store.pendingApprovals[input.RunID]
	_, concurrentLeased := store.leases[input.SessionID]
	store.mu.Unlock()
	if len(concurrentModel.requests) != 0 || concurrentRun.Status != beforeRun.Status ||
		concurrentRun.PendingApprovalDigest != beforeRun.PendingApprovalDigest ||
		concurrentSession.Revision != beforeSession.Revision || concurrentGeneration != beforeGeneration ||
		concurrentApproval.Digest != beforeApproval.Digest || concurrentLeased {
		t.Fatalf("concurrent resume mutated authority: requests=%d run=%+v session=%+v generation=%d approval=%q leased=%v",
			len(concurrentModel.requests), concurrentRun, concurrentSession, concurrentGeneration, concurrentApproval.Digest, concurrentLeased)
	}
	if normalizeCalls != 0 || previewCalls != 0 {
		t.Fatalf("pre-commit acceptance invoked host callbacks: normalize=%d preview=%d", normalizeCalls, previewCalls)
	}
}

func TestPendingApprovalCompleteAuthorityRejectsMutationBeforeResumeCommit(t *testing.T) {
	mutations := []struct {
		name            string
		mutate          func(*PendingApprovalCommit)
		recomputeLegacy bool
	}{
		{name: "input user", mutate: func(pending *PendingApprovalCommit) { pending.Request.Operation.Input.User = "changed" }},
		{name: "normalized arguments", mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.Arguments = map[string]any{"unexpected": true}
		}},
		{name: "approval reason", mutate: func(pending *PendingApprovalCommit) { pending.Request.Reason = "changed reason" }},
		{name: "operation summary", mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.Operation.Description = "changed summary"
		}},
		{name: "originating lease", mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.SessionLease.RunID = "another-run"
		}},
		{name: "attempt injection", mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.AttemptID = "injected-attempt"
		}},
		{name: "audit field", mutate: func(pending *PendingApprovalCommit) { pending.Audit.RequestID = "injected-request" }},
		{name: "unversioned subset authority", mutate: func(pending *PendingApprovalCommit) { pending.AuthorityVersion = 0 }},
		{name: "legacy input user", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.Input.User = "changed"
		}},
		{name: "legacy normalized arguments", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.Arguments = map[string]any{"unexpected": true}
		}},
		{name: "legacy operation summary", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.Operation.Description = "changed summary"
		}},
		{name: "legacy originating lease", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.SessionLease.RunID = "another-run"
		}},
		{name: "legacy attempt injection", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Request.Operation.AttemptID = "injected-attempt"
		}},
		{name: "legacy audit field", recomputeLegacy: true, mutate: func(pending *PendingApprovalCommit) {
			pending.Audit.RequestID = "injected-request"
		}},
	}

	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			operations := NewOperationRegistry()
			if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
				t.Fatal(err)
			}
			policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
				return PolicyDecision{Action: PolicyRequireApproval, Reason: "confirm"}, nil
			})
			store := &recordingStore{}
			input := Input{
				RunID: "complete-authority-run", SessionID: "complete-authority-session",
				User: "apply", IdempotencyKey: "complete-authority-request",
			}
			first, err := NewRuntime(RuntimeConfig{
				Model: &scriptedModel{responses: []*ModelResponse{
					callResponse("complete-authority-response", ToolCall{ID: "complete-authority-call", Name: "apply_change", Input: json.RawMessage(`{}`)}),
				}},
				Operations: operations, Policy: policy,
				Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
					t.Fatal("pending operation executed")
					return OperationResult{}, nil
				}),
				Verifier: confirmingVerifier(), Approver: &resumableApprover{}, RunStore: store, Executions: store,
			})
			if err != nil {
				t.Fatal(err)
			}
			waiting, err := first.Run(t.Context(), input)
			if err != nil || waiting == nil || waiting.Status != RunStatusWaitingUser {
				t.Fatalf("waiting result=%+v error=%v", waiting, err)
			}

			store.mu.Lock()
			beforeRun := store.runs[0]
			beforeSession := store.sessions[input.SessionID]
			beforeApproval := store.pendingApprovals[input.RunID]
			beforeGeneration := store.leaseGenerations[input.SessionID]
			store.mu.Unlock()
			beforeJSON, err := json.Marshal([]any{beforeRun, beforeSession, beforeApproval, beforeGeneration})
			if err != nil {
				t.Fatal(err)
			}

			var mutationErr error
			adapter := &corruptingResumeStartAdapter{recordingStore: store, corrupt: func(resumed *ResumedRun) {
				if resumed.PendingApproval == nil {
					mutationErr = errors.New("missing pending approval")
					return
				}
				if test.recomputeLegacy {
					var digest string
					digest, mutationErr = legacyPendingApprovalAuthorityDigest(*resumed.PendingApproval)
					if mutationErr == nil {
						resumed.PendingApproval.Digest = digest
						resumed.PendingApprovalDigest = digest
					}
				}
				test.mutate(resumed.PendingApproval)
			}}
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			second, err := NewRuntime(RuntimeConfig{
				Model: model, Operations: operations, Policy: policy,
				Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
					t.Fatal("corrupt approval executed")
					return OperationResult{}, nil
				}),
				Verifier: confirmingVerifier(), ApprovalResumer: &persistedApprovalResumer{store: store},
				RunStore: adapter, Executions: adapter,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := second.Run(t.Context(), input); result != nil || !errors.Is(err, ErrOperationPlanChanged) {
				t.Fatalf("resume result=%+v error=%v, want ErrOperationPlanChanged", result, err)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			store.mu.Lock()
			afterRun := store.runs[0]
			afterSession := store.sessions[input.SessionID]
			afterApproval := store.pendingApprovals[input.RunID]
			afterGeneration := store.leaseGenerations[input.SessionID]
			_, leased := store.leases[input.SessionID]
			store.mu.Unlock()
			afterJSON, err := json.Marshal([]any{afterRun, afterSession, afterApproval, afterGeneration})
			if err != nil {
				t.Fatal(err)
			}
			if string(afterJSON) != string(beforeJSON) || leased || len(model.requests) != 0 {
				t.Fatalf("corrupt authority mutated state: before=%s after=%s leased=%v requests=%d",
					beforeJSON, afterJSON, leased, len(model.requests))
			}
		})
	}
}

func TestRuntimeRejectsInvalidRunStartBeforeCommit(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		store *invalidPrecommitStartStore
		want  error
	}{
		{
			name: "created handle names another run", input: Input{User: "generated"},
			store: &invalidPrecommitStartStore{start: RunStart{Handle: RunHandle{RunID: "other-run"}}},
		},
		{
			name: "create callback error then nil", input: Input{User: "generated"},
			store: &invalidPrecommitStartStore{
				start: RunStart{Handle: RunHandle{RunID: "other-run"}}, ignoreCallbackError: true,
			},
		},
		{
			name: "resume omits waiting authority", input: Input{RunID: "explicit-resume", User: "explicit"},
			store: &invalidPrecommitStartStore{resume: true}, want: ErrOperationPlanChanged,
		},
		{
			name: "resume callback error then nil", input: Input{RunID: "explicit-resume", User: "explicit"},
			store: &invalidPrecommitStartStore{
				resume: true, digest: "not-an-approval-digest", ignoreCallbackError: true,
			},
			want: ErrOperationPlanChanged,
		},
		{
			name: "create omits pre-commit acceptance", input: Input{User: "generated"},
			store: &invalidPrecommitStartStore{omit: true}, want: ErrRunStoreProtocol,
		},
		{
			name: "resume omits pre-commit acceptance", input: Input{RunID: "explicit-resume", User: "explicit"},
			store: &invalidPrecommitStartStore{resume: true, omit: true}, want: ErrRunStoreProtocol,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			runtime, err := NewRuntime(RuntimeConfig{Model: model, RunStore: test.store})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runtime.Run(t.Context(), test.input)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Run error=%v, want %v", err, test.want)
			}
			if test.want == nil && (err == nil || !strings.Contains(err.Error(), "handle for run")) {
				t.Fatalf("Run error=%v, want mismatched handle rejection", err)
			}
			if len(model.requests) != 0 || len(test.store.runs) != 0 || len(test.store.items) != 0 {
				t.Fatalf("invalid pre-commit start crossed boundary: requests=%d runs=%d items=%d", len(model.requests), len(test.store.runs), len(test.store.items))
			}
		})
	}
}

func TestCreateRunPrecommitRejectsRetryMalformedAuthorityAndCancellation(t *testing.T) {
	t.Run("concurrent duplicate callback", func(t *testing.T) {
		base := &recordingStore{}
		store := &concurrentStartAcceptanceAdapter{recordingStore: base, concurrentCreate: true}
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
		var events []Event
		runtime, err := NewRuntime(RuntimeConfig{
			Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(t.Context(), Input{User: "hello", SessionID: "concurrent-create-session"}); !errors.Is(err, ErrRunStoreProtocol) {
			t.Fatalf("Run error=%v, want concurrent callback rejection", err)
		}
		if len(model.requests) != 0 || len(events) != 0 || len(base.runs) != 0 ||
			len(base.sessions) != 0 || len(base.leases) != 0 || len(base.leaseGenerations) != 0 {
			t.Fatalf("concurrent create mutated state: requests=%d events=%d runs=%+v sessions=%+v leases=%+v generations=%+v",
				len(model.requests), len(events), base.runs, base.sessions, base.leases, base.leaseGenerations)
		}
	})

	t.Run("invalid then valid callback", func(t *testing.T) {
		store := &retryingStartAcceptanceAdapter{recordingStore: &recordingStore{}, retryCreate: true}
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
		runtime, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(t.Context(), Input{User: "hello"}); !errors.Is(err, ErrRunStoreProtocol) {
			t.Fatalf("Run error=%v, want callback cardinality rejection", err)
		}
		if len(model.requests) != 0 || len(store.runs) != 0 || len(store.items) != 0 {
			t.Fatalf("retried create acceptance mutated state: requests=%d runs=%+v items=%+v", len(model.requests), store.runs, store.items)
		}
	})

	t.Run("cancellation during callback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		store := &cancelingStartAcceptanceAdapter{recordingStore: &recordingStore{}, cancelCreate: cancel}
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
		var events []Event
		runtime, err := NewRuntime(RuntimeConfig{
			Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(ctx, Input{User: "hello"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v, want context.Canceled", err)
		}
		if len(model.requests) != 0 || len(events) != 0 || len(store.runs) != 0 || len(store.items) != 0 {
			t.Fatalf("cancelled create acceptance crossed boundary: requests=%d events=%d runs=%+v items=%+v", len(model.requests), len(events), store.runs, store.items)
		}
	})

	t.Run("explicit create cancellation after callback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		store := &cancelingStartAcceptanceAdapter{
			recordingStore: &recordingStore{}, cancelCreate: cancel, cancelCreateAfter: true,
		}
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
		var events []Event
		runtime, err := NewRuntime(RuntimeConfig{
			Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(ctx, Input{RunID: "cancelled-explicit-create", User: "hello"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v, want context.Canceled", err)
		}
		if len(model.requests) != 0 || len(events) != 0 || len(store.runs) != 0 || len(store.items) != 0 {
			t.Fatalf("post-callback cancellation crossed boundary: requests=%d events=%d runs=%+v items=%+v", len(model.requests), len(events), store.runs, store.items)
		}
	})

	tests := []struct {
		name      string
		sessionID string
		corrupt   func(*RunStart)
	}{
		{
			name: "substituted stateful lease id", sessionID: "lease-substitution-session",
			corrupt: func(start *RunStart) { start.Handle.LeaseID = "different-lease" },
		},
		{
			name: "expired stateful lease", sessionID: "expired-lease-session",
			corrupt: func(start *RunStart) { start.Handle.LeaseDeadline = time.Unix(1, 0) },
		},
		{
			name: "padded stateful lease id", sessionID: "padded-lease-session",
			corrupt: func(start *RunStart) { start.Handle.LeaseID = " padded-lease " },
		},
		{
			name: "stateless lease authority",
			corrupt: func(start *RunStart) {
				start.Handle.LeaseID = "injected-lease"
				start.Handle.LeaseGeneration = 1
				start.Handle.LeaseDeadline = time.Now().Add(time.Minute)
			},
		},
		{
			name: "stateless session injection",
			corrupt: func(start *RunStart) {
				start.Session = &SessionState{ID: "", Transcript: []ModelInputItem{{Type: ModelInputUserMessage, Text: "injected"}}}
			},
		},
		{
			name: "revision zero session replay", sessionID: "revision-zero-replay-session",
			corrupt: func(start *RunStart) {
				start.Session = &SessionState{
					ID:         "revision-zero-replay-session",
					Transcript: []ModelInputItem{{Type: ModelInputUserMessage, Text: "injected"}},
				}
			},
		},
		{
			name: "modern last response mismatch", sessionID: "modern-response-session",
			corrupt: func(start *RunStart) {
				start.Handle.SessionRevision = 1
				start.Session = &SessionState{
					ID: "modern-response-session", Revision: 1,
					Transcript: []ModelInputItem{
						{Type: ModelInputUserMessage, Text: "earlier"},
						{
							Type: ModelInputAssistantOutput, Text: "answer", ResponseID: "actual-last-response",
							OutputType: ModelOutputMessage, Raw: json.RawMessage(`{"id":"modern-response-item"}`),
						},
					},
					LastResponseID: "injected-last-response",
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := &recordingStore{}
			store := &corruptingCreateStartAdapter{recordingStore: base, corrupt: test.corrupt}
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			runtime, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Run(t.Context(), Input{User: "hello", SessionID: test.sessionID}); err == nil {
				t.Fatal("Run accepted malformed proposed start authority")
			}
			if len(model.requests) != 0 || len(base.runs) != 0 || len(base.sessions) != 0 || len(base.leases) != 0 || len(base.leaseGenerations) != 0 {
				t.Fatalf("malformed start mutated state: requests=%d runs=%+v sessions=%+v leases=%+v generations=%+v", len(model.requests), base.runs, base.sessions, base.leases, base.leaseGenerations)
			}
		})
	}
}

func TestExplicitRunFallbackSharesLateResumeCallbackProtocol(t *testing.T) {
	base := &recordingStore{}
	store := newLateResumeFallbackAdapter(base)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	var events []Event
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{RunID: "late-resume-fallback", User: "hello"}); !errors.Is(err, ErrRunStoreProtocol) {
		t.Fatalf("Run error=%v, want shared callback protocol rejection", err)
	}
	if len(model.requests) != 0 || len(events) != 0 || len(base.runs) != 0 || len(base.items) != 0 {
		t.Fatalf("late resume callback crossed fallback boundary: requests=%d events=%d runs=%+v items=%+v",
			len(model.requests), len(events), base.runs, base.items)
	}
}

func TestRunStartRevalidatesLeaseAfterStoreReturn(t *testing.T) {
	base := &recordingStore{}
	store := &delayingCreateReturnAdapter{recordingStore: base, delay: 25 * time.Millisecond}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	var events []Event
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		SessionLeaseTTL: 5 * time.Millisecond, LeaseRenewalInterval: time.Millisecond,
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "hello", SessionID: "post-return-expiry-session"}); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("Run error=%v, want post-return ErrSessionLeaseLost", err)
	}
	if len(model.requests) != 0 || len(events) != 0 || len(base.items) != 0 {
		t.Fatalf("expired returned lease crossed first-work boundary: requests=%d events=%d items=%+v",
			len(model.requests), len(events), base.items)
	}
}

func TestRunStartRejectsExpiredLeaseAtAcceptanceCompletion(t *testing.T) {
	base := &recordingStore{}
	store := &delayingCreateAcceptanceAdapter{recordingStore: base, delay: 25 * time.Millisecond}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	var events []Event
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		SessionLeaseTTL: 5 * time.Millisecond, LeaseRenewalInterval: time.Millisecond,
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "hello", SessionID: "delayed-lease-session"}); err == nil ||
		(!errors.Is(err, ErrSessionConflict) && !strings.Contains(err.Error(), "expired session lease")) {
		t.Fatalf("Run error=%v, want expired pre-commit lease rejection", err)
	}
	if len(model.requests) != 0 || len(events) != 0 || len(base.runs) != 0 ||
		len(base.sessions) != 0 || len(base.leases) != 0 || len(base.leaseGenerations) != 0 {
		t.Fatalf("expired acceptance mutated state: requests=%d events=%d runs=%+v sessions=%+v leases=%+v generations=%+v",
			len(model.requests), len(events), base.runs, base.sessions, base.leases, base.leaseGenerations)
	}
}

func TestExplicitRunCreateFallbackRejectsAmbiguousNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "joined conflict", err: errors.Join(ErrRunNotFound, ErrSessionConflict)},
		{name: "joined cancellation", err: errors.Join(ErrRunNotFound, context.Canceled)},
		{name: "joined timeout", err: errors.Join(ErrRunNotFound, errors.New("persistence timeout"))},
		{name: "wrapped absence", err: fmt.Errorf("lookup failed: %w", ErrRunNotFound)},
		{name: "custom is absence", err: customRunNotFoundError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := &recordingStore{}
			store := &ambiguousResumeStore{recordingStore: base, err: test.err}
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			var events []Event
			runtime, err := NewRuntime(RuntimeConfig{
				Model: model, RunStore: store, EventSink: func(event Event) { events = append(events, event) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Run(t.Context(), Input{RunID: "ambiguous-explicit-run", User: "hello"}); err == nil || !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("Run error=%v, want original ambiguous absence error", err)
			}
			if store.createCalls != 0 || len(model.requests) != 0 || len(events) != 0 || len(base.runs) != 0 || len(base.items) != 0 {
				t.Fatalf("ambiguous absence crossed create boundary: creates=%d requests=%d events=%d runs=%+v items=%+v",
					store.createCalls, len(model.requests), len(events), base.runs, base.items)
			}
		})
	}
}

func TestRunStoreRejectsRepeatedGeneratedRunIdentityAcrossRuntimeRestart(t *testing.T) {
	store := &recordingStore{}
	newFactory := func() func() string {
		index := 0
		return func() string {
			index++
			if index == 1 {
				return "repeated-generated-run"
			}
			return fmt.Sprintf("generated-record-%d", index)
		}
	}
	first, err := NewRuntime(RuntimeConfig{
		Model:    &scriptedModel{responses: []*ModelResponse{messageResponse("first-response", "done")}},
		RunStore: store, NewID: newFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(t.Context(), Input{User: "first"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	secondModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	second, err := NewRuntime(RuntimeConfig{Model: secondModel, RunStore: store, NewID: newFactory()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(t.Context(), Input{User: "second"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("second Run error=%v, want ErrIdentityConflict", err)
	}
	if len(secondModel.requests) != 0 {
		t.Fatalf("second model requests=%d, want zero", len(secondModel.requests))
	}
}

func TestRuntimeRejectsNoncanonicalExplicitRunIdentityBeforeSideEffects(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	store := &recordingStore{}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		NewID: func() string { return "unused-generated-id" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{RunID: " host-run ", User: "hello"}); err == nil || !strings.Contains(err.Error(), "invalid runtime identity") {
		t.Fatalf("Run error=%v, want noncanonical identity rejection", err)
	}
	if len(model.requests) != 0 || len(store.runs) != 0 || len(store.items) != 0 {
		t.Fatalf("noncanonical identity crossed side-effect boundary: requests=%d runs=%d items=%d", len(model.requests), len(store.runs), len(store.items))
	}
}

func TestRuntimeRejectsCompletedGeneratedRunIdentityReuse(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("response-write", ToolCall{ID: "call-write", Name: "apply_change", Input: json.RawMessage(`{}`)}),
		messageResponse("response-done", "done"),
		messageResponse("must-not-run", "bad"),
	}}
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	executorCalls := 0
	next := 0
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: ops, RunStore: store, Executions: store,
		Policy: allowPolicy(),
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			executorCalls++
			return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
		}),
		Verifier: confirmingVerifier(),
		NewID: func() string {
			next++
			if next == 1 {
				return "shared-write-run"
			}
			return fmt.Sprintf("shared-write-id-%d", next)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Run(t.Context(), Input{User: "first", IdempotencyKey: "write-one", IdempotencyScope: "tenant"})
	if err != nil || first.RunID != "shared-write-run" || first.Status != RunStatusCompleted {
		t.Fatalf("first Run result=%+v error=%v", first, err)
	}
	if _, err := runtime.Run(t.Context(), Input{RunID: "shared-write-run", User: "second", IdempotencyKey: "write-two", IdempotencyScope: "tenant"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("second Run error=%v, want ErrIdentityConflict", err)
	}
	if executorCalls != 1 || len(model.requests) != 2 {
		t.Fatalf("executor calls=%d model requests=%d, want one write and no second-run model call", executorCalls, len(model.requests))
	}
}

func TestRuntimeIdentityClaimsRetireAfterTerminalCalls(t *testing.T) {
	const runs = 1000
	responses := make([]*ModelResponse, runs)
	for index := range responses {
		responses[index] = messageResponse(fmt.Sprintf("bounded-response-%d", index), "done")
	}
	next := 0
	store := &recordingStore{}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{responses: responses}, RunStore: store,
		NewID: func() string {
			next++
			return fmt.Sprintf("bounded-id-%d", next)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < runs; index++ {
		if _, err := runtime.Run(t.Context(), Input{User: "bounded"}); err != nil {
			t.Fatalf("Run %d: %v", index, err)
		}
	}
	claimsAfterSuccess := runtimeIdentityClaimCount(runtime)
	if claimsAfterSuccess != 0 {
		t.Fatalf("identity claims after %d completed runs=%d, want zero", runs, claimsAfterSuccess)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "expected failure"}); err == nil {
		t.Fatal("Run with exhausted model unexpectedly succeeded")
	}
	claimsAfterFailure := runtimeIdentityClaimCount(runtime)
	if claimsAfterFailure != 0 {
		t.Fatalf("identity claims after failed run=%d, want zero", claimsAfterFailure)
	}
}

func runtimeIdentityClaimCount(runtime *Runtime) int {
	runtime.identityMu.Lock()
	defer runtime.identityMu.Unlock()
	return len(runtime.assignedIdentities)
}

func TestRunStoreRejectsDuplicateDurableIdentityAcrossRuntimeRestart(t *testing.T) {
	newFactory := func() func() string {
		next := 0
		return func() string {
			next++
			return fmt.Sprintf("restarted-%d", next)
		}
	}
	store := &recordingStore{}
	firstModel := &scriptedModel{responses: []*ModelResponse{messageResponse("response-first", "first")}}
	first, err := NewRuntime(RuntimeConfig{Model: firstModel, RunStore: store, NewID: newFactory()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(t.Context(), Input{RunID: "run-first", User: "first"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	secondModel := &scriptedModel{responses: []*ModelResponse{messageResponse("response-second", "second")}}
	second, err := NewRuntime(RuntimeConfig{Model: secondModel, RunStore: store, NewID: newFactory()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Run(t.Context(), Input{RunID: "run-second", User: "second"})
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("second Run error=%v, want ErrIdentityConflict", err)
	}
	if len(secondModel.requests) != 0 {
		t.Fatalf("second model requests=%d, want zero", len(secondModel.requests))
	}
	store.mu.Lock()
	items := append([]ItemRecord(nil), store.items...)
	store.mu.Unlock()
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, duplicate := seen[item.ID]; duplicate {
			t.Fatalf("duplicate durable item identity persisted: %+v", items)
		}
		seen[item.ID] = struct{}{}
	}
}

func TestRunStoreRejectsRunIdentityPreviouslyAssignedToItem(t *testing.T) {
	store := &recordingStore{}
	first, err := NewRuntime(RuntimeConfig{
		Model:    &scriptedModel{responses: []*ModelResponse{messageResponse("response-first", "first")}},
		RunStore: store,
		NewID: func() func() string {
			next := 0
			return func() string {
				next++
				return fmt.Sprintf("durable-%d", next)
			}
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(t.Context(), Input{RunID: "run-first", User: "first"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	secondModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "second")}}
	second, err := NewRuntime(RuntimeConfig{
		Model: secondModel, RunStore: store,
		NewID: func() string { return "unused-second-id" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(t.Context(), Input{RunID: "durable-1", User: "second"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("second Run error=%v, want ErrIdentityConflict", err)
	}
	if len(secondModel.requests) != 0 {
		t.Fatalf("second model requests=%d, want zero", len(secondModel.requests))
	}
}

func TestRuntimeDoesNotHoldIdentityStateLockWhileCallingNewID(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("response-inner", "inner"),
		messageResponse("response-outer", "outer"),
	}}
	var runtime *Runtime
	next := 0
	reentered := false
	var nestedErr error
	var err error
	runtime, err = NewRuntime(RuntimeConfig{
		Model: model, RunStore: &recordingStore{},
		NewID: func() string {
			next++
			id := fmt.Sprintf("reentrant-%d", next)
			if !reentered {
				reentered = true
				_, nestedErr = runtime.Run(t.Context(), Input{User: "nested"})
			}
			return id
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(t.Context(), Input{User: "outer"})
		done <- runErr
	}()
	select {
	case runErr := <-done:
		if runErr != nil || nestedErr != nil {
			t.Fatalf("outer error=%v nested error=%v", runErr, nestedErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NewID callback deadlocked while reentering Runtime.Run")
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
	var projection func(any) ([]TerminalSessionProjection, error)
	if effect == OperationEffectWrite {
		confirmation = ConfirmationSpec{Mode: ConfirmationRequired, Description: "Confirm the requested state change is observable."}
		preview = func(any) (json.RawMessage, error) { return json.RawMessage(`{"change":"test"}`), nil }
		projection = func(any) ([]TerminalSessionProjection, error) { return nil, nil }
	}
	return Operation{
		Name: name, ContractVersion: "test-v1", Description: name, Effect: effect, Confirmation: confirmation,
		ApprovalPreview: preview, ProjectTerminalSession: projection,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
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
