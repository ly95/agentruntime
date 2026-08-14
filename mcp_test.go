package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRuntimeDiscoversAndCallsMCPTool(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("response-1", ToolCall{
			ID: "call-1", Name: "projects.read",
			Input: json.RawMessage(`{"project_id":"project-1"}`),
		}),
		messageResponse("response-2", "done"),
	}}
	operations := NewOperationRegistry()
	read := operation("projects.read", OperationEffectRead)
	read.PreviousNames = []string{"projects.get"}
	read.InputSchema = json.RawMessage(`{
		"type":"object",
		"properties":{"project_id":{"type":"string"}},
		"required":["project_id"],
		"additionalProperties":false
	}`)
	if err := operations.Register(read); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var executed OperationRequest
	var events []Event
	runtime := newTestRuntimeWithEventSink(
		t, model, operations, allowPolicy(),
		OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
			executed = request
			return OperationResult{Output: json.RawMessage(`{"title":"Demo"}`)}, nil
		}),
		nil, nil, nil, func(event Event) { events = append(events, event) },
	)

	result, err := runtime.Run(t.Context(), Input{User: "inspect"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "done" || executed.Operation.Name != "projects.read" {
		t.Fatalf("result=%+v executed=%+v", result, executed)
	}
	if len(model.requests) != 2 || len(model.requests[0].Tools) != 1 {
		t.Fatalf("model requests=%+v", model.requests)
	}
	tool := model.requests[0].Tools[0]
	if tool.Name != "projects.read" ||
		len(tool.PreviousNames) != 1 || tool.PreviousNames[0] != "projects.get" {
		t.Fatalf("discovered tool=%+v", tool)
	}
	if strings.Contains(model.requests[0].Instructions, "load_skill") ||
		strings.Contains(strings.ToLower(model.requests[0].Instructions), "loaded skill") {
		t.Fatalf("legacy Skill instructions leaked into MCP prompt: %s", model.requests[0].Instructions)
	}
	mcpEvents := 0
	for _, event := range events {
		if event.Type == EventMCPConnected {
			mcpEvents++
			if event.MCPServer != mcpServerName || event.MCPProtocol == "" ||
				event.MCPToolCount != 1 {
				t.Fatalf("MCP event=%+v", event)
			}
		}
	}
	if mcpEvents != 1 {
		t.Fatalf("mcp_connected events=%d, want 1", mcpEvents)
	}
}

func TestLocalMCPPreservesExecutionErrorClassification(t *testing.T) {
	operations := NewOperationRegistry()
	if err := operations.Register(operation("projects.read", OperationEffectRead)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := operations.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	local, err := newLocalMCP(
		operations,
		OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, MarkOperationNotApplied(errors.New("provider refused request"))
		}),
		"",
	)
	if err != nil {
		t.Fatalf("newLocalMCP: %v", err)
	}
	_, err = local.Execute(t.Context(), OperationRequest{
		Operation: operationSummary(operation("projects.read", OperationEffectRead)),
		Call: ToolCall{
			ID: "call-1", Name: "projects.read", Input: json.RawMessage(`{}`),
		},
		Arguments: map[string]any{},
	})
	if !errors.Is(err, ErrOperationNotApplied) ||
		!strings.Contains(err.Error(), "provider refused request") {
		t.Fatalf("Execute error=%v", err)
	}
}

func TestValidateDiscoveredToolsRejectsContractDrift(t *testing.T) {
	operation := operationSummary(operation("projects.read", OperationEffectRead))
	drifted := &mcpsdk.Tool{
		Name: "projects.read", Description: "untrusted replacement",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint: true, IdempotentHint: true,
		},
	}
	if _, err := validateDiscoveredTools(
		[]*mcpsdk.Tool{drifted}, []OperationSummary{operation},
	); err == nil || !strings.Contains(err.Error(), "contract differs") {
		t.Fatalf("validateDiscoveredTools error=%v", err)
	}
}

func TestValidateDiscoveredToolsRejectsUnsafeAnnotationDrift(t *testing.T) {
	operation := operationSummary(operation("projects.update", OperationEffectWrite))
	notDestructive := false
	drifted := &mcpsdk.Tool{
		Name:         operation.Name,
		Description:  operationToolDescription(operation),
		InputSchema:  append(json.RawMessage(nil), operation.InputSchema...),
		OutputSchema: append(json.RawMessage(nil), operation.OutputSchema...),
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint: false, DestructiveHint: &notDestructive, IdempotentHint: true,
		},
	}
	if _, err := validateDiscoveredTools(
		[]*mcpsdk.Tool{drifted}, []OperationSummary{operation},
	); err == nil || !strings.Contains(err.Error(), "annotations differ") {
		t.Fatalf("validateDiscoveredTools error=%v", err)
	}
}

