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
			name: "failed",
			payload: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_failed\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.failed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_failed\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"},\"output\":[]}}\n\n",
			providerType: "response.failed", code: "server_error", hasSequence: true,
		},
		{
			name: "incomplete",
			payload: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_incomplete\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.incomplete\",\"sequence_number\":2,\"response\":{\"id\":\"resp_incomplete\",\"object\":\"response\",\"created_at\":1,\"model\":\"test-model\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[]}}\n\n",
			providerType: "response.incomplete", code: "incomplete", hasSequence: true,
		},
		{
			name:         "provider error",
			payload:      "data: {\"type\":\"error\",\"sequence_number\":2,\"code\":\"bad_request\",\"message\":\"boom\",\"param\":\"request\"}\n\n",
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

			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), openAITestModelConfig("test-model"))
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

func TestOpenAIModelRejectsTerminalFailureAuthority(t *testing.T) {
	const privateModel = "provider-private-model-marker"
	responseEvent := func(eventType string, sequence int64, response map[string]any) map[string]any {
		return map[string]any{"type": eventType, "sequence_number": sequence, "response": response}
	}
	failed := func(id, model string) map[string]any {
		response := openAIEventsTestResponse(id, model, "failed")
		response["error"] = map[string]any{"code": "server_error", "message": "private failure"}
		return response
	}
	incomplete := func(id, model string) map[string]any {
		response := openAIEventsTestResponse(id, model, "incomplete")
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		return response
	}

	tests := []struct {
		name   string
		marker string
		build  func() []map[string]any
	}{
		{
			name: "failed requires response created",
			build: func() []map[string]any {
				return []map[string]any{responseEvent("response.failed", 0, failed("resp_failed", "test-model"))}
			},
		},
		{
			name: "incomplete requires response created",
			build: func() []map[string]any {
				return []map[string]any{responseEvent("response.incomplete", 0, incomplete("resp_incomplete", "test-model"))}
			},
		},
		{
			name: "failed response id must match",
			build: func() []map[string]any {
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.failed", 1, failed("resp_other", "test-model")),
				}
			},
		},
		{
			name: "incomplete response id must match",
			build: func() []map[string]any {
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.incomplete", 1, incomplete("resp_other", "test-model")),
				}
			},
		},
		{
			name: "failed response model is required",
			build: func() []map[string]any {
				terminal := failed("resp_authority", "")
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.failed", 1, terminal),
				}
			},
		},
		{
			name:   "incomplete response model must match without leaking",
			marker: privateModel,
			build: func() []map[string]any {
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.incomplete", 1, incomplete("resp_authority", privateModel)),
				}
			},
		},
		{
			name: "failed immutable envelope must match",
			build: func() []map[string]any {
				terminal := failed("resp_authority", "test-model")
				terminal["created_at"] = int64(2)
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.failed", 1, terminal),
				}
			},
		},
		{
			name: "failed event status must agree",
			build: func() []map[string]any {
				terminal := failed("resp_authority", "test-model")
				terminal["status"] = "completed"
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.failed", 1, terminal),
				}
			},
		},
		{
			name: "incomplete event status must agree",
			build: func() []map[string]any {
				terminal := incomplete("resp_authority", "test-model")
				terminal["status"] = "completed"
				return []map[string]any{
					openAIEventsTestCreated("resp_authority", 0),
					responseEvent("response.incomplete", 1, terminal),
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(test.build()...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete response=%+v error=%v, want ErrInvalidModelOutput", response, err)
			}
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("failed Complete emitted response_done: %+v", chunks)
				}
			}
			if test.marker == "" {
				return
			}
			if strings.Contains(err.Error(), test.marker) {
				t.Fatalf("top-level model authority error leaked provider model: %v", err)
			}
			trustedRaw := false
			for _, chunk := range chunks {
				if chunk.Type != ModelStreamError || !strings.Contains(chunk.RawJSON, test.marker) {
					continue
				}
				trustedRaw = true
				public, marshalErr := json.Marshal(chunk)
				if marshalErr != nil {
					t.Fatalf("Marshal stream error: %v", marshalErr)
				}
				if strings.Contains(string(public), test.marker) {
					t.Fatalf("public stream error leaked provider model: %s", public)
				}
			}
			if !trustedRaw {
				t.Fatalf("trusted stream events did not retain model mismatch RawJSON: %+v", chunks)
			}
		})
	}
}

