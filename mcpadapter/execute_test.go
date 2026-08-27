package mcpadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ly95/agentruntime"
)

func TestExecuteRejectsBindingAndOperationContractDriftBeforeDispatch(t *testing.T) {
	t.Run("binding drift", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		transport.setBindingID("binding:changed-after-discovery")

		_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
		assertTestErrorContains(t, err, "transport binding changed")
		if got := transport.requestCount("tools/call"); got != 0 {
			t.Fatalf("tools/call requests=%d, want 0", got)
		}
	})

	t.Run("operation contract mismatch", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		request := testDefaultOperationRequest(t, snapshot)
		request.Operation.ContractID = "contract_drifted"

		_, err := snapshot.Execute(t.Context(), request)
		assertTestErrorContains(t, err, "contract does not match the frozen snapshot")
		if got := transport.requestCount("tools/call"); got != 0 {
			t.Fatalf("tools/call requests=%d, want 0", got)
		}
	})
}

func TestExecuteDoesNotRetryTransportFailureAndRedactsSecrets(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	secret := "transport-bearer-sk_test_secret"
	sentinel := errors.New(secret)
	transport.enqueue(
		"tools/call",
		func(context.Context, Request) (json.RawMessage, error) {
			return nil, sentinel
		},
		testRPCResultResponder(map[string]any{
			"resultType":        "complete",
			"content":           []any{},
			"structuredContent": map[string]any{"answer": "unexpected retry", "count": 1},
		}),
	)

	_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
	if err == nil {
		t.Fatal("Execute succeeded after transport failure")
	}
	if errors.Is(err, sentinel) {
		t.Fatalf("Execute error unexpectedly exposes transport sentinel through errors.Is: %v", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("transport error unwrap=%v, want nil opaque boundary", unwrapped)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("transport secret leaked from Error(): %q", err.Error())
	}
	if got := transport.requestCount("tools/call"); got != 1 {
		t.Fatalf("tools/call requests=%d, want exactly 1", got)
	}
}

func TestExecuteRedactsServerRPCSecretText(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	messageSecret := "server says credential=server-secret-value"
	dataSecret := "private-debug-token"
	transport.enqueue("tools/call", testRPCErrorResponder(
		-32001,
		messageSecret,
		map[string]any{"debug": dataSecret},
	))

	_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
	if err == nil {
		t.Fatal("Execute unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "-32001") {
		t.Fatalf("server error=%q, want JSON-RPC code", err.Error())
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("server error unwrap=%v, want nil opaque boundary", unwrapped)
	}
	for _, secret := range []string{messageSecret, dataSecret, "server-secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("server secret %q leaked from Error(): %q", secret, err.Error())
		}
	}
}

func TestExecutePreservesContextCancellationIdentity(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := snapshot.Execute(ctx, testDefaultOperationRequest(t, snapshot))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error=%v, want errors.Is context.Canceled", err)
		}
		if got := transport.requestCount("tools/call"); got != 0 {
			t.Fatalf("tools/call requests=%d, want 0", got)
		}
	})

	t.Run("deadline already exceeded", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		_, err := snapshot.Execute(ctx, testDefaultOperationRequest(t, snapshot))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Execute error=%v, want errors.Is context.DeadlineExceeded", err)
		}
		if got := transport.requestCount("tools/call"); got != 0 {
			t.Fatalf("tools/call requests=%d, want 0", got)
		}
	})

	t.Run("transport wraps cancellation", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		rawTransportError := fmt.Errorf("transport-private-detail: %w", context.Canceled)
		transport.enqueue("tools/call", func(context.Context, Request) (json.RawMessage, error) {
			return nil, rawTransportError
		})

		_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error=%v, want errors.Is context.Canceled", err)
		}
		if errors.Is(err, rawTransportError) || errors.Unwrap(err) != nil {
			t.Fatalf("transport cancellation exposed raw error through unwrap: %v", err)
		}
		if strings.Contains(err.Error(), "transport-private-detail") {
			t.Fatalf("wrapped cancellation leaked transport text: %q", err.Error())
		}
	})

	t.Run("transport wraps deadline", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		rawTransportError := fmt.Errorf("deadline-private-detail: %w", context.DeadlineExceeded)
		transport.enqueue("tools/call", func(context.Context, Request) (json.RawMessage, error) {
			return nil, rawTransportError
		})

		_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Execute error=%v, want errors.Is context.DeadlineExceeded", err)
		}
		if errors.Is(err, rawTransportError) || errors.Unwrap(err) != nil {
			t.Fatalf("transport deadline exposed raw error through unwrap: %v", err)
		}
		if strings.Contains(err.Error(), "deadline-private-detail") {
			t.Fatalf("wrapped deadline leaked transport text: %q", err.Error())
		}
	})

	t.Run("context canceled during round trip", func(t *testing.T) {
		snapshot, transport := testDiscoverSnapshot(t)
		ctx, cancel := context.WithCancel(t.Context())
		transport.enqueue("tools/call", func(context.Context, Request) (json.RawMessage, error) {
			cancel()
			return nil, errors.New("transport response after cancellation")
		})

		_, err := snapshot.Execute(ctx, testDefaultOperationRequest(t, snapshot))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error=%v, want errors.Is context.Canceled", err)
		}
		if strings.Contains(err.Error(), "transport response after cancellation") || errors.Unwrap(err) != nil {
			t.Fatalf("canceled context exposed transport text through unwrap: %v", err)
		}
		if got := transport.requestCount("tools/call"); got != 1 {
			t.Fatalf("tools/call requests=%d, want 1", got)
		}
	})
}

