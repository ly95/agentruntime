package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEventDispatcherContainsObserverPanic(t *testing.T) {
	called := false
	dispatcher := NewEventDispatcher(
		func(Event) { panic("observer failed") },
		func(event Event) { called = event.Type == EventRunStarted },
	)
	dispatcher.Emit(Event{Type: EventRunStarted, RunID: "run-dispatch"})
	if !called {
		t.Fatal("panic in one observer prevented the next observer")
	}
}

func TestRuntimeEventObserverPanicDoesNotFailRun(t *testing.T) {
	runtime, err := NewRuntime(RuntimeConfig{
		Model:     &scriptedModel{responses: []*ModelResponse{messageResponse("response-event-panic", "done")}},
		EventSink: func(Event) { panic("observer failed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(t.Context(), Input{User: "complete"})
	if err != nil || result.Output != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRuntimeModelCompletedEventCarriesUsage(t *testing.T) {
	response := messageResponse("response-usage-event", "done")
	response.Usage = Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}
	var completed Event
	runtime, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{responses: []*ModelResponse{response}},
		EventSink: func(event Event) {
			if event.Type == EventModelCompleted {
				completed = event
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "complete"}); err != nil {
		t.Fatal(err)
	}
	if completed.InputTokens != 11 || completed.OutputTokens != 7 || completed.TotalTokens != 18 {
		t.Fatalf("completed event=%+v", completed)
	}
}

func TestBufferedEventSinkDropNewestDoesNotBlockAndCloses(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	buffered, err := NewBufferedEventSink(func(Event) {
		once.Do(func() { close(started) })
		<-release
	}, 1, EventOverflowDropNewest)
	if err != nil {
		t.Fatal(err)
	}
	sink := buffered.EventSink()
	sink(Event{Type: EventRunStarted})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("downstream did not receive first event")
	}
	sink(Event{Type: EventModelStarted})
	returned := make(chan struct{})
	go func() {
		sink(Event{Type: EventOperationStarted})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("drop-newest sink blocked Runtime")
	}
	if buffered.Dropped() == 0 {
		t.Fatal("queue overflow was not observable")
	}
	close(release)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := buffered.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := buffered.Dropped()
	sink(Event{Type: EventRunCompleted})
	if buffered.Dropped() != before+1 {
		t.Fatal("event submitted after Close was not rejected")
	}
}

func TestBufferedEventSinkCloseDrainsAcceptedEvents(t *testing.T) {
	var delivered atomic.Int64
	buffered, err := NewBufferedEventSink(func(Event) { delivered.Add(1) }, 16, EventOverflowDropNewest)
	if err != nil {
		t.Fatal(err)
	}
	sink := buffered.EventSink()
	for index := 0; index < 10; index++ {
		sink(Event{Type: EventModelStarted})
	}
	if err := buffered.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := delivered.Load(); got != 10 || buffered.Dropped() != 0 {
		t.Fatalf("delivered=%d dropped=%d", got, buffered.Dropped())
	}
}

func TestBufferedEventSinkCloseReleasesBlockedSender(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var delivered atomic.Int64
	buffered, err := NewBufferedEventSink(func(Event) {
		delivered.Add(1)
		once.Do(func() { close(started) })
		<-release
	}, 1, EventOverflowBlock)
	if err != nil {
		t.Fatal(err)
	}
	sink := buffered.EventSink()
	sink(Event{Type: EventRunStarted})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("downstream did not receive first event")
	}
	sink(Event{Type: EventModelStarted})
	returned := make(chan struct{})
	go func() {
		sink(Event{Type: EventOperationStarted})
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("block policy sender returned while the queue was full")
	case <-time.After(20 * time.Millisecond):
	}

	closeCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := buffered.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want deadline while downstream is blocked", err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked sender")
	}
	if buffered.Dropped() != 1 {
		t.Fatalf("dropped=%d, want 1", buffered.Dropped())
	}

	close(release)
	if err := buffered.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := delivered.Load(); got != 2 {
		t.Fatalf("delivered=%d, want 2", got)
	}
}

