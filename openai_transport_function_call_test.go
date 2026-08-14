package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIModelRepairsMissingCompletedFunctionArgumentsFromFinalizedStreamItem(
	t *testing.T,
) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repaired\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\"},\"output_index\":0,\"sequence_number\":4}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_repaired\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
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
	if len(response.Items) != 1 || response.Items[0].Call == nil {
		t.Fatalf("response items=%+v", response.Items)
	}
	call := response.Items[0].Call
	if call.ID != "call_1" || call.Name != "echo" ||
		string(call.Input) != `{"value":"hi"}` {
		t.Fatalf("call=%+v", call)
	}
	var replayed map[string]any
	if err := json.Unmarshal(response.Items[0].Raw, &replayed); err != nil {
		t.Fatalf("unmarshal replay item: %v", err)
	}
	if replayed["arguments"] != `{"value":"hi"}` {
		t.Fatalf("replayed arguments=%#v", replayed["arguments"])
	}
	if _, err := json.Marshal(response); err != nil {
		t.Fatalf("marshal repaired response: %v", err)
	}
}

func TestOpenAIModelDoesNotExecuteUnfinalizedFunctionArgumentDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_unfinalized\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
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
	if !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
	}
}
