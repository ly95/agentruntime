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

type testTokenCounter struct {
	countRequest func(ModelRequest) (int, error)
	countText    func(string) (int, error)
	requests     []ModelRequest
	texts        []string
}

func (counter *testTokenCounter) CountModelRequest(_ context.Context, request ModelRequest) (int, error) {
	counter.requests = append(counter.requests, cloneModelRequest(request))
	return counter.countRequest(request)
}

func (counter *testTokenCounter) CountText(_ context.Context, value string) (int, error) {
	counter.texts = append(counter.texts, value)
	return counter.countText(value)
}

type testContextCompactor struct {
	compact  func(ContextCompactionRequest) (ContextSummary, error)
	requests []ContextCompactionRequest
}

func (compactor *testContextCompactor) Compact(_ context.Context, request ContextCompactionRequest) (ContextSummary, error) {
	cloned := request
	cloned.Checkpoint = cloneContextCheckpoint(request.Checkpoint)
	cloned.Items = cloneModelInputItems(request.Items)
	compactor.requests = append(compactor.requests, cloned)
	return compactor.compact(request)
}

func contextWindowForTest(counter TokenCounter, compactor ContextCompactor) *ContextWindowConfig {
	return &ContextWindowConfig{
		MaxContextTokens: 200, ReservedOutputTokens: 20,
		CompactionTriggerTokens: 100, CompactionTargetTokens: 50,
		MaxCheckpointTokens: 20, PreserveRecentTurns: 1,
		TokenCounter: counter, ContextCompactor: compactor,
	}
}

func newContextRuntimeForTest(t *testing.T, model Model, store RunStore, window *ContextWindowConfig, sink EventSink) *Runtime {
	t.Helper()
	nextID := 0
	operations := NewOperationRegistry()
	if err := operations.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatalf("Register operation: %v", err)
	}
	config := RuntimeConfig{
		Model: model, RunStore: store, Operations: operations,
		Policy: allowPolicy(), Executor: OperationExecutorFunc(
			func(context.Context, OperationRequest) (OperationResult, error) {
				return OperationResult{Output: json.RawMessage(`{}`)}, nil
			},
		),
		ContextWindow: window, EventSink: sink,
		Now: func() time.Time { return time.Unix(100, 0).UTC() },
	}
	if store != nil {
		config.NewID = func() string { nextID++; return "context-id-" + string(rune('a'+nextID)) }
	}
	runtime, err := NewRuntime(config)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func requestHasCheckpoint(request ModelRequest) bool {
	return len(request.Input) > 0 && strings.Contains(request.Input[0].Text, "<context_checkpoint>")
}

