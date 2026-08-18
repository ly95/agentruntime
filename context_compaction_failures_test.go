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

func TestRuntimeAllowsCallIDAfterItsReplayContextWasCompacted(t *testing.T) {
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
		return ContextSummary{Summary: "Old tool work completed."}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("first-response", "done"),
		callResponse("reused-response", ToolCall{ID: "old-call", Name: "read_context", Input: json.RawMessage(`{}`)}),
		messageResponse("second-response", "done again"),
	}}
	store := &recordingStore{}
	seedContextSession(store, "seen-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "old-call", Raw: json.RawMessage(`{"id":"old-call-item"}`)},
		{Type: ModelInputToolResult, CallID: "old-call", Output: json.RawMessage(`{"ok":true}`)},
	}, nil)
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)
	if _, err := runtime.Run(context.Background(), Input{User: "compact", SessionID: "seen-session"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := store.sessions["seen-session"].SeenCallIDs; len(got) != 0 {
		t.Fatalf("compacted call ids were retained: %v", got)
	}
	result, err := runtime.Run(context.Background(), Input{User: "reuse", SessionID: "seen-session"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if result.Output != "done again" {
		t.Fatalf("second output=%q", result.Output)
	}
}

func TestRuntimeRecompactsExistingCheckpointAndAccumulatesItemCount(t *testing.T) {
	requestCounts := 0
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) {
			requestCounts++
			if requestCounts == 1 {
				return 100, nil
			}
			return 40, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(request ContextCompactionRequest) (ContextSummary, error) {
		request.Checkpoint.Summary.Summary = "compactor-mutated"
		request.Items[0].Text = "compactor-mutated"
		return ContextSummary{Summary: "replacement checkpoint"}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "recompact-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "older retained turn"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "older answer", Raw: json.RawMessage(`{"id":"older-answer"}`)},
	}, nil)
	before.Checkpoint = &ContextCheckpoint{
		Version:            contextCheckpointVersion,
		Summary:            ContextSummary{Summary: "first checkpoint", Facts: []string{"first fact"}},
		CompactedItemCount: 4, SourceSessionRevision: 1,
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	store.sessions["recompact-session"] = before
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("recompact-response", "done")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	if _, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "recompact-session"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(compactor.requests) != 1 {
		t.Fatalf("compactor requests=%d, want 1", len(compactor.requests))
	}
	request := compactor.requests[0]
	if request.Checkpoint == nil || request.Checkpoint.Summary.Summary != "first checkpoint" || request.Checkpoint.CompactedItemCount != 4 || len(request.Items) != 2 || request.Items[0].Text != "older retained turn" {
		t.Fatalf("recompaction request=%+v", request)
	}
	after := store.sessions["recompact-session"]
	if after.Checkpoint == nil || after.Checkpoint.Summary.Summary != "replacement checkpoint" || after.Checkpoint.CompactedItemCount != 6 || after.Checkpoint.SourceSessionRevision != 3 {
		t.Fatalf("replacement checkpoint=%+v", after.Checkpoint)
	}
	if len(after.Transcript) != 2 || after.Transcript[0].Text != "current" {
		t.Fatalf("replacement transcript=%+v", after.Transcript)
	}
}

func TestRuntimeRejectsOversizedLoadedCheckpointBeforeCountingRequest(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) {
			t.Fatal("CountModelRequest must not run for an oversized loaded checkpoint")
			return 0, nil
		},
		countText: func(string) (int, error) { return 21, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("compactor must not run for an oversized loaded checkpoint")
		return ContextSummary{}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "oversized-checkpoint-session", nil, nil)
	before.Checkpoint = &ContextCheckpoint{
		Version:            contextCheckpointVersion,
		Summary:            ContextSummary{Summary: "existing checkpoint"},
		CompactedItemCount: 2, SourceSessionRevision: 1,
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	store.sessions["oversized-checkpoint-session"] = before
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "oversized-checkpoint-session"})
	if !errors.Is(err, ErrContextLimitExceeded) || !strings.Contains(err.Error(), "existing checkpoint") {
		t.Fatalf("Run error=%v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called with an oversized loaded checkpoint")
	}
	if after := store.sessions["oversized-checkpoint-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after oversized loaded checkpoint:\nbefore=%+v\nafter=%+v", before, after)
	}
}

