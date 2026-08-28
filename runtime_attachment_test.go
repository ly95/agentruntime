package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateModelInputAttachmentAcceptsScriptTextFormats(t *testing.T) {
	t.Parallel()

	for _, filename := range []string{"story.fountain", "story.fdx"} {
		err := ValidateModelInputAttachment(ModelInputAttachment{
			Kind: ModelInputAttachmentText, ID: "attachment-1",
			Filename: filename, MIMEType: "text/plain", Text: "A screenplay",
		})
		if err != nil {
			t.Fatalf("validate %s: %v", filename, err)
		}
	}
}

type scriptedModel struct {
	responses []*ModelResponse
	requests  []ModelRequest
}

type streamCallbackModel struct{}

type failingModel struct {
	err error
}

func (m failingModel) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, m.err
}

type streamSinkObservingModel struct {
	sawStreamSink bool
}

func (m *streamSinkObservingModel) Complete(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.sawStreamSink = req.StreamSink != nil
	return messageResponse("resp-no-sink", "done"), nil
}

type multiTurnChunkModel struct {
	turn int
}

type mutatingRequestModel struct {
	turn int
}

func (m *multiTurnChunkModel) Complete(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.turn++
	if req.StreamSink == nil {
		return nil, errors.New("multiTurnChunkModel: stream sink is nil")
	}
	responseID := fmt.Sprintf("resp-%d", m.turn)
	req.StreamSink(ModelStreamEvent{
		Type: ModelStreamResponseStarted, ProviderType: "response.created",
		SequenceNumber: int64Pointer(0), ResponseID: responseID,
	})
	if m.turn == 1 {
		return callResponse(responseID, ToolCall{ID: "call-1", Name: "read_context", Input: json.RawMessage(`{"id":"doc1"}`)}), nil
	}
	return messageResponse(responseID, "done"), nil
}

func (m *mutatingRequestModel) Complete(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.turn++
	toolIndex := len(req.Tools) - 1
	if m.turn == 1 {
		req.Input[0].Text = "model-mutated"
		req.Tools[toolIndex].Name = "model-mutated"
		req.Tools[toolIndex].InputSchema[0] = '['
		return callResponse("resp-1", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}), nil
	}
	if req.Input[0].Text != "apply" {
		return nil, fmt.Errorf("model input mutation leaked into transcript: %q", req.Input[0].Text)
	}
	if req.Tools[toolIndex].Name != "apply_change" || !json.Valid(req.Tools[toolIndex].InputSchema) {
		return nil, fmt.Errorf("model tool mutation leaked into snapshot: %+v", req.Tools[toolIndex])
	}
	return messageResponse("resp-2", "done"), nil
}

func (streamCallbackModel) Complete(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	if req.StreamSink == nil {
		return nil, errors.New("streamCallbackModel: stream sink is nil")
	}
	req.StreamSink(ModelStreamEvent{Type: ModelStreamReasoningSummaryDelta, ProviderType: "response.reasoning_summary_text.delta", SequenceNumber: int64Pointer(4), ItemID: "rs-1", Delta: "checking", RawJSON: `{"type":"reasoning"}`})
	req.StreamSink(ModelStreamEvent{Type: ModelStreamCommentaryDelta, SequenceNumber: int64Pointer(5), ItemID: "msg-c", Delta: "using tool"})
	req.StreamSink(ModelStreamEvent{Type: ModelStreamTextDelta, SequenceNumber: int64Pointer(6), ItemID: "msg-1", Delta: "hel"})
	req.StreamSink(ModelStreamEvent{Type: ModelStreamTextDelta, SequenceNumber: int64Pointer(7), ItemID: "msg-1", Delta: "lo"})
	return messageResponse("resp-stream", "hello"), nil
}

func (m *scriptedModel) Complete(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.requests = append(m.requests, req)
	if len(m.responses) == 0 {
		return nil, errors.New("scriptedModel: no response")
	}
	out := m.responses[0]
	m.responses = m.responses[1:]
	return out, nil
}