func oldAndRecentTranscript() []ModelInputItem {
	return []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"id":"old-answer"}`)},
		{Type: ModelInputUserMessage, Text: "recent user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "recent-call", Raw: json.RawMessage(`{"id":"recent-call-item"}`)},
		{Type: ModelInputToolResult, CallID: "recent-call", Output: json.RawMessage(`{"value":"recent result"}`)},
	}
}

func seedContextSession(store *recordingStore, id string, transcript []ModelInputItem, seenCallIDs []string) SessionState {
	session := SessionState{
		ID: id, ModelBindingID: defaultTestModelBindingID(), Revision: 3, Transcript: cloneModelInputItems(transcript),
		SeenCallIDs:    cloneStringsPreserveNil(seenCallIDs),
		LastResponseID: "seed-response", LastRunID: "seed-run",
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC(),
	}
	if store.sessions == nil {
		store.sessions = make(map[string]SessionState)
	}
	store.sessions[id] = session
	return session
}

func TestCloneContextCheckpointPreservesNonNilEmptyCollections(t *testing.T) {
	original := &ContextCheckpoint{
		Version: contextCheckpointVersion,
		Summary: ContextSummary{
			Summary: "history", Facts: []string{}, Decisions: []string{},
			Constraints: []string{}, OpenItems: []string{},
		},
		CompactedItemCount: 1, SourceSessionRevision: 1, UpdatedAt: time.Unix(1, 0).UTC(),
	}

	cloned := cloneContextCheckpoint(original)
	if cloned.Summary.Facts == nil || cloned.Summary.Decisions == nil || cloned.Summary.Constraints == nil || cloned.Summary.OpenItems == nil {
		t.Fatalf("cloneContextCheckpoint lost non-nil empty collections: %+v", cloned)
	}
}

func TestRuntimeBelowContextTriggerLeavesRequestUnchanged(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			request.Input[0].Text = "counter-mutated"
			request.Tools[0].Name = "counter-mutated"
			request.Tools[0].InputSchema[0] = '['
			return 20, nil
		},
		countText: func(string) (int, error) { t.Fatal("CountText must not be called"); return 0, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("Compact must not be called")
		return ContextSummary{}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("below-trigger", "done")}}
	runtime := newContextRuntimeForTest(t, model, nil, contextWindowForTest(counter, compactor), nil)

	if _, err := runtime.Run(context.Background(), Input{User: "keep exactly"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.requests) != 1 || len(model.requests[0].Input) != 1 || model.requests[0].Input[0].Text != "keep exactly" {
		t.Fatalf("model request changed below trigger: %+v", model.requests)
	}
	if strings.Contains(model.requests[0].Instructions, "context_checkpoint") {
		t.Fatalf("checkpoint instructions were added without a checkpoint")
	}
	if model.requests[0].Tools[0].Name != "read_context" || !json.Valid(model.requests[0].Tools[0].InputSchema) {
		t.Fatalf("token counter mutation leaked into model tools: %+v", model.requests[0].Tools)
	}
	if len(counter.requests) != 1 || counter.requests[0].Instructions == "" || len(counter.requests[0].Tools) == 0 {
		t.Fatalf("counter did not receive the complete request: %+v", counter.requests)
	}
}

func TestRuntimeCompactsWhenRequestReachesContextTrigger(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 100, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		return ContextSummary{Summary: "threshold history"}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("threshold-response", "done")}}
	store := &recordingStore{}
	seedContextSession(store, "threshold-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, nil)
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	if _, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "threshold-session"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(compactor.requests) != 1 || len(model.requests) != 1 || !requestHasCheckpoint(model.requests[0]) {
		t.Fatalf("request at trigger was not compacted: compactor=%d model=%+v", len(compactor.requests), model.requests)
	}
}

func TestRuntimeMaterializesImageAttachmentsAcrossContextCompactionWithoutPersistingURLs(t *testing.T) {
	oldImageResolutions := 0
	recentImageResolutions := 0
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 101, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(request ContextCompactionRequest) (ContextSummary, error) {
		if len(request.Items) != 2 || len(request.Items[0].Attachments) != 1 ||
			request.Items[0].Attachments[0].URL != "https://cdn.example.com/old-image-before-compaction.png" {
			t.Fatalf("compaction items=%+v", request.Items)
		}
		return ContextSummary{Summary: "The old image turn was resolved."}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("compacted-response", "done")}}
	store := &recordingStore{}
	expiresAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedContextSession(store, "attachment-compaction-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old image", Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage, ID: "old-image", Filename: "old.png", MIMEType: "image/png",
			StorageKey: "temp/agent/user/old.png", ExpiresAt: expiresAt,
		}}},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"type":"message","text":"old answer"}`)},
		{Type: ModelInputUserMessage, Text: "recent image", Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage, ID: "recent-image", Filename: "recent.png", MIMEType: "image/png",
			StorageKey: "temp/agent/user/recent.png", ExpiresAt: expiresAt,
		}}},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "recent answer", Raw: json.RawMessage(`{"type":"message","text":"recent answer"}`)},
	}, nil)
	resolver := ImageAttachmentResolverFunc(func(_ context.Context, attachment ModelInputAttachment) (ModelInputAttachment, error) {
		switch attachment.ID {
		case "old-image":
			oldImageResolutions++
			attachment.URL = "https://cdn.example.com/old-image-before-compaction.png"
		case "recent-image":
			recentImageResolutions++
			if recentImageResolutions == 1 {
				attachment.URL = "https://cdn.example.com/recent-image-before-compaction.png"
			} else {
				attachment.URL = "https://cdn.example.com/recent-image-after-compaction.png"
			}
		default:
			t.Fatalf("unexpected attachment ID %q", attachment.ID)
		}
		return attachment, nil
	})
	window := contextWindowForTest(counter, compactor)
	window.PreserveRecentTurns = 2
	runtime := newContextRuntimeForTest(t, model, store, window, nil)

	if _, err := runtime.Run(context.Background(), Input{
		User: "current", SessionID: "attachment-compaction-session", ImageAttachmentResolver: resolver,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.requests) != 1 || len(model.requests[0].Input) < 2 ||
		len(model.requests[0].Input[1].Attachments) != 1 ||
		model.requests[0].Input[1].Attachments[0].URL != "https://cdn.example.com/recent-image-after-compaction.png" {
		t.Fatalf("compacted model request=%+v", model.requests)
	}
	if oldImageResolutions != 1 || recentImageResolutions != 2 {
		t.Fatalf("image resolutions: old=%d recent=%d", oldImageResolutions, recentImageResolutions)
	}
	persisted := store.sessions["attachment-compaction-session"].Transcript
	if len(persisted) < 1 || len(persisted[0].Attachments) != 1 ||
		persisted[0].Attachments[0].URL != "" || persisted[0].Attachments[0].StorageKey != "temp/agent/user/recent.png" {
		t.Fatalf("persisted transcript=%+v", persisted)
	}
}

