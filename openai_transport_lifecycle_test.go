package agentruntime

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