func TestRuntimeCarriesAttachmentsIntoModelTranscript(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}}
	store := &recordingStore{}
	rt, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	attachment := ModelInputAttachment{
		Kind: ModelInputAttachmentImage,
		ID:   "attachment-1", Filename: "image.png", MIMEType: "image/png",
		StorageKey: "temp/agent/user/image.png", ExpiresAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		URL: "https://cdn.example.com/image.png",
	}
	resolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
		return attachment, nil
	})
	if _, err := rt.Run(t.Context(), Input{
		User: "inspect", SessionID: "session-attachments", Attachments: []ModelInputAttachment{attachment}, ImageAttachmentResolver: resolver,
	}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || len(model.requests[0].Input) != 1 || len(model.requests[0].Input[0].Attachments) != 1 {
		t.Fatalf("model requests=%+v", model.requests)
	}
	wantModelAttachment := attachment
	wantModelAttachment.CurrentRun = true
	if model.requests[0].Input[0].Attachments[0] != wantModelAttachment {
		t.Fatalf("model attachment=%+v want %+v", model.requests[0].Input[0].Attachments[0], wantModelAttachment)
	}
	store.mu.Lock()
	session := store.sessions["session-attachments"]
	store.mu.Unlock()
	wantPersistedAttachment := attachment
	wantPersistedAttachment.URL = ""
	if len(session.Transcript) != 2 || len(session.Transcript[0].Attachments) != 1 || session.Transcript[0].Attachments[0] != wantPersistedAttachment {
		t.Fatalf("session transcript=%+v", session.Transcript)
	}
	encoded, err := json.Marshal(session.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("https://cdn.example.com/image.png")) || !bytes.Contains(encoded, []byte(`"storage_key":"temp/agent/user/image.png"`)) {
		t.Fatalf("persisted transcript=%s", encoded)
	}
}