type contextCompactionFailureCase struct {
	name      string
	noHistory bool
	counter   func(ModelRequest) (int, error)
	countText func(string) (int, error)
	compact   func(ContextCompactionRequest) (ContextSummary, error)
	wantError error
}

func TestRuntimeContextCompactionFailuresDoNotCallModelOrChangeSession(t *testing.T) {
	tests := append(contextCompactionInputFailures(), contextCompactionLimitFailures()...)
	runContextCompactionFailureCases(t, tests)
}

func contextCompactionInputFailures() []contextCompactionFailureCase {
	return []contextCompactionFailureCase{
		{
			name: "counter failure",
			counter: func(ModelRequest) (int, error) {
				return 0, errors.New("tokenizer unavailable")
			},
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "unused"}, nil
			},
			wantError: ErrContextCompactionFailed,
		},
		{
			name:      "zero request count",
			counter:   func(ModelRequest) (int, error) { return 0, nil },
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "unused"}, nil
			},
			wantError: ErrContextCompactionFailed,
		},
		{
			name:      "compactor failure",
			counter:   func(ModelRequest) (int, error) { return 101, nil },
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{}, errors.New("summary model unavailable")
			},
			wantError: ErrContextCompactionFailed,
		},
		{
			name:      "invalid summary",
			counter:   func(ModelRequest) (int, error) { return 101, nil },
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{}, nil
			},
			wantError: ErrContextCompactionFailed,
		},
	}
}

func contextCompactionLimitFailures() []contextCompactionFailureCase {
	return []contextCompactionFailureCase{
		{
			name:      "checkpoint too large",
			counter:   func(ModelRequest) (int, error) { return 101, nil },
			countText: func(string) (int, error) { return 21, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "large"}, nil
			},
			wantError: ErrContextLimitExceeded,
		},
		{
			name:      "zero checkpoint count",
			counter:   func(ModelRequest) (int, error) { return 101, nil },
			countText: func(string) (int, error) { return 0, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "zero-sized checkpoint"}, nil
			},
			wantError: ErrContextCompactionFailed,
		},
		{
			name: "compacted request above target",
			counter: func(request ModelRequest) (int, error) {
				if requestHasCheckpoint(request) {
					return 51, nil
				}
				return 101, nil
			},
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "still too large"}, nil
			},
			wantError: ErrContextLimitExceeded,
		},
		{
			name: "zero compacted request count",
			counter: func(request ModelRequest) (int, error) {
				if requestHasCheckpoint(request) {
					return 0, nil
				}
				return 101, nil
			},
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "zero-sized request"}, nil
			},
			wantError: ErrContextCompactionFailed,
		},
		{
			name: "no safe prefix", noHistory: true,
			counter:   func(ModelRequest) (int, error) { return 101, nil },
			countText: func(string) (int, error) { return 10, nil },
			compact: func(ContextCompactionRequest) (ContextSummary, error) {
				return ContextSummary{Summary: "unused"}, nil
			},
			wantError: ErrContextLimitExceeded,
		},
	}
}