func TestExecuteExtractsMCPParameterHeadersAndBase64EncodesValues(t *testing.T) {
	transport := newFakeTransport(testBindingID)
	tool := testTool(testRemoteName)
	tool["inputSchema"] = testDocument(t, `{
		"type":"object",
		"properties":{
			"token":{
				"type":"string",
				"x-mcp-header":"Token",
				"allOf":[{"minLength":1}],
				"oneOf":[{"pattern":"^snowman"},{"pattern":"^alternate"}]
			},
			"nested":{
				"type":"object",
				"properties":{
					"tenant":{"type":"integer","x-mcp-header":"Tenant-ID"},
					"active":{"type":"boolean","x-mcp-header":"Active"}
				},
				"required":["tenant","active"],
				"additionalProperties":false
			}
		},
		"required":["token","nested"],
		"additionalProperties":false
	}`)
	testEnqueueDiscovery(transport, testToolPage([]any{tool}, nil))
	snapshot, err := Discover(t.Context(), testConfig(
		transport,
		[]Mapping{testMapping(testRemoteName, testOperationName)},
		Limits{},
	))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	registeredInput := testDecodeObject(t, snapshot.validator.Summaries()[0].InputSchema)
	properties := registeredInput["properties"].(map[string]any)
	registeredToken := properties["token"].(map[string]any)
	if _, exists := registeredToken["x-mcp-header"]; exists {
		t.Fatalf("model-facing token schema retained x-mcp-header: %#v", registeredToken)
	}
	if len(registeredToken["allOf"].([]any)) != 1 || len(registeredToken["oneOf"].([]any)) != 2 {
		t.Fatalf("token sibling composition was not preserved: %#v", registeredToken)
	}
	nested := properties["nested"].(map[string]any)
	nestedProperties := nested["properties"].(map[string]any)
	tenantSchema := nestedProperties["tenant"].(map[string]any)
	minimum, minimumOK := tenantSchema["minimum"].(json.Number)
	maximum, maximumOK := tenantSchema["maximum"].(json.Number)
	if !minimumOK || !maximumOK ||
		minimum.String() != "-9007199254740991" || maximum.String() != "9007199254740991" {
		t.Fatalf("annotated integer bounds=%#v, want safe JSON integer range", tenantSchema)
	}

	token := "snowman ☃"
	argumentsWithTenant := func(tenant string) map[string]any {
		return map[string]any{
			"token": token,
			"nested": map[string]any{
				"tenant": json.Number(tenant),
				"active": true,
			},
		}
	}
	for _, outOfRange := range []string{"9007199254740992", "-9007199254740992"} {
		_, err := snapshot.Execute(
			t.Context(),
			testOperationRequest(t, snapshot, testOperationName, argumentsWithTenant(outOfRange)),
		)
		assertTestErrorContains(t, err, "arguments violate the frozen input contract")
		if got := transport.requestCount("tools/call"); got != 0 {
			t.Fatalf("out-of-range integer %s dispatched %d tools/call requests", outOfRange, got)
		}
	}

	transport.enqueue("tools/call", testRPCResultResponder(map[string]any{
		"resultType":        "complete",
		"content":           []any{},
		"structuredContent": map[string]any{"answer": "ok", "count": 1},
	}))
	arguments := argumentsWithTenant("42")
	request := testOperationRequest(t, snapshot, testOperationName, arguments)
	if _, err := snapshot.Execute(t.Context(), request); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	requests := transport.requestsFor("tools/call")
	if len(requests) != 1 {
		t.Fatalf("tools/call requests=%d, want 1", len(requests))
	}
	headers := requests[0].MetadataHeaders
	wantToken := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(token)) + "?="
	wantHeaders := map[string]string{
		"Mcp-Param-Token":     wantToken,
		"Mcp-Param-Tenant-ID": "42",
		"Mcp-Param-Active":    "true",
	}
	for name, want := range wantHeaders {
		if got := headers[name]; got != want {
			t.Errorf("header %s=%q, want %q", name, got, want)
		}
	}
	if got := headers["Mcp-Name"]; got != testRemoteName {
		t.Errorf("Mcp-Name=%q, want %q", got, testRemoteName)
	}
	params := testDecodeObject(t, requests[0].Params)
	encodedArguments, ok := params["arguments"].(map[string]any)
	if !ok || encodedArguments["token"] != token {
		t.Fatalf("tools/call arguments=%#v", params["arguments"])
	}
}

