package agentruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/ly95/agentruntime"
	"github.com/ly95/agentruntime/modeltest"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const (
	openAIConformanceModel    = "modeltest-openai-model"
	openAIConformanceToolName = "modeltest_echo"
)

func TestOpenAIModelConformance(t *testing.T) {
	modeltest.RunModelConformance(t, func(t *testing.T, scenario modeltest.Scenario) agent.BoundModel {
		t.Helper()
		fixture := &openAIConformanceFixture{t: t, scenario: scenario}
		server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
		t.Cleanup(server.Close)

		return newOpenAIConformanceModel(t, server.URL+"/v1")
	})
}

func TestOpenAIModelConformanceRejectsResponseModelSubstitution(t *testing.T) {
	tests := []struct {
		name       string
		modelField string
	}{
		{name: "missing terminal model"},
		{name: "substituted terminal model", modelField: `"model":"substituted-model",`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_model_authority\",\"object\":\"response\",\"created_at\":1,\"model\":%q,\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n", openAIConformanceModel)
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_model_authority\",\"object\":\"response\",\"created_at\":1,%s\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}},\"sequence_number\":1}\n\n", test.modelField)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			model := newOpenAIConformanceModel(t, server.URL+"/v1")
			var doneEvents int
			response, err := model.Complete(t.Context(), openAIConformanceRequest(func(event agent.ModelStreamEvent) {
				if event.Type == agent.ModelStreamResponseDone {
					doneEvents++
				}
			}))
			if response != nil || !errors.Is(err, agent.ErrInvalidModelOutput) || doneEvents != 0 {
				t.Fatalf("Complete response=%+v error=%v response_done=%d, want model authority failure", response, err, doneEvents)
			}
		})
	}
}

func TestOpenAIModelConformanceSanitizesTransportFailure(t *testing.T) {
	const marker = "modeltest_private_transport_marker"
	privateCause := errors.New(marker)
	httpClient := &http.Client{Transport: openAIConformanceRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &openAIConformanceErrorBody{cause: privateCause},
			Request:    request,
		}, nil
	})}
	model := newOpenAIConformanceModel(
		t,
		"https://modeltest.invalid/v1",
		option.WithHTTPClient(httpClient),
	)
	var events []agent.ModelStreamEvent
	response, err := model.Complete(t.Context(), openAIConformanceRequest(func(event agent.ModelStreamEvent) {
		events = append(events, event)
	}))
	if response != nil || err == nil {
		t.Fatalf("Complete response=%+v error=%v, want transport failure", response, err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("top-level transport error leaked private marker: %v", err)
	}
	if !errors.Is(err, privateCause) {
		t.Fatalf("transport error=%v does not preserve its private cause", err)
	}
	if len(events) != 1 || events[0].Type != agent.ModelStreamError || !strings.Contains(events[0].ErrorMessage, marker) {
		t.Fatalf("trusted transport events=%+v, want marker-bearing stream error", events)
	}
}

func TestOpenAIModelConformancePreservesDeadlineExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "pre-expired deadline unexpectedly contacted provider", http.StatusInternalServerError)
	}))
	defer server.Close()
	model := newOpenAIConformanceModel(t, server.URL+"/v1")

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	response, err := model.Complete(ctx, openAIConformanceRequest(nil))
	if response != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Complete response=%+v error=%v, want context.DeadlineExceeded", response, err)
	}
	var providerErr *agent.ProviderError
	if errors.As(err, &providerErr) {
		t.Fatalf("deadline was misclassified as provider failure: %v", err)
	}
}