func TestRuntimeAttachmentResolverDefaultAndPerRunOverride(t *testing.T) {
	attachment := ModelInputAttachment{
		Kind: ModelInputAttachmentImage,
		ID:   "attachment-default", Filename: "image.png", MIMEType: "image/png",
		StorageKey: "images/default.png", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		URL: "https://cdn.example.com/original.png",
	}
	t.Run("runtime default", func(t *testing.T) {
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-default-resolver", "done")}}
		calls := 0
		resolver := ImageAttachmentResolverFunc(func(_ context.Context, value ModelInputAttachment) (ModelInputAttachment, error) {
			calls++
			value.URL = "https://cdn.example.com/default.png"
			return value, nil
		})
		runtime, err := NewRuntime(RuntimeConfig{
			Model: model, RunStore: &recordingStore{}, ImageAttachmentResolver: resolver,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(t.Context(), Input{
			User: "inspect", SessionID: "session-default-resolver", Attachments: []ModelInputAttachment{attachment},
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || model.requests[0].Input[0].Attachments[0].URL != "https://cdn.example.com/default.png" {
			t.Fatalf("resolver calls=%d request=%+v", calls, model.requests[0])
		}
	})

	t.Run("per-run override", func(t *testing.T) {
		model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-override-resolver", "done")}}
		defaultCalls := 0
		defaultResolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
			defaultCalls++
			return ModelInputAttachment{}, errors.New("runtime default must be overridden")
		})
		overrideCalls := 0
		override := ImageAttachmentResolverFunc(func(_ context.Context, value ModelInputAttachment) (ModelInputAttachment, error) {
			overrideCalls++
			value.URL = "https://cdn.example.com/override.png"
			return value, nil
		})
		runtime, err := NewRuntime(RuntimeConfig{
			Model: model, RunStore: &recordingStore{}, ImageAttachmentResolver: defaultResolver,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(t.Context(), Input{
			User: "inspect", SessionID: "session-override-resolver", Attachments: []ModelInputAttachment{attachment},
			ImageAttachmentResolver: override,
		}); err != nil {
			t.Fatal(err)
		}
		if defaultCalls != 0 || overrideCalls != 1 ||
			model.requests[0].Input[0].Attachments[0].URL != "https://cdn.example.com/override.png" {
			t.Fatalf("default=%d override=%d request=%+v", defaultCalls, overrideCalls, model.requests[0])
		}
	})
}

func TestRuntimeRejectsSessionImageWithoutDurableResolutionContract(t *testing.T) {
	base := ModelInputAttachment{
		Kind: ModelInputAttachmentImage, ID: "attachment-1", Filename: "image.png", MIMEType: "image/png",
		URL: "https://cdn.example.com/image.png",
	}
	resolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
		return ModelInputAttachment{}, errors.New("must not resolve")
	})
	tests := []struct {
		name       string
		attachment ModelInputAttachment
		resolver   ImageAttachmentResolver
		want       string
	}{
		{name: "missing stable metadata", attachment: base, resolver: resolver, want: "stable storage metadata"},
		{name: "missing resolver", attachment: func() ModelInputAttachment {
			attachment := base
			attachment.StorageKey = "temp/agent/user/image.png"
			attachment.ExpiresAt = time.Now().Add(time.Hour)
			return attachment
		}(), want: "requires a resolver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
			rt, err := NewRuntime(RuntimeConfig{Model: model, RunStore: &recordingStore{}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = rt.Run(t.Context(), Input{
				User: "inspect", SessionID: "session-attachments", Attachments: []ModelInputAttachment{test.attachment},
				ImageAttachmentResolver: test.resolver,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || len(model.requests) != 0 {
				t.Fatalf("err=%v model_requests=%d", err, len(model.requests))
			}
		})
	}
}

func TestPersistedTranscriptRejectsTransientOrUnstableImageAttachments(t *testing.T) {
	base := ModelInputAttachment{
		Kind: ModelInputAttachmentImage, ID: "persisted-image", Filename: "image.png", MIMEType: "image/png",
		StorageKey: "images/persisted.png", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, test := range []struct {
		name   string
		mutate func(*ModelInputAttachment)
	}{
		{name: "materialized URL", mutate: func(attachment *ModelInputAttachment) {
			attachment.URL = "https://attacker.invalid/injected.png"
		}},
		{name: "current run authority", mutate: func(attachment *ModelInputAttachment) {
			attachment.CurrentRun = true
		}},
		{name: "missing storage key", mutate: func(attachment *ModelInputAttachment) {
			attachment.StorageKey = ""
		}},
		{name: "missing expiry", mutate: func(attachment *ModelInputAttachment) {
			attachment.ExpiresAt = time.Time{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attachment := base
			test.mutate(&attachment)
			err := validatePersistedModelInputItems([]ModelInputItem{{
				Type: ModelInputUserMessage, Text: "history", Attachments: []ModelInputAttachment{attachment},
			}})
			if err == nil {
				t.Fatal("persisted transcript accepted transient or unstable image attachment")
			}
		})
	}
}

func TestRuntimeRefreshesHistoricalImageForEveryProviderCall(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-refresh-tool", ToolCall{ID: "call-refresh", Name: "read_context", Input: json.RawMessage(`{}`)}),
		messageResponse("resp-final", "done"),
	}}
	store := &recordingStore{}
	expiresAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedContextSession(store, "session-refresh", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "inspect", Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage, ID: "attachment-1", Filename: "image.png", MIMEType: "image/png",
			StorageKey: "temp/agent/user/image.png", ExpiresAt: expiresAt,
		}}},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "seen", Raw: json.RawMessage(`{"type":"message","text":"seen"}`)},
	}, nil)
	resolveCalls := 0
	resolver := ImageAttachmentResolverFunc(func(_ context.Context, attachment ModelInputAttachment) (ModelInputAttachment, error) {
		resolveCalls++
		attachment.URL = fmt.Sprintf("https://cdn.example.com/fresh-%d.png", resolveCalls)
		return attachment, nil
	})
	operations := NewOperationRegistry()
	if err := operations.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	rt := newTestRuntime(t, model, operations, allowPolicy(), OperationExecutorFunc(
		func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{Output: json.RawMessage(`{}`)}, nil
		},
	), nil, nil, store)
	if _, err := rt.Run(t.Context(), Input{User: "continue", SessionID: "session-refresh", ImageAttachmentResolver: resolver}); err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 2 || len(model.requests) != 2 {
		t.Fatalf("resolve_calls=%d model_requests=%d", resolveCalls, len(model.requests))
	}
	if got := model.requests[0].Input[0].Attachments[0].URL; got != "https://cdn.example.com/fresh-1.png" {
		t.Fatalf("first URL=%q", got)
	}
	if got := model.requests[1].Input[0].Attachments[0].URL; got != "https://cdn.example.com/fresh-2.png" {
		t.Fatalf("second provider call URL=%q", got)
	}
	store.mu.Lock()
	persisted := store.sessions["session-refresh"].Transcript[0].Attachments[0]
	store.mu.Unlock()
	if persisted.URL != "" || persisted.StorageKey != "temp/agent/user/image.png" || persisted.ExpiresAt != expiresAt {
		t.Fatalf("persisted attachment=%+v", persisted)
	}
}