func TestExecuteNormalizesStrictOptionalNullsBeforeDispatch(t *testing.T) {
	transport := newFakeTransport(testBindingID)
	tool := testTool(testRemoteName)
	tool["inputSchema"] = testDocument(t, `{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer","x-mcp-header":"Limit"},
			"note":{"type":["string","null"]},
			"context":{"type":["object","null"]}
		},
		"required":["query","context"],
		"additionalProperties":false
	}`)
	testEnqueueDiscovery(transport, testToolPage([]any{tool}, nil))
	snapshot, err := Discover(t.Context(), testConfig(
		transport,
		[]Mapping{testMapping(testRemoteName, testOperationName)},
		Limits{},
	))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	arguments := map[string]any{
		"query":   "status",
		"limit":   nil,
		"note":    nil,
		"context": nil,
	}
	registry := agentruntime.NewOperationRegistry()
	if err := snapshot.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	normalized, err := registry.NormalizeInput(testOperationName, arguments)
	if err != nil {
		t.Fatalf("NormalizeInput: %v", err)
	}
	wantNormalized := map[string]any{"query": "status", "note": nil, "context": nil}
	if !reflect.DeepEqual(normalized, wantNormalized) {
		t.Fatalf("NormalizeInput=%#v, want %#v", normalized, wantNormalized)
	}
	if _, exists := arguments["limit"]; !exists {
		t.Fatalf("NormalizeInput mutated caller arguments: %#v", arguments)
	}

	transport.enqueue("tools/call", testRPCResultResponder(map[string]any{
		"resultType":        "complete",
		"content":           []any{},
		"structuredContent": map[string]any{"answer": "ok", "count": 1},
	}))
	if _, err := snapshot.Execute(
		t.Context(),
		testOperationRequest(t, snapshot, testOperationName, arguments),
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	requests := transport.requestsFor("tools/call")
	if len(requests) != 1 {
		t.Fatalf("tools/call requests=%d, want 1", len(requests))
	}
	params := testDecodeObject(t, requests[0].Params)
	dispatched, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call arguments=%#v, want object", params["arguments"])
	}
	if !reflect.DeepEqual(dispatched, wantNormalized) {
		t.Fatalf("tools/call arguments=%#v, want %#v", dispatched, wantNormalized)
	}
	if _, exists := requests[0].MetadataHeaders["Mcp-Param-Limit"]; exists {
		t.Fatalf("omitted limit produced a metadata header: %#v", requests[0].MetadataHeaders)
	}
}