func newOpenAIConformanceModel(t *testing.T, baseURL string, options ...option.RequestOption) *agent.OpenAIModel {
	t.Helper()
	clientOptions := []option.RequestOption{
		option.WithAPIKey("sk-modeltest"),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
	clientOptions = append(clientOptions, options...)
	client := openai.NewClient(clientOptions...)
	model, err := agent.NewOpenAIModel(client, agent.OpenAIModelConfig{
		Model:               openAIConformanceModel,
		EndpointClass:       "modeltest-endpoint",
		CredentialPrincipal: "modeltest-principal",
	})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	return model
}

func openAIConformanceRequest(sink agent.ModelStreamSink) agent.ModelRequest {
	return agent.ModelRequest{
		Instructions: "Run OpenAI conformance validation.",
		Input:        []agent.ModelInputItem{{Type: agent.ModelInputUserMessage, Text: "modeltest"}},
		StreamSink:   sink,
	}
}

type openAIConformanceFixture struct {
	t        *testing.T
	scenario modeltest.Scenario

	mu    sync.Mutex
	calls int
}

func (fixture *openAIConformanceFixture) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
		fixture.failRequest(w, "unexpected request %s %s", request.Method, request.URL.Path)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		fixture.failRequest(w, "decode request: %v", err)
		return
	}
	if body["model"] != openAIConformanceModel {
		fixture.failRequest(w, "request model=%#v, want %q", body["model"], openAIConformanceModel)
		return
	}
	if stream, ok := body["stream"].(bool); !ok || !stream {
		fixture.failRequest(w, "request stream=%#v, want true", body["stream"])
		return
	}
	if store, ok := body["store"].(bool); !ok || store {
		fixture.failRequest(w, "request store=%#v, want false", body["store"])
		return
	}

	fixture.mu.Lock()
	fixture.calls++
	call := fixture.calls
	fixture.mu.Unlock()

	switch fixture.scenario.Name() {
	case modeltest.ScenarioV1Binding:
		fixture.failRequest(w, "binding scenario contacted the provider")
	case modeltest.ScenarioV1CancelPreCanceled:
		fixture.failRequest(w, "pre-canceled scenario contacted the provider")
	case modeltest.ScenarioV1CancelAfterResponseStarted:
		fixture.writeCreated(w, "resp_cancel_after_start", 0)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	case modeltest.ScenarioV1ErrorAuthentication:
		fixture.writeHTTPError(w, http.StatusUnauthorized, "invalid_request_error")
	case modeltest.ScenarioV1ErrorQuota:
		fixture.writeHTTPError(w, http.StatusTooManyRequests, "insufficient_quota")
	case modeltest.ScenarioV1ErrorRateLimit:
		fixture.writeHTTPError(w, http.StatusTooManyRequests, "rate_limit_error")
	case modeltest.ScenarioV1ErrorRejected:
		fixture.writeHTTPError(w, http.StatusBadRequest, "invalid_request_error")
	case modeltest.ScenarioV1ErrorTransient:
		fixture.writeHTTPError(w, http.StatusServiceUnavailable, "server_error")
	case modeltest.ScenarioV1InvalidUnknownOutput:
		fixture.writeCreated(w, "resp_invalid_unknown", 0)
		fixture.writeEvent(w, map[string]any{
			"type": "response.future_event", "sequence_number": 1,
		})
		fixture.writeMessageCompletion(w, "resp_invalid_unknown", "msg_after_unknown", "valid after unknown", 2)
	case modeltest.ScenarioV1InvalidDuplicateOutput:
		fixture.writeCreated(w, "resp_invalid_duplicate", 0)
		fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage("msg_duplicate", "in_progress", "", ""), 0, 1)
		done := openAIConformanceMessage("msg_duplicate", "completed", "duplicate evidence", "")
		fixture.writeItemEvent(w, "response.output_item.done", done, 0, 2)
		fixture.writeItemEvent(w, "response.output_item.done", done, 0, 3)
		fixture.writeCompleted(w, "resp_invalid_duplicate", []any{done}, 4, 2, 2)
		fixture.writeDone(w)
	case modeltest.ScenarioV1InvalidReorderedOutput:
		fixture.writeCreated(w, "resp_invalid_reordered", 0)
		fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage("msg_reordered", "in_progress", "", ""), 0, 2)
		done := openAIConformanceMessage("msg_reordered", "completed", "reordered", "")
		fixture.writeItemEvent(w, "response.output_item.done", done, 0, 1)
		fixture.writeCompleted(w, "resp_invalid_reordered", []any{done}, 3, 2, 2)
		fixture.writeDone(w)
	case modeltest.ScenarioV1InvalidContradictoryID:
		fixture.writeCreated(w, "resp_identity_a", 0)
		fixture.writeEvent(w, map[string]any{
			"type":            "response.in_progress",
			"sequence_number": 1,
			"response":        fixture.response("resp_identity_b", "in_progress", []any{}, false),
		})
		fixture.writeMessageCompletion(w, "resp_identity_a", "msg_after_identity", "valid after identity", 2)
	case modeltest.ScenarioV1InvalidPartialCompletion:
		fixture.writeCreated(w, "resp_invalid_partial", 0)
		fixture.writeDone(w)
	case modeltest.ScenarioV1SuccessText:
		fixture.writeTextSuccess(w, "resp_text", "msg_text", "OpenAI conformance text", false)
	case modeltest.ScenarioV1SuccessRefusal:
		fixture.writeRefusalSuccess(w)
	case modeltest.ScenarioV1SuccessReasoning:
		fixture.writeReasoningSuccess(w)
	case modeltest.ScenarioV1SuccessTool:
		fixture.requireConformanceTool(w, body)
		fixture.writeToolSuccess(w, "resp_tool", "fc_tool", "call_tool", "tool value")
	case modeltest.ScenarioV1SuccessStream:
		fixture.writeTextSuccess(w, "resp_stream", "msg_stream", "stream complete", true)
	case modeltest.ScenarioV1SuccessUsage:
		fixture.writeTextSuccess(w, "resp_usage", "msg_usage", "usage mapped", false)
	case modeltest.ScenarioV1SuccessReplay:
		fixture.requireConformanceTool(w, body)
		if call == 1 {
			fixture.writeReplaySource(w)
			return
		}
		if call != 2 {
			fixture.failRequest(w, "replay scenario received unexpected call %d", call)
			return
		}
		fixture.validateReplayRequest(body)
		fixture.writeTextSuccess(w, "resp_replay_continuation", "msg_replay_continuation", "replay accepted", false)
	case modeltest.ScenarioV1ConcurrencyBoundModel:
		fixture.requireConformanceTool(w, body)
		fixture.writeTextSuccess(
			w,
			fmt.Sprintf("resp_concurrent_%d", call),
			fmt.Sprintf("msg_concurrent_%d", call),
			"concurrent response",
			false,
		)
	default:
		fixture.failRequest(w, "unsupported conformance scenario %q", fixture.scenario.Name())
	}
}

