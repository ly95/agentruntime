package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestRuntimeRollsBackSuccessfulCompactionWhenModelFails(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 101, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "tentative checkpoint"}, nil
	}}
	store := &recordingStore{}
	original := seedContextSession(store, "rollback-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, []string{"historical-call"})
	runtime := newContextRuntimeForTest(t, failingModel{err: errors.New("model failed")}, store, contextWindowForTest(counter, compactor), nil)

	if _, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "rollback-session"}); err == nil {
		t.Fatal("Run unexpectedly succeeded")
	}
	after := store.sessions["rollback-session"]
	if after.Checkpoint != nil || !cmp.Equal(after.Transcript, original.Transcript) || !cmp.Equal(after.SeenCallIDs, original.SeenCallIDs) {
		t.Fatalf("tentative compaction leaked into failed session: %+v", after)
	}
	checkpointItems := 0
	for _, item := range store.items {
		if item.Type == ItemTypeContextCheckpoint {
			checkpointItems++
		}
	}
	if checkpointItems != 1 {
		t.Fatalf("successful compaction audit items=%d, want 1", checkpointItems)
	}
}

func TestRuntimeDoesNotApplyCompactionWhenCheckpointAuditWriteFails(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 101, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "must not be applied"}, nil
	}}
	store := &appendFailingStore{failType: ItemTypeContextCheckpoint, err: errors.New("checkpoint audit unavailable")}
	before := seedContextSession(&store.recordingStore, "audit-failure-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, nil)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	var events []Event
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), func(event Event) { events = append(events, event) })

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "audit-failure-session"})
	if !errors.Is(err, ErrContextCompactionFailed) || !strings.Contains(err.Error(), "checkpoint audit unavailable") {
		t.Fatalf("Run error=%v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called after checkpoint audit failure")
	}
	if after := store.sessions["audit-failure-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after checkpoint audit failure:\nbefore=%+v\nafter=%+v", before, after)
	}
	started, failed, completed := false, false, false
	for _, event := range events {
		started = started || event.Type == EventContextCompactionStarted
		failed = failed || event.Type == EventContextCompactionFailed
		completed = completed || event.Type == EventContextCompactionCompleted
	}
	if !started || !failed || completed {
		t.Fatalf("checkpoint audit failure lifecycle events=%+v", events)
	}
}

func TestRuntimeRollsBackCompactionWhenSessionCommitFails(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 101, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "tentative commit"}, nil
	}}
	store := &completeFailingStore{err: errors.New("session commit unavailable")}
	original := seedContextSession(&store.recordingStore, "commit-failure-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, []string{"historical-call"})
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("successful-model-response", "done")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "commit-failure-session"})
	if err == nil || !strings.Contains(err.Error(), "session commit unavailable") {
		t.Fatalf("Run error=%v", err)
	}
	after := store.sessions["commit-failure-session"]
	if after.Checkpoint != nil || !cmp.Equal(after.Transcript, original.Transcript) || !cmp.Equal(after.SeenCallIDs, original.SeenCallIDs) {
		t.Fatalf("tentative compaction leaked after session commit failure: %+v", after)
	}
	if len(model.requests) != 1 || !requestHasCheckpoint(model.requests[0]) {
		t.Fatalf("model did not receive the tentative compacted request: %+v", model.requests)
	}
}

func TestRuntimeRejectsCompactedItemCountOverflowWithoutChangingSession(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 100, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "replacement"}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "overflow-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, nil)
	before.Checkpoint = &ContextCheckpoint{
		Version:            contextCheckpointVersion,
		Summary:            ContextSummary{Summary: "existing"},
		CompactedItemCount: int(^uint(0) >> 1), SourceSessionRevision: 2,
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	store.sessions["overflow-session"] = before
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "overflow-session"})
	if !errors.Is(err, ErrContextCompactionFailed) || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("Run error=%v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called after compacted item count overflow")
	}
	if after := store.sessions["overflow-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after compacted item count overflow:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestNewRuntimeValidatesContextWindowBeforeFreezingRegistry(t *testing.T) {
	registry := NewOperationRegistry()
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 1, nil },
		countText:    func(string) (int, error) { return 1, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "summary"}, nil
	}}
	window := contextWindowForTest(counter, compactor)
	window.CompactionTargetTokens = window.CompactionTriggerTokens
	_, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Operations: registry,
		ContextWindow: window,
	})
	if err == nil || !strings.Contains(err.Error(), "compaction target") {
		t.Fatalf("NewRuntime error=%v", err)
	}
	if err := registry.Register(operation("late_read", OperationEffectRead)); err != nil {
		t.Fatalf("invalid context config froze registry: %v", err)
	}
}

func TestNewRuntimeRejectsTypedNilContextDependencies(t *testing.T) {
	var nilCounter *testTokenCounter
	var nilCompactor *testContextCompactor
	tests := []struct {
		name   string
		mutate func(*ContextWindowConfig)
		want   string
	}{
		{name: "token counter", mutate: func(window *ContextWindowConfig) { window.TokenCounter = nilCounter }, want: "token counter is required"},
		{name: "compactor", mutate: func(window *ContextWindowConfig) { window.ContextCompactor = nilCompactor }, want: "compactor is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			counter := &testTokenCounter{
				countRequest: func(ModelRequest) (int, error) { return 1, nil },
				countText:    func(string) (int, error) { return 1, nil },
			}
			compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "summary"}, nil
			}}
			window := contextWindowForTest(counter, compactor)
			test.mutate(window)
			registry := NewOperationRegistry()
			_, err := NewRuntime(RuntimeConfig{
				Model: &scriptedModel{}, Operations: registry,
				ContextWindow: window,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewRuntime error=%v, want %q", err, test.want)
			}
			if err := registry.Register(operation("late_read", OperationEffectRead)); err != nil {
				t.Fatalf("typed nil config froze registry: %v", err)
			}
		})
	}
}

func TestContextErrorCodesAreStable(t *testing.T) {
	if got := errorCode(ErrContextLimitExceeded); got != "context_limit_exceeded" {
		t.Fatalf("limit code=%q", got)
	}
	if got := errorCode(ErrContextCompactionFailed); got != "context_compaction_failed" {
		t.Fatalf("compaction code=%q", got)
	}
}