func TestRuntimeTurnsUnavailableHistoricalImageIntoOrderedTombstone(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}}
	store := &recordingStore{}
	var legacyTranscript []ModelInputItem
	if err := json.Unmarshal([]byte(`[
		{"type":"user_message","text":"compare","attachments":[
			{"kind":"image","id":"legacy-image","filename":"old.png","mime_type":"image/png","storage_key":"images/old.png","expires_at":"2025-01-01T00:00:00Z","url":"https://expired.example.com/old.png"},
			{"kind":"text","id":"notes","filename":"notes.txt","mime_type":"text/plain","text":"keep this"}
		]},
		{"type":"assistant_output","text":"seen","output_type":"message","raw":{"type":"message","text":"seen"}}
	]`), &legacyTranscript); err != nil {
		t.Fatal(err)
	}
	if legacyTranscript[0].Attachments[0].URL != "" {
		t.Fatalf("legacy transient URL was unexpectedly trusted: %+v", legacyTranscript[0].Attachments[0])
	}
	seedContextSession(store, "session-expired", legacyTranscript, nil)
	resolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
		return ModelInputAttachment{}, fmt.Errorf("%w: gone", ErrImageAttachmentUnavailable)
	})
	rt, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(t.Context(), Input{User: "continue", SessionID: "session-expired", ImageAttachmentResolver: resolver}); err != nil {
		t.Fatal(err)
	}
	attachments := model.requests[0].Input[0].Attachments
	if len(attachments) != 2 || attachments[0].Kind != ModelInputAttachmentText || attachments[0].Filename != "historical-image-unavailable.txt" ||
		attachments[0].StorageKey != "" || attachments[0].URL != "" || !strings.Contains(attachments[0].Text, `filename="old.png"`) ||
		attachments[1].ID != "notes" {
		t.Fatalf("materialized attachments=%+v", attachments)
	}
	store.mu.Lock()
	persisted := store.sessions["session-expired"].Transcript[0].Attachments
	store.mu.Unlock()
	if len(persisted) != 2 || persisted[0].ID != "legacy-image" || persisted[0].Kind != ModelInputAttachmentImage {
		t.Fatalf("persisted attachments=%+v", persisted)
	}
}