func TestOpenAIStreamTerminalProviderErrorClassification(t *testing.T) {
	const marker = "provider-private-stream-message"
	tests := []struct {
		name      string
		eventType string
		code      string
		category  ProviderErrorCategory
		sentinel  error
		retryable bool
	}{
		{name: "error authentication", eventType: "error", code: "invalid_api_key", category: ProviderErrorAuthentication, sentinel: ErrProviderAuthentication},
		{name: "error quota", eventType: "error", code: "insufficient_quota", category: ProviderErrorQuota, sentinel: ErrProviderQuotaExceeded},
		{name: "error rate limit", eventType: "error", code: "rate_limit_exceeded", category: ProviderErrorRateLimit, sentinel: ErrProviderRateLimited, retryable: true},
		{name: "error rejected", eventType: "error", code: "invalid_request_error", category: ProviderErrorRejected, sentinel: ErrProviderRequestRejected},
		{name: "error transient", eventType: "error", code: "server_error", category: ProviderErrorTransient, sentinel: ErrProviderUnavailable, retryable: true},
		{name: "failed rate limit", eventType: "response.failed", code: "rate_limit_exceeded", category: ProviderErrorRateLimit, sentinel: ErrProviderRateLimited, retryable: true},
		{name: "failed rejected", eventType: "response.failed", code: "invalid_prompt", category: ProviderErrorRejected, sentinel: ErrProviderRequestRejected},
		{name: "failed transient", eventType: "response.failed", code: "vector_store_timeout", category: ProviderErrorTransient, sentinel: ErrProviderUnavailable, retryable: true},
	}
	providerSentinels := []error{
		ErrProviderAuthentication,
		ErrProviderQuotaExceeded,
		ErrProviderRateLimited,
		ErrProviderRequestRejected,
		ErrProviderUnavailable,
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := []map[string]any{openAIEventsTestCreated("resp_provider_failure", 0)}
			if test.eventType == "error" {
				events = append(events, map[string]any{
					"type": "error", "sequence_number": int64(1), "code": test.code,
					"message": marker, "param": "request",
				})
			} else {
				terminal := openAIEventsTestResponse("resp_provider_failure", "test-model", "failed")
				terminal["error"] = map[string]any{"code": test.code, "message": marker}
				events = append(events, map[string]any{
					"type": "response.failed", "sequence_number": int64(1), "response": terminal,
				})
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || err == nil || !errors.Is(err, test.sentinel) {
				t.Fatalf("Complete response=%+v error=%v, want %v", response, err, test.sentinel)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("top-level provider error leaked private message: %v", err)
			}
			category, ok := ProviderErrorCategoryOf(err)
			if !ok || category != test.category || IsRetryableProviderError(err) != test.retryable {
				t.Fatalf("category=%q ok=%v retryable=%v", category, ok, IsRetryableProviderError(err))
			}
			for _, sentinel := range providerSentinels {
				if sentinel != test.sentinel && errors.Is(err, sentinel) {
					t.Fatalf("provider error also matched contradictory sentinel %v", sentinel)
				}
			}
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr == nil {
				t.Fatalf("error=%v does not expose *ProviderError", err)
			}
			if providerErr.StatusCode != 0 || providerErr.RequestID != "" || providerErr.Code != test.code {
				t.Fatalf("ProviderError=%+v, stream response id must not become HTTP request id", providerErr)
			}
			if cause := providerErr.Unwrap(); cause == nil || !strings.Contains(cause.Error(), marker) {
				t.Fatalf("trusted provider cause=%v does not retain private message", cause)
			}
			trustedFailure := false
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("provider failure emitted response_done: %+v", chunks)
				}
				if chunk.Type == ModelStreamError && strings.Contains(chunk.ErrorMessage, marker) && strings.Contains(chunk.RawJSON, marker) {
					trustedFailure = true
				}
			}
			if !trustedFailure {
				t.Fatalf("trusted stream events did not retain private failure: %+v", chunks)
			}
		})
	}
}

