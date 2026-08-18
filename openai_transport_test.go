package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

func newOpenAITestClient(baseURL string, opts ...option.RequestOption) openai.Client {
	clientOpts := []option.RequestOption{
		option.WithAPIKey("sk-test"),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
	clientOpts = append(clientOpts, opts...)
	return openai.NewClient(clientOpts...)
}

func TestOpenAIModelUsesStreamingResponsesTransport(t *testing.T) {
	var requestBody map[string]any
	server := newStreamingResponsesTestServer(t, &requestBody)
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{
		Model:     "test-model",
		Reasoning: &shared.ReasoningParam{Effort: shared.ReasoningEffortHigh, Summary: shared.ReasoningSummaryDetailed},
	})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	var reasoning, commentary, streamed strings.Builder
	var chunks []ModelStreamEvent
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		Tools: []ToolDefinition{{
			Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		StreamSink: func(event ModelStreamEvent) {
			chunks = append(chunks, event)
			switch event.Type {
			case ModelStreamReasoningSummaryDelta:
				reasoning.WriteString(event.Delta)
			case ModelStreamCommentaryDelta:
				commentary.WriteString(event.Delta)
			case ModelStreamTextDelta:
				streamed.WriteString(event.Delta)
			}
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	assertOpenAIStreamingRequest(t, requestBody)
	if streamed.String() != "hello" {
		t.Fatalf("streamed=%q, want hello", streamed.String())
	}
	if reasoning.String() != "checking" {
		t.Fatalf("reasoning=%q, want checking", reasoning.String())
	}
	if commentary.String() != "using tool" {
		t.Fatalf("commentary=%q, want using tool", commentary.String())
	}
	assertStreamChunk(t, chunks, ModelStreamResponseStarted, 0, "", "resp_stream")
	assertStreamChunk(t, chunks, ModelStreamReasoningSummaryDelta, 2, "rs_1", "resp_stream")
	argumentsDelta := assertStreamChunk(t, chunks, ModelStreamToolArgumentsDelta, 12, "fc_1", "resp_stream")
	argumentsDone := assertStreamChunk(t, chunks, ModelStreamToolArgumentsDone, 13, "fc_1", "resp_stream")
	if argumentsDelta.CallID != "call_1" || argumentsDone.CallID != "call_1" || argumentsDone.Name != "echo" || argumentsDone.Arguments != `{"value":"hi"}` || argumentsDone.Delta != "" || argumentsDone.OutputIndex == nil || *argumentsDone.OutputIndex != 3 {
		t.Fatalf("arguments done=%+v", argumentsDone)
	}
	assertStreamChunk(t, chunks, ModelStreamItemDone, 14, "fc_1", "resp_stream")
	assertStreamChunk(t, chunks, ModelStreamResponseDone, 15, "", "resp_stream")
	if response.ID != "resp_stream" || response.OutputText != "hello" || response.Usage.TotalTokens != 3 {
		t.Fatalf("response=%+v", response)
	}
}

func newStreamingResponsesTestServer(
	t *testing.T,
	requestBody *map[string]any,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		if err := json.Unmarshal(body, requestBody); err != nil {
			t.Errorf("Unmarshal request: %v", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"checking\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"checking\"}]},\"output_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_c\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[]},\"output_index\":1,\"sequence_number\":4}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"using tool\",\"item_id\":\"msg_c\",\"output_index\":1,\"content_index\":0,\"sequence_number\":5}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_c\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[{\"type\":\"output_text\",\"text\":\"using tool\",\"annotations\":[]}]},\"output_index\":1,\"sequence_number\":6}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":2,\"sequence_number\":7}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"hel\",\"item_id\":\"msg_1\",\"output_index\":2,\"content_index\":0,\"sequence_number\":8}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"lo\",\"item_id\":\"msg_1\",\"output_index\":2,\"content_index\":0,\"sequence_number\":9}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]},\"output_index\":2,\"sequence_number\":10}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":3,\"sequence_number\":11}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":3,\"sequence_number\":12}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":3,\"sequence_number\":13}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\"},\"output_index\":3,\"sequence_number\":14}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":15,\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"checking\"}]},{\"id\":\"msg_c\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[{\"type\":\"output_text\",\"text\":\"using tool\",\"annotations\":[]}]},{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]},{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func assertOpenAIStreamingRequest(t *testing.T, requestBody map[string]any) {
	t.Helper()
	if stream, ok := requestBody["stream"].(bool); !ok || !stream {
		t.Fatalf("request stream=%v, want true", requestBody["stream"])
	}
	assertOpenAIStatelessReasoningRequest(t, requestBody)
	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok || reasoningConfig["effort"] != "high" || reasoningConfig["summary"] != "detailed" {
		t.Fatalf("request reasoning=%#v", requestBody["reasoning"])
	}
	tools, ok := requestBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools=%#v", requestBody["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("request tool=%#v", tools[0])
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("request parameters=%#v", tool["parameters"])
	}
	if properties, exists := parameters["properties"]; !exists {
		t.Fatalf("empty object schema is missing properties: %#v", parameters)
	} else if propertyMap, ok := properties.(map[string]any); !ok || len(propertyMap) != 0 {
		t.Fatalf("request properties=%#v, want empty object", properties)
	}
}

func TestOpenAIModelHonorsDisableReasoning(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Errorf("Unmarshal request: %v", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_no_reasoning\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_no_reasoning\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{
		Model:     "test-model",
		Reasoning: &shared.ReasoningParam{Effort: shared.ReasoningEffortHigh, Summary: shared.ReasoningSummaryDetailed},
	})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions:     "Answer the user.",
		Input:            []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		DisableReasoning: true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.OutputText != "done" {
		t.Fatalf("response=%+v", response)
	}
	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok || reasoningConfig["effort"] != "none" {
		t.Fatalf("request reasoning=%#v, want effort none", requestBody["reasoning"])
	}
	if _, exists := reasoningConfig["summary"]; exists {
		t.Fatalf("request reasoning=%#v, want summary omitted", requestBody["reasoning"])
	}
}

func TestOpenAIModelReplaysEncryptedReasoningForStatelessToolContinuation(t *testing.T) {
	requests := make(chan map[string]any, 2)
	server := newReasoningReplayServer(t, requests)
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{
		Model:     "test-model",
		Reasoning: &shared.ReasoningParam{Effort: shared.ReasoningEffortHigh, Summary: shared.ReasoningSummaryDetailed},
	})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	tools := []ToolDefinition{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	transcript := []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}
	firstResponse, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        transcript,
		Tools:        tools,
	})
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	firstRequest := <-requests
	assertOpenAIStatelessReasoningRequest(t, firstRequest)
	if len(firstResponse.Items) != 2 || firstResponse.Items[0].Type != ModelOutputReasoning || firstResponse.Items[1].Type != ModelOutputFunctionCall {
		t.Fatalf("first response items=%+v", firstResponse.Items)
	}
	var reasoningRaw map[string]any
	if err := json.Unmarshal(firstResponse.Items[0].Raw, &reasoningRaw); err != nil {
		t.Fatalf("unmarshal reasoning raw item: %v", err)
	}
	if reasoningRaw["encrypted_content"] != "encrypted-reasoning-1" {
		t.Fatalf("reasoning encrypted_content=%#v", reasoningRaw["encrypted_content"])
	}

	transcript, err = appendModelOutputItems(transcript, firstResponse.Items, firstResponse.ID)
	if err != nil {
		t.Fatalf("appendModelOutputItems: %v", err)
	}
	transcript = append(transcript, ModelInputItem{
		Type: ModelInputToolResult, CallID: "call_1", Output: json.RawMessage(`{"value":"hi"}`),
	})
	secondResponse, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        transcript,
		Tools:        tools,
	})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	assertReasoningReplayRequest(t, <-requests)
	if secondResponse.OutputText != "done" {
		t.Fatalf("second response=%+v", secondResponse)
	}
}