func TestRuntimeFailsBeforeModelWhenCurrentImageBecomesUnavailable(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	store := &recordingStore{}
	var events []Event
	resolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
		return ModelInputAttachment{}, fmt.Errorf("%w: gone", ErrImageAttachmentUnavailable)
	})
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: store,
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment := ModelInputAttachment{
		Kind: ModelInputAttachmentImage, ID: "current-image", Filename: "current.png", MIMEType: "image/png",
		StorageKey: "temp/agent/user/current.png", ExpiresAt: time.Now().Add(time.Hour), URL: "https://cdn.example.com/current.png",
	}
	_, err = rt.Run(t.Context(), Input{User: "inspect", SessionID: "session-current", Attachments: []ModelInputAttachment{attachment}, ImageAttachmentResolver: resolver})
	if !errors.Is(err, ErrImageAttachmentUnavailable) || len(model.requests) != 0 {
		t.Fatalf("err=%v model_requests=%d", err, len(model.requests))
	}
	store.mu.Lock()
	failed := append([]RunRecord(nil), store.failed...)
	store.mu.Unlock()
	if len(failed) != 1 || failed[0].Status != RunStatusFailed || failed[0].ErrorCode != "image_attachment_unavailable" {
		t.Fatalf("failed runs=%+v", failed)
	}
	foundFailureEvent := false
	for _, event := range events {
		if event.Type == EventRunFailed {
			foundFailureEvent = true
			if event.ErrorCode != "image_attachment_unavailable" {
				t.Fatalf("failure event=%+v", event)
			}
		}
	}
	if !foundFailureEvent {
		t.Fatalf("events=%+v", events)
	}
}

func TestRuntimePropagatesHistoricalImageResolverInfrastructureFailure(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	store := &recordingStore{}
	before := seedContextSession(store, "session-resolver-failure", []ModelInputItem{{
		Type: ModelInputUserMessage, Text: "inspect", Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage, ID: "attachment-1", Filename: "image.png", MIMEType: "image/png",
			StorageKey: "temp/agent/user/image.png", ExpiresAt: time.Now().Add(time.Hour),
		}},
	}}, nil)
	resolverErr := errors.New("attachment metadata timeout")
	resolver := ImageAttachmentResolverFunc(func(context.Context, ModelInputAttachment) (ModelInputAttachment, error) {
		return ModelInputAttachment{}, errors.Join(ErrRunInterrupted, resolverErr)
	})
	rt, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Run(t.Context(), Input{User: "continue", SessionID: "session-resolver-failure", ImageAttachmentResolver: resolver})
	if !errors.Is(err, resolverErr) || !errors.Is(err, ErrRunInterrupted) || len(model.requests) != 0 {
		t.Fatalf("err=%v model_requests=%d", err, len(model.requests))
	}
	store.mu.Lock()
	after := store.sessions["session-resolver-failure"]
	runs := append([]RunRecord(nil), store.runs...)
	store.mu.Unlock()
	if after.Revision != before.Revision || len(after.Transcript) != len(before.Transcript) {
		t.Fatalf("session advanced on retryable resolution failure: before=%+v after=%+v", before, after)
	}
	if len(runs) != 1 || runs[0].Status != RunStatusInterrupted || runs[0].ErrorCode != "run_interrupted" {
		t.Fatalf("runs=%+v", runs)
	}
}

func TestRuntimeAddsTrustedHostContextToInstructionsWithoutUserImpersonation(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("resp-1", "done")}}
	rt, err := NewRuntime(RuntimeConfig{Model: model, RunStore: &recordingStore{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Run(t.Context(), Input{User: "继续", TrustedContext: `{"modification_proposals":[{"id":"proposal-1","status":"applied"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || !strings.Contains(model.requests[0].Instructions, "trusted_host_context") ||
		!strings.Contains(model.requests[0].Instructions, `"status":"applied"`) ||
		!strings.Contains(model.requests[0].Instructions, "trusted current state supplied by the host") ||
		strings.Contains(model.requests[0].Instructions, "修改方案") ||
		strings.Contains(model.requests[0].Instructions, "modification-proposal") {
		t.Fatalf("instructions=%q", model.requests[0].Instructions)
	}
	if len(model.requests[0].Input) != 1 || model.requests[0].Input[0].Type != ModelInputUserMessage || model.requests[0].Input[0].Text != "继续" {
		t.Fatalf("model input=%+v", model.requests[0].Input)
	}
}