func runContextCompactionFailureCases(t *testing.T, tests []contextCompactionFailureCase) {
	t.Helper()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			counter := &testTokenCounter{countRequest: test.counter, countText: test.countText}
			compactor := &testContextCompactor{compact: test.compact}
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			store := &recordingStore{}
			var transcript []ModelInputItem
			if !test.noHistory {
				transcript = []ModelInputItem{
					{Type: ModelInputUserMessage, Text: "old"},
					{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"id":"old"}`)},
				}
			}
			before := seedContextSession(store, "failure-session", transcript, nil)
			var events []Event
			runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), func(event Event) { events = append(events, event) })

			_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "failure-session"})
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Run error=%v, want %v", err, test.wantError)
			}
			if len(model.requests) != 0 {
				t.Fatalf("model was called: %+v", model.requests)
			}
			if after := store.sessions["failure-session"]; !cmp.Equal(after, before) {
				t.Fatalf("session changed on compaction failure:\nbefore=%+v\nafter=%+v", before, after)
			}
			failed := false
			for _, event := range events {
				if event.Type == EventContextCompactionFailed && event.ErrorCode == errorCode(test.wantError) {
					failed = true
				}
			}
			if !failed {
				t.Fatalf("missing stable compaction failure event: %+v", events)
			}
		})
	}
}

func TestRuntimeDoesNotCompactOrphanItemsBeforeFirstUserTurn(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 101, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("compactor must not receive an orphan transcript prefix")
		return ContextSummary{}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "orphan-session", []ModelInputItem{
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "orphan", Raw: json.RawMessage(`{"id":"orphan"}`)},
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"id":"old-answer"}`)},
	}, nil)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "orphan-session"})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "starts with") {
		t.Fatalf("Run error=%v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called after unsafe prefix detection")
	}
	if after := store.sessions["orphan-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after unsafe prefix detection:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestRuntimeDoesNotSplitFunctionCallAndToolResultAcrossCompactionBoundary(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 101, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("compactor must not receive a split function call sequence")
		return ContextSummary{}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "split-call-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "split-call", Raw: json.RawMessage(`{"id":"split-call-item"}`)},
		{Type: ModelInputUserMessage, Text: "recent user"},
		{Type: ModelInputToolResult, CallID: "split-call", Output: json.RawMessage(`{"ok":true}`)},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "recent answer", Raw: json.RawMessage(`{"id":"recent-answer"}`)},
	}, nil)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	window := contextWindowForTest(counter, compactor)
	window.PreserveRecentTurns = 2
	runtime := newContextRuntimeForTest(t, model, store, window, nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "split-call-session"})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "missing results before user message") {
		t.Fatalf("Run error=%v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called after split function call detection")
	}
	if after := store.sessions["split-call-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after split function call detection:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestContextCompactionPrefixAllowsCompleteParallelToolSequence(t *testing.T) {
	transcript := []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call-a"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call-b"},
		{Type: ModelInputToolResult, CallID: "call-a"},
		{Type: ModelInputToolResult, CallID: "call-b"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer"},
		{Type: ModelInputUserMessage, Text: "current user"},
	}

	got, err := contextCompactionPrefixEnd(transcript, 1)
	if err != nil {
		t.Fatalf("contextCompactionPrefixEnd error=%v", err)
	}
	if got != 6 {
		t.Fatalf("contextCompactionPrefixEnd=%d, want 6", got)
	}
}

func TestRuntimeRejectsMismatchedToolCallIDsBeforeCompaction(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 101, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("compactor must not receive a transcript with mismatched tool call IDs")
		return ContextSummary{}, nil
	}}
	store := &recordingStore{}
	before := seedContextSession(store, "mismatched-call-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call-a", Raw: json.RawMessage(`{"id":"call-a-item"}`)},
		{Type: ModelInputToolResult, CallID: "call-b", Output: json.RawMessage(`{"ok":true}`)},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"id":"old-answer"}`)},
	}, nil)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "mismatched-call-session"})
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), `references call ID "call-b", pending call IDs are [call-a]`) {
		t.Fatalf("Run error=%v", err)
	}
	if len(compactor.requests) != 0 {
		t.Fatalf("compactor received %d requests after mismatched call ID detection", len(compactor.requests))
	}
	if len(model.requests) != 0 {
		t.Fatalf("model was called after mismatched call ID detection")
	}
	if after := store.sessions["mismatched-call-session"]; !cmp.Equal(after, before) {
		t.Fatalf("session changed after mismatched call ID detection:\nbefore=%+v\nafter=%+v", before, after)
	}
}
