# Provider mapping and compatibility

The public `Model` interface is provider-neutral. OpenAI transport code is split
by responsibility:

- `openai_transport.go` owns request/stream orchestration;
- `openai_mapping.go` maps validated SDK objects to runtime types;
- `openai_stream.go` owns ordered stream state;
- `openai_validate.go` owns closed-schema reflection;
- `openai_validate_deep.go` owns identity validation, while
  `openai_validate_{types,response,items,events}.go` own the other semantic domains;
- `provider_error.go` maps provider status/code metadata to stable categories.

Provider raw JSON is never public event JSON. Unsupported fields and
contradictory lifecycle evidence fail explicitly instead of being ignored.

OpenAI Responses remains the only bundled model adapter. The readiness work
below freezes contracts for adapters; it does not implement or select a second
provider.

## Public provider-neutral conformance corpus

Every model adapter must run the exported fixed v1 corpus from `modeltest` in
its own tests:

```go
func TestModelConformance(t *testing.T) {
    modeltest.RunModelConformance(t, func(t *testing.T, scenario modeltest.Scenario) agentruntime.BoundModel {
        return newFixtureBackedModel(t, scenario.Name(), scenario.PayloadMarker())
    })
}
```

The factory owns provider wire fixtures, mock servers, SDK objects, and raw
payloads. It must implement every opaque scenario. The runner has no capability
flags, provider branches, fallback paths, or optional members; calling
`testing.T.Skip` is not conformant. A provider that cannot represent one member
is not a compatible adapter.

The v1 rules are fixed:

- **Binding:** every fixture returns a non-nil `BoundModel` whose five binding
  components are valid and whose `Binding()` value is stable before, during, and
  after the scenario.
- **Successful lifecycle:** text, refusal, reasoning, function-call, streaming,
  and usage fixtures return a non-nil response and nil error. There is exactly
  one first `response_started` event and one last `response_done` event; response
  and item identities agree, output items are unique and closed to the public
  message/reasoning/function-call union, the complete response passes
  `ValidateModelResponse` (including every `ValidateModelOutputItem` invariant
  and aggregate text/refusal replay), and every final item has exactly one
  ordered add and done event with matching index. Scenario output type order is
  exact, not just a set of required types. Function-call IDs, names, argument
  deltas, and final arguments must reconcile. Any sequence numbers that
  are present strictly increase. The streaming fixture has at least two ordered
  text deltas between item add and item done whose concatenation equals final
  text. Usage is non-negative, `total=input+output`, and the dedicated usage
  fixture requires positive counts.
- **Replay:** `Raw` is an adapter-owned replay envelope in agentruntime's
  canonical message/reasoning/function-call schema. An adapter may use an exact
  provider object when it already has that shape; otherwise it must map native
  evidence into the canonical envelope and reverse the same versioned mapping on
  input. The adapter must accept reasoning, message, and function-call envelopes
  followed by the matching tool result and produce a continuation with a new
  response ID. Dropping required replay evidence or changing the mapping without
  changing `AdapterVersion` is not allowed.
- **Concurrency and sink lifetime:** the same `BoundModel` handles two
  concurrently invoked successful calls with isolated response/item identities.
  `Binding()` remains stable and returns while each call is active. A terminal
  `StreamSink` callback is synchronous backpressure: `Complete` cannot return
  before that callback returns and cannot use the sink afterward.
- **Cancellation:** a pre-cancelled call and a cancellation triggered by the
  first `response_started` event must return within the fixed five-second corpus
  timeout with a nil response and an error matching `context.Canceled`.
  Pre-cancellation emits no start; post-start cancellation emits exactly one
  start; neither emits
  `response_done` or becomes a provider/retryable error.
- **Errors:** authentication, quota, rate-limit, rejected-request, and transient
  fixtures map to exactly one stable `ProviderError` category and sentinel.
  Only rate-limit and transient categories are retryable. Provider-private
  payload detail remains available only through trusted fields/private causes;
  it cannot enter public event JSON, semantic response fields,
  `ProviderError` public fields/JSON, or the returned top-level error string.
  The OpenAI adapter therefore keeps raw provider request IDs and unknown codes
  only in the trusted SDK error cause.
