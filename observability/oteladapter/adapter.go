// Package oteladapter maps agentruntime's dependency-neutral Event contract to
// OpenTelemetry spans. It intentionally lives outside the core package so hosts
// that do not use OpenTelemetry need no global telemetry setup.
package oteladapter

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/ly95/agentruntime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	Tracer      trace.Tracer
	RootContext context.Context
}

type Adapter struct {
	mu     sync.Mutex
	tracer trace.Tracer
	root   context.Context
	spans  map[spanKey]trace.Span
	closed bool
}

type spanKey struct {
	kind string
	id   string
}

func New(config Config) (*Adapter, error) {
	if config.Tracer == nil || (reflect.ValueOf(config.Tracer).Kind() == reflect.Pointer && reflect.ValueOf(config.Tracer).IsNil()) {
		return nil, errors.New("oteladapter: tracer is required")
	}
	root := config.RootContext
	if root == nil {
		root = context.Background()
	}
	return &Adapter{
		tracer: config.Tracer, root: root, spans: make(map[spanKey]trace.Span),
	}, nil
}

// EventSink returns the callback passed to agentruntime.RuntimeConfig.EventSink.
// Only stable identifiers, enumerations, token counts, and error codes become
// attributes; raw errors, previews, arguments, and trusted Event.Data are never
// recorded.
func (adapter *Adapter) EventSink() agentruntime.EventSink {
	if adapter == nil {
		return nil
	}
	return adapter.observe
}

func (adapter *Adapter) observe(event agentruntime.Event) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.closed {
		return
	}
	switch event.Type {
	case agentruntime.EventRunStarted:
		adapter.start(spanKey{kind: "run", id: event.RunID}, "agentruntime.run", event)
	case agentruntime.EventModelStarted:
		adapter.start(spanKey{kind: "model", id: event.ModelCallID}, "agentruntime.model", event)
	case agentruntime.EventOperationStarted:
		adapter.start(operationSpanKey(event), "agentruntime.operation", event)
	case agentruntime.EventReconciliationStarted:
		adapter.start(reconciliationSpanKey(event), "agentruntime.reconciliation", event)
	case agentruntime.EventModelCompleted, agentruntime.EventModelFailed:
		adapter.end(spanKey{kind: "model", id: event.ModelCallID}, event)
	case agentruntime.EventOperationCompleted, agentruntime.EventOperationFailed,
		agentruntime.EventOperationCancelled:
		adapter.end(operationSpanKey(event), event)
	case agentruntime.EventReconciliationCompleted, agentruntime.EventReconciliationFailed:
		adapter.end(reconciliationSpanKey(event), event)
	case agentruntime.EventRunCompleted, agentruntime.EventRunWaitingUser,
		agentruntime.EventRunFailed, agentruntime.EventRunInterrupted,
		agentruntime.EventRunCancelled:
		adapter.end(spanKey{kind: "run", id: event.RunID}, event)
	case agentruntime.EventSessionLeaseRenewed:
		adapter.addRunEvent(event, "session.lease_renewed")
	case agentruntime.EventApprovalRequested:
		adapter.addRunEvent(event, "approval.requested")
	case agentruntime.EventApprovalCompleted:
		adapter.addRunEvent(event, "approval.completed")
	case agentruntime.EventApprovalFailed:
		adapter.addRunEvent(event, "approval.failed")
	}
}

func operationSpanKey(event agentruntime.Event) spanKey {
	id := event.ExecutionID
	if id == "" {
		id = event.CallID
	}
	return spanKey{kind: "operation", id: id}
}

func reconciliationSpanKey(event agentruntime.Event) spanKey {
	return spanKey{kind: "reconciliation", id: event.ExecutionID + "\x00" + event.AttemptID}
}

func (adapter *Adapter) start(key spanKey, name string, event agentruntime.Event) {
	if key.id == "" {
		return
	}
	if _, exists := adapter.spans[key]; exists {
		return
	}
	_, span := adapter.tracer.Start(adapter.root, name, trace.WithAttributes(eventAttributes(event)...))
	adapter.spans[key] = span
}

func (adapter *Adapter) end(key spanKey, event agentruntime.Event) {
	span, exists := adapter.spans[key]
	if !exists {
		return
	}
	delete(adapter.spans, key)
	span.SetAttributes(eventAttributes(event)...)
	if event.ErrorCode != "" || event.Type == agentruntime.EventModelFailed ||
		event.Type == agentruntime.EventOperationFailed ||
		event.Type == agentruntime.EventReconciliationFailed ||
		event.Type == agentruntime.EventRunFailed ||
		event.Type == agentruntime.EventRunInterrupted {
		span.SetStatus(codes.Error, agentruntime.PublicErrorMessage(event.ErrorCode))
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func (adapter *Adapter) addRunEvent(event agentruntime.Event, name string) {
	span, exists := adapter.spans[spanKey{kind: "run", id: event.RunID}]
	if !exists {
		return
	}
	span.AddEvent(name, trace.WithAttributes(eventAttributes(event)...))
}

func eventAttributes(event agentruntime.Event) []attribute.KeyValue {
	attributes := make([]attribute.KeyValue, 0, 13)
	addString := func(key, value string) {
		if value != "" {
			attributes = append(attributes, attribute.String(key, value))
		}
	}
	addString("agentruntime.event.type", string(event.Type))
	addString("agentruntime.run.id", event.RunID)
	addString("agentruntime.session.id", event.SessionID)
	addString("agentruntime.model_call.id", event.ModelCallID)
	addString("agentruntime.operation.name", event.Operation)
	addString("agentruntime.operation.call_id", event.CallID)
	addString("agentruntime.operation.execution_id", event.ExecutionID)
	addString("agentruntime.operation.attempt_id", event.AttemptID)
	addString("agentruntime.reconciliation.action", event.Reconciliation)
	addString("error.type", event.ErrorCode)
	if event.InputTokens != 0 {
		attributes = append(attributes, attribute.Int("gen_ai.usage.input_tokens", event.InputTokens))
	}
	if event.OutputTokens != 0 {
		attributes = append(attributes, attribute.Int("gen_ai.usage.output_tokens", event.OutputTokens))
	}
	if event.TotalTokens != 0 {
		attributes = append(attributes, attribute.Int("agentruntime.usage.total_tokens", event.TotalTokens))
	}
	if event.LeaseGeneration != 0 {
		attributes = append(attributes, attribute.Int64("agentruntime.session.lease_generation", int64(event.LeaseGeneration)))
	}
	return attributes
}

// Close ends any spans still open because the process stopped before a terminal
// event. It is idempotent and never flushes or shuts down the host-owned tracer
// provider.
func (adapter *Adapter) Close() {
	if adapter == nil {
		return
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.closed {
		return
	}
	adapter.closed = true
	for key, span := range adapter.spans {
		span.SetStatus(codes.Error, "event stream closed before terminal event")
		span.End()
		delete(adapter.spans, key)
	}
}
