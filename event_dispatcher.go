package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// EventDispatcher is the single fan-out boundary for Runtime events. It clones
// each event per observer and contains observer panics. Sinks may still be
// invoked concurrently by concurrent Runs; use BufferedEventSink when an
// observer is slow or requires a dedicated worker.
type EventDispatcher struct {
	sinks []EventSink
}

func NewEventDispatcher(sinks ...EventSink) *EventDispatcher {
	dispatcher := &EventDispatcher{}
	for _, sink := range sinks {
		if sink == nil || isNilDependency(sink) {
			continue
		}
		dispatcher.sinks = append(dispatcher.sinks, RecoveringEventSink(sink, nil))
	}
	return dispatcher
}

func (dispatcher *EventDispatcher) Emit(event Event) {
	if dispatcher == nil {
		return
	}
	for _, sink := range dispatcher.sinks {
		sink(cloneRuntimeEvent(event))
	}
}

func (dispatcher *EventDispatcher) EventSink() EventSink {
	if dispatcher == nil || len(dispatcher.sinks) == 0 {
		return nil
	}
	return dispatcher.Emit
}

// RecoveringEventSink prevents an observer panic from unwinding Runtime. The
// optional callback receives the recovered value and must itself not panic.
func RecoveringEventSink(sink EventSink, onPanic func(any)) EventSink {
	if sink == nil {
		return nil
	}
	return func(event Event) {
		defer func() {
			if recovered := recover(); recovered != nil && onPanic != nil {
				func() {
					defer func() { _ = recover() }()
					onPanic(recovered)
				}()
			}
		}()
		sink(cloneRuntimeEvent(event))
	}
}

// EventOverflowPolicy defines the queue-full behavior of BufferedEventSink.
type EventOverflowPolicy string

const (
	EventOverflowBlock      EventOverflowPolicy = "block"
	EventOverflowDropNewest EventOverflowPolicy = "drop_newest"
)

// BufferedEventSink moves downstream observer work off the Runtime call path.
// DropNewest never blocks Runtime and exposes a dropped count; Block preserves
// events and intentionally applies backpressure.
type BufferedEventSink struct {
	downstream EventSink
	policy     EventOverflowPolicy
	queue      chan Event
	stopping   chan struct{}
	done       chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	acceptMu   sync.Mutex
	closing    bool
	senders    sync.WaitGroup
	dropped    atomic.Uint64
}

func NewBufferedEventSink(downstream EventSink, capacity int, policy EventOverflowPolicy) (*BufferedEventSink, error) {
	if downstream == nil || isNilDependency(downstream) {
		return nil, errors.New("agent: buffered event downstream is required")
	}
	if capacity <= 0 {
		return nil, errors.New("agent: buffered event capacity must be positive")
	}
	if policy != EventOverflowBlock && policy != EventOverflowDropNewest {
		return nil, fmt.Errorf("agent: unsupported event overflow policy %q", policy)
	}
	buffered := &BufferedEventSink{
		downstream: RecoveringEventSink(downstream, nil), policy: policy,
		queue: make(chan Event, capacity), stopping: make(chan struct{}),
		done: make(chan struct{}), closed: make(chan struct{}),
	}
	go buffered.run()
	return buffered, nil
}

func (buffered *BufferedEventSink) run() {
	defer close(buffered.closed)
	for {
		select {
		case event := <-buffered.queue:
			buffered.downstream(event)
		case <-buffered.done:
			for {
				select {
				case event := <-buffered.queue:
					buffered.downstream(event)
				default:
					return
				}
			}
		}
	}
}

// EventSink returns the callback passed to RuntimeConfig.EventSink.
func (buffered *BufferedEventSink) EventSink() EventSink {
	if buffered == nil {
		return nil
	}
	return func(event Event) {
		event = cloneRuntimeEvent(event)
		buffered.acceptMu.Lock()
		if buffered.closing {
			buffered.acceptMu.Unlock()
			buffered.dropped.Add(1)
			return
		}
		buffered.senders.Add(1)
		buffered.acceptMu.Unlock()
		defer buffered.senders.Done()
		if buffered.policy == EventOverflowBlock {
			select {
			case buffered.queue <- event:
			case <-buffered.stopping:
				buffered.dropped.Add(1)
			}
			return
		}
		select {
		case buffered.queue <- event:
		default:
			buffered.dropped.Add(1)
		}
	}
}

func (buffered *BufferedEventSink) Dropped() uint64 {
	if buffered == nil {
		return 0
	}
	return buffered.dropped.Load()
}