func newReasoningReplayServer(
	t *testing.T,
	requests chan<- map[string]any,
) *httptest.Server {
	t.Helper()
	requestNumber := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		var requestBody map[string]any
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Errorf("Unmarshal request: %v", err)
			return
		}
		requests <- requestBody
		requestNumber++

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tool\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[],\"status\":\"in_progress\"},\"output_index\":0,\"sequence_number\":1}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"encrypted-reasoning-1\",\"status\":\"completed\"},\"output_index\":0,\"sequence_number\":2}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\",\"status\":\"in_progress\"},\"output_index\":1,\"sequence_number\":3}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":1,\"sequence_number\":4}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"status\":\"completed\"},\"output_index\":1,\"sequence_number\":5}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":6,\"response\":{\"id\":\"resp_tool\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"encrypted-reasoning-1\",\"status\":\"completed\"},{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"status\":\"completed\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
		case 2:
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_final\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":2}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_final\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"total_tokens\":5}}}\n\n")
		default:
			t.Errorf("unexpected request number %d", requestNumber)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func assertReasoningReplayRequest(t *testing.T, secondRequest map[string]any) {
	t.Helper()
	assertOpenAIStatelessReasoningRequest(t, secondRequest)
	input, ok := secondRequest["input"].([]any)
	if !ok || len(input) != 4 {
		t.Fatalf("second request input=%#v", secondRequest["input"])
	}
	replayedReasoning, ok := input[1].(map[string]any)
	if !ok || replayedReasoning["type"] != "reasoning" || replayedReasoning["encrypted_content"] != "encrypted-reasoning-1" {
		t.Fatalf("replayed reasoning=%#v", input[1])
	}
	replayedCall, callOK := input[2].(map[string]any)
	replayedResult, resultOK := input[3].(map[string]any)
	if !callOK || !resultOK || replayedCall["type"] != "function_call" || replayedCall["call_id"] != "call_1" || replayedResult["type"] != "function_call_output" || replayedResult["call_id"] != "call_1" {
		t.Fatalf("replayed call=%#v result=%#v", input[2], input[3])
	}
}

