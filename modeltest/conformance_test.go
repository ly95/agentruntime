package modeltest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	agent "github.com/ly95/agentruntime"
	"github.com/ly95/agentruntime/modeltest"
)

const fakeToolName = "modeltest_echo"

func TestFakeAdapterConformance(t *testing.T) {
	modeltest.RunModelConformance(t, func(t *testing.T, scenario modeltest.Scenario) agent.BoundModel {
		t.Helper()
		return &fakeAdapter{
			binding: newFakeBinding(t),
			name:    scenario.Name(),
			marker:  scenario.PayloadMarker(),
		}
	})
}

type fakeAdapter struct {
	binding agent.ModelBinding
	name    string
	marker  string

	mu    sync.Mutex
	calls int
}

var _ agent.BoundModel = (*fakeAdapter)(nil)

func (adapter *fakeAdapter) Binding() agent.ModelBinding { return adapter.binding }

func (adapter *fakeAdapter) Complete(ctx context.Context, request agent.ModelRequest) (*agent.ModelResponse, error) {
	adapter.mu.Lock()
	adapter.calls++
	call := adapter.calls
	adapter.mu.Unlock()

	switch adapter.name {
	case modeltest.ScenarioV1Binding:
		return nil, errors.New("fake adapter: binding scenario must not call Complete")
	case modeltest.ScenarioV1CancelPreCanceled:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("fake adapter: expected a pre-canceled context")
	case modeltest.ScenarioV1CancelAfterResponseStarted:
		responseID := adapter.responseID(call)
		adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
		<-ctx.Done()
		adapter.emit(request, agent.ModelStreamEvent{
			Type:           agent.ModelStreamError,
			ProviderType:   "fake.transport.canceled",
			ModelCallID:    request.ModelCallID,
			SequenceNumber: fakeInt64(1),
			ResponseID:     responseID,
			ErrorCode:      "canceled",
			ErrorMessage:   adapter.marker,
			RawJSON:        adapter.privateRaw("canceled"),
		})
		return nil, ctx.Err()
	case modeltest.ScenarioV1ErrorAuthentication:
		return adapter.providerFailure(request, agent.ProviderErrorAuthentication, "invalid_credentials", 401)
	case modeltest.ScenarioV1ErrorQuota:
		return adapter.providerFailure(request, agent.ProviderErrorQuota, "quota_exceeded", 429)
	case modeltest.ScenarioV1ErrorRateLimit:
		return adapter.providerFailure(request, agent.ProviderErrorRateLimit, "rate_limited", 429)
	case modeltest.ScenarioV1ErrorRejected:
		return adapter.providerFailure(request, agent.ProviderErrorRejected, "request_rejected", 400)
	case modeltest.ScenarioV1ErrorTransient:
		return adapter.providerFailure(request, agent.ProviderErrorTransient, "temporarily_unavailable", 503)
	case modeltest.ScenarioV1InvalidUnknownOutput,
		modeltest.ScenarioV1InvalidDuplicateOutput,
		modeltest.ScenarioV1InvalidReorderedOutput,
		modeltest.ScenarioV1InvalidContradictoryID,
		modeltest.ScenarioV1InvalidPartialCompletion:
		return adapter.invalidOutput(request, call)
	case modeltest.ScenarioV1SuccessText:
		return adapter.textSuccess(request, call, "fake text response", agent.Usage{})
	case modeltest.ScenarioV1SuccessRefusal:
		return adapter.refusalSuccess(request, call)
	case modeltest.ScenarioV1SuccessReasoning:
		return adapter.reasoningSuccess(request, call)
	case modeltest.ScenarioV1SuccessTool:
		return adapter.toolSuccess(request, call)
	case modeltest.ScenarioV1SuccessStream:
		return adapter.streamSuccess(request, call)
	case modeltest.ScenarioV1SuccessUsage:
		return adapter.textSuccess(request, call, "usage mapped", agent.Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 12})
	case modeltest.ScenarioV1SuccessReplay:
		return adapter.replaySuccess(request, call)
	case modeltest.ScenarioV1ConcurrencyBoundModel:
		return adapter.textSuccess(request, call, "concurrent response", agent.Usage{})
	default:
		return nil, errors.New("fake adapter: unsupported modeltest scenario")
	}
}

func (adapter *fakeAdapter) textSuccess(request agent.ModelRequest, call int, text string, usage agent.Usage) (*agent.ModelResponse, error) {
	responseID := adapter.responseID(call)
	item := fakeMessageItem(responseID+"-message", text, "")
	adapter.emitTextLifecycle(request, responseID, item.ID, []string{text})
	return &agent.ModelResponse{
		ID: responseID, OutputText: text, Items: []agent.ModelOutputItem{item},
		FinishReason: "completed", Usage: usage,
	}, nil
}

