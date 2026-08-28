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

func TestNormalizeRuntimeInputPreservesUserAndRejectsNonCanonicalIdentities(t *testing.T) {
	input := Input{
		RunID:            "run-canonical",
		User:             "  preserve this prompt exactly\n",
		SessionID:        "session-canonical",
		IdempotencyKey:   "request-canonical",
		IdempotencyScope: "ignored-for-session",
	}
	normalized, err := normalizeRuntimeInput(input)
	if err != nil {
		t.Fatalf("normalizeRuntimeInput: %v", err)
	}
	if normalized.User != input.User {
		t.Fatalf("user=%q, want exact caller text %q", normalized.User, input.User)
	}

	for _, test := range []struct {
		name  string
		input Input
	}{
		{name: "run id", input: Input{RunID: " run", User: "prompt"}},
		{name: "session id", input: Input{SessionID: " session", User: "prompt"}},
		{name: "idempotency key", input: Input{IdempotencyKey: "request ", User: "prompt"}},
		{name: "idempotency scope", input: Input{IdempotencyScope: " tenant ", User: "prompt"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeRuntimeInput(test.input); err == nil || !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("normalizeRuntimeInput error=%v, want canonical identity rejection", err)
			}
		})
	}

	if _, err := normalizeRuntimeInput(Input{User: " \t\n "}); err == nil {
		t.Fatal("normalizeRuntimeInput accepted whitespace-only user text")
	}
}

func TestRuntimeStartsLeaseRenewalBeforeSynchronousEvents(t *testing.T) {
	store := &renewalSignalStore{renewed: make(chan struct{})}
	store.now = time.Now
	renewedDuringStartEvent := false
	runtime, err := NewRuntime(RuntimeConfig{
		Model:    &scriptedModel{responses: []*ModelResponse{messageResponse("lease-event-response", "done")}},
		RunStore: store,
		EventSink: func(event Event) {
			if event.Type != EventRunStarted {
				return
			}
			select {
			case <-store.renewed:
				renewedDuringStartEvent = true
			case <-time.After(200 * time.Millisecond):
			}
		},
		SessionLeaseTTL:      time.Second,
		LeaseRenewalInterval: 10 * time.Millisecond,
		Now:                  time.Now,
		NewID: func() string {
			return fmt.Sprintf("lease-event-id-%d", testRuntimeIdentitySequence.Add(1))
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	result, err := runtime.Run(context.Background(), Input{SessionID: "lease-event-session", User: "prompt"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil || result.Status != RunStatusCompleted {
		t.Fatalf("result=%+v, want completed", result)
	}
	if !renewedDuringStartEvent {
		t.Fatal("lease renewal was not active while EventRunStarted blocked")
	}
}

type failingFreshApprovalResumer struct {
	calls int
	err   error
}

func (resumer *failingFreshApprovalResumer) ResumeApproval(context.Context, string) (*ApprovalResume, error) {
	resumer.calls++
	return nil, resumer.err
}

func TestFreshRunDoesNotContactApprovalResumerWithoutPendingDigest(t *testing.T) {
	resumeErr := errors.New("approval backend unavailable")
	resumer := &failingFreshApprovalResumer{err: resumeErr}
	runtime, err := NewRuntime(RuntimeConfig{
		Model:           &scriptedModel{responses: []*ModelResponse{messageResponse("fresh-response", "done")}},
		ApprovalResumer: resumer,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	result, err := runtime.Run(context.Background(), Input{User: "fresh prompt"})
	if err != nil {
		t.Fatalf("Run contacted an irrelevant approval backend: %v", err)
	}
	if result == nil || result.Output != "done" {
		t.Fatalf("result=%+v, want completed output", result)
	}
	if resumer.calls != 0 {
		t.Fatalf("ResumeApproval calls=%d, want 0", resumer.calls)
	}
}

func TestRuntimePreservesExactModelResponseText(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *ModelResponse
		want     string
	}{
		{name: "output", response: messageResponse("spaced-output", "  answer\n"), want: "  answer\n"},
		{name: "refusal", response: refusalResponse("spaced-refusal", "\trefused  "), want: "\trefused  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newTestRuntime(t, &scriptedModel{responses: []*ModelResponse{test.response}}, nil, nil, nil, nil, nil, nil)
			result, err := runtime.Run(context.Background(), Input{User: "prompt"})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Output != test.want {
				t.Fatalf("output=%q, want exact model text %q", result.Output, test.want)
			}
		})
	}
}

func TestRuntimePreservesExactTerminalOperationResponse(t *testing.T) {
	const finalResponse = "  terminal response\n"
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("terminal-response", ToolCall{ID: "terminal-call", Name: "finish_exact", Input: json.RawMessage(`{}`)}),
	}}
	operations := NewOperationRegistry()
	terminal := operation("finish_exact", OperationEffectRead)
	terminal.Terminal = true
	if err := operations.Register(terminal); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	runtime := newTestRuntime(
		t, model, operations, allowPolicy(),
		OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{Output: json.RawMessage(`{"done":true}`), FinalResponse: finalResponse}, nil
		}),
		nil, nil, store,
	)

	result, err := runtime.Run(t.Context(), Input{User: "finish exactly"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != finalResponse {
		t.Fatalf("output=%q, want exact terminal response %q", result.Output, finalResponse)
	}
	if len(store.completed) != 1 || store.completed[0].Result != finalResponse {
		t.Fatalf("completed runs=%+v, want exact terminal response", store.completed)
	}
	if equalOperationResult(
		OperationResult{Output: json.RawMessage(`{"done":true}`), FinalResponse: finalResponse},
		OperationResult{Output: json.RawMessage(`{"done":true}`), FinalResponse: strings.TrimSpace(finalResponse)},
	) {
		t.Fatal("operation result identity ignored final response whitespace")
	}
}

func TestRuntimeRejectsSessionRevisionOverflowBeforeModelCall(t *testing.T) {
	const sessionID = "maximum-revision-session"
	maximumRevision := ^uint64(0)
	store := &recordingStore{sessions: map[string]SessionState{
		sessionID: {ID: sessionID, ModelBindingID: defaultTestModelBindingID(), Revision: maximumRevision},
	}}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("unexpected-response", "done")}}
	runtime := newTestRuntime(t, model, nil, nil, nil, nil, nil, store)

	result, err := runtime.Run(context.Background(), Input{SessionID: sessionID, User: "prompt"})
	if result != nil || err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("result=%+v error=%v, want explicit revision overflow rejection", result, err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model calls=%d, want 0 before rejecting revision overflow", len(model.requests))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions[sessionID].Revision != maximumRevision || len(store.runs) != 0 {
		t.Fatalf("store mutated before overflow rejection: session=%+v runs=%d", store.sessions[sessionID], len(store.runs))
	}
}
