package oteladapter

import (
	"testing"

	"github.com/ly95/agentruntime"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestAdapterCreatesSafeCoreSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })
	adapter, err := New(Config{Tracer: provider.Tracer("test")})
	if err != nil {
		t.Fatal(err)
	}
	sink := adapter.EventSink()
	sink(agentruntime.Event{Type: agentruntime.EventRunStarted, RunID: "run-1", Data: []byte(`{"secret":true}`)})
	sink(agentruntime.Event{Type: agentruntime.EventSessionLeaseRenewed, RunID: "run-1", LeaseGeneration: 2})
	sink(agentruntime.Event{Type: agentruntime.EventModelStarted, RunID: "run-1", ModelCallID: "model-1"})
	sink(agentruntime.Event{
		Type: agentruntime.EventModelCompleted, RunID: "run-1", ModelCallID: "model-1",
		InputTokens: 10, OutputTokens: 4, TotalTokens: 14,
	})
	sink(agentruntime.Event{Type: agentruntime.EventRunCompleted, RunID: "run-1"})
	adapter.Close()

	ended := recorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("ended spans=%d, want run and model", len(ended))
	}
	for _, span := range ended {
		for _, value := range span.Attributes() {
			if value.Key == "secret" || value.Value.AsString() == `{"secret":true}` {
				t.Fatalf("trusted event data escaped into span: %+v", value)
			}
		}
	}
}