func (fixture *openAIConformanceFixture) failRequest(w http.ResponseWriter, format string, args ...any) {
	fixture.t.Errorf(format, args...)
	http.Error(w, "invalid modeltest fixture request", http.StatusBadRequest)
}

func (fixture *openAIConformanceFixture) writeHTTPError(w http.ResponseWriter, status int, errorType string) {
	marker := fixture.scenario.PayloadMarker()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-request-id", "private-request-"+marker)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "private OpenAI failure: " + marker,
			"type":    errorType,
			"param":   nil,
			"code":    "private-code-" + marker,
		},
	})
}

func (fixture *openAIConformanceFixture) writeMessageCompletion(
	w http.ResponseWriter,
	responseID string,
	itemID string,
	text string,
	sequence int64,
) {
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage(itemID, "in_progress", "", ""), 0, sequence)
	done := openAIConformanceMessage(itemID, "completed", text, "")
	fixture.writeItemEvent(w, "response.output_item.done", done, 0, sequence+1)
	fixture.writeCompleted(w, responseID, []any{done}, sequence+2, 2, 2)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) writeTextSuccess(w http.ResponseWriter, responseID, itemID, text string, streamed bool) {
	fixture.writeCreated(w, responseID, 0)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage(itemID, "in_progress", "", ""), 0, 1)
	sequence := int64(2)
	if streamed {
		for _, delta := range []string{"stream ", "complete"} {
			fixture.writeEvent(w, map[string]any{
				"type":            "response.output_text.delta",
				"sequence_number": sequence,
				"item_id":         itemID,
				"output_index":    0,
				"content_index":   0,
				"delta":           delta,
				"logprobs":        []any{},
			})
			sequence++
		}
	}
	done := openAIConformanceMessage(itemID, "completed", text, "")
	fixture.writeItemEvent(w, "response.output_item.done", done, 0, sequence)
	fixture.writeCompleted(w, responseID, []any{done}, sequence+1, 7, 5)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) writeRefusalSuccess(w http.ResponseWriter) {
	const (
		responseID = "resp_refusal"
		itemID     = "msg_refusal"
		refusal    = "OpenAI conformance refusal"
	)
	fixture.writeCreated(w, responseID, 0)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage(itemID, "in_progress", "", ""), 0, 1)
	fixture.writeEvent(w, map[string]any{
		"type":            "response.refusal.delta",
		"sequence_number": 2,
		"item_id":         itemID,
		"output_index":    0,
		"content_index":   0,
		"delta":           refusal,
	})
	done := openAIConformanceMessage(itemID, "completed", "", refusal)
	fixture.writeItemEvent(w, "response.output_item.done", done, 0, 3)
	fixture.writeCompleted(w, responseID, []any{done}, 4, 4, 3)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) writeReasoningSuccess(w http.ResponseWriter) {
	const responseID = "resp_reasoning"
	reasoning := openAIConformanceReasoning("rs_reasoning", "completed")
	message := openAIConformanceMessage("msg_reasoning", "completed", "reasoned answer", "")

	fixture.writeCreated(w, responseID, 0)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceReasoning("rs_reasoning", "in_progress"), 0, 1)
	fixture.writeItemEvent(w, "response.output_item.done", reasoning, 0, 2)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage("msg_reasoning", "in_progress", "", ""), 1, 3)
	fixture.writeItemEvent(w, "response.output_item.done", message, 1, 4)
	fixture.writeCompleted(w, responseID, []any{reasoning, message}, 5, 8, 6)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) writeToolSuccess(w http.ResponseWriter, responseID, itemID, callID, value string) {
	arguments, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		fixture.t.Errorf("marshal tool arguments: %v", err)
		return
	}
	argumentText := string(arguments)
	done := openAIConformanceFunctionCall(itemID, callID, "completed", argumentText)

	fixture.writeCreated(w, responseID, 0)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceFunctionCall(itemID, callID, "in_progress", ""), 0, 1)
	fixture.writeEvent(w, map[string]any{
		"type":            "response.function_call_arguments.delta",
		"sequence_number": 2,
		"item_id":         itemID,
		"output_index":    0,
		"delta":           argumentText,
	})
	fixture.writeEvent(w, map[string]any{
		"type":            "response.function_call_arguments.done",
		"sequence_number": 3,
		"item_id":         itemID,
		"output_index":    0,
		"name":            openAIConformanceToolName,
		"arguments":       argumentText,
	})
	fixture.writeItemEvent(w, "response.output_item.done", done, 0, 4)
	fixture.writeCompleted(w, responseID, []any{done}, 5, 6, 2)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) writeReplaySource(w http.ResponseWriter) {
	const responseID = "resp_replay_source"
	reasoning := openAIConformanceReasoning("rs_replay", "completed")
	message := openAIConformanceMessage("msg_replay", "completed", "replay source", "")
	arguments := `{"value":"replay input"}`
	function := openAIConformanceFunctionCall("fc_replay", "call_replay", "completed", arguments)

	fixture.writeCreated(w, responseID, 0)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceReasoning("rs_replay", "in_progress"), 0, 1)
	fixture.writeItemEvent(w, "response.output_item.done", reasoning, 0, 2)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceMessage("msg_replay", "in_progress", "", ""), 1, 3)
	fixture.writeItemEvent(w, "response.output_item.done", message, 1, 4)
	fixture.writeItemEvent(w, "response.output_item.added", openAIConformanceFunctionCall("fc_replay", "call_replay", "in_progress", ""), 2, 5)
	fixture.writeEvent(w, map[string]any{
		"type":            "response.function_call_arguments.done",
		"sequence_number": 6,
		"item_id":         "fc_replay",
		"output_index":    2,
		"name":            openAIConformanceToolName,
		"arguments":       arguments,
	})
	fixture.writeItemEvent(w, "response.output_item.done", function, 2, 7)
	fixture.writeCompleted(w, responseID, []any{reasoning, message, function}, 8, 10, 7)
	fixture.writeDone(w)
}