func (adapter *fakeAdapter) refusalSuccess(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	responseID := adapter.responseID(call)
	refusal := "fake refusal"
	item := fakeMessageItem(responseID+"-message", "", refusal)
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, item.ID, 0))
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamRefusalDelta, ProviderType: "fake.refusal.delta",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(2),
		ResponseID: responseID, ItemID: item.ID, OutputIndex: fakeInt64(0), Delta: refusal,
		RawJSON: adapter.privateRaw("refusal_delta"),
	})
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 3, responseID, item.ID, 0))
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseDone, 4, responseID, ""))
	return &agent.ModelResponse{
		ID: responseID, Refusal: refusal, Items: []agent.ModelOutputItem{item},
		FinishReason: "completed",
	}, nil
}

func (adapter *fakeAdapter) reasoningSuccess(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	responseID := adapter.responseID(call)
	reasoning := fakeReasoningItem(responseID + "-reasoning")
	message := fakeMessageItem(responseID+"-message", "reasoned answer", "")
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, reasoning.ID, 0))
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamReasoningSummaryDelta, ProviderType: "fake.reasoning.delta",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(2),
		ResponseID: responseID, ItemID: reasoning.ID, OutputIndex: fakeInt64(0), Delta: "checked",
		RawJSON: adapter.privateRaw("reasoning_delta"),
	})
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 3, responseID, reasoning.ID, 0))
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 4, responseID, message.ID, 1))
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamTextDelta, ProviderType: "fake.text.delta",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(5),
		ResponseID: responseID, ItemID: message.ID, OutputIndex: fakeInt64(1), Delta: "reasoned answer",
		RawJSON: adapter.privateRaw("text_delta"),
	})
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 6, responseID, message.ID, 1))
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseDone, 7, responseID, ""))
	return &agent.ModelResponse{
		ID: responseID, OutputText: "reasoned answer", HadReasoning: true,
		Items: []agent.ModelOutputItem{reasoning, message}, FinishReason: "completed",
	}, nil
}

func (adapter *fakeAdapter) toolSuccess(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	if !fakeRequestOffersTool(request) {
		return nil, errors.New("fake adapter: conformance tool was not offered")
	}
	responseID := adapter.responseID(call)
	item := fakeCallItem(responseID+"-function", responseID+"-call", "tool value")
	adapter.emitToolLifecycle(request, responseID, item)
	return &agent.ModelResponse{
		ID: responseID, Items: []agent.ModelOutputItem{item}, FinishReason: "completed",
	}, nil
}

func (adapter *fakeAdapter) streamSuccess(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	responseID := adapter.responseID(call)
	item := fakeMessageItem(responseID+"-message", "stream complete", "")
	adapter.emitTextLifecycle(request, responseID, item.ID, []string{"stream ", "complete"})
	return &agent.ModelResponse{
		ID: responseID, OutputText: "stream complete", Items: []agent.ModelOutputItem{item},
		FinishReason: "completed",
	}, nil
}

func (adapter *fakeAdapter) replaySuccess(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	if !fakeRequestOffersTool(request) {
		return nil, errors.New("fake adapter: replay request omitted conformance tool")
	}
	if call == 1 {
		responseID := adapter.responseID(call)
		reasoning := fakeReasoningItem(responseID + "-reasoning")
		message := fakeMessageItem(responseID+"-message", "replay source", "")
		function := fakeCallItem(responseID+"-function", responseID+"-call", "replay input")
		adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, reasoning.ID, 0))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 2, responseID, reasoning.ID, 0))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 3, responseID, message.ID, 1))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 4, responseID, message.ID, 1))
		functionAdded := fakeItemEvent(adapter, agent.ModelStreamItemAdded, 5, responseID, function.ID, 2)
		functionAdded.CallID, functionAdded.Name = function.Call.ID, function.Call.Name
		adapter.emit(request, functionAdded)
		adapter.emit(request, agent.ModelStreamEvent{
			Type: agent.ModelStreamToolArgumentsDone, ProviderType: "fake.function.done",
			ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(6),
			ResponseID: responseID, ItemID: function.ID, OutputIndex: fakeInt64(2),
			CallID: function.Call.ID, Name: function.Call.Name, Arguments: string(function.Call.Input),
			RawJSON: adapter.privateRaw("tool_done"),
		})
		functionDone := fakeItemEvent(adapter, agent.ModelStreamItemDone, 7, responseID, function.ID, 2)
		functionDone.CallID, functionDone.Name = function.Call.ID, function.Call.Name
		adapter.emit(request, functionDone)
		adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseDone, 8, responseID, ""))
		return &agent.ModelResponse{
			ID: responseID, OutputText: "replay source", HadReasoning: true,
			Items: []agent.ModelOutputItem{reasoning, message, function}, FinishReason: "completed",
		}, nil
	}
	if call != 2 || !validFakeReplayRequest(request) {
		return nil, errors.New("fake adapter: replay continuation did not preserve native output")
	}
	return adapter.textSuccess(request, call, "replay accepted", agent.Usage{})
}