- **Invalid output:** unknown output, duplicate evidence, reordered evidence,
  contradictory identity, and partial completion all return a nil response with
  `ErrInvalidModelOutput`, no `response_done`, and no provider or retryable
  classification.

Passing this corpus is mandatory for every adapter, but it does not weaken the
provider-specific closed-schema and golden-protocol tests.

## Immutable durable model binding

`ModelBinding` is the immutable five-tuple `(provider, model, endpoint class,
credential principal, adapter version)`. A Runtime configured with `RunStore`
requires its `Model` to implement `BoundModel`; construction fails rather than
running durably with unknown model authority.

`ModelBinding.ID` hashes the versioned five-tuple with SHA-256 and returns a
`model_binding_...` identifier. Durable runtime records persist only this ID in
`RunRecord`, `SessionState`, and approval checkpoints, not the tuple itself.
`EndpointClass` and `CredentialPrincipal` are stable, non-secret host labels:
never put an endpoint URL, API key, bearer token, or other secret in either
field. Generic `ModelBinding.Validate` can enforce canonical text but cannot
reliably determine whether arbitrary text is secret; hosts remain responsible
for that boundary. `NewOpenAIModel` additionally rejects obvious URL, `sk-`, and
Bearer-token forms to catch common misconfiguration.

`AdapterVersion` identifies the adapter's mapping and replay contract, not the
`agentruntime` module version. Changing it creates a new binding, as does
changing any other tuple member. Keep the old runtime binary and adapter
configuration available to drain sessions created under the old binding; a new
runtime must not claim them. Secret rotation may retain the binding only when
the authenticated principal is unchanged and its stable
`CredentialPrincipal` label remains the same.

`RunStore` V4 compares the incoming binding ID with the run, session, active
owner, and approval checkpoint before callbacks, lease acquisition, or fencing.
Runtime compares the frozen `Binding()` value before any durable run-store
mutation, before and after every durable `Complete` call, after an approval
resumer callback, and again before reserving a resumed operation plan. Drift
fails with `ErrModelBindingMismatch`; a drifted model response or approved write
is not committed or executed.
`Model.Complete` and `BoundModel.Binding` may be called concurrently for
independent runs and must be concurrency-safe. A different or empty legacy ID
returns `ErrModelBindingMismatch` without mutation. Runtime and store adapters
do not infer, backfill, or upgrade an empty
legacy binding. See the [store adapter guide](store-adapter-guide.md) for rollout
requirements.

## No substitution or hidden retry

Fallback, provider downgrade, and provider/model substitution are forbidden.
The bundled OpenAI adapter sends the configured model and rejects every response
lifecycle event whose `response.model` is missing or differs; immutable
response-envelope validation also prevents later model drift. It does not silently accept a provider
alias or a compatibility endpoint that substitutes another model.

The injected `openai.Client` owns authentication, endpoint selection,
middleware, timeout, transport, and retry policy. Any bounded retry before a
stream starts is therefore an explicit host decision. Runtime does not silently
retry `Model.Complete`, replay a semantic turn after response evidence, or
select a replacement provider/model. Cancellation and ambiguous partial streams
fail explicitly.

## Updating the bundled OpenAI adapter

Before bumping `openai-go`:

1. run `go test ./...` against `testdata/openai/*.sse`;
2. add a golden stream for each newly accepted lifecycle/union shape;
3. keep a negative golden for new authority fields until validation is updated;
4. run the fixed `modeltest` corpus and provider-specific tests;
5. run fuzz, race, Go 1.26, Windows, and macOS CI;
6. run the public API diff and classify every reported change in release notes.

Golden files are provider protocol evidence, not snapshots to regenerate
blindly. Review additions for authorization, identity, ordering, binding, and
public-data impact.
