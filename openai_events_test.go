package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestOpenAIModelEmitsStructuredErrorChunks(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		providerType string
		code         string
		hasSequence  bool
	}{
		{
			name:         "failed",
			payload:      "data: {\"type\":\"response.failed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"},\"output\":[]}}\n\n",
			providerType: "response.failed", code: "server_error", hasSequence: true,
		},
		{
			name:         "incomplete",
			payload:      "data: {\"type\":\"response.incomplete\",\"sequence_number\":2,\"response\":{\"id\":\"resp_incomplete\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[]}}\n\n",
			providerType: "response.incomplete", code: "incomplete", hasSequence: true,
		},
		{
			name:         "provider error",
			payload:      "data: {\"type\":\"error\",\"sequence_number\":2,\"code\":\"bad_request\",\"message\":\"boom\"}\n\n",
			providerType: "error", code: "bad_request", hasSequence: true,
		},
		{
			name: "transport decode error", payload: "data: {not-json}\n\n",
			providerType: "transport.error", code: "transport_error", hasSequence: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.payload)
			}))
			defer server.Close()

			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), OpenAIModelConfig{Model: "test-model"})
			if err != nil {
				t.Fatalf("NewOpenAIModel: %v", err)
			}
			var chunks []ModelStreamEvent
			_, err = model.Complete(context.Background(), ModelRequest{
				Instructions: "Answer.",
				Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
				StreamSink:   func(event ModelStreamEvent) { chunks = append(chunks, event) },
			})
			if err == nil {
				t.Fatal("expected stream error")
			}
			if len(chunks) == 0 {
				t.Fatal("expected error chunk")
			}
			chunk := chunks[len(chunks)-1]
			if chunk.Type != ModelStreamError || chunk.ProviderType != test.providerType || chunk.ErrorCode != test.code {
				t.Fatalf("chunk=%+v", chunk)
			}
			if test.hasSequence != (chunk.SequenceNumber != nil) || chunk.OutputIndex != nil {
				t.Fatalf("optional positions chunk=%+v", chunk)
			}
		})
	}
}