func (adapter *fakeAdapter) providerFailure(request agent.ModelRequest, category agent.ProviderErrorCategory, code string, status int) (*agent.ModelResponse, error) {
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamError, ProviderType: "fake.provider.error",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(0),
		ErrorCode: code, ErrorMessage: adapter.marker, RawJSON: adapter.privateRaw("provider_error"),
	})
	providerErr, err := agent.NewProviderError(
		"modeltest", category, code, status, "fake-request-id",
		errors.New("fake private provider payload: "+adapter.marker),
	)
	if err != nil {
		return nil, err
	}
	return nil, providerErr
}

func (adapter *fakeAdapter) invalidOutput(request agent.ModelRequest, call int) (*agent.ModelResponse, error) {
	responseID := adapter.responseID(call)
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
	switch adapter.name {
	case modeltest.ScenarioV1InvalidUnknownOutput:
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, responseID+"-unknown", 0))
	case modeltest.ScenarioV1InvalidDuplicateOutput:
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, responseID+"-duplicate", 0))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 2, responseID, responseID+"-duplicate", 1))
	case modeltest.ScenarioV1InvalidReorderedOutput:
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 2, responseID, responseID+"-reordered", 0))
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, 1, responseID, responseID+"-reordered", 0))
	case modeltest.ScenarioV1InvalidContradictoryID:
		event := fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, responseID+"-item", 0)
		event.ResponseID = responseID + "-other"
		adapter.emit(request, event)
	case modeltest.ScenarioV1InvalidPartialCompletion:
		adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, responseID+"-partial", 0))
		adapter.emit(request, agent.ModelStreamEvent{
			Type: agent.ModelStreamTextDelta, ProviderType: "fake.text.delta",
			ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(2),
			ResponseID: responseID, ItemID: responseID + "-partial", OutputIndex: fakeInt64(0), Delta: "partial",
			RawJSON: adapter.privateRaw("partial_delta"),
		})
	}
	return nil, fmt.Errorf("%w: fake adapter rejected provider output", agent.ErrInvalidModelOutput)
}

func (adapter *fakeAdapter) emitTextLifecycle(request agent.ModelRequest, responseID, itemID string, deltas []string) {
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, itemID, 0))
	sequence := int64(2)
	for _, delta := range deltas {
		adapter.emit(request, agent.ModelStreamEvent{
			Type: agent.ModelStreamTextDelta, ProviderType: "fake.text.delta",
			ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(sequence),
			ResponseID: responseID, ItemID: itemID, OutputIndex: fakeInt64(0), Delta: delta,
			RawJSON: adapter.privateRaw("text_delta"),
		})
		sequence++
	}
	adapter.emit(request, fakeItemEvent(adapter, agent.ModelStreamItemDone, sequence, responseID, itemID, 0))
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseDone, sequence+1, responseID, ""))
}

func (adapter *fakeAdapter) emitToolLifecycle(request agent.ModelRequest, responseID string, item agent.ModelOutputItem) {
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseStarted, 0, responseID, ""))
	added := fakeItemEvent(adapter, agent.ModelStreamItemAdded, 1, responseID, item.ID, 0)
	added.CallID, added.Name = item.Call.ID, item.Call.Name
	adapter.emit(request, added)
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamToolArgumentsDelta, ProviderType: "fake.function.delta",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(2),
		ResponseID: responseID, ItemID: item.ID, OutputIndex: fakeInt64(0),
		CallID: item.Call.ID, Name: item.Call.Name, Delta: string(item.Call.Input),
		RawJSON: adapter.privateRaw("tool_delta"),
	})
	adapter.emit(request, agent.ModelStreamEvent{
		Type: agent.ModelStreamToolArgumentsDone, ProviderType: "fake.function.done",
		ModelCallID: request.ModelCallID, SequenceNumber: fakeInt64(3),
		ResponseID: responseID, ItemID: item.ID, OutputIndex: fakeInt64(0),
		CallID: item.Call.ID, Name: item.Call.Name, Arguments: string(item.Call.Input),
		RawJSON: adapter.privateRaw("tool_done"),
	})
	done := fakeItemEvent(adapter, agent.ModelStreamItemDone, 4, responseID, item.ID, 0)
	done.CallID, done.Name = item.Call.ID, item.Call.Name
	adapter.emit(request, done)
	adapter.emit(request, fakeEvent(adapter, agent.ModelStreamResponseDone, 5, responseID, ""))
}