func TestOpenAIStreamRejectsEventAfterProviderTerminalAsProtocolError(t *testing.T) {
	const marker = "provider-private-terminal-before-protocol-error"
	inProgress := openAIEventsTestResponse("resp_terminal_then_event", "test-model", "in_progress")
	events := []map[string]any{
		openAIEventsTestCreated("resp_terminal_then_event", 0),
		{
			"type": "error", "sequence_number": int64(1), "code": "server_error",
			"message": marker, "param": "request",
		},
		{
			"type": "response.in_progress", "sequence_number": int64(2), "response": inProgress,
		},
	}

	response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
	if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
		t.Fatalf("Complete response=%+v error=%v, want ErrInvalidModelOutput", response, err)
	}
	if category, ok := ProviderErrorCategoryOf(err); ok || category != "" || IsRetryableProviderError(err) {
		t.Fatalf("protocol error category=%q ok=%v retryable=%v", category, ok, IsRetryableProviderError(err))
	}
	for _, chunk := range chunks {
		if chunk.Type == ModelStreamResponseDone {
			t.Fatalf("terminal protocol violation emitted response_done: %+v", chunks)
		}
	}
}

func TestOpenAIStreamOutputItemStatusMatchesLifecycle(t *testing.T) {
	allItemTypes := []string{"message", "reasoning", "function_call"}
	lifecycleTests := []struct {
		name          string
		eventType     string
		status        any
		includeStatus bool
		itemTypes     []string
	}{
		{name: "added missing required status", eventType: "response.output_item.added", itemTypes: []string{"message"}},
		{name: "added null status", eventType: "response.output_item.added", includeStatus: true, itemTypes: allItemTypes},
		{name: "added completed status", eventType: "response.output_item.added", status: "completed", includeStatus: true, itemTypes: allItemTypes},
		{name: "added incomplete status", eventType: "response.output_item.added", status: "incomplete", includeStatus: true, itemTypes: allItemTypes},
		{name: "done missing required status", eventType: "response.output_item.done", itemTypes: []string{"message"}},
		{name: "done null status", eventType: "response.output_item.done", includeStatus: true, itemTypes: allItemTypes},
		{name: "done in progress status", eventType: "response.output_item.done", status: "in_progress", includeStatus: true, itemTypes: allItemTypes},
	}

	for _, lifecycle := range lifecycleTests {
		for _, itemType := range lifecycle.itemTypes {
			t.Run(itemType+"/"+lifecycle.name, func(t *testing.T) {
				itemID := "item_status_" + itemType
				events := []map[string]any{openAIEventsTestCreated("resp_item_status", 0)}
				sequence := int64(1)
				if lifecycle.eventType == "response.output_item.done" {
					events = append(events, openAIEventsTestItemEvent(
						"response.output_item.added",
						openAIEventsTestItem(itemType, itemID, "in_progress", true),
						0,
						sequence,
					))
					sequence++
				}
				events = append(events, openAIEventsTestItemEvent(
					lifecycle.eventType,
					openAIEventsTestItem(itemType, itemID, lifecycle.status, lifecycle.includeStatus),
					0,
					sequence,
				))

				response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
				if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
					t.Fatalf("Complete response=%+v error=%v, want ErrInvalidModelOutput", response, err)
				}
				for _, chunk := range chunks {
					if chunk.Type == ModelStreamResponseDone {
						t.Fatalf("invalid item lifecycle emitted response_done: %+v", chunks)
					}
				}
			})
		}
	}
}