func TestExecuteRejectsCompactExponentRPCError(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	transport.enqueue("tools/call", func(_ context.Context, request Request) (json.RawMessage, error) {
		id, err := json.Marshal(request.ID)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":1e1000000000,"message":"invalid"}}`), nil
	})

	_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
	assertTestErrorContains(t, err, "error code must be an integer")
	if got := transport.requestCount("tools/call"); got != 1 {
		t.Fatalf("tools/call requests=%d, want exactly 1", got)
	}
}

func TestExecuteRejectsToolCallFailureShapesAndOutputDrift(t *testing.T) {
	tests := []struct {
		name      string
		responder fakeResponder
		wantError string
	}{
		{
			name: "isError",
			responder: testRPCResultResponder(map[string]any{
				"resultType": "complete",
				"content":    []any{},
				"isError":    true,
			}),
			wantError: "remote tool reported an execution error",
		},
		{
			name:      "JSON-RPC error",
			responder: testRPCErrorResponder(-32001, "server internal secret", map[string]any{"secret": "hidden"}),
			wantError: "JSON-RPC error code -32001",
		},
		{
			name:      "undefined reserved JSON-RPC error",
			responder: testRPCErrorResponder(-32077, "undefined reserved code", nil),
			wantError: "error code is reserved but undefined",
		},
		{
			name: "input_required",
			responder: testRPCResultResponder(map[string]any{
				"resultType": "input_required",
			}),
			wantError: "multi-round-trip input is not supported",
		},
		{
			name: "unknown resultType",
			responder: testRPCResultResponder(map[string]any{
				"resultType": "future_result",
			}),
			wantError: "resultType is not supported",
		},
		{
			name: "missing content",
			responder: testRPCResultResponder(map[string]any{
				"resultType":        "complete",
				"structuredContent": map[string]any{"answer": "ok", "count": 1},
			}),
			wantError: "content array is required",
		},
		{
			name: "missing structuredContent",
			responder: testRPCResultResponder(map[string]any{
				"resultType": "complete",
				"content":    []any{},
			}),
			wantError: "did not return structuredContent",
		},
		{
			name: "output schema drift",
			responder: testRPCResultResponder(map[string]any{
				"resultType":        "complete",
				"content":           []any{},
				"structuredContent": map[string]any{"answer": 99, "count": 1},
			}),
			wantError: "violates the frozen output contract",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, transport := testDiscoverSnapshot(t)
			transport.enqueue("tools/call", test.responder)

			_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
			assertTestErrorContains(t, err, test.wantError)
			if got := transport.requestCount("tools/call"); got != 1 {
				t.Fatalf("tools/call requests=%d, want 1", got)
			}
		})
	}
}