func (adapter *fakeAdapter) emit(request agent.ModelRequest, event agent.ModelStreamEvent) {
	if request.StreamSink != nil {
		request.StreamSink(event)
	}
}

func (adapter *fakeAdapter) responseID(call int) string {
	name := strings.NewReplacer("/", "-", "_", "-").Replace(adapter.name)
	return fmt.Sprintf("fake-%s-response-%d", name, call)
}

func (adapter *fakeAdapter) privateRaw(event string) string {
	encoded, _ := json.Marshal(map[string]string{"event": event, "private": adapter.marker})
	return string(encoded)
}

func fakeEvent(adapter *fakeAdapter, eventType agent.ModelStreamEventType, sequence int64, responseID, itemID string) agent.ModelStreamEvent {
	return agent.ModelStreamEvent{
		Type: eventType, ProviderType: "fake." + string(eventType),
		SequenceNumber: fakeInt64(sequence), ResponseID: responseID, ItemID: itemID,
		RawJSON: adapter.privateRaw(string(eventType)),
	}
}

func fakeItemEvent(adapter *fakeAdapter, eventType agent.ModelStreamEventType, sequence int64, responseID, itemID string, outputIndex int64) agent.ModelStreamEvent {
	event := fakeEvent(adapter, eventType, sequence, responseID, itemID)
	event.OutputIndex = fakeInt64(outputIndex)
	return event
}

func fakeInt64(value int64) *int64 { return &value }

func fakeMessageItem(id, text, refusal string) agent.ModelOutputItem {
	content := make([]map[string]any, 0, 1)
	if text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text})
	}
	if refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}
	raw, _ := json.Marshal(map[string]any{
		"id": id, "type": string(agent.ModelOutputMessage), "role": "assistant",
		"status": "completed", "content": content,
	})
	return agent.ModelOutputItem{ID: id, Type: agent.ModelOutputMessage, Text: text, Raw: raw}
}

func fakeReasoningItem(id string) agent.ModelOutputItem {
	raw, _ := json.Marshal(map[string]any{
		"id": id, "type": string(agent.ModelOutputReasoning), "status": "completed",
		"summary": []map[string]any{{"type": "summary_text", "text": "checked"}},
	})
	return agent.ModelOutputItem{ID: id, Type: agent.ModelOutputReasoning, Raw: raw}
}

func fakeCallItem(id, callID, value string) agent.ModelOutputItem {
	input, _ := json.Marshal(map[string]string{"value": value})
	raw, _ := json.Marshal(map[string]any{
		"id": id, "type": string(agent.ModelOutputFunctionCall), "status": "completed",
		"call_id": callID, "name": fakeToolName, "arguments": string(input),
	})
	return agent.ModelOutputItem{
		ID: id, Type: agent.ModelOutputFunctionCall,
		Call: &agent.ToolCall{ID: callID, Name: fakeToolName, Input: input}, Raw: raw,
	}
}

func fakeRequestOffersTool(request agent.ModelRequest) bool {
	return len(request.Tools) == 1 && request.Tools[0].Name == fakeToolName && json.Valid(request.Tools[0].InputSchema)
}

func validFakeReplayRequest(request agent.ModelRequest) bool {
	seen := map[agent.ModelOutputItemType]bool{}
	responseID := ""
	callID := ""
	toolResult := false
	for _, item := range request.Input {
		switch item.Type {
		case agent.ModelInputAssistantOutput:
			if item.ResponseID == "" || !json.Valid(item.Raw) {
				return false
			}
			if responseID == "" {
				responseID = item.ResponseID
			} else if item.ResponseID != responseID {
				return false
			}
			seen[item.OutputType] = true
			if item.OutputType == agent.ModelOutputFunctionCall {
				callID = item.CallID
			}
		case agent.ModelInputToolResult:
			if callID == "" || item.CallID != callID || !json.Valid(item.Output) {
				return false
			}
			toolResult = true
		}
	}
	return responseID != "" && callID != "" && toolResult &&
		seen[agent.ModelOutputReasoning] && seen[agent.ModelOutputMessage] && seen[agent.ModelOutputFunctionCall]
}

func newFakeBinding(t *testing.T) agent.ModelBinding {
	t.Helper()
	binding := agent.ModelBinding{
		Provider:            "modeltest",
		Model:               "modeltest-v1",
		EndpointClass:       "test",
		CredentialPrincipal: "modeltest-principal",
		AdapterVersion:      "v1",
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("fake adapter: ModelBinding is invalid: %v", err)
	}
	return binding
}
