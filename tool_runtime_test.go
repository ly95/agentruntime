package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRuntimeDiscoversAndCallsRegisteredTool(t *testing.T) {
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
	runtime := newTestRuntimeWithEventSink(
		t, model, operations, allowPolicy(),
		OperationExecutorFunc(func(_ context.Context, request OperationRequest) (OperationResult, error) {
			executed = request
			return OperationResult{Output: json.RawMessage(`{"title":"Demo"}`)}, nil
		}),
		nil, nil, nil, nil,
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
		t.Fatalf("legacy Skill instructions leaked into tool prompt: %s", model.requests[0].Instructions)
	}
}

func TestRuntimePreservesExecutionErrorClassification(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("response-1", ToolCall{
			ID: "call-1", Name: "projects.read", Input: json.RawMessage(`{}`),
		}),
	}}
	operations := NewOperationRegistry()
	if err := operations.Register(operation("projects.read", OperationEffectRead)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	runtime := newTestRuntime(
		t, model, operations, allowPolicy(),
		OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, MarkOperationNotApplied(errors.New("provider refused request"))
		}),
		nil, nil, nil,
	)
	_, err := runtime.Run(t.Context(), Input{User: "inspect"})
	if !errors.Is(err, ErrOperationNotApplied) ||
		!strings.Contains(err.Error(), "provider refused request") {
		t.Fatalf("Run error=%v", err)
	}
}