func TestOpenAIStreamAcceptsOptionalReasoningAndFunctionStatuses(t *testing.T) {
	tests := []struct {
		name                   string
		addedStatus            any
		includeAddedStatus     bool
		doneStatus             any
		includeDoneStatus      bool
		completedStatus        any
		includeCompletedStatus bool
	}{
		{name: "all omitted"},
		{name: "done only", doneStatus: "completed", includeDoneStatus: true},
		{
			name: "added and completed only", addedStatus: "in_progress", includeAddedStatus: true,
			completedStatus: "completed", includeCompletedStatus: true,
		},
	}
	for _, itemType := range []string{"reasoning", "function_call"} {
		for _, test := range tests {
			t.Run(itemType+"/"+test.name, func(t *testing.T) {
				itemID := "item_optional_status_" + itemType
				added := openAIEventsTestItem(itemType, itemID, test.addedStatus, test.includeAddedStatus)
				done := openAIEventsTestItem(itemType, itemID, test.doneStatus, test.includeDoneStatus)
				completedItem := openAIEventsTestItem(itemType, itemID, test.completedStatus, test.includeCompletedStatus)
				completed := openAIEventsTestResponse("resp_optional_status", "test-model", "completed")
				completed["output"] = []any{completedItem}
				events := []map[string]any{
					openAIEventsTestCreated("resp_optional_status", 0),
					openAIEventsTestItemEvent("response.output_item.added", added, 0, 1),
				}
				sequence := int64(2)
				if itemType == "function_call" {
					events = append(events, map[string]any{
						"type": "response.function_call_arguments.done", "sequence_number": sequence,
						"item_id": itemID, "output_index": int64(0), "name": "echo", "arguments": "{}",
					})
					sequence++
				}
				events = append(events,
					openAIEventsTestItemEvent("response.output_item.done", done, 0, sequence),
					map[string]any{"type": "response.completed", "sequence_number": sequence + 1, "response": completed},
				)

				response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
				if err != nil || response == nil || response.ID != "resp_optional_status" || len(response.Items) != 1 {
					t.Fatalf("Complete response=%+v error=%v", response, err)
				}
				if response.Items[0].Type != ModelOutputItemType(itemType) {
					t.Fatalf("response item=%+v, want type %q", response.Items[0], itemType)
				}
				rawValue, err := decodeExactJSON(response.Items[0].Raw)
				if err != nil {
					t.Fatalf("decode response raw: %v", err)
				}
				rawObject, ok := rawValue.(map[string]any)
				if !ok {
					t.Fatalf("response raw=%s, want object", response.Items[0].Raw)
				}
				rawStatus, hasRawStatus := rawObject["status"]
				if hasRawStatus != test.includeCompletedStatus || hasRawStatus && rawStatus != test.completedStatus {
					t.Fatalf("response raw status=%#v present=%t, want provider envelope status=%#v present=%t", rawStatus, hasRawStatus, test.completedStatus, test.includeCompletedStatus)
				}
				if itemType == "function_call" && response.Items[0].Call == nil {
					t.Fatalf("response function call=%+v", response.Items[0])
				}
				responseDone := 0
				for _, chunk := range chunks {
					if chunk.Type == ModelStreamResponseDone {
						responseDone++
					}
				}
				if responseDone != 1 {
					t.Fatalf("response_done=%d chunks=%+v, want one", responseDone, chunks)
				}
			})
		}
	}
}

func TestOpenAIStreamIncompleteItemDoneReachesTerminalIncomplete(t *testing.T) {
	for _, itemType := range []string{"message", "reasoning", "function_call"} {
		t.Run(itemType, func(t *testing.T) {
			itemID := "item_incomplete_" + itemType
			added := openAIEventsTestItem(itemType, itemID, "in_progress", true)
			done := openAIEventsTestItem(itemType, itemID, "incomplete", true)
			terminal := openAIEventsTestResponse("resp_incomplete_item", "test-model", "incomplete")
			terminal["output"] = []any{done}
			terminal["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			events := []map[string]any{
				openAIEventsTestCreated("resp_incomplete_item", 0),
				openAIEventsTestItemEvent("response.output_item.added", added, 0, 1),
				openAIEventsTestItemEvent("response.output_item.done", done, 0, 2),
				{"type": "response.incomplete", "sequence_number": int64(3), "response": terminal},
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete response=%+v error=%v, want terminal incomplete ErrInvalidModelOutput", response, err)
			}
			if strings.Contains(err.Error(), "status contradicts") {
				t.Fatalf("incomplete item was rejected before terminal response: %v", err)
			}
			if category, ok := ProviderErrorCategoryOf(err); ok {
				t.Fatalf("terminal incomplete category=%q, want invalid model output", category)
			}
			itemDone := 0
			for _, chunk := range chunks {
				switch chunk.Type {
				case ModelStreamItemDone:
					itemDone++
				case ModelStreamResponseDone:
					t.Fatalf("terminal incomplete emitted response_done: %+v", chunks)
				}
			}
			if itemDone != 1 {
				t.Fatalf("item_done=%d chunks=%+v, want lifecycle accepted before terminal incomplete", itemDone, chunks)
			}
		})
	}
}