// Close drains queued events or returns ctx's cause. No new event is accepted
// after Close begins.
func (buffered *BufferedEventSink) Close(ctx context.Context) error {
	if buffered == nil {
		return nil
	}
	buffered.closeOnce.Do(func() {
		buffered.acceptMu.Lock()
		buffered.closing = true
		close(buffered.stopping)
		buffered.acceptMu.Unlock()
		go func() {
			buffered.senders.Wait()
			close(buffered.done)
		}()
	})
	select {
	case <-buffered.closed:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// SanitizedEvent is the public JSON/log view of Event. Trusted Data, raw Error,
// raw provider payloads, and tool argument deltas are excluded.
type SanitizedEvent struct {
	Event
	ErrorMessage string `json:"error_message,omitempty"`
}

func (event SanitizedEvent) MarshalJSON() ([]byte, error) {
	type publicEvent Event
	public := publicEvent(sanitizePublicEvent(event.Event))
	return json.Marshal(struct {
		publicEvent
		ErrorMessage string `json:"error_message,omitempty"`
	}{publicEvent: public, ErrorMessage: event.ErrorMessage})
}

// SanitizeEvent creates the stable public event view.
func SanitizeEvent(event Event) SanitizedEvent {
	event = sanitizePublicEvent(event)
	message := PublicErrorMessage(event.ErrorCode)
	return SanitizedEvent{Event: event, ErrorMessage: message}
}

func sanitizePublicEvent(event Event) Event {
	event = cloneRuntimeEvent(event)
	event.Data = nil
	event.Error = ""
	event.ErrorCode = safePublicErrorCode(event.ErrorCode)
	event.Text = RedactText(event.Text, 4096)
	event.ApprovalReason = RedactText(event.ApprovalReason, 512)
	if event.Chunk != nil {
		event.Chunk.RawJSON = ""
		event.Chunk.ErrorMessage = ""
		event.Chunk.ErrorCode = safePublicErrorCode(event.Chunk.ErrorCode)
		event.Chunk.Delta = RedactText(event.Chunk.Delta, 4096)
		event.Chunk.Arguments = ""
		// Function arguments are trusted only after Runtime validates the final
		// ModelResponse. Incremental and done payloads are never a public view.
		if event.Chunk.Type == ModelStreamToolArgumentsDelta || event.Chunk.Type == ModelStreamToolArgumentsDone {
			event.Chunk.Delta = ""
		}
	}
	return event
}

// EventStream is a non-blocking sanitized event subscription. A slow consumer
// drops newest events and can inspect Dropped to surface the gap.
type EventStream struct {
	mu        sync.RWMutex
	events    chan SanitizedEvent
	closed    bool
	closeOnce sync.Once
	dropped   atomic.Uint64
}

func NewEventStream(capacity int) (*EventStream, error) {
	if capacity <= 0 {
		return nil, errors.New("agent: event stream capacity must be positive")
	}
	return &EventStream{events: make(chan SanitizedEvent, capacity)}, nil
}

func (stream *EventStream) EventSink() EventSink {
	if stream == nil {
		return nil
	}
	return func(event Event) {
		stream.mu.RLock()
		defer stream.mu.RUnlock()
		if stream.closed {
			stream.dropped.Add(1)
			return
		}
		select {
		case stream.events <- SanitizeEvent(event):
		default:
			stream.dropped.Add(1)
		}
	}
}

func (stream *EventStream) Events() <-chan SanitizedEvent {
	if stream == nil {
		return nil
	}
	return stream.events
}

func (stream *EventStream) Dropped() uint64 {
	if stream == nil {
		return 0
	}
	return stream.dropped.Load()
}

func (stream *EventStream) Close() {
	if stream == nil {
		return
	}
	stream.closeOnce.Do(func() {
		stream.mu.Lock()
		defer stream.mu.Unlock()
		stream.closed = true
		close(stream.events)
	})
}

// RedactText removes control characters and bounds one untrusted display/log
// field. It is not a secret detector; hosts must still omit credentials and
// private domain data at their source.
func RedactText(text string, maxRunes int) string {
	if maxRunes <= 0 || !utf8.ValidString(text) {
		return ""
	}
	redacted := make([]rune, 0)
	truncated := false
	for _, character := range text {
		if len(redacted) == maxRunes {
			truncated = true
			break
		}
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			redacted = append(redacted, '�')
		} else {
			redacted = append(redacted, character)
		}
	}
	if truncated {
		redacted[len(redacted)-1] = '…'
	}
	return string(redacted)
}

// RedactOperationResult removes private artifact projections and receipts from
// the log-safe copy while retaining model-visible output.
func RedactOperationResult(result OperationResult) OperationResult {
	return OperationResult{
		Output:        append(json.RawMessage(nil), result.Output...),
		FinalResponse: RedactText(result.FinalResponse, 512),
		Artifacts:     publicResultArtifacts(result.Artifacts),
		Continue:      result.Continue,
	}
}

func cloneRuntimeEvent(event Event) Event {
	event.Data = append(json.RawMessage(nil), event.Data...)
	event.ApprovalPreview = append(json.RawMessage(nil), event.ApprovalPreview...)
	if event.Chunk != nil {
		chunk := *event.Chunk
		if event.Chunk.SequenceNumber != nil {
			sequence := *event.Chunk.SequenceNumber
			chunk.SequenceNumber = &sequence
		}
		if event.Chunk.OutputIndex != nil {
			index := *event.Chunk.OutputIndex
			chunk.OutputIndex = &index
		}
		event.Chunk = &chunk
	}
	return event
}