func TestMCPResultEnvelopeRoundTripsExecutionErrorClassification(t *testing.T) {
	tests := []struct {
		name         string
		cause        error
		wantCode     string
		wantIs       error
		wantMessage  string
		wantUnmarked bool
	}{
		{
			name:     "definitely not applied",
			cause:    MarkOperationNotApplied(errors.New("provider rejected before commit")),
			wantCode: "not_applied", wantIs: ErrOperationNotApplied,
			wantMessage: "provider rejected before commit",
		},
		{
			name:     "outcome unknown",
			cause:    errors.Join(ErrOperationOutcomeUnknown, errors.New("connection lost after send")),
			wantCode: "outcome_unknown", wantIs: ErrOperationOutcomeUnknown,
			wantMessage: "connection lost after send",
		},
		{
			name: "ordinary execution failure", cause: errors.New("provider validation failed"),
			wantCode: "execution_failed", wantMessage: "provider validation failed", wantUnmarked: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodeMCPError(test.cause)
			if err != nil {
				t.Fatalf("encodeMCPError: %v", err)
			}
			metadata, ok := encoded.Meta[mcpResultMetaKey].(map[string]any)
			if !ok {
				t.Fatalf("encoded metadata=%#v, want object", encoded.Meta[mcpResultMetaKey])
			}
			operationError, ok := metadata["error"].(map[string]any)
			if !ok || operationError["code"] != test.wantCode {
				t.Fatalf("encoded error=%#v, want code %q", metadata["error"], test.wantCode)
			}
			_, err = decodeMCPResult(encoded)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("decodeMCPResult error=%v, want message %q", err, test.wantMessage)
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("decodeMCPResult error=%v, want errors.Is(%v)", err, test.wantIs)
			}
			if test.wantUnmarked && (errors.Is(err, ErrOperationNotApplied) || errors.Is(err, ErrOperationOutcomeUnknown)) {
				t.Fatalf("ordinary failure received side-effect classification: %v", err)
			}
		})
	}
}

func TestMCPResultEnvelopeRoundTripsSuccess(t *testing.T) {
	want := OperationResult{
		Output:  json.RawMessage(`{"applied":true}`),
		Receipt: json.RawMessage(`{"version":2}`),
		Artifacts: []ResultArtifact{{
			Type: "change_set", Data: json.RawMessage(`{"id":"change-1"}`),
		}},
	}
	encoded, err := encodeMCPSuccess(want)
	if err != nil {
		t.Fatalf("encodeMCPSuccess: %v", err)
	}
	got, err := decodeMCPResult(encoded)
	if err != nil {
		t.Fatalf("decodeMCPResult: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded result=%+v, want %+v", got, want)
	}
}

func TestDecodeMCPResultRejectsMalformedOrContradictoryEnvelope(t *testing.T) {
	validResult := map[string]any{"output": map[string]any{"applied": true}}
	tests := []struct {
		name    string
		result  *mcpsdk.CallToolResult
		wantErr string
	}{
		{name: "nil result", wantErr: "returned no result"},
		{
			name: "missing host metadata", result: &mcpsdk.CallToolResult{},
			wantErr: "missing host metadata",
		},
		{
			name: "invalid envelope shape",
			result: &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{
				mcpResultMetaKey: map[string]any{"result": "not-an-object"},
			}},
			wantErr: "decode MCP tool result",
		},
		{
			name: "unclassified error",
			result: &mcpsdk.CallToolResult{IsError: true, Meta: mcpsdk.Meta{
				mcpResultMetaKey: map[string]any{},
			}},
			wantErr: "returned an unclassified error",
		},
		{
			name: "unknown error code",
			result: &mcpsdk.CallToolResult{IsError: true, Meta: mcpsdk.Meta{
				mcpResultMetaKey: map[string]any{"error": map[string]any{
					"code": "tampered", "message": "do not trust",
				}},
			}},
			wantErr: `unknown error code "tampered"`,
		},
		{
			name: "successful transport without result",
			result: &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{
				mcpResultMetaKey: map[string]any{},
			}},
			wantErr: "invalid result envelope",
		},
		{
			name: "successful transport with result and error",
			result: &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{
				mcpResultMetaKey: map[string]any{
					"result": validResult,
					"error":  map[string]any{"code": "execution_failed", "message": "conflict"},
				},
			}},
			wantErr: "invalid result envelope",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeMCPResult(test.result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("decodeMCPResult error=%v, want message containing %q", err, test.wantErr)
			}
		})
	}
}