func (fixture *openAIConformanceFixture) requireConformanceTool(w http.ResponseWriter, body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		fixture.failRequest(w, "request tools=%#v, want one conformance tool", body["tools"])
		return
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["type"] != "function" || tool["name"] != openAIConformanceToolName {
		fixture.failRequest(w, "request tool=%#v, want function %q", tools[0], openAIConformanceToolName)
	}
}

func (fixture *openAIConformanceFixture) validateReplayRequest(body map[string]any) {
	input, ok := body["input"].([]any)
	if !ok || len(input) != 5 {
		fixture.t.Errorf("replay input=%#v, want five ordered native items", body["input"])
		return
	}
	items := make([]map[string]any, len(input))
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			fixture.t.Errorf("replay input[%d]=%#v, want object", index, raw)
			return
		}
		items[index] = item
	}

	user := items[0]
	userType, hasUserType := user["type"]
	if hasUserType && userType != "message" || user["role"] != "user" ||
		user["content"] != "modeltest request for "+modeltest.ScenarioV1SuccessReplay {
		fixture.t.Errorf("replayed user message=%#v", user)
	}

	reasoning := items[1]
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_replay" ||
		reasoning["encrypted_content"] != "sealed-modeltest-reasoning" {
		fixture.t.Errorf("replayed reasoning=%#v", reasoning)
	}
	if status, present := reasoning["status"]; present && status != "completed" {
		fixture.t.Errorf("replayed reasoning status=%#v, want completed when present", status)
	}

	message := items[2]
	content, contentOK := message["content"].([]any)
	if message["type"] != "message" || message["id"] != "msg_replay" || message["role"] != "assistant" ||
		message["phase"] != "final_answer" || !contentOK || len(content) != 1 {
		fixture.t.Errorf("replayed message=%#v", message)
	} else if part, ok := content[0].(map[string]any); !ok || part["type"] != "output_text" || part["text"] != "replay source" {
		fixture.t.Errorf("replayed message content=%#v", content[0])
	}

	call := items[3]
	if call["type"] != "function_call" || call["id"] != "fc_replay" || call["call_id"] != "call_replay" ||
		call["name"] != openAIConformanceToolName || call["arguments"] != `{"value":"replay input"}` {
		fixture.t.Errorf("replayed function call=%#v", call)
	}
	if status, present := call["status"]; present && status != "completed" {
		fixture.t.Errorf("replayed function call status=%#v, want completed when present", status)
	}

	result := items[4]
	if result["type"] != "function_call_output" || result["call_id"] != "call_replay" ||
		result["output"] != `{"value":"modeltest replay result"}` {
		fixture.t.Errorf("replayed function result=%#v", result)
	}
}

