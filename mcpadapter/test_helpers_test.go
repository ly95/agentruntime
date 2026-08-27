package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/ly95/agentruntime"
)

const (
	testBindingID     = "binding:test-server:principal"
	testRemoteName    = "remote.lookup"
	testOperationName = "documents.lookup"
)

type fakeResponder func(context.Context, Request) (json.RawMessage, error)

// fakeTransport is safe for concurrent use. Responders are dequeued atomically,
// while callbacks run without the transport lock so they may cancel contexts or
// change the binding without deadlocking.
type fakeTransport struct {
	mu         sync.Mutex
	bindingID  string
	requests   []Request
	responders map[string][]fakeResponder
}

func newFakeTransport(bindingID string) *fakeTransport {
	return &fakeTransport{
		bindingID:  bindingID,
		responders: make(map[string][]fakeResponder),
	}
}

func (transport *fakeTransport) BindingID() string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.bindingID
}

func (transport *fakeTransport) RoundTrip(ctx context.Context, request Request) (json.RawMessage, error) {
	request = cloneTestRequest(request)
	transport.mu.Lock()
	transport.requests = append(transport.requests, request)
	queue := transport.responders[request.Method]
	var responder fakeResponder
	if len(queue) > 0 {
		responder = queue[0]
		transport.responders[request.Method] = queue[1:]
	}
	transport.mu.Unlock()

	if responder == nil {
		return nil, fmt.Errorf("fake transport: unexpected %s request", request.Method)
	}
	response, err := responder(ctx, request)
	return append(json.RawMessage(nil), response...), err
}

func (transport *fakeTransport) enqueue(method string, responders ...fakeResponder) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.responders[method] = append(transport.responders[method], responders...)
}

func (transport *fakeTransport) setBindingID(bindingID string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.bindingID = bindingID
}

func (transport *fakeTransport) allRequests() []Request {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	requests := make([]Request, len(transport.requests))
	for index, request := range transport.requests {
		requests[index] = cloneTestRequest(request)
	}
	return requests
}

func (transport *fakeTransport) requestsFor(method string) []Request {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	var requests []Request
	for _, request := range transport.requests {
		if request.Method == method {
			requests = append(requests, cloneTestRequest(request))
		}
	}
	return requests
}

func (transport *fakeTransport) requestCount(method string) int {
	return len(transport.requestsFor(method))
}

func cloneTestRequest(request Request) Request {
	request.Params = append(json.RawMessage(nil), request.Params...)
	if request.MetadataHeaders != nil {
		headers := make(map[string]string, len(request.MetadataHeaders))
		for name, value := range request.MetadataHeaders {
			headers[name] = value
		}
		request.MetadataHeaders = headers
	}
	return request
}

func testRPCResultResponder(result map[string]any) fakeResponder {
	return func(_ context.Context, request Request) (json.RawMessage, error) {
		return testRPCResult(request, result), nil
	}
}

func testRawRPCResultResponder(resultJSON string) fakeResponder {
	return func(_ context.Context, request Request) (json.RawMessage, error) {
		id, err := json.Marshal(request.ID)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + resultJSON + `}`), nil
	}
}

func testRPCErrorResponder(code int64, message string, data any) fakeResponder {
	return func(_ context.Context, request Request) (json.RawMessage, error) {
		errorObject := map[string]any{"code": code, "message": message}
		if data != nil {
			errorObject["data"] = data
		}
		response, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   errorObject,
		})
		return json.RawMessage(response), err
	}
}

func testRPCResult(request Request, result map[string]any) json.RawMessage {
	response, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  result,
	})
	if err != nil {
		panic(err)
	}
	return json.RawMessage(response)
}

func testDiscoveryResult() map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"ttlMs":             60_000,
		"cacheScope":        "private",
		"supportedVersions": []any{ProtocolVersion},
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
	}
}

func testToolPage(tools []any, nextCursor *string) map[string]any {
	result := map[string]any{
		"resultType": "complete",
		"ttlMs":      60_000,
		"cacheScope": "private",
		"tools":      tools,
	}
	if nextCursor != nil {
		result["nextCursor"] = *nextCursor
	}
	return result
}

func testEnqueueDiscovery(transport *fakeTransport, pages ...map[string]any) {
	transport.enqueue("server/discover", testRPCResultResponder(testDiscoveryResult()))
	for _, page := range pages {
		transport.enqueue("tools/list", testRPCResultResponder(page))
	}
}

func testInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required":             []any{"query"},
		"additionalProperties": false,
	}
}

func testOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
			"count":  map[string]any{"type": "integer"},
		},
		"required":             []any{"answer", "count"},
		"additionalProperties": false,
	}
}

func testTool(remoteName string) map[string]any {
	return map[string]any{
		"name":         remoteName,
		"description":  "untrusted server description",
		"inputSchema":  testInputSchema(),
		"outputSchema": testOutputSchema(),
		"annotations":  map[string]any{"readOnlyHint": true},
	}
}

func testMapping(remoteName, operationName string) Mapping {
	return Mapping{
		RemoteName:    remoteName,
		OperationName: operationName,
		Description:   "Trusted host lookup",
		Capabilities:  []string{"capability.zeta", "capability.alpha"},
		HostVersion:   "host-v1",
		ReadOnly:      true,
	}
}

func testConfig(transport *fakeTransport, mappings []Mapping, limits Limits) Config {
	return Config{
		Transport: transport,
		BindingID: transport.BindingID(),
		Mappings:  mappings,
		Limits:    limits,
	}
}

func testDiscoverSnapshot(t *testing.T) (*Snapshot, *fakeTransport) {
	t.Helper()
	transport := newFakeTransport(testBindingID)
	testEnqueueDiscovery(transport, testToolPage([]any{testTool(testRemoteName)}, nil))
	snapshot, err := Discover(t.Context(), testConfig(
		transport,
		[]Mapping{testMapping(testRemoteName, testOperationName)},
		Limits{},
	))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return snapshot, transport
}

func testOperationRequest(t *testing.T, snapshot *Snapshot, operationName string, arguments any) agentruntime.OperationRequest {
	t.Helper()
	var summary agentruntime.OperationSummary
	found := false
	for _, candidate := range snapshot.validator.Summaries() {
		if candidate.Name == operationName {
			summary = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("snapshot has no operation summary for %q", operationName)
	}
	input, err := canonicalJSONValue(arguments)
	if err != nil {
		t.Fatalf("encode operation arguments: %v", err)
	}
	return agentruntime.OperationRequest{
		RunID:       "run-1",
		SessionID:   "session-1",
		ExecutionID: "execution-1",
		AttemptID:   "attempt-1",
		Operation:   summary,
		Call: agentruntime.ToolCall{
			ID:    "call-1",
			Name:  operationName,
			Input: input,
		},
		Arguments: arguments,
	}
}

func testDefaultOperationRequest(t *testing.T, snapshot *Snapshot) agentruntime.OperationRequest {
	t.Helper()
	return testOperationRequest(t, snapshot, testOperationName, map[string]any{"query": "status"})
}

func testDecodeObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	value, err := decodeExactJSON(raw)
	if err != nil {
		t.Fatalf("decode exact JSON: %v", err)
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		t.Fatalf("JSON value has type %T, want object", value)
	}
	return object
}

func testDocument(t *testing.T, raw string) map[string]any {
	t.Helper()
	return testDecodeObject(t, json.RawMessage(raw))
}

func testStringPointer(value string) *string {
	return &value
}