func TestOpenAICompletedResponseRejectsDoneStatusMismatch(t *testing.T) {
	for _, itemType := range []string{"message", "reasoning", "function_call"} {
		t.Run(itemType, func(t *testing.T) {
			itemID := "item_done_status_mismatch_" + itemType
			added := openAIEventsTestItem(itemType, itemID, "in_progress", true)
			done := openAIEventsTestItem(itemType, itemID, "incomplete", true)
			completedItem := openAIEventsTestItem(itemType, itemID, "completed", true)
			completed := openAIEventsTestResponse("resp_done_status_mismatch", "test-model", "completed")
			completed["output"] = []any{completedItem}
			events := []map[string]any{
				openAIEventsTestCreated("resp_done_status_mismatch", 0),
				openAIEventsTestItemEvent("response.output_item.added", added, 0, 1),
				openAIEventsTestItemEvent("response.output_item.done", done, 0, 2),
				{"type": "response.completed", "sequence_number": int64(3), "response": completed},
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete response=%+v error=%v, want status mismatch ErrInvalidModelOutput", response, err)
			}
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("status mismatch emitted response_done: %+v", chunks)
				}
			}
		})
	}
}

func TestOpenAICompletedResponseRejectsContradictoryItemStatus(t *testing.T) {
	for _, itemType := range []string{"message", "reasoning", "function_call"} {
		t.Run(itemType, func(t *testing.T) {
			itemID := "item_completed_status_" + itemType
			added := openAIEventsTestItem(itemType, itemID, "in_progress", true)
			done := openAIEventsTestItem(itemType, itemID, "completed", true)
			contradictory := openAIEventsTestItem(itemType, itemID, "incomplete", true)
			completed := openAIEventsTestResponse("resp_completed_status", "test-model", "completed")
			completed["output"] = []any{contradictory}
			events := []map[string]any{
				openAIEventsTestCreated("resp_completed_status", 0),
				openAIEventsTestItemEvent("response.output_item.added", added, 0, 1),
				openAIEventsTestItemEvent("response.output_item.done", done, 0, 2),
				{"type": "response.completed", "sequence_number": int64(3), "response": completed},
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("Complete response=%+v error=%v, want ErrInvalidModelOutput", response, err)
			}
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("contradictory completed item emitted response_done: %+v", chunks)
				}
			}
		})
	}
}

func TestOpenAICompletedResponseRejectsUnfinishedAddedItem(t *testing.T) {
	completed := openAIEventsTestResponse("resp_unfinished_item", "test-model", "completed")
	events := []map[string]any{
		openAIEventsTestCreated("resp_unfinished_item", 0),
		openAIEventsTestItemEvent(
			"response.output_item.added",
			openAIEventsTestItem("message", "msg_unfinished", "in_progress", true),
			0,
			1,
		),
		{"type": "response.completed", "sequence_number": int64(2), "response": completed},
	}

	response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
	if response != nil || !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("Complete response=%+v error=%v, want unfinished-item ErrInvalidModelOutput", response, err)
	}
	for _, chunk := range chunks {
		if chunk.Type == ModelStreamResponseDone {
			t.Fatalf("unfinished item emitted response_done: %+v", chunks)
		}
	}
}

func TestOpenAIResponseEnvelopeRejectsOmittedImmutableFields(t *testing.T) {
	for _, field := range []string{"object", "created_at"} {
		t.Run(field, func(t *testing.T) {
			completed := openAIEventsTestResponse("resp_omitted_immutable", "test-model", "completed")
			delete(completed, field)
			events := []map[string]any{
				openAIEventsTestCreated("resp_omitted_immutable", 0),
				{"type": "response.completed", "sequence_number": int64(1), "response": completed},
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "omitted immutable response field") {
				t.Fatalf("Complete response=%+v error=%v, want omitted immutable field failure", response, err)
			}
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("omitted immutable field emitted response_done: %+v", chunks)
				}
			}
		})
	}
}

