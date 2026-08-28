// Package modeltest provides a provider-neutral black-box conformance suite for
// agentruntime model adapters.
//
// Adapter packages call RunModelConformance from their own tests and provide a
// Factory that maps each opaque Scenario to adapter-owned provider fixtures. The
// scenario name selects one fixed v1 behavior; PayloadMarker returns a canary
// for provider-private payload or error detail. Successful, post-start-cancel,
// and invalid-output fixtures put the canary in raw data retained by a trusted
// ModelStreamEvent field. Provider-error fixtures retain it in the private cause
// of their ProviderError. The binding and pre-canceled scenarios do not contact
// a provider and therefore need no canary evidence. The suite verifies that the
// canary never crosses public stream JSON, semantic model output, ModelBinding,
// ProviderError public fields/JSON, or a returned top-level error string.
// RawJSON and ErrorMessage remain trusted in-process fields and may retain it.
//
// This package intentionally defines neither a provider wire DSL nor provider
// raw fixtures. A Factory owns protocol framing, mock servers, SDK objects, and
// raw fixture data for the adapter under test. It must return a non-nil
// agentruntime.BoundModel for every scenario; typed nils are invalid. Calling
// testing.T.Skip or testing.T.SkipNow is converted to a failure, and the v1
// corpus has no capability flags.
//
// The v1 success fixtures produce, respectively, one text message, one refusal
// message, reasoning followed by text, one call to the offered modeltest_echo
// tool, at least two text deltas matching final text, positive internally
// consistent usage, and a replayable reasoning/message/tool response followed
// by a successful tool-result continuation. Raw output uses agentruntime's
// canonical adapter replay envelopes; adapters own the versioned mapping to and
// from provider-native evidence. Output type order is exact, and
// every response must pass agentruntime.ValidateModelResponse (including each
// ValidateModelOutputItem invariant) before it is treated as replayable. Every
// successful response item has exactly one ordered
// item_added/item_done pair at its final OutputIndex; function calls additionally
// reconcile CallID, Name, and argument evidence. The concurrency fixture invokes
// two successful calls on the same BoundModel, probes Binding while each call is
// active, requires distinct response/item identities, and blocks each terminal
// StreamSink to verify synchronous callback lifetime.
// Cancellation fixtures cover a
// context canceled before Complete and cancellation synchronously triggered by
// response_started. Provider-error fixtures cover authentication, quota, rate
// limit, rejected, and transient categories. Invalid-output fixtures present an
// unknown output, duplicate evidence, reordered evidence, contradictory
// identity, or a stream ending after partial evidence. The provider-specific
// representation of all such evidence remains wholly owned by the Factory.
//
// A typical adapter test has this shape:
//
//	func TestModelConformance(t *testing.T) {
//		modeltest.RunModelConformance(t, func(t *testing.T, scenario modeltest.Scenario) agentruntime.BoundModel {
//			return newFixtureBackedModel(t, scenario.Name(), scenario.PayloadMarker())
//		})
//	}
package modeltest
