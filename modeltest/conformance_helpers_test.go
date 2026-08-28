package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	agent "github.com/ly95/agentruntime"
)

const helperSkipModeFlag = "modeltest-factory-skip-mode"

var helperSkipMode = flag.String(helperSkipModeFlag, "", "run the factory-skip subprocess helper")

func TestRunModelConformanceConvertsFactorySkipToFailure(t *testing.T) {
	if *helperSkipMode != "" {
		originalCorpus := v1Corpus
		defer func() { v1Corpus = originalCorpus }()
		for index := range v1Corpus {
			v1Corpus[index].kind = scenarioBinding
		}

		RunModelConformance(t, func(t *testing.T, _ Scenario) agent.BoundModel {
			switch *helperSkipMode {
			case "Skip":
				t.Skip("factory skip must fail conformance")
			case "SkipNow":
				t.SkipNow()
			case "CleanupSkip":
				t.Cleanup(func() { t.Skip("factory cleanup skip must fail conformance") })
			case "CleanupSkipNow":
				t.Cleanup(func() { t.SkipNow() })
			default:
				t.Fatalf("unknown helper skip mode %q", *helperSkipMode)
			}
			return &helperBoundModel{bindings: []agent.ModelBinding{validHelperBinding()}}
		})
		return
	}

	for _, mode := range []string{"Skip", "SkipNow", "CleanupSkip", "CleanupSkipNow"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(
				os.Args[0],
				"-test.run=^TestRunModelConformanceConvertsFactorySkipToFailure$",
				"-test.count=1",
				"-"+helperSkipModeFlag+"="+mode,
			)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("factory %s subprocess passed, want conformance failure\n%s", mode, output)
			}
			if !strings.Contains(string(output), "factory called t.Skip or t.SkipNow") {
				t.Fatalf("factory %s failure omitted mandatory-corpus diagnostic\n%s", mode, output)
			}
		})
	}
}

func TestValidateFactoryModelRejectsNilAndTypedNil(t *testing.T) {
	valid := &helperBoundModel{bindings: []agent.ModelBinding{validHelperBinding()}}
	if err := validateFactoryModel(valid); err != nil {
		t.Fatalf("valid BoundModel rejected: %v", err)
	}

	var nilModel agent.BoundModel
	if err := validateFactoryModel(nilModel); err == nil || !strings.Contains(err.Error(), "nil agentruntime.BoundModel") {
		t.Fatalf("nil BoundModel error=%v", err)
	}

	var typedNil *helperBoundModel
	if err := validateFactoryModel(typedNil); err == nil || !strings.Contains(err.Error(), "typed-nil") {
		t.Fatalf("typed-nil BoundModel error=%v", err)
	}
}

func TestValidatedModelBindingChecksStabilityAndEveryMarkerField(t *testing.T) {
	const marker = "modeltest_private_payload::binding_helper::v1_canary"
	binding := validHelperBinding()
	model := &helperBoundModel{bindings: []agent.ModelBinding{binding}}
	if got, err := validatedModelBinding(model, marker); err != nil || got != binding {
		t.Fatalf("valid binding got=%+v error=%v", got, err)
	}

	unstable := binding
	unstable.Model = "modeltest-v2"
	if _, err := validatedModelBinding(&helperBoundModel{bindings: []agent.ModelBinding{binding, unstable}}, marker); err == nil || !strings.Contains(err.Error(), "unstable") {
		t.Fatalf("unstable binding error=%v", err)
	}

	tests := []struct {
		name   string
		mutate func(*agent.ModelBinding)
	}{
		{name: "Provider", mutate: func(binding *agent.ModelBinding) { binding.Provider += marker }},
		{name: "Model", mutate: func(binding *agent.ModelBinding) { binding.Model += marker }},
		{name: "EndpointClass", mutate: func(binding *agent.ModelBinding) { binding.EndpointClass += marker }},
		{name: "CredentialPrincipal", mutate: func(binding *agent.ModelBinding) { binding.CredentialPrincipal += marker }},
		{name: "AdapterVersion", mutate: func(binding *agent.ModelBinding) { binding.AdapterVersion += marker }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leaking := binding
			test.mutate(&leaking)
			_, err := validatedModelBinding(&helperBoundModel{bindings: []agent.ModelBinding{leaking}}, marker)
			if err == nil || !strings.Contains(err.Error(), test.name) || !strings.Contains(err.Error(), "PayloadMarker") {
				t.Fatalf("marker-bearing %s error=%v", test.name, err)
			}
		})
	}
}