func TestOpenAIStreamRejectsTextEvidenceAfterTerminalSubevent(t *testing.T) {
	annotation := map[string]any{
		"type": "url_citation", "start_index": int64(0), "end_index": int64(4),
		"title": "source", "url": "https://example.test",
	}
	for _, test := range []struct {
		name        string
		lateEvent   map[string]any
		annotations []any
	}{
		{
			name: "delta after text done",
			lateEvent: map[string]any{
				"type": "response.output_text.delta", "sequence_number": int64(3),
				"item_id": "msg_late_text", "output_index": int64(0), "content_index": int64(0),
				"delta": "safe", "logprobs": []any{},
			},
			annotations: []any{},
		},
		{
			name: "annotation after text done",
			lateEvent: map[string]any{
				"type": "response.output_text.annotation.added", "sequence_number": int64(3),
				"item_id": "msg_late_text", "output_index": int64(0), "content_index": int64(0),
				"annotation_index": int64(0), "annotation": annotation,
			},
			annotations: []any{annotation},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			done := openAIEventsTestItem("message", "msg_late_text", "completed", true)
			done["content"] = []any{map[string]any{
				"type": "output_text", "text": "safe", "annotations": test.annotations,
			}}
			completed := openAIEventsTestResponse("resp_late_text", "test-model", "completed")
			completed["output"] = []any{done}
			events := []map[string]any{
				openAIEventsTestCreated("resp_late_text", 0),
				openAIEventsTestItemEvent(
					"response.output_item.added",
					openAIEventsTestItem("message", "msg_late_text", "in_progress", true),
					0,
					1,
				),
				{
					"type": "response.output_text.done", "sequence_number": int64(2),
					"item_id": "msg_late_text", "output_index": int64(0), "content_index": int64(0),
					"text": "safe", "logprobs": []any{},
				},
				test.lateEvent,
				openAIEventsTestItemEvent("response.output_item.done", done, 0, 4),
				{"type": "response.completed", "sequence_number": int64(5), "response": completed},
			}

			response, chunks, err := completeOpenAIEventsTestStream(t, openAIEventsTestSSE(events...))
			if response != nil || !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "after text done") {
				t.Fatalf("Complete response=%+v error=%v, want closed text lifecycle failure", response, err)
			}
			for _, chunk := range chunks {
				if chunk.Type == ModelStreamResponseDone {
					t.Fatalf("late text evidence emitted response_done: %+v", chunks)
				}
			}
		})
	}
}

func completeOpenAIEventsTestStream(t *testing.T, payload string) (*ModelResponse, []ModelStreamEvent, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, payload)
	}))
	t.Cleanup(server.Close)
	model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), openAITestModelConfig("test-model"))
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	var chunks []ModelStreamEvent
	response, completeErr := model.Complete(t.Context(), ModelRequest{
		Instructions: "Answer.",
		Input:        []ModelInputItem{{Type: ModelInputUserMessage, Text: "hello"}},
		StreamSink:   func(event ModelStreamEvent) { chunks = append(chunks, event) },
	})
	return response, chunks, completeErr
}

func openAIEventsTestSSE(events ...map[string]any) string {
	var stream strings.Builder
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		_, _ = fmt.Fprintf(&stream, "data: %s\n\n", payload)
	}
	stream.WriteString("data: [DONE]\n\n")
	return stream.String()
}

func openAIEventsTestResponse(id, model, status string) map[string]any {
	response := map[string]any{
		"id": id, "object": "response", "created_at": int64(1),
		"status": status, "output": []any{},
	}
	if model != "" {
		response["model"] = model
	}
	return response
}

func openAIEventsTestCreated(responseID string, sequence int64) map[string]any {
	return map[string]any{
		"type": "response.created", "sequence_number": sequence,
		"response": openAIEventsTestResponse(responseID, "test-model", "in_progress"),
	}
}