func TestSanitizedEventJSONExcludesTrustedFields(t *testing.T) {
	event := Event{
		Type: EventOperationFailed, RunID: "run-safe", ErrorCode: "operation_denied",
		Error: "credential=secret", Data: json.RawMessage(`{"private":true}`),
		ApprovalPreview: json.RawMessage(`{"safe":"preview"}`),
		Chunk: &ModelStreamEvent{
			Type: ModelStreamToolArgumentsDelta, Delta: "secret arguments",
			Arguments: `{"secret":true}`, RawJSON: `{"provider_secret":true}`,
			ErrorMessage: "provider secret",
		},
	}
	sanitized := SanitizeEvent(event)
	if sanitized.Data != nil || sanitized.Error != "" || sanitized.Chunk.RawJSON != "" ||
		sanitized.Chunk.ErrorMessage != "" || sanitized.Chunk.Arguments != "" || sanitized.Chunk.Delta != "" {
		t.Fatalf("sanitized struct retains trusted fields: %+v", sanitized)
	}
	raw, err := json.Marshal(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{
		"credential=secret", `\"private\"`, "secret arguments", "provider_secret", "provider secret",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sanitized JSON contains %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"error_code":"operation_denied"`) {
		t.Fatalf("sanitized JSON lost stable error code: %s", text)
	}
}

func TestPublicRedactionHandlesMaliciousCorpus(t *testing.T) {
	for _, input := range []string{
		`</script><img src=x onerror=alert(1)>`,
		"line\x00with\x1bcontrols",
		strings.Repeat("界", 600),
		string([]byte{0xff, 0xfe}),
	} {
		redacted := RedactText(input, 64)
		raw, err := json.Marshal(map[string]string{"text": redacted})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "</script>") || strings.ContainsRune(redacted, '\x00') ||
			strings.ContainsRune(redacted, '\x1b') {
			t.Fatalf("unsafe redacted text: input=%q output=%q json=%s", input, redacted, raw)
		}
		if utf8.RuneCountInString(redacted) > 64 {
			t.Fatalf("redacted text exceeded rune bound: input=%q output=%q", input, redacted)
		}
	}
	if got := RedactText("甲乙丙", 2); got != "甲…" {
		t.Fatalf("RedactText truncation=%q, want %q", got, "甲…")
	}

	result := OperationResult{
		Output:        json.RawMessage(`{"visible":true}`),
		Receipt:       json.RawMessage(`{"token":"secret"}`),
		FinalResponse: "done\x00",
		Artifacts: []ResultArtifact{{
			Type: "change", Data: json.RawMessage(`{"id":"public"}`),
			InternalData:   json.RawMessage(`{"storage_key":"secret"}`),
			SessionSummary: json.RawMessage(`{"private_projection":true}`),
		}},
	}
	public := RedactOperationResult(result)
	if len(public.Receipt) != 0 || len(public.Artifacts) != 1 ||
		len(public.Artifacts[0].InternalData) != 0 || len(public.Artifacts[0].SessionSummary) != 0 {
		t.Fatalf("private operation fields survived redaction: %+v", public)
	}
	result.Output[0] = '['
	result.Artifacts[0].Data[0] = '['
	if string(public.Output) != `{"visible":true}` || string(public.Artifacts[0].Data) != `{"id":"public"}` {
		t.Fatal("public operation result aliases caller-owned JSON")
	}
}

func TestMetricsEventSinkMapsCoreSignals(t *testing.T) {
	var metrics []RuntimeMetric
	calls := 0
	sink := MetricsEventSink(func(metric RuntimeMetric) {
		metrics = append(metrics, metric)
		if calls == 0 {
			metric.Attributes["sink_mutation"] = "must not leak"
		}
		calls++
	})
	sink(Event{Type: EventModelStarted})
	sink(Event{Type: EventModelCompleted, InputTokens: 10, OutputTokens: 4, TotalTokens: 14})
	sink(Event{Type: EventSessionLeaseRenewed, LeaseGeneration: 2})
	sink(Event{Type: EventReconciliationFailed, Reconciliation: "complete", ErrorCode: "invalid_operation_reconciliation"})
	if len(metrics) != 6 {
		t.Fatalf("metrics=%+v", metrics)
	}
	if metrics[5].Attributes["outcome"] != "failed" {
		t.Fatalf("reconciliation metric=%+v", metrics[5])
	}
	for index := 1; index < len(metrics); index++ {
		if metrics[index].Attributes["sink_mutation"] != "" {
			t.Fatalf("metric %d aliases an earlier sink-owned map: %+v", index, metrics[index])
		}
	}
}

func TestDecodeArgumentsAndTrustedInput(t *testing.T) {
	type arguments struct {
		Count int64 `json:"count"`
	}
	decoded, err := DecodeArguments[arguments](map[string]any{"count": json.Number("9007199254740993")})
	if err != nil || decoded.Count != 9007199254740993 {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if _, err := DecodeArguments[arguments](json.RawMessage(`{"count":1,"extra":true}`)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	input, err := ApplyTrustedInput(Input{User: "write", IdempotencyKey: "key"}, TrustedInputFields{
		RunID: "trusted-run", IdempotencyScope: "tenant-a", TrustedContext: `{"role":"operator"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.RunID != "trusted-run" || input.IdempotencyScope != "tenant-a" {
		t.Fatalf("trusted input=%+v", input)
	}
	if err := ValidateWriteInput(input); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTrustedInput(input, TrustedInputFields{}); err == nil {
		t.Fatal("prepopulated trusted fields were accepted")
	}
}

func TestDefaultContextAdaptersAndRunOutcome(t *testing.T) {
	config, err := NewDefaultContextWindowConfig(4096)
	if err != nil {
		t.Fatal(err)
	}
	count, err := config.TokenCounter.CountText(t.Context(), "abc")
	if err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	summary, err := config.ContextCompactor.Compact(t.Context(), ContextCompactionRequest{
		Items:               []ModelInputItem{{Type: ModelInputUserMessage, Text: "retain this bounded fact"}},
		MaxCheckpointTokens: 512,
	})
	if err != nil || len(summary.Facts) != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	outcome := ClassifyRunOutcome(nil, errors.Join(ErrOperationOutcomeUnknown, errors.New("private detail")))
	if !outcome.RequiresReconciliation || outcome.Retryable || outcome.Status != RunStatusInterrupted ||
		strings.Contains(outcome.Message, "private detail") {
		t.Fatalf("outcome=%+v", outcome)
	}
	outcome = ClassifyRunOutcome(nil, errors.Join(context.Canceled, ErrOperationOutcomeUnknown))
	if outcome.ErrorCode != "operation_outcome_unknown" || !outcome.RequiresReconciliation ||
		outcome.Retryable || outcome.Status != RunStatusInterrupted {
		t.Fatalf("joined cancellation outcome=%+v", outcome)
	}
	for _, test := range []struct {
		err  error
		code string
	}{
		{err: context.Canceled, code: "run_cancelled"},
		{err: context.DeadlineExceeded, code: "run_interrupted"},
	} {
		classified := ClassifyRunOutcome(nil, test.err)
		if classified.ErrorCode != test.code || classified.Message == "The run failed." {
			t.Fatalf("context outcome=%+v, want code %q", classified, test.code)
		}
	}
}

type failureAuditModel struct{}

func (failureAuditModel) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("model unavailable in atomic audit test")
}

func TestInMemoryRunStoreCommitsFailureAuditAtomically(t *testing.T) {
	store := NewInMemoryStore()
	runtime, err := NewRuntime(RuntimeConfig{Model: failureAuditModel{}, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := runtime.Run(t.Context(), Input{RunID: "run-failure-audit", User: "fail"})
	if runErr == nil {
		t.Fatal("Run unexpectedly succeeded")
	}
	run, err := store.GetRun(t.Context(), "run-failure-audit")
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.ListItems(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.FailureAuditStatus != FailureAuditCommitted || len(items) == 0 || items[len(items)-1].Type != ItemTypeError {
		t.Fatalf("run=%+v items=%+v", run, items)
	}
}