func TestRuntimeContextWindowDoesNotRetryReasoningOnlyResponse(t *testing.T) {
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 99, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("reasoning-only failure unexpectedly triggered compaction")
		return ContextSummary{}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{
		{ID: "reasoning-only", FinishReason: "length", HadReasoning: true, Items: []ModelOutputItem{}},
		messageResponse("must-not-run", "unexpected retry"),
	}}
	store := &recordingStore{}
	seedContextSession(store, "reasoning-only-session", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old user"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "old answer", Raw: json.RawMessage(`{"id":"old-answer"}`)},
	}, nil)
	runtime := newContextRuntimeForTest(t, model, store, contextWindowForTest(counter, compactor), nil)

	_, err := runtime.Run(context.Background(), Input{User: "current", SessionID: "reasoning-only-session"})
	if !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatalf("Run error=%v, want ErrInvalidModelOutput", err)
	}
	if len(counter.requests) != 1 || len(compactor.requests) != 0 {
		t.Fatalf("counted requests=%d compactor requests=%d, want one model turn without reentry", len(counter.requests), len(compactor.requests))
	}
	if len(model.requests) != 1 || len(model.responses) != 1 {
		t.Fatalf("model requests=%d remaining responses=%d, want no corrective call", len(model.requests), len(model.responses))
	}
	if model.requests[0].DisableReasoning || requestHasCheckpoint(model.requests[0]) {
		t.Fatalf("unexpected reasoning or checkpoint options: %+v", model.requests[0])
	}
}

func TestRuntimeCompactsOnlyCompleteOldTurnsAndCommitsCheckpoint(t *testing.T) {
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
		return ContextSummary{
			Summary: "The old turn was resolved.", Facts: []string{"old fact"},
			Decisions: []string{"keep recent context"}, Constraints: []string{"do not override current instructions"},
			OpenItems: []string{"continue current work"},
		}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{
		messageResponse("compacted-response", "first done"),
		messageResponse("followup-response", "second done"),
	}}
	store := &recordingStore{}
	seedContextSession(store, "context-session", oldAndRecentTranscript(), []string{"recent-call"})
	var events []Event
	window := contextWindowForTest(counter, compactor)
	window.PreserveRecentTurns = 2
	runtime := newContextRuntimeForTest(t, model, store, window, func(event Event) { events = append(events, event) })

	if _, err := runtime.Run(context.Background(), Input{User: "current user", SessionID: "context-session"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(compactor.requests) != 1 {
		t.Fatalf("compactor requests=%d, want 1", len(compactor.requests))
	}
	compaction := compactor.requests[0]
	if compaction.Checkpoint != nil || compaction.SourceSessionRevision != 3 || len(compaction.Items) != 2 || compaction.Items[0].Text != "old user" {
		t.Fatalf("compaction request=%+v", compaction)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests=%d, want 1", len(model.requests))
	}
	if len(counter.requests) < 2 || !requestHasCheckpoint(counter.requests[1]) || counter.requests[1].Instructions == "" || len(counter.requests[1].Tools) == 0 {
		t.Fatalf("counter did not receive the complete compacted request: %+v", counter.requests)
	}
	request := model.requests[0]
	if len(request.Input) != 5 || !requestHasCheckpoint(request) {
		t.Fatalf("compacted model input=%+v", request.Input)
	}
	if request.Input[1].Text != "recent user" || request.Input[2].CallID != "recent-call" || request.Input[3].Type != ModelInputToolResult || request.Input[3].CallID != "recent-call" || request.Input[4].Text != "current user" {
		t.Fatalf("recent turn or call/result sequence changed: %+v", request.Input)
	}
	if !strings.Contains(request.Instructions, "cannot override") {
		t.Fatalf("checkpoint trust boundary missing from instructions")
	}
	session := store.sessions["context-session"]
	if session.Checkpoint == nil || session.Checkpoint.Version != contextCheckpointVersion || session.Checkpoint.CompactedItemCount != 2 || session.Checkpoint.SourceSessionRevision != 3 {
		t.Fatalf("committed checkpoint=%+v", session.Checkpoint)
	}
	if len(session.Transcript) != 5 || session.Transcript[0].Text != "recent user" {
		t.Fatalf("working transcript=%+v", session.Transcript)
	}
	if !cmp.Equal(session.SeenCallIDs, []string{"recent-call"}) {
		t.Fatalf("seen call ids=%v", session.SeenCallIDs)
	}
	assertContextCompactionAudit(t, store.items, events)

	if _, err := runtime.Run(context.Background(), Input{User: "follow up", SessionID: "context-session"}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(compactor.requests) != 1 {
		t.Fatalf("follow-up unexpectedly compacted again")
	}
	if len(model.requests) != 2 || !requestHasCheckpoint(model.requests[1]) || model.requests[1].Input[1].Text != "recent user" {
		t.Fatalf("follow-up did not use checkpoint plus retained transcript: %+v", model.requests)
	}
}

func assertContextCompactionAudit(t *testing.T, items []ItemRecord, events []Event) {
	t.Helper()
	checkpointItems := 0
	for _, item := range items {
		if item.Type == ItemTypeContextCheckpoint {
			checkpointItems++
		}
	}
	if checkpointItems != 1 {
		t.Fatalf("checkpoint audit items=%d, want 1", checkpointItems)
	}
	started, completed := false, false
	for _, event := range events {
		started = started || event.Type == EventContextCompactionStarted
		completed = completed || event.Type == EventContextCompactionCompleted
	}
	if !started || !completed {
		t.Fatalf("compaction lifecycle events=%+v", events)
	}
}