func openAIEventsTestItemEvent(eventType string, item map[string]any, outputIndex, sequence int64) map[string]any {
	return map[string]any{
		"type": eventType, "sequence_number": sequence,
		"output_index": outputIndex, "item": item,
	}
}

func openAIEventsTestItem(itemType, itemID string, status any, includeStatus bool) map[string]any {
	item := map[string]any{"id": itemID, "type": itemType}
	if includeStatus {
		item["status"] = status
	}
	switch itemType {
	case "message":
		item["role"] = "assistant"
		item["content"] = []any{}
	case "reasoning":
		item["summary"] = []any{}
	case "function_call":
		item["call_id"] = "call_status"
		item["name"] = "echo"
		item["arguments"] = "{}"
	default:
		panic("unsupported OpenAI events test item type: " + itemType)
	}
	return item
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

func TestParseOpenAIResponseRejectsDuplicateOutputItemIDs(t *testing.T) {
	var raw responses.Response
	if err := json.Unmarshal([]byte(`{
		"id":"resp_duplicate_items",
		"status":"completed",
		"output":[
			{"id":"msg_duplicate","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"one","annotations":[]}]},
			{"id":"msg_duplicate","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"two","annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
	}`), &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	_, err := parseOpenAIResponse(&raw)
	if !errors.Is(err, ErrInvalidModelOutput) || !strings.Contains(err.Error(), "repeats output item id") {
		t.Fatalf("parseOpenAIResponse error=%v, want duplicate item rejection", err)
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
			CallID: "call_1",
			Raw:    json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"memory_put","arguments":"{\"key\":\"color\",\"value\":\"red\"}","status":"completed"}`),
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

func TestBuildOpenAIInputItemsRejectsNoncanonicalFunctionIdentities(t *testing.T) {
	for name, items := range map[string][]ModelInputItem{
		"assistant call id": {
			{Type: ModelInputUserMessage, Text: "remember red"},
			{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: " call_1 ", Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"memory_put","arguments":"{}","status":"completed"}`)},
		},
		"raw call name": {
			{Type: ModelInputUserMessage, Text: "remember red"},
			{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call_1", Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":" memory_put ","arguments":"{}","status":"completed"}`)},
		},
		"tool result call id": {
			{Type: ModelInputUserMessage, Text: "remember red"},
			{Type: ModelInputAssistantOutput, OutputType: ModelOutputFunctionCall, CallID: "call_1", Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"memory_put","arguments":"{}","status":"completed"}`)},
			{Type: ModelInputToolResult, CallID: " call_1 ", Output: json.RawMessage(`{}`)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildOpenAIInputItems(items); err == nil {
				t.Fatal("OpenAI replay accepted noncanonical function identity")
			}
		})
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

func TestToolDefinitionsIDEncodesCollectionAndToolBoundaries(t *testing.T) {
	left := []ToolDefinition{
		{Name: "a", PreviousNames: []string{"b"}, Description: "{}", InputSchema: json.RawMessage(`{}`)},
		{Name: "e", Description: "f", InputSchema: json.RawMessage(`{}`)},
	}
	right := []ToolDefinition{
		{Name: "a", Description: "b", InputSchema: json.RawMessage(`{}`)},
		{Name: "{}", PreviousNames: []string{"e"}, Description: "f", InputSchema: json.RawMessage(`{}`)},
	}
	if leftID, rightID := toolDefinitionsID(left), toolDefinitionsID(right); leftID == rightID {
		t.Fatalf("structurally distinct tool collections produced the same ID: %s", leftID)
	}

	model := &OpenAIModel{toolCache: make(map[string][]responses.ToolUnionParam)}
	if _, err := model.cachedOpenAITools("", left); err != nil {
		t.Fatalf("cache left tools: %v", err)
	}
	if _, err := model.cachedOpenAITools("", right); err != nil {
		t.Fatalf("cache right tools: %v", err)
	}
	if len(model.toolCache) != 2 {
		t.Fatalf("tool cache entries=%d, want two structurally distinct snapshots", len(model.toolCache))
	}
}