func assertOpenAIStatelessReasoningRequest(t *testing.T, requestBody map[string]any) {
	t.Helper()
	if store, ok := requestBody["store"].(bool); !ok || store {
		t.Fatalf("request store=%v, want false", requestBody["store"])
	}
	if _, exists := requestBody["previous_response_id"]; exists {
		t.Fatalf("request unexpectedly depends on previous_response_id")
	}
	include, ok := requestBody["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("request include=%#v, want reasoning.encrypted_content", requestBody["include"])
	}
}

func TestOpenAIModelStreamsAndReturnsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_refusal\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.refusal.delta\",\"delta\":\"I cannot help with that.\",\"item_id\":\"msg_refusal\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"I cannot help with that.\"}]},\"output_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_refusal\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"I cannot help with that.\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":6,\"total_tokens\":8}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	var chunks []ModelStreamEvent
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		StreamSink:   func(event ModelStreamEvent) { chunks = append(chunks, event) },
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	refusal := assertStreamChunk(t, chunks, ModelStreamRefusalDelta, 2, "msg_refusal", "resp_refusal")
	if refusal.Delta != "I cannot help with that." {
		t.Fatalf("refusal delta=%q", refusal.Delta)
	}
	if response.OutputText != "" || response.Refusal != "I cannot help with that." {
		t.Fatalf("response=%+v", response)
	}
}

func TestOpenAIModelCompletesWithoutStreamSink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_no_sink\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_no_sink\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.ID != "resp_no_sink" || response.OutputText != "done" {
		t.Fatalf("response=%+v", response)
	}
}

