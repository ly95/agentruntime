package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIModelRepairsMissingCompletedFunctionArgumentsFromFinalizedStreamItem(
	t *testing.T,
) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repaired\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"name\":\"echo\",\"output_index\":0,\"sequence_number\":3}\n\n")
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

func TestOpenAIModelRejectsFinalizedArgumentsFromAnotherResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_A\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"from_A\\\"}\",\"item_id\":\"fc_1\",\"name\":\"echo\",\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"from_A\\\"}\"},\"output_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_B\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
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
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "response") {
		t.Fatalf("Complete error=%v, want response-bound ErrInvalidModelOutput", err)
	}
}

func TestOpenAIModelRejectsRepeatedStreamLifecycleAndItemIdentities(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "repeated response created",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repeat\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repeat\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "duplicate item id",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_items\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_duplicate\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_duplicate\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]},\"output_index\":1,\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
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
		})
	}
}

func TestOpenAIModelDoesNotExecuteUnfinalizedFunctionArgumentDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_unfinalized\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"hi\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_unfinalized\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
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

func TestOpenAIModelRejectsIncompleteOrContradictoryStreamEvidence(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "repeated arguments done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repeat_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"same\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"same\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "done before added",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_done_first\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "completed arguments contradict finalized stream",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_argument_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"stream\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"stream\\\"}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_argument_conflict\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"completed\\\"}\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "completed item identity contradicts stream",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_item_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_stream\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_stream\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_item_conflict\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_completed\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}] }],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "completed only item",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_completed_only\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_completed_only\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}] }],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "stream only item",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_only\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_stream_only\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"test-model\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "arguments delta after arguments done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_after_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"},\"output_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_delta_after_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "arguments delta targets message",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wrong_delta_kind\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\",\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_wrong_delta_kind\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "function done omits type",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_done_type\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_missing_done_type\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "function done omits call identity",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_done_identity\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"arguments\":\"{}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_missing_done_identity\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "added arguments contradict finalization",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_added_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"added\\\"}\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_added_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "argument deltas contradict finalization",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"\\\"delta\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"},\"output_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":6,\"response\":{\"id\":\"resp_delta_conflict\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "response created omits object",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_object\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "response created omits created at",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_created_at\",\"object\":\"response\",\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "response created omits model",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_model\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "response created has invalid immutable types",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_invalid_created_shape\",\"object\":1,\"created_at\":\"now\",\"model\":{},\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "missing event sequence number",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_sequence\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "nonincreasing event sequence number",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_repeated_sequence\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "function added omits arguments field",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_added_arguments\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_missing_added_arguments\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"}]},\"sequence_number\":4}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "item events omit output index",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_item_indexes\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]},\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_missing_item_indexes\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]}]},\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "item done omits output index",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_done_index\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]},\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "arguments done omits name and output index",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_argument_fields\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\",\"item_id\":\"fc_1\",\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_missing_argument_fields\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"}]},\"sequence_number\":4}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "message items omit required shape",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_message_shape\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\"},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_missing_message_shape\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[]}]},\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "reasoning item omits required summary",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_reasoning_shape\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "text delta omits content index",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing_content_index\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"text\",\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{
				Instructions: "Answer the user.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
			})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsUnclassifiedAndContradictoryNonFunctionEvidence(t *testing.T) {
	validEmptyCompletion := func(responseID string, sequence int) string {
		return fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[]},\"sequence_number\":%d}\n\n", responseID, sequence)
	}
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "unknown event within lifecycle",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_unknown\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.future_event\",\"sequence_number\":1}\n\n" + validEmptyCompletion("resp_unknown", 2) + "data: [DONE]\n\n",
		},
		{
			name: "unknown event after completion",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_post_terminal\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				validEmptyCompletion("resp_post_terminal", 1) +
				"data: {\"type\":\"response.future_event\",\"sequence_number\":2}\n\n" + "data: [DONE]\n\n",
		},
		{
			name: "auxiliary response identity drift",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_aux_A\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_aux_B\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n" + validEmptyCompletion("resp_aux_A", 2) + "data: [DONE]\n\n",
		},
		{
			name: "auxiliary response before created",
			events: "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_aux_first\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "message phase drift",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_phase_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"stream\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"stream\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_phase_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"stream\",\"annotations\":[]}] }]},\"sequence_number\":4}\n\n" + "data: [DONE]\n\n",
		},
		{
			name: "message content drift",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_content_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"stream\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"stream\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_content_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"different\",\"annotations\":[]}] }]},\"sequence_number\":4}\n\n" + "data: [DONE]\n\n",
		},
		{
			name: "text done contradicts item done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_text_done_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"stream\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"different\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "reasoning summary changes after item done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"stream\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"stream\"}]},\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reasoning_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"different\"}]}]},\"sequence_number\":4}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "reasoning content and encrypted content change after item done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning_aux_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"first\"}],\"encrypted_content\":\"sealed-first\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"first\"}],\"encrypted_content\":\"sealed-first\"},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reasoning_aux_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"second\"}],\"encrypted_content\":\"sealed-second\"}]},\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text omits required annotations",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotations_missing\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\"}]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_annotations_missing\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\"}]}]},\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text has null annotations",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotations_null\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":null}]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_annotations_null\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":null}]}]},\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "content part added payload contradicts finalized text",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_part_added_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"ATTACKER\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_part_added_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]}]},\"sequence_number\":6}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "text done precedes content part added",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_part_order\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_part_order\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]}]},\"sequence_number\":6}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "immutable response envelope drifts",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_envelope_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"model-a\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_envelope_drift\",\"object\":\"response\",\"created_at\":2,\"model\":\"model-b\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_envelope_drift\",\"object\":\"response\",\"created_at\":3,\"model\":\"model-c\",\"status\":\"completed\",\"output\":[]},\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "immutable response fields appear after response created",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_envelope_late\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_envelope_late\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"metadata\":{\"late\":\"authority\"},\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text delta omits required logprobs",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_logprobs_missing\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"safe\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text delta logprobs has wrong type",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_logprobs_type\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"safe\",\"logprobs\":\"wrong\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text delta logprobs has malformed nested fields",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_logprobs_nested\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"safe\",\"logprobs\":[{\"token\":1,\"logprob\":\"bad\",\"top_logprobs\":{}}],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text done omits required logprobs",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_done_logprobs_missing\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text done logprobs has wrong type",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_done_logprobs_type\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":\"wrong\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text done logprobs has malformed nested fields",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_done_logprobs_nested\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[{\"token\":1,\"logprob\":\"bad\",\"top_logprobs\":{}}],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text logprobs has wrong type",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_logprobs_type\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[],\"logprobs\":\"wrong\"}]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output text logprobs has malformed nested fields",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_logprobs_nested\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[],\"logprobs\":[{\"token\":1,\"bytes\":null,\"logprob\":\"bad\",\"top_logprobs\":{}}]}]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "output item annotation is replaced by part added",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_initial_annotation_replace\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"attacker\",\"url\":\"https://attacker.test\"}]}]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"safe\",\"url\":\"https://safe.test\"}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "part added annotation disappears",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotation_disappears\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":4}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "annotation append skips an index",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotation_gap\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.annotation.added\",\"annotation\":{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"},\"annotation_index\":1,\"content_index\":0,\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "part added annotation is replaced",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotation_replace\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"attacker\",\"url\":\"https://attacker.test\"}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1}]}],\"delta\":\"safe\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.output_text.annotation.added\",\"annotation\":{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"safe\",\"url\":\"https://safe.test\"},\"annotation_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":4}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsMalformedPresentImmutableResponseFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "instructions", field: `"instructions":123`},
		{name: "instructions nested", field: `"instructions":[1]`},
		{name: "instructions item type", field: `"instructions":[{"type":1}]`},
		{name: "instructions item unsupported", field: `"instructions":[{"type":"future"}]`},
		{name: "instructions message role", field: `"instructions":[{"type":"message","role":1,"content":"system"}]`},
		{name: "instructions message content", field: `"instructions":[{"type":"message","role":"system","content":false}]`},
		{name: "instructions function status", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}","status":"future"}]`},
		{name: "instructions file search status", field: `"instructions":[{"type":"file_search_call","id":"fs_1","queries":[],"status":"future"}]`},
		{name: "instructions empty function call id", field: `"instructions":[{"type":"function_call","call_id":"","name":"echo","arguments":"{}","status":"completed"}]`},
		{name: "instructions empty function name", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"","arguments":"{}","status":"completed"}]`},
		{name: "instructions invalid function arguments", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"not json","status":"completed"}]`},
		{name: "instructions ambiguous function arguments", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":1,\"value\":2}","status":"completed"}]`},
		{name: "instructions function namespace authority", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","namespace":"ns","arguments":"{}","status":"completed"}]`},
		{name: "instructions custom namespace authority", field: `"instructions":[{"type":"custom_tool_call","call_id":"call_1","name":"custom","namespace":"ns","input":"value"}]`},
		{name: "instructions mcp ambiguous arguments", field: `"instructions":[{"type":"mcp_call","id":"mcp_1","name":"read","server_label":"server","arguments":"{\"value\":1,\"value\":2}","status":"completed"}]`},
		{name: "instructions image generation status", field: `"instructions":[{"type":"image_generation_call","id":"image_1","result":"","status":"future"}]`},
		{name: "instructions tool search execution", field: `"instructions":[{"type":"tool_search_call","arguments":{},"execution":"future","status":"completed"}]`},
		{name: "instructions apply patch status", field: `"instructions":[{"type":"apply_patch_call","call_id":"call_1","operation":{"type":"delete_file","path":"file.txt"},"status":"future"}]`},
		{name: "instructions function sibling queries", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}","status":"completed","queries":["query"]}]`},
		{name: "instructions custom sibling status", field: `"instructions":[{"type":"custom_tool_call","call_id":"call_1","name":"custom","input":"value","status":"future"}]`},
		{name: "instructions compaction sibling identity", field: `"instructions":[{"type":"compaction","encrypted_content":"sealed","call_id":"call_1","name":"echo"}]`},
		{name: "instructions system output content", field: `"instructions":[{"type":"message","role":"system","content":[{"type":"output_text","text":"wrong variant","annotations":[]}]}]`},
		{name: "instructions incomplete assistant output", field: `"instructions":[{"type":"message","role":"assistant","status":"completed","content":"missing id"}]`},
		{name: "instructions nested image action", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"image_generation","action":"future"}]}]`},
		{name: "instructions nested image partial images", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"image_generation","partial_images":4}]}]`},
		{name: "instructions nested file search max results", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"file_search","vector_store_ids":["vs_1"],"max_num_results":51}]}]`},
		{name: "instructions nested file search ranker", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"file_search","vector_store_ids":["vs_1"],"ranking_options":{"ranker":"future"}}]}]`},
		{name: "instructions nested mcp connector", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","connector_id":"future"}]}]`},
		{name: "instructions unknown function field", field: `"instructions":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}","future_field":1}]`},
		{name: "instructions empty optional function id", field: `"instructions":[{"type":"function_call","id":"","call_id":"call_1","name":"echo","arguments":"{}"}]`},
		{name: "instructions padded optional function id", field: `"instructions":[{"type":"function_call","id":" fc_1 ","call_id":"call_1","name":"echo","arguments":"{}"}]`},
		{name: "instructions empty optional custom id", field: `"instructions":[{"type":"custom_tool_call","id":"","call_id":"call_1","name":"custom","input":"value"}]`},
		{name: "instructions empty nullable tool search output id", field: `"instructions":[{"type":"tool_search_output","id":"","tools":[]}]`},
		{name: "instructions empty nullable tool search output call id", field: `"instructions":[{"type":"tool_search_output","call_id":"","tools":[]}]`},
		{name: "instructions system assistant phase", field: `"instructions":[{"type":"message","role":"system","phase":"final_answer","content":"system"}]`},
		{name: "instructions user assistant phase", field: `"instructions":[{"type":"message","role":"user","phase":"commentary","content":"user"}]`},
		{name: "instructions developer assistant phase", field: `"instructions":[{"type":"message","role":"developer","phase":"final_answer","content":"developer"}]`},
		{name: "instructions nested mcp missing endpoint", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server"}]}]`},
		{name: "instructions nested mcp empty server url", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","server_url":""}]}]`},
		{name: "instructions nested mcp malformed server url", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","server_url":"not a URL"}]}]`},
		{name: "instructions nested mcp userinfo server url", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","server_url":"https://user@example.test/mcp"}]}]`},
		{name: "instructions nested mcp out of range port", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test:99999/mcp"}]}]`},
		{name: "instructions nested ranking unknown field", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"file_search","vector_store_ids":["vs_1"],"ranking_options":{"future_field":1}}]}]`},
		{name: "instructions nested mcp filter unknown field", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","allowed_tools":{"future_field":1}}]}]`},
		{name: "instructions nested mcp empty allowed tool", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","allowed_tools":[""]}]}]`},
		{name: "instructions nested file search empty stores", field: `"instructions":[{"type":"tool_search_output","tools":[{"type":"file_search","vector_store_ids":[]}]}]`},
		{name: "response unknown field", field: `"future_field":1`},
		{name: "response error type", field: `"error":"boom"`},
		{name: "response incomplete details type", field: `"incomplete_details":[]`},
		{name: "response completed at type", field: `"completed_at":"now"`},
		{name: "response usage type", field: `"usage":[]`},
		{name: "response usage scalar type", field: `"usage":{"input_tokens":"1","output_tokens":1,"total_tokens":2}`},
		{name: "response usage detail type", field: `"usage":{"input_tokens":1,"input_tokens_details":[],"output_tokens":1,"total_tokens":2}`},
		{name: "response usage nested scalar type", field: `"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":"1"},"output_tokens":1,"total_tokens":2}`},
		{name: "response usage negative", field: `"usage":{"input_tokens":-1,"output_tokens":1,"total_tokens":0}`},
		{name: "response usage inconsistent total", field: `"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":1}`},
		{name: "response usage cached exceeds input", field: `"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":2},"output_tokens":1,"total_tokens":2}`},
		{name: "empty previous response id", field: `"previous_response_id":""`},
		{name: "padded previous response id", field: `"previous_response_id":" resp_previous "`},
		{name: "metadata", field: `"metadata":[]`},
		{name: "metadata value", field: `"metadata":{"key":1}`},
		{name: "parallel tool calls", field: `"parallel_tool_calls":"false"`},
		{name: "temperature", field: `"temperature":"0.7"`},
		{name: "temperature range", field: `"temperature":3`},
		{name: "tool choice", field: `"tool_choice":[]`},
		{name: "tool choice mode", field: `"tool_choice":"future"`},
		{name: "tool choice nested", field: `"tool_choice":{"type":"function"}`},
		{name: "tool choice allowed tools", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[1]}`},
		{name: "tool choice allowed selector", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function"}]}`},
		{name: "tool choice selector crossover", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation","name":1}]}`},
		{name: "function tool choice padded name", field: `"tool_choice":{"type":"function","name":" echo "}`},
		{name: "custom tool choice padded name", field: `"tool_choice":{"type":"custom","name":" custom "}`},
		{name: "mcp tool choice padded server label", field: `"tool_choice":{"type":"mcp","server_label":" server "}`},
		{name: "mcp tool choice empty optional name", field: `"tool_choice":{"type":"mcp","server_label":"server","name":""}`},
		{name: "tool choice unknown field", field: `"tool_choice":{"type":"function","name":"echo","future_field":1}`},
		{name: "allowed selector padded name", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":" echo "}]}`},
		{name: "allowed selector padded server label", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"mcp","server_label":" server "}]}`},
		{name: "allowed selector empty optional name", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"mcp","server_label":"server","name":""}]}`},
		{name: "allowed selector unknown field", field: `"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"echo","future_field":1}]}`},
		{name: "tools", field: `"tools":{}`},
		{name: "tools nested", field: `"tools":[{"type":"function","name":"f"}]`},
		{name: "function tool description", field: `"tools":[{"type":"function","name":"f","parameters":{},"strict":true,"description":1}]`},
		{name: "function tool defer loading", field: `"tools":[{"type":"function","name":"f","parameters":{},"strict":true,"defer_loading":"yes"}]`},
		{name: "function tool variant crossover", field: `"tools":[{"type":"function","name":"f","parameters":{},"strict":true,"vector_store_ids":[1]}]`},
		{name: "file search vector ids", field: `"tools":[{"type":"file_search","vector_store_ids":[1]}]`},
		{name: "file search max results", field: `"tools":[{"type":"file_search","vector_store_ids":["vs_1"],"max_num_results":"many"}]`},
		{name: "file search ranking unknown field", field: `"tools":[{"type":"file_search","vector_store_ids":["vs_1"],"ranking_options":{"ranker":"auto","future_field":1}}]`},
		{name: "file search hybrid ranking unknown field", field: `"tools":[{"type":"file_search","vector_store_ids":["vs_1"],"ranking_options":{"hybrid_search":{"embedding_weight":0.5,"text_weight":0.5,"future_field":1}}}]`},
		{name: "file search filter unknown field", field: `"tools":[{"type":"file_search","vector_store_ids":["vs_1"],"filters":{"type":"eq","key":"kind","value":"text","future_field":1}}]`},
		{name: "mcp authorization", field: `"tools":[{"type":"mcp","server_label":"server","authorization":{}}]`},
		{name: "mcp headers", field: `"tools":[{"type":"mcp","server_label":"server","headers":[]}]`},
		{name: "mcp missing endpoint", field: `"tools":[{"type":"mcp","server_label":"server"}]`},
		{name: "mcp empty server url", field: `"tools":[{"type":"mcp","server_label":"server","server_url":""}]`},
		{name: "mcp duplicate endpoints", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test/mcp","connector_id":"connector_googledrive"}]`},
		{name: "mcp non url endpoint", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"not a URL"}]`},
		{name: "mcp hostless endpoint", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://"}]`},
		{name: "mcp url userinfo", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://user@example.test/mcp"}]`},
		{name: "mcp url uppercase host", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://EXAMPLE.test/mcp"}]`},
		{name: "mcp url escaped unreserved", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test/%7Eserver"}]`},
		{name: "mcp url escaped separator", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test/a%2Fb"}]`},
		{name: "mcp url default port", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test:443/mcp"}]`},
		{name: "mcp url out of range port", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test:65536/mcp"}]`},
		{name: "mcp url empty query", field: `"tools":[{"type":"mcp","server_label":"server","server_url":"https://example.test/mcp?"}]`},
		{name: "mcp empty allowed tool", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","allowed_tools":[""]}]`},
		{name: "mcp duplicate allowed tool", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","allowed_tools":["read","read"]}]`},
		{name: "mcp allowed filter unknown field", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","allowed_tools":{"read_only":true,"future_field":1}}]`},
		{name: "mcp approval unknown outer field", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","require_approval":{"always":{"read_only":true},"future_field":1}}]`},
		{name: "mcp approval filter unknown field", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","require_approval":{"always":{"read_only":true,"future_field":1}}}]`},
		{name: "mcp duplicate approval tool name", field: `"tools":[{"type":"mcp","server_label":"server","connector_id":"connector_googledrive","require_approval":{"always":{"tool_names":["read","read"]}}}]`},
		{name: "file search empty stores", field: `"tools":[{"type":"file_search","vector_store_ids":[]}]`},
		{name: "file search padded store", field: `"tools":[{"type":"file_search","vector_store_ids":[" vs_1 "]}]`},
		{name: "file search duplicate stores", field: `"tools":[{"type":"file_search","vector_store_ids":["vs_1","vs_1"]}]`},
		{name: "function tool unknown field", field: `"tools":[{"type":"function","name":"f","parameters":{},"strict":true,"future_field":1}]`},
		{name: "code interpreter file ids", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":[1]}}]`},
		{name: "code interpreter padded container id", field: `"tools":[{"type":"code_interpreter","container":" container_1 "}]`},
		{name: "code interpreter empty file id", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":[""]}}]`},
		{name: "code interpreter padded file id", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":[" file_1 "]}}]`},
		{name: "code interpreter container unknown field", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","future_field":1}}]`},
		{name: "code interpreter network policy unknown field", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":[],"future_field":1}}}]`},
		{name: "code interpreter domain secret unknown field", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"TOKEN","value":"secret","future_field":1}]}}}]`},
		{name: "code interpreter empty allowed domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":[""]}}}]`},
		{name: "code interpreter padded allowed domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":[" example.test "]}}}]`},
		{name: "code interpreter uppercase allowed domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["EXAMPLE.test"]}}}]`},
		{name: "code interpreter trailing dot domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["example.test."]}}}]`},
		{name: "code interpreter wildcard domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["*.example.test"]}}}]`},
		{name: "code interpreter empty secret domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":[],"domain_secrets":[{"domain":"","name":"TOKEN","value":"secret"}]}}}]`},
		{name: "code interpreter out of scope secret domain", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"attacker.test","name":"TOKEN","value":"secret"}]}}}]`},
		{name: "code interpreter duplicate secret authority", field: `"tools":[{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"TOKEN","value":"one"},{"domain":"example.test","name":"TOKEN","value":"two"}]}}}]`},
		{name: "shell empty file id", field: `"tools":[{"type":"shell","environment":{"type":"container_auto","file_ids":[""]}}]`},
		{name: "shell duplicate file id", field: `"tools":[{"type":"shell","environment":{"type":"container_auto","file_ids":["file_1","file_1"]}}]`},
		{name: "shell zero skill version", field: `"tools":[{"type":"shell","environment":{"type":"container_auto","skills":[{"type":"skill_reference","skill_id":"skill_1","version":"0"}]}}]`},
		{name: "shell padded skill version", field: `"tools":[{"type":"shell","environment":{"type":"container_auto","skills":[{"type":"skill_reference","skill_id":"skill_1","version":" 1 "}]}}]`},
		{name: "shell future skill version", field: `"tools":[{"type":"shell","environment":{"type":"container_auto","skills":[{"type":"skill_reference","skill_id":"skill_1","version":"future"}]}}]`},
		{name: "custom tool format", field: `"tools":[{"type":"custom","name":"custom","format":"text"}]`},
		{name: "computer use domain", field: `"tools":[{"type":"computer_use_preview","display_height":-1,"display_width":0,"environment":"future"}]`},
		{name: "namespace nested tool", field: `"tools":[{"type":"namespace","description":"namespace","name":"ns","tools":[{"type":"function"}]}]`},
		{name: "namespace empty nested function name", field: `"tools":[{"type":"namespace","description":"namespace","name":"ns","tools":[{"type":"function","name":"","parameters":null,"strict":null}]}]`},
		{name: "namespace empty nested custom name", field: `"tools":[{"type":"namespace","description":"namespace","name":"ns","tools":[{"type":"custom","name":"","format":{"type":"text"}}]}]`},
		{name: "web search location discriminator", field: `"tools":[{"type":"web_search_preview","user_location":{"type":"future"}}]`},
		{name: "web search location unknown field", field: `"tools":[{"type":"web_search_preview","user_location":{"type":"approximate","future_field":1}}]`},
		{name: "top p", field: `"top_p":"1"`},
		{name: "top p range", field: `"top_p":2`},
		{name: "background", field: `"background":"false"`},
		{name: "conversation", field: `"conversation":[]`},
		{name: "conversation nested", field: `"conversation":{}`},
		{name: "conversation empty id", field: `"conversation":{"id":""}`},
		{name: "conversation unknown field", field: `"conversation":{"id":"conv_1","future_field":1}`},
		{name: "max output tokens", field: `"max_output_tokens":"many"`},
		{name: "max tool calls", field: `"max_tool_calls":{}`},
		{name: "previous response id", field: `"previous_response_id":1`},
		{name: "prompt", field: `"prompt":[]`},
		{name: "prompt nested", field: `"prompt":{}`},
		{name: "prompt empty id", field: `"prompt":{"id":""}`},
		{name: "prompt unknown field", field: `"prompt":{"id":"prompt_1","future_field":1}`},
		{name: "prompt variables", field: `"prompt":{"id":"prompt_1","variables":{"value":1}}`},
		{name: "prompt input text", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_text"}}}`},
		{name: "prompt input image", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_image","detail":"future"}}}`},
		{name: "prompt input file", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_file","file_data":1}}}`},
		{name: "prompt variant crossover", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_text","text":"ok","detail":1}}}`},
		{name: "prompt variable unknown field", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_text","text":"ok","future_field":1}}}`},
		{name: "prompt image empty file id", field: `"prompt":{"id":"prompt_1","variables":{"value":{"type":"input_image","detail":"auto","file_id":""}}}`},
		{name: "instruction image padded file id", field: `"instructions":[{"type":"message","role":"user","content":[{"type":"input_image","detail":"auto","file_id":" file_1 "}]}]`},
		{name: "instruction annotation padded file id", field: `"instructions":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"prior","annotations":[{"type":"file_citation","file_id":" file_1 ","filename":"file.txt","index":0}]}]}]`},
		{name: "prompt cache key", field: `"prompt_cache_key":false`},
		{name: "prompt cache retention", field: `"prompt_cache_retention":{}`},
		{name: "prompt cache retention value", field: `"prompt_cache_retention":"future"`},
		{name: "reasoning", field: `"reasoning":[]`},
		{name: "reasoning nested", field: `"reasoning":{"effort":1}`},
		{name: "reasoning enum", field: `"reasoning":{"effort":"future"}`},
		{name: "reasoning unknown field", field: `"reasoning":{"effort":"medium","future_field":1}`},
		{name: "safety identifier", field: `"safety_identifier":{}`},
		{name: "service tier", field: `"service_tier":1`},
		{name: "service tier value", field: `"service_tier":"future"`},
		{name: "text", field: `"text":[]`},
		{name: "text nested", field: `"text":{"format":{"type":"json_schema"}}`},
		{name: "text schema types", field: `"text":{"format":{"type":"json_schema","name":1,"schema":[]}}`},
		{name: "text schema empty name", field: `"text":{"format":{"type":"json_schema","name":"","schema":{}}}`},
		{name: "text schema invalid name", field: `"text":{"format":{"type":"json_schema","name":"invalid.name","schema":{}}}`},
		{name: "text schema overlength name", field: `"text":{"format":{"type":"json_schema","name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","schema":{}}}`},
		{name: "text format crossover", field: `"text":{"format":{"type":"text","name":1}}`},
		{name: "text unknown field", field: `"text":{"format":{"type":"text"},"future_field":1}`},
		{name: "text format unknown field", field: `"text":{"format":{"type":"text","future_field":1}}`},
		{name: "text verbosity", field: `"text":{"verbosity":"future"}`},
		{name: "top logprobs", field: `"top_logprobs":"many"`},
		{name: "top logprobs range", field: `"top_logprobs":21`},
		{name: "truncation", field: `"truncation":[]`},
		{name: "truncation value", field: `"truncation":"future"`},
		{name: "user", field: `"user":[]`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseID := fmt.Sprintf("resp_immutable_type_%d", index)
			events := fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",%s,\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n", responseID, test.field) +
				fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",%s,\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n", responseID, test.field) +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsUnknownStreamAuthorityFields(t *testing.T) {
	const created = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_closed_stream\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n"
	tests := []struct {
		name   string
		events string
	}{
		{
			name:   "response envelope",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_closed_stream\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[],\"future_field\":1},\"sequence_number\":0}\n\n",
		},
		{
			name:   "stream event",
			events: created + "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_closed_stream\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1,\"future_field\":1}\n\n",
		},
		{
			name:   "output item",
			events: created + "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[],\"future_field\":1},\"output_index\":0,\"sequence_number\":1}\n\n",
		},
		{
			name:   "output content",
			events: created + "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[],\"future_field\":1}]},\"output_index\":0,\"sequence_number\":1}\n\n",
		},
		{
			name:   "function namespace authority",
			events: created + "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"namespace\":\"ns\",\"arguments\":\"{}\"},\"output_index\":0,\"sequence_number\":1}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsMalformedTerminalAuthority(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name:   "failed missing error",
			events: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "failed missing error message",
			events: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\"},\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "failed unsupported error code",
			events: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"future_error\",\"message\":\"boom\"},\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "failed empty error message",
			events: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"\"},\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "incomplete missing details",
			events: "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete\",\"status\":\"incomplete\",\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "incomplete unsupported reason",
			events: "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"future_reason\"},\"output\":[]},\"sequence_number\":0}\n\n",
		},
		{
			name:   "error event missing param",
			events: "data: {\"type\":\"error\",\"code\":\"bad_request\",\"message\":\"boom\",\"sequence_number\":0}\n\n",
		},
		{
			name:   "error event null param",
			events: "data: {\"type\":\"error\",\"code\":\"bad_request\",\"message\":\"boom\",\"param\":null,\"sequence_number\":0}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelAcceptsTypedImmutableResponseFieldsAndNullableNulls(t *testing.T) {
	const immutableFields = `"instructions":"system","metadata":{"key":"value"},"parallel_tool_calls":false,"temperature":0.7,"tool_choice":"auto","tools":[],"top_p":1,"background":null,"conversation":{"id":"conv_1"},"max_output_tokens":null,"max_tool_calls":null,"previous_response_id":null,"prompt":{"id":"prompt_1","variables":null,"version":null},"prompt_cache_key":"cache","prompt_cache_retention":null,"reasoning":{"effort":null,"generate_summary":null,"summary":null},"safety_identifier":"safe","service_tier":null,"text":{"format":{"type":"text"}},"top_logprobs":null,"truncation":null,"user":"user_1"`
	events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_typed_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + immutableFields + ",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_typed_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + immutableFields + ",\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n" +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.ID != "resp_typed_immutable" {
		t.Fatalf("response=%+v", response)
	}
}

func TestOpenAIModelAcceptsCanonicalTerminalAccounting(t *testing.T) {
	events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_accounting\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"error\":null,\"incomplete_details\":null,\"completed_at\":null,\"usage\":null,\"output\":[]},\"sequence_number\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_accounting\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"error\":null,\"incomplete_details\":null,\"completed_at\":2,\"usage\":{\"input_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":1},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":5},\"output\":[]},\"sequence_number\":1}\n\n" +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.ID != "resp_accounting" {
		t.Fatalf("response=%+v", response)
	}
}

func TestOpenAIModelAcceptsTypedImmutableToolChoiceAndTools(t *testing.T) {
	const fields = `"tool_choice":{"type":"function","name":"echo"},"tools":[{"type":"function","name":"echo","parameters":{"type":"object"},"strict":true,"description":null}]`
	events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_typed_tools\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_typed_tools\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n" +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	if _, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestOpenAIModelAcceptsStructuredInstructionsAndPromptVariables(t *testing.T) {
	const fields = `"instructions":[{"type":"message","role":"system","content":"system","phase":null},{"type":"message","role":"assistant","content":"prior answer","phase":"final_answer"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"prior answer","annotations":[]}]},{"type":"function_call","call_id":"call_0","name":"echo","arguments":"{}"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":"{}","status":"completed"},{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"file_search_call","id":"fs_1","queries":["query"],"status":"searching","results":null},{"type":"mcp_call","id":"mcp_1","arguments":"{}","name":"read","server_label":"server","status":"completed"},{"type":"tool_search_output","id":null,"call_id":null,"tools":[{"type":"image_generation","action":"auto","partial_images":3},{"type":"file_search","vector_store_ids":["vs_1"],"max_num_results":50,"ranking_options":{"ranker":"auto","score_threshold":1}},{"type":"mcp","server_label":"server","connector_id":"connector_googledrive"}]}],"prompt":{"id":"prompt_1","variables":{"plain":"value","rich":{"type":"input_text","text":"hello"}},"version":"1"}`
	events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_structured_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_structured_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n" +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	if _, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestOpenAIModelAcceptsFullyTypedNestedImmutableUnions(t *testing.T) {
	const fields = `"instructions":[{"type":"message","role":"system","content":"system"}],"previous_response_id":"resp_previous",` +
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"echo"},{"type":"mcp","server_label":"server","name":"read"}]},` +
		`"tools":[` +
		`{"type":"function","name":"echo","parameters":{},"strict":true,"description":null,"defer_loading":false},` +
		`{"type":"file_search","vector_store_ids":["vs_1"],"max_num_results":5,"ranking_options":{"ranker":"auto","score_threshold":0.5}},` +
		`{"type":"computer_use_preview","display_height":768,"display_width":1024,"environment":"browser"},` +
		`{"type":"mcp","server_label":"server","server_url":"https://example.test:8443/mcp?mode=read","allowed_tools":["read","write"],"headers":{"Authorization":"token"},"require_approval":"never"},` +
		`{"type":"code_interpreter","container":{"type":"auto","file_ids":["file_1"],"memory_limit":"4g","network_policy":{"type":"allowlist","allowed_domains":["example.com"],"domain_secrets":[{"domain":"example.com","name":"TOKEN","value":"secret"}]}}},` +
		`{"type":"custom","name":"grammar_tool","format":{"type":"grammar","definition":"start: WORD","syntax":"lark"}},` +
		`{"type":"namespace","description":"group","name":"ns","tools":[{"type":"function","name":"nested","parameters":null,"strict":null,"description":null,"defer_loading":false}]},` +
		`{"type":"image_generation","action":"auto","background":"auto","input_fidelity":null,"moderation":"auto","output_compression":100,"output_format":"png","partial_images":0,"quality":"auto","size":"auto"},` +
		`{"type":"shell","environment":{"type":"container_auto","file_ids":["file_2","file_3"],"skills":[{"type":"skill_reference","skill_id":"skill_latest","version":"latest"},{"type":"skill_reference","skill_id":"skill_v2","version":"2"}]}},{"type":"tool_search","description":null,"execution":"client","parameters":null},` +
		`{"type":"web_search_preview","search_content_types":["text","image"],"search_context_size":"medium","user_location":{"type":"approximate","city":null,"country":"US","region":null,"timezone":null}}],` +
		`"prompt":{"id":"prompt_1","variables":{"plain":"value","rich":{"type":"input_text","text":"hello"},"image":{"type":"input_image","detail":"auto","file_id":"file_3"},"file":{"type":"input_file","file_id":"file_4"}}},` +
		`"text":{"format":{"type":"json_schema","name":"result","schema":{},"description":"result schema","strict":null}}`
	events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_nested_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_nested_immutable\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\"," + fields + ",\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n" +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	if _, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestOpenAIModelValidatesFinalResponseBeforeResponseDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_invalid_final\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"future\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_invalid_final\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"future\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}]},\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	doneEvents := 0
	_, err = model.Complete(t.Context(), ModelRequest{
		Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		StreamSink: func(event ModelStreamEvent) {
			if event.Type == ModelStreamResponseDone {
				doneEvents++
			}
		},
	})
	if !errors.Is(err, ErrInvalidModelOutput) || doneEvents != 0 {
		t.Fatalf("Complete error=%v response done events=%d", err, doneEvents)
	}
}

func TestOpenAIModelAcceptsValidatedAuxiliaryLifecycleAndExplicitEmptyArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_aux_valid\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_aux_valid\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]},\"output_index\":0,\"sequence_number\":2}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"logprobs\":[],\"delta\":\"hello\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":4}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.done\",\"text\":\"hello\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":5}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":6}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":7}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":1,\"sequence_number\":8}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\",\"name\":\"echo\",\"item_id\":\"fc_1\",\"output_index\":1,\"sequence_number\":9}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"},\"output_index\":1,\"sequence_number\":10}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_aux_valid\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]},{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{}\"}]},\"sequence_number\":11}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	response, err := model.Complete(t.Context(), ModelRequest{
		Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if response.ID != "resp_aux_valid" || response.OutputText != "hello" || len(response.Items) != 2 || response.Items[1].Call == nil {
		t.Fatalf("response=%+v", response)
	}
}

func TestOpenAIModelAcceptsConsistentReasoningAndAnnotationLifecycles(t *testing.T) {
	tests := []struct {
		name   string
		events string
		check  func(*testing.T, *ModelResponse)
	}{
		{
			name: "reasoning summary content and encrypted replay",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning_consistent\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_reasoning_consistent\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[],\"content\":[]},\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_part.added\",\"part\":{\"type\":\"summary_text\",\"text\":\"\"},\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"summary\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_text.done\",\"text\":\"summary\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_part.done\",\"part\":{\"type\":\"summary_text\",\"text\":\"summary\"},\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"sequence_number\":6}\n\n" +
				"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"reasoning\",\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":7}\n\n" +
				"data: {\"type\":\"response.reasoning_text.done\",\"text\":\"reasoning\",\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":8}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"summary\"}],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"reasoning\"}],\"encrypted_content\":\"sealed\"},\"output_index\":0,\"sequence_number\":9}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reasoning_consistent\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"summary\"}],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"reasoning\"}],\"encrypted_content\":\"sealed\"}]},\"sequence_number\":10}\n\n" +
				"data: [DONE]\n\n",
			check: func(t *testing.T, response *ModelResponse) {
				t.Helper()
				if response.ID != "resp_reasoning_consistent" || len(response.Items) != 1 || response.Items[0].Type != ModelOutputReasoning {
					t.Fatalf("response=%+v", response)
				}
			},
		},
		{
			name: "output text annotation",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotation_consistent\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1}]}],\"delta\":\"safe\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.output_text.annotation.added\",\"annotation\":{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"},\"annotation_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"logprob\":-0.1}]}],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}],\"logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1}]}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":6}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}],\"logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1}]}]}]},\"output_index\":0,\"sequence_number\":7}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_annotation_consistent\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}],\"logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1,\"top_logprobs\":[{\"token\":\"safe\",\"bytes\":[115,97,102,101],\"logprob\":-0.1}]}]}]}]},\"sequence_number\":8}\n\n" +
				"data: [DONE]\n\n",
			check: func(t *testing.T, response *ModelResponse) {
				t.Helper()
				if response.OutputText != "safe" {
					t.Fatalf("response=%+v", response)
				}
			},
		},
		{
			name: "part annotation exact confirmation",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_annotation_confirmed\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.annotation.added\",\"annotation\":{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"},\"annotation_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"safe\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.content_part.done\",\"part\":{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}]},\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"sequence_number\":5}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}]}]},\"output_index\":0,\"sequence_number\":6}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_annotation_confirmed\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[{\"type\":\"url_citation\",\"start_index\":0,\"end_index\":4,\"title\":\"source\",\"url\":\"https://example.test\"}]}]}]},\"sequence_number\":7}\n\n" +
				"data: [DONE]\n\n",
			check: func(t *testing.T, response *ModelResponse) {
				t.Helper()
				if response.OutputText != "safe" {
					t.Fatalf("response=%+v", response)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			response, err := model.Complete(t.Context(), ModelRequest{Instructions: "Answer the user.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			test.check(t, response)
		})
	}
}

func TestOpenAIModelRejectsUnboundStreamEventCoordinates(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "item event output index differs from added item",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wrong_event_index\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"spoofed\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"sequence_number\":2}\n\n",
		},
		{
			name: "text evidence index is absent from final item",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_dangling_content\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"spoofed\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":1,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"text\":\"spoofed\",\"logprobs\":[],\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":1,\"sequence_number\":3}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]},\"output_index\":0,\"sequence_number\":4}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_dangling_content\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"safe\",\"annotations\":[]}]}]},\"sequence_number\":5}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{
				Instructions: "Answer the user.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
			})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsFunctionArgumentEvidenceWithoutArgumentsDone(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "added arguments contradict item done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_added_without_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"added\\\"}\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"},\"output_index\":0,\"sequence_number\":2}\n\n",
		},
		{
			name: "argument deltas contradict item done",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delta_without_done\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"value\\\":\\\"delta\\\"}\",\"item_id\":\"fc_1\",\"output_index\":0,\"sequence_number\":2}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"final\\\"}\"},\"output_index\":0,\"sequence_number\":3}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{
				Instructions: "Answer the user.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
			})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsReasoningDriftBetweenItemAddedAndDone(t *testing.T) {
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "summary changes",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_summary_added_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"first\"}]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"second\"}]},\"output_index\":0,\"sequence_number\":2}\n\n",
		},
		{
			name: "reasoning content changes",
			events: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_content_added_drift\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"first\"}]},\"output_index\":0,\"sequence_number\":1}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[],\"content\":[{\"type\":\"reasoning_text\",\"text\":\"second\"}]},\"output_index\":0,\"sequence_number\":2}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.events)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			_, err = model.Complete(t.Context(), ModelRequest{
				Instructions: "Answer the user.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
			})
			if !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete error=%v, want ErrInvalidModelOutput", err)
			}
		})
	}
}

func TestOpenAIModelRejectsTransportErrorAfterCompletedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_trailing_transport_error\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_trailing_transport_error\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"completed\",\"output\":[]},\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "data: {not-json}\n\n")
	}))
	defer server.Close()
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	var chunks []ModelStreamEvent
	response, err := model.Complete(t.Context(), ModelRequest{
		Instructions: "Answer the user.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		StreamSink:   func(event ModelStreamEvent) { chunks = append(chunks, event) },
	})
	if response != nil || err == nil || !strings.Contains(err.Error(), "OpenAI stream failed") {
		t.Fatalf("Complete response=%+v error=%v, want trailing transport failure", response, err)
	}
	if len(chunks) < 2 || chunks[len(chunks)-2].Type != ModelStreamResponseDone ||
		chunks[len(chunks)-1].Type != ModelStreamError || chunks[len(chunks)-1].ErrorCode != "transport_error" {
		t.Fatalf("chunks=%+v, want response done followed by transport error", chunks)
	}
}