func TestValidateSuccessfulLifecycleReconcilesAllItemEvidence(t *testing.T) {
	response, events := validHelperLifecycle()
	if err := validateSuccessfulLifecycle(response, events); err != nil {
		t.Fatalf("valid lifecycle rejected: %v", err)
	}

	tests := []struct {
		name   string
		want   string
		mutate func([]agent.ModelStreamEvent) []agent.ModelStreamEvent
	}{
		{
			name: "item added order",
			want: "want next final item index",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				events[4].OutputIndex = helperInt64(0)
				events[4].ItemID = "message-1"
				return events
			},
		},
		{
			name: "missing item done",
			want: "item_done",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				return append(events[:8], events[9:]...)
			},
		},
		{
			name: "function call id",
			want: "call ID",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				events[4].CallID = "contradictory-call"
				return events
			},
		},
		{
			name: "missing arguments done",
			want: "tool_arguments_done",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				return append(events[:7], events[8:]...)
			},
		},
		{
			name: "final arguments mismatch",
			want: "arguments do not match",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				events[7].Arguments = `{"value":"wrong"}`
				return events
			},
		},
		{
			name: "argument delta mismatch",
			want: "argument deltas do not match",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				events[6].Delta = `"wrong"}`
				return events
			},
		},
		{
			name: "duplicate started",
			want: "response_started=2",
			mutate: func(events []agent.ModelStreamEvent) []agent.ModelStreamEvent {
				events[2].Type = agent.ModelStreamResponseStarted
				events[2].ItemID = ""
				events[2].OutputIndex = nil
				return events
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, candidate := validHelperLifecycle()
			candidate = test.mutate(candidate)
			err := validateSuccessfulLifecycle(response, candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("lifecycle error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateProviderErrorPrivacyAndChain(t *testing.T) {
	const marker = "modeltest_private_payload::provider_helper::v1_canary"
	want := providerFailureExpectation{
		category: agent.ProviderErrorAuthentication,
		sentinel: agent.ErrProviderAuthentication,
	}
	valid := newHelperProviderError(t, agent.ProviderErrorAuthentication, "safe-code", "safe-request", errors.New("private cause: "+marker))
	if err := validateProviderErrorPrivacyAndChain(valid, valid, marker, want); err != nil {
		t.Fatalf("valid ProviderError rejected: %v", err)
	}

	withoutMarker := newHelperProviderError(t, agent.ProviderErrorAuthentication, "safe-code", "safe-request", errors.New("private cause without canary"))
	if err := validateProviderErrorPrivacyAndChain(withoutMarker, withoutMarker, marker, want); err == nil || !strings.Contains(err.Error(), "trusted error cause") {
		t.Fatalf("missing private marker error=%v", err)
	}

	inner := newHelperProviderError(t, agent.ProviderErrorTransient, "transient", "inner-request", errors.New("inner: "+marker))
	outerCause := fmt.Errorf("private cause %s: %w", marker, inner)
	outer := newHelperProviderError(t, agent.ProviderErrorAuthentication, "authentication", "outer-request", outerCause)
	if err := validateProviderErrorPrivacyAndChain(outer, outer, marker, want); err == nil || !strings.Contains(err.Error(), "contradictory category") {
		t.Fatalf("contradictory provider chain error=%v", err)
	}
}

func TestValidateProviderErrorPublicDataRejectsEveryStringField(t *testing.T) {
	const marker = "modeltest_private_payload::provider_public::v1_canary"
	tests := []struct {
		name   string
		mutate func(*agent.ProviderError)
	}{
		{name: "Provider", mutate: func(providerErr *agent.ProviderError) { providerErr.Provider = marker }},
		{name: "Category", mutate: func(providerErr *agent.ProviderError) { providerErr.Category = agent.ProviderErrorCategory(marker) }},
		{name: "Code", mutate: func(providerErr *agent.ProviderError) { providerErr.Code = marker }},
		{name: "RequestID", mutate: func(providerErr *agent.ProviderError) { providerErr.RequestID = marker }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			providerErr := newHelperProviderError(t, agent.ProviderErrorAuthentication, "safe-code", "safe-request", errors.New("private: "+marker))
			test.mutate(providerErr)
			err := validateProviderErrorPublicData(providerErr, marker)
			if err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("public %s marker error=%v", test.name, err)
			}
		})
	}
}

func TestAssertPublicStreamBoundaryAllowsTrustedMarkerFields(t *testing.T) {
	scenario := newCorpusScenario("helper/trusted_stream_fields", scenarioSuccessText)
	assertPublicStreamBoundary(t, scenario, []agent.ModelStreamEvent{{
		Type:         agent.ModelStreamError,
		RawJSON:      `{"private":"` + scenario.PayloadMarker() + `"}`,
		ErrorMessage: "private detail: " + scenario.PayloadMarker(),
	}}, true)
}

type helperBoundModel struct {
	bindings     []agent.ModelBinding
	bindingCalls int
}

var _ agent.BoundModel = (*helperBoundModel)(nil)

func (*helperBoundModel) Complete(context.Context, agent.ModelRequest) (*agent.ModelResponse, error) {
	return nil, errors.New("helper BoundModel Complete should not be called")
}

func (model *helperBoundModel) Binding() agent.ModelBinding {
	index := model.bindingCalls
	model.bindingCalls++
	if index >= len(model.bindings) {
		index = len(model.bindings) - 1
	}
	return model.bindings[index]
}

func validHelperBinding() agent.ModelBinding {
	return agent.ModelBinding{
		Provider:            "modeltest",
		Model:               "modeltest-v1",
		EndpointClass:       "test",
		CredentialPrincipal: "modeltest-principal",
		AdapterVersion:      "v1",
	}
}

func validHelperLifecycle() (*agent.ModelResponse, []agent.ModelStreamEvent) {
	arguments := json.RawMessage(`{"value":"x"}`)
	response := &agent.ModelResponse{
		ID: "response-1",
		Items: []agent.ModelOutputItem{
			{ID: "message-1", Type: agent.ModelOutputMessage},
			{
				ID: "function-1", Type: agent.ModelOutputFunctionCall,
				Call: &agent.ToolCall{ID: "call-1", Name: "echo", Input: arguments},
			},
		},
	}
	events := []agent.ModelStreamEvent{
		{Type: agent.ModelStreamResponseStarted, SequenceNumber: helperInt64(0), ResponseID: response.ID},
		{Type: agent.ModelStreamItemAdded, SequenceNumber: helperInt64(1), ResponseID: response.ID, ItemID: "message-1", OutputIndex: helperInt64(0)},
		{Type: agent.ModelStreamTextDelta, SequenceNumber: helperInt64(2), ResponseID: response.ID, ItemID: "message-1", OutputIndex: helperInt64(0), Delta: "answer"},
		{Type: agent.ModelStreamItemDone, SequenceNumber: helperInt64(3), ResponseID: response.ID, ItemID: "message-1", OutputIndex: helperInt64(0)},
		{Type: agent.ModelStreamItemAdded, SequenceNumber: helperInt64(4), ResponseID: response.ID, ItemID: "function-1", OutputIndex: helperInt64(1), CallID: "call-1"},
		{Type: agent.ModelStreamToolArgumentsDelta, SequenceNumber: helperInt64(5), ResponseID: response.ID, ItemID: "function-1", OutputIndex: helperInt64(1), CallID: "call-1", Delta: `{"value":`},
		{Type: agent.ModelStreamToolArgumentsDelta, SequenceNumber: helperInt64(6), ResponseID: response.ID, ItemID: "function-1", OutputIndex: helperInt64(1), CallID: "call-1", Delta: `"x"}`},
		{Type: agent.ModelStreamToolArgumentsDone, SequenceNumber: helperInt64(7), ResponseID: response.ID, ItemID: "function-1", OutputIndex: helperInt64(1), CallID: "call-1", Name: "echo", Arguments: string(arguments)},
		{Type: agent.ModelStreamItemDone, SequenceNumber: helperInt64(8), ResponseID: response.ID, ItemID: "function-1", OutputIndex: helperInt64(1), CallID: "call-1"},
		{Type: agent.ModelStreamResponseDone, SequenceNumber: helperInt64(9), ResponseID: response.ID},
	}
	return response, events
}

func newHelperProviderError(t *testing.T, category agent.ProviderErrorCategory, code, requestID string, cause error) *agent.ProviderError {
	t.Helper()
	providerErr, err := agent.NewProviderError("modeltest", category, code, 400, requestID, cause)
	if err != nil {
		t.Fatalf("NewProviderError: %v", err)
	}
	return providerErr
}

func helperInt64(value int64) *int64 { return &value }
