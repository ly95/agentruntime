package agentruntime

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