func TestExecuteValidatesContentBlocks(t *testing.T) {
	tests := []struct {
		name      string
		content   any
		wantError string
	}{
		{
			name:      "content is not an array",
			content:   map[string]any{},
			wantError: "content array is required",
		},
		{
			name:      "content block is not an object",
			content:   []any{"text"},
			wantError: "content block is invalid",
		},
		{
			name:      "text block missing text",
			content:   []any{map[string]any{"type": "text"}},
			wantError: "text content is invalid",
		},
		{
			name:      "media block data is not a string",
			content:   []any{map[string]any{"type": "image", "data": 42, "mimeType": "image/png"}},
			wantError: "media content is invalid",
		},
		{
			name:      "media block data is malformed Base64",
			content:   []any{map[string]any{"type": "image", "data": "not-base64!", "mimeType": "image/png"}},
			wantError: "media content is invalid",
		},
		{
			name:      "content annotations are malformed",
			content:   []any{map[string]any{"type": "text", "text": "ok", "annotations": "untrusted"}},
			wantError: "content annotations are invalid",
		},
		{
			name: "annotation audience contains an unsupported role",
			content: []any{map[string]any{
				"type": "text", "text": "ok",
				"annotations": map[string]any{"audience": []any{"system"}},
			}},
			wantError: "content audience is invalid",
		},
		{
			name: "annotation priority is outside the unit interval",
			content: []any{map[string]any{
				"type": "text", "text": "ok",
				"annotations": map[string]any{"priority": 1.01},
			}},
			wantError: "content priority is invalid",
		},
		{
			name: "annotation priority has a compact exponent bomb",
			content: []any{map[string]any{
				"type": "text", "text": "ok",
				"annotations": map[string]any{"priority": json.Number("1e-1000000000")},
			}},
			wantError: "content priority is invalid",
		},
		{
			name: "resource icon src is malformed",
			content: []any{map[string]any{
				"type": "resource_link", "uri": "file:///resource", "name": "resource",
				"icons": []any{map[string]any{"src": 42}},
			}},
			wantError: "resource icon is invalid",
		},
		{
			name: "resource icon sizes contain a malformed field",
			content: []any{map[string]any{
				"type": "resource_link", "uri": "file:///resource", "name": "resource",
				"icons": []any{map[string]any{"src": "file:///icon.png", "sizes": []any{"16x16", 32}}},
			}},
			wantError: "resource icon sizes are invalid",
		},
		{
			name: "resource icon theme is unsupported",
			content: []any{map[string]any{
				"type": "resource_link", "uri": "file:///resource", "name": "resource",
				"icons": []any{map[string]any{"src": "file:///icon.png", "theme": "automatic"}},
			}},
			wantError: "resource icon theme is invalid",
		},
		{
			name: "embedded resource blob is malformed Base64",
			content: []any{map[string]any{
				"type":     "resource",
				"resource": map[string]any{"uri": "file:///resource.bin", "blob": "not-base64!"},
			}},
			wantError: "embedded resource is invalid",
		},
		{
			name:      "unknown content block type",
			content:   []any{map[string]any{"type": "future_block", "value": "unknown"}},
			wantError: "content block type is unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, transport := testDiscoverSnapshot(t)
			transport.enqueue("tools/call", testRPCResultResponder(map[string]any{
				"resultType":        "complete",
				"content":           test.content,
				"structuredContent": map[string]any{"answer": "ok", "count": 1},
			}))

			_, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
			assertTestErrorContains(t, err, test.wantError)
		})
	}
}

func TestExecuteAcceptsRepresentativeImageAndResourceContentButUsesStructuredContent(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	transport.enqueue("tools/call", testRPCResultResponder(map[string]any{
		"resultType": "complete",
		"content": []any{
			map[string]any{
				"type":     "image",
				"data":     base64.StdEncoding.EncodeToString([]byte("representative image")),
				"mimeType": "image/png",
				"annotations": map[string]any{
					"audience":     []any{"user", "assistant"},
					"priority":     0.5,
					"lastModified": "2026-08-27T00:00:00Z",
				},
				"_meta": map[string]any{"ignored": true},
			},
			map[string]any{
				"type":        "resource_link",
				"uri":         "file:///representative.txt",
				"name":        "representative",
				"title":       "Representative resource",
				"description": "ignored resource link",
				"mimeType":    "text/plain",
				"size":        23,
				"icons": []any{map[string]any{
					"src":      "file:///representative-light.png",
					"mimeType": "image/png",
					"sizes":    []any{"16x16", "32x32"},
					"theme":    "light",
				}},
			},
			map[string]any{
				"type": "resource",
				"resource": map[string]any{
					"uri":      "file:///representative.bin",
					"mimeType": "application/octet-stream",
					"blob":     base64.StdEncoding.EncodeToString([]byte("representative resource")),
					"_meta":    map[string]any{"ignored": true},
				},
			},
		},
		"structuredContent": map[string]any{"answer": "structured", "count": 7},
	}))

	result, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
	if err != nil {
		t.Fatalf("Execute with valid image and resource content: %v", err)
	}
	if got, want := string(result.Output), `{"answer":"structured","count":7}`; got != want {
		t.Fatalf("Execute output=%s, want structuredContent %s", got, want)
	}
}

func TestExecuteAcceptsHarmlessUnknownResultMembers(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	transport.enqueue("tools/call", testRPCResultResponder(map[string]any{
		"resultType":        "complete",
		"content":           []any{},
		"structuredContent": map[string]any{"answer": "ok", "count": 1},
		"futureExtension":   map[string]any{"ignored": true},
	}))

	result, err := snapshot.Execute(t.Context(), testDefaultOperationRequest(t, snapshot))
	if err != nil {
		t.Fatalf("Execute with harmless unknown result member: %v", err)
	}
	if got, want := string(result.Output), `{"answer":"ok","count":1}`; got != want {
		t.Fatalf("Execute output=%s, want %s", got, want)
	}
}