func (fixture *openAIConformanceFixture) writeCreated(w http.ResponseWriter, responseID string, sequence int64) {
	fixture.writeEvent(w, map[string]any{
		"type":            "response.created",
		"sequence_number": sequence,
		"response":        fixture.response(responseID, "in_progress", []any{}, false),
	})
}

func (fixture *openAIConformanceFixture) writeCompleted(w http.ResponseWriter, responseID string, output []any, sequence, inputTokens, outputTokens int64) {
	response := fixture.response(responseID, "completed", output, true)
	response["usage"] = map[string]any{
		"input_tokens": inputTokens, "output_tokens": outputTokens,
		"total_tokens": inputTokens + outputTokens,
	}
	fixture.writeEvent(w, map[string]any{
		"type": "response.completed", "sequence_number": sequence, "response": response,
	})
}

func (fixture *openAIConformanceFixture) response(responseID, status string, output []any, terminal bool) map[string]any {
	response := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": int64(1),
		"model":      openAIConformanceModel,
		"metadata": map[string]string{
			"modeltest_private": fixture.scenario.PayloadMarker(),
		},
		"status": status,
		"output": output,
	}
	if terminal {
		response["completed_at"] = int64(2)
	}
	return response
}

func (fixture *openAIConformanceFixture) writeItemEvent(w http.ResponseWriter, eventType string, item map[string]any, outputIndex, sequence int64) {
	fixture.writeEvent(w, map[string]any{
		"type": eventType, "sequence_number": sequence,
		"output_index": outputIndex, "item": item,
	})
}

func (fixture *openAIConformanceFixture) writeEvent(w http.ResponseWriter, event map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, err := json.Marshal(event)
	if err != nil {
		fixture.t.Errorf("marshal stream event: %v", err)
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

func (fixture *openAIConformanceFixture) writeDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func openAIConformanceMessage(id, status, text, refusal string) map[string]any {
	content := make([]any, 0, 1)
	if text != "" {
		content = append(content, map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		})
	}
	if refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}
	return map[string]any{
		"id": id, "type": "message", "status": status,
		"role": "assistant", "phase": "final_answer", "content": content,
	}
}

func openAIConformanceReasoning(id, status string) map[string]any {
	item := map[string]any{
		"id": id, "type": "reasoning", "status": status, "summary": []any{},
	}
	if status == "completed" {
		item["encrypted_content"] = "sealed-modeltest-reasoning"
	}
	return item
}

func openAIConformanceFunctionCall(id, callID, status, arguments string) map[string]any {
	return map[string]any{
		"id": id, "type": "function_call", "status": status,
		"call_id": callID, "name": openAIConformanceToolName, "arguments": arguments,
	}
}

type openAIConformanceRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip openAIConformanceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type openAIConformanceErrorBody struct {
	cause error
}

func (body *openAIConformanceErrorBody) Read([]byte) (int, error) {
	return 0, body.cause
}

func (*openAIConformanceErrorBody) Close() error {
	return nil
}