func TestPublicEventJSONOmitsProviderRaw(t *testing.T) {
	encoded, err := json.Marshal(Event{
		Type: EventModelStreamChunk,
		Chunk: &ModelStreamEvent{
			Type: ModelStreamTextDelta, Delta: "visible delta",
			RawJSON: `{"provider_secret":"must-not-cross-boundary"}`,
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "provider_secret") || !strings.Contains(string(encoded), "visible delta") {
		t.Fatalf("public event JSON=%s", encoded)
	}

	toolEvent, err := json.Marshal(Event{
		Type: EventModelStreamChunk,
		Chunk: &ModelStreamEvent{
			Type: ModelStreamToolArgumentsDone, CallID: "call-1",
			Delta: `{"secret":"delta"}`, Arguments: `{"secret":"complete"}`,
		},
	})
	if err != nil {
		t.Fatalf("Marshal tool event: %v", err)
	}
	if strings.Contains(string(toolEvent), "secret") || !strings.Contains(string(toolEvent), "call-1") {
		t.Fatalf("public tool event JSON=%s", toolEvent)
	}
}

func TestPublicEventJSONOmitsTrustedOperationData(t *testing.T) {
	for _, eventType := range []EventType{EventOperationStarted, EventOperationCompleted} {
		encoded, err := json.Marshal(Event{
			Type: eventType, RunID: "run-1", Operation: "call_api", CallID: "call-1",
			Data: json.RawMessage(`{"api_key":"must-not-cross-boundary"}`),
		})
		if err != nil {
			t.Fatalf("Marshal %s: %v", eventType, err)
		}
		if strings.Contains(string(encoded), "api_key") || strings.Contains(string(encoded), "must-not-cross-boundary") || strings.Contains(string(encoded), `"data"`) {
			t.Fatalf("public %s JSON=%s", eventType, encoded)
		}
		if !strings.Contains(string(encoded), "call-1") || !strings.Contains(string(encoded), "call_api") {
			t.Fatalf("public %s JSON lost safe identifiers: %s", eventType, encoded)
		}
	}
}

func TestPublicEventJSONExposesOnlySafeErrors(t *testing.T) {
	encoded, err := json.Marshal(Event{
		Type: EventRunFailed, RunID: "run-1",
		ErrorCode: "internal_error", Error: "database password=must-not-cross-boundary",
		Chunk: &ModelStreamEvent{
			Type: ModelStreamError, ErrorCode: "bad code with details",
			ErrorMessage: "provider token=must-not-cross-boundary",
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var public map[string]any
	if err := json.Unmarshal(encoded, &public); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, exists := public["error"]; exists || public["error_code"] != "internal_error" {
		t.Fatalf("public event error fields=%#v", public)
	}
	chunk, ok := public["chunk"].(map[string]any)
	if !ok || chunk["error_code"] != "provider_error" {
		t.Fatalf("public chunk=%#v", public["chunk"])
	}
	if _, exists := chunk["error_message"]; exists || strings.Contains(string(encoded), "must-not-cross-boundary") {
		t.Fatalf("public event JSON=%s", encoded)
	}
}

func TestParseOpenAIResponsePreservesNativeOutputItems(t *testing.T) {
	var raw responses.Response
	if err := json.Unmarshal([]byte(`{
		"id":"resp_123",
		"status":"completed",
		"output":[
			{"id":"rs_1","type":"reasoning","summary":[],"status":"completed"},
			{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_context","arguments":"{\"id\":\"doc1\"}","status":"completed"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`), &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, err := parseOpenAIResponse(&raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if got.ID != "resp_123" || len(got.Items) != 2 {
		t.Fatalf("response=%+v", got)
	}
	if !got.HadReasoning || got.FinishReason != "completed" {
		t.Fatalf("response reasoning metadata=%+v", got)
	}
	if got.Items[0].Type != ModelOutputReasoning || len(got.Items[0].Raw) == 0 {
		t.Fatalf("reasoning item=%+v", got.Items[0])
	}
	call := got.Items[1].Call
	if call == nil || call.ID != "call_1" || call.Name != "read_context" || string(call.Input) != `{"id":"doc1"}` {
		t.Fatalf("function call=%+v", call)
	}
	var replayedCall map[string]any
	if err := json.Unmarshal(got.Items[1].Raw, &replayedCall); err != nil {
		t.Fatalf("unmarshal raw function call: %v", err)
	}
	if replayedCall["arguments"] != `{"id":"doc1"}` {
		t.Fatalf("raw arguments=%#v", replayedCall["arguments"])
	}
	if got.Usage.TotalTokens != 15 {
		t.Fatalf("usage=%+v", got.Usage)
	}
}

func TestParseOpenAIResponseExcludesCommentaryFromFinalOutput(t *testing.T) {
	var raw responses.Response
	if err := json.Unmarshal([]byte(`{
		"id":"resp_phases",
		"status":"completed",
		"output":[
			{"id":"msg_commentary","type":"message","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"checking tools","annotations":[]}]},
			{"id":"msg_final","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"final answer","annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
	}`), &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, err := parseOpenAIResponse(&raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if got.OutputText != "final answer" || strings.Contains(got.OutputText, "checking tools") {
		t.Fatalf("output text=%q", got.OutputText)
	}
	if len(got.Items) != 2 {
		t.Fatalf("native output items=%d, want 2", len(got.Items))
	}
}

func TestParseOpenAIResponseRejectsUnsupportedOutputItem(t *testing.T) {
	var raw responses.Response
	if err := json.Unmarshal([]byte(`{
		"id":"resp_unsupported",
		"status":"completed",
		"output":[{"id":"ws_1","type":"web_search_call","status":"completed"}],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
	}`), &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	_, err := parseOpenAIResponse(&raw)
	if err == nil || !strings.Contains(err.Error(), "unsupported OpenAI output item type") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseOpenAIResponseRejectsUnsupportedMessageContent(t *testing.T) {
	var raw responses.Response
	if err := json.Unmarshal([]byte(`{
		"id":"resp_unsupported_content",
		"status":"completed",
		"output":[{
			"id":"msg_1","type":"message","role":"assistant","status":"completed",
			"content":[{"type":"input_text","text":"not valid output content"}]
		}],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
	}`), &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	_, err := parseOpenAIResponse(&raw)
	if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildOpenAIInputItemsReplaysFunctionCallBeforeToolResult(t *testing.T) {
	items, err := buildOpenAIInputItems([]ModelInputItem{
		{Type: ModelInputUserMessage, Text: "remember red"},
		{
			Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall,
			Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"memory_put","arguments":"{\"key\":\"color\",\"value\":\"red\"}","status":"completed"}`),
		},
		{Type: ModelInputToolResult, CallID: "call_1", Output: json.RawMessage(`{"stored":true}`)},
	})
	if err != nil {
		t.Fatalf("buildOpenAIInputItems: %v", err)
	}
	if len(items) != 3 || items[1].OfFunctionCall == nil || items[2].OfFunctionCallOutput == nil {
		t.Fatalf("items=%+v", items)
	}
	if items[1].OfFunctionCall.CallID != "call_1" || items[2].OfFunctionCallOutput.CallID != "call_1" {
		t.Fatalf("function call replay=%+v result=%+v", items[1], items[2])
	}
}

func TestBuildOpenAIInputItemsIncludesImageAttachments(t *testing.T) {
	items, err := buildOpenAIInputItems([]ModelInputItem{{
		Type: ModelInputUserMessage, Text: "inspect",
		Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage,
			ID:   "attachment-1", Filename: "image.png", MIMEType: "image/png",
			URL: "https://cdn.example.com/image.png",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].OfMessage == nil {
		t.Fatalf("items=%+v", items)
	}
	parts := items[0].OfMessage.Content.OfInputItemContentList
	if len(parts) != 2 || parts[0].OfInputText == nil || parts[1].OfInputImage == nil ||
		!parts[1].OfInputImage.ImageURL.Valid() || parts[1].OfInputImage.ImageURL.Value != "https://cdn.example.com/image.png" {
		t.Fatalf("content parts=%+v", parts)
	}
}

func TestBuildOpenAIInputItemsRejectsAttachmentsOnNonUserItems(t *testing.T) {
	_, err := buildOpenAIInputItems([]ModelInputItem{{
		Type: ModelInputToolResult, CallID: "call-1", Output: json.RawMessage(`{"ok":true}`),
		Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentImage,
			ID:   "attachment-1", Filename: "image.png", MIMEType: "image/png",
			URL: "https://cdn.example.com/image.png",
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "cannot contain attachments") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateImageAttachmentRejectsDataURLs(t *testing.T) {
	err := ValidateImageAttachment(ModelInputAttachment{
		Kind: ModelInputAttachmentImage,
		ID:   "attachment-1", Filename: "image.png", MIMEType: "image/png",
		URL: "data:image/png;base64,aGVsbG8=",
	})
	if err == nil || !strings.Contains(err.Error(), "absolute HTTPS URL") {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildOpenAIInputItemsIncludesTextAttachments(t *testing.T) {
	items, err := buildOpenAIInputItems([]ModelInputItem{{
		Type: ModelInputUserMessage, Text: "discuss this script",
		Attachments: []ModelInputAttachment{{
			Kind: ModelInputAttachmentText, ID: "attachment-1", Filename: "script.md",
			MIMEType: "text/markdown", Text: "# Opening\nHello.",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	parts := items[0].OfMessage.Content.OfInputItemContentList
	if len(parts) != 2 || parts[1].OfInputText == nil || !strings.Contains(parts[1].OfInputText.Text, "# Opening") || parts[1].OfInputImage != nil {
		t.Fatalf("content parts=%+v", parts)
	}
}

func TestBuildOpenAIReplayItemRejectsMismatchedRawType(t *testing.T) {
	_, err := buildOpenAIReplayItem(ModelInputItem{
		Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage,
		Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_context","arguments":"{}","status":"completed"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match raw type") {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenAIModelCachesStrictToolMapping(t *testing.T) {
	model := &OpenAIModel{toolCache: make(map[string][]responses.ToolUnionParam)}
	tools := []ToolDefinition{{
		Name: "read_context", Description: "read",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}}
	toolSetID := toolDefinitionsID(tools)
	first, err := model.cachedOpenAITools(toolSetID, tools)
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.cachedOpenAITools(toolSetID, tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.toolCache) != 1 || len(first) != 1 || len(second) != 1 || &first[0] != &second[0] {
		t.Fatalf("tool mapping was not reused")
	}
}

func TestOpenAIModelRejectsMismatchedToolSetID(t *testing.T) {
	model := &OpenAIModel{toolCache: make(map[string][]responses.ToolUnionParam)}
	tools := []ToolDefinition{{
		Name:        "read_context",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}
	if _, err := model.cachedOpenAITools("stale-tool-set", tools); err == nil {
		t.Fatal("expected mismatched tool set id error")
	}
	if len(model.toolCache) != 0 {
		t.Fatalf("tool cache entries=%d, want 0", len(model.toolCache))
	}
}

func TestOpenAIModelRejectsNullToolSchemaWithoutPanicking(t *testing.T) {
	model := &OpenAIModel{toolCache: make(map[string][]responses.ToolUnionParam)}
	tools := []ToolDefinition{{Name: "read_context", InputSchema: json.RawMessage(`null`)}}
	if _, err := model.cachedOpenAITools("", tools); err == nil || !strings.Contains(err.Error(), "schema must be a JSON object") {
		t.Fatalf("err=%v", err)
	}
	if len(model.toolCache) != 0 {
		t.Fatalf("tool cache entries=%d, want 0", len(model.toolCache))
	}
}

func TestToolDefinitionsIDUsesUnambiguousFieldEncoding(t *testing.T) {
	left := toolDefinitionsID([]ToolDefinition{{
		Name: "a", Description: "b\x00c", InputSchema: json.RawMessage("d"),
	}})
	right := toolDefinitionsID([]ToolDefinition{{
		Name: "a\x00b", Description: "c", InputSchema: json.RawMessage("d"),
	}})
	if left == right {
		t.Fatalf("distinct tool definitions produced the same ID: %s", left)
	}
}
