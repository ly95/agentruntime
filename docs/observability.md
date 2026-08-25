# Events, metrics, tracing, and UI

Runtime calls one `EventDispatcher` boundary. Each observer receives a defensive
copy and observer panics are contained. Concurrent runs may invoke the
dispatcher concurrently; choose an adapter based on the observer's behavior:

| Adapter | Behavior |
| --- | --- |
| `RecoveringEventSink` | contains a single observer panic |
| `BufferedEventSink` + `block` | preserves events and intentionally applies backpressure |
| `BufferedEventSink` + `drop_newest` | never waits for a full queue; exposes `Dropped()` |
| `EventStream` | sanitized, bounded, drop-newest subscription for UI transport |
| `MetricsEventSink` | maps events to dependency-neutral counters |
| `observability/oteladapter` | creates host-owned OpenTelemetry spans |

Call `BufferedEventSink.Close(ctx)` during shutdown. It rejects new sends and
drains the accepted queue within the supplied context. A blocking downstream
may make Close return the context cause; that is an explicit telemetry shutdown
failure, not a runtime failure.

## Public JSON contract

Serialize `SanitizedEvent`, not the trusted internal `Event`. Sanitization
removes `Data`, raw error text, provider raw JSON, and tool argument
deltas/completions. It also bounds display text to 4,096 runes, approval reasons
to 512 runes, replaces non-newline control characters, and normalizes public
error codes. These guarantees apply to the returned struct as well as its JSON
encoding. `PlanBatch` is omitted when zero. Approval previews remain JSON data
and must follow the rendering rules in
[approval-ui-security.md](approval-ui-security.md).

`CoreUIEvent` selects only run, approval, and reconciliation state changes. UIs
should refresh the durable run/approval record when `EventStream.Dropped()` is
nonzero; the event stream is notification, not authority.

## Metric vocabulary

- `agentruntime.model.iterations` — model calls started;
- `agentruntime.model.input_tokens`, `.output_tokens`, `.total_tokens` — one
  completed model call's provider usage;
- `agentruntime.session.lease_renewals` — successful renewals;
- `agentruntime.operation.reconciliations` — completed/failed decisions;
- `agentruntime.runs` — terminal or waiting run outcomes.

Metric attributes contain operation name, reconciliation action, stable error
code, and lease generation. They intentionally omit user/session content and
raw dependency errors. Hosts can attach tenant or deployment dimensions outside
the runtime after applying their own cardinality policy.

## OpenTelemetry

```go
adapter, err := oteladapter.New(oteladapter.Config{
    Tracer: tracerProvider.Tracer("agentruntime-host"),
})
if err != nil { return err }
defer adapter.Close()

runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
    // ...
    EventSink: agentruntime.NewEventDispatcher(
        adapter.EventSink(),
        agentruntime.MetricsEventSink(recordMetric),
    ).EventSink(),
})
```

The adapter creates run, model, operation, and reconciliation spans and adds
lease/approval/MCP diagnostic events to the run span. It records IDs, enum
values, token counts, and error codes only. The tracer provider remains owned by
the host; `Adapter.Close` does not shut it down.