func TestOpenAIModelUsesInjectedClientRetryPolicyBeforeStreaming(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_retry\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_retry\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newOpenAITestClient(server.URL+"/v1", option.WithMaxRetries(1))
	model, err := NewOpenAIModel(client, OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if attempts != 2 || response.ID != "resp_retry" {
		t.Fatalf("attempts=%d response=%+v, want two attempts and resp_retry", attempts, response)
	}
}

func assertStreamChunk(t *testing.T, chunks []ModelStreamEvent, eventType ModelStreamEventType, sequence int64, itemID, responseID string) ModelStreamEvent {
	t.Helper()
	for _, chunk := range chunks {
		if chunk.Type != eventType || chunk.SequenceNumber == nil || *chunk.SequenceNumber != sequence {
			continue
		}
		if chunk.ItemID != itemID || chunk.ResponseID != responseID || chunk.RawJSON == "" {
			t.Fatalf("chunk=%+v, want item=%q response=%q with raw payload", chunk, itemID, responseID)
		}
		return chunk
	}
	t.Fatalf("missing chunk type=%s sequence=%d: %+v", eventType, sequence, chunks)
	return ModelStreamEvent{}
}

func TestNewOpenAIModelValidatesReasoningConfiguration(t *testing.T) {
	_, err := NewOpenAIModel(newOpenAITestClient("https://example.com/v1"), OpenAIModelConfig{
		Model:     "test-model",
		Reasoning: &shared.ReasoningParam{Effort: shared.ReasoningEffortNone, Summary: shared.ReasoningSummaryAuto},
	})
	if err == nil || !strings.Contains(err.Error(), "summary must be omitted") {
		t.Fatalf("err=%v, want incompatible reasoning configuration", err)
	}
}

func TestOpenAIModelRejectsStreamWithoutCompletedEvent(t *testing.T) {
	var requestBody map[string]any
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		attempts++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Errorf("Unmarshal request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_partial\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"partial\",\"item_id\":\"msg_partial\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newOpenAITestClient(server.URL+"/v1", option.WithMaxRetries(2))
	model, err := NewOpenAIModel(client, OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	var chunks []ModelStreamEvent
	_, err = model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		StreamSink:   func(event ModelStreamEvent) { chunks = append(chunks, event) },
	})
	if err == nil || !strings.Contains(err.Error(), "without response.completed") {
		t.Fatalf("err=%v, want missing completed event", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want no retry after streaming starts", attempts)
	}
	if _, exists := requestBody["reasoning"]; exists {
		t.Fatalf("unconfigured model sent reasoning=%#v", requestBody["reasoning"])
	}
	assertOpenAIStatelessReasoningRequest(t, requestBody)
	last := chunks[len(chunks)-1]
	if last.Type != ModelStreamError || last.ProviderType != "stream.ended" || last.ErrorCode != "incomplete_stream" || last.SequenceNumber != nil || last.OutputIndex != nil {
		t.Fatalf("terminal chunk=%+v", last)
	}
}

func TestOpenAIModelRejectsUnofferedToolLifecycleEvents(t *testing.T) {
	// The transport only ever offers function tools derived from registered
	// operations, so a tool-call lifecycle for any other tool class is provider
	// drift and must fail explicitly instead of being silently ignored.
	for _, eventType := range []string{
		"response.web_search_call.completed",
		"response.code_interpreter_call.completed",
		"response.mcp_call.completed",
		"response.image_generation_call.completed",
		"response.file_search_call.completed",
		"response.custom_tool_call_input.done",
	} {
		t.Run(eventType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_unoffered\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
				fmt.Fprintf(w, "data: {\"type\":%q,\"sequence_number\":1}\n\n", eventType)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(context.Background(), ModelRequest{
				Instructions: "Answer the user.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
			})
			if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "never offers") {
				t.Fatalf("Complete error=%v, want unoffered-tool-class ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelAcceptsRefusalDoneLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_refusal_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.refusal.delta\",\"delta\":\"I cannot help with that.\",\"item_id\":\"msg_refusal\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.refusal.done\",\"refusal\":\"I cannot help with that.\",\"item_id\":\"msg_refusal\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"I cannot help with that.\"}]},\"output_index\":0,\"sequence_number\":4}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_refusal_done\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_refusal\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"I cannot help with that.\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":6,\"total_tokens\":8}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(context.Background(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.OutputText != "" || response.Refusal != "I cannot help with that." {
		t.Fatalf("response=%+v", response)
	}
}
