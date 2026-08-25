package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ly95/agentruntime"
)

type offlineBasicModel struct{}

func (offlineBasicModel) Complete(_ context.Context, request agentruntime.ModelRequest) (*agentruntime.ModelResponse, error) {
	if len(request.Input) != 1 || request.Input[0].Text != "offline prompt" {
		return nil, &unexpectedRequestError{request: request}
	}
	return &agentruntime.ModelResponse{
		ID: "offline-basic", OutputText: "offline answer",
		Items: []agentruntime.ModelOutputItem{{
			ID: "offline-basic-message", Type: agentruntime.ModelOutputMessage, Text: "offline answer",
			Raw: json.RawMessage(`{"id":"offline-basic-message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"offline answer","annotations":[]}]}`),
		}},
	}, nil
}

type unexpectedRequestError struct {
	request agentruntime.ModelRequest
}

func (err *unexpectedRequestError) Error() string {
	return "basic example received an unexpected model request"
}

func TestBasicExampleRunsOffline(t *testing.T) {
	result, err := runWithModel(t.Context(), offlineBasicModel{}, "offline prompt")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agentruntime.RunStatusCompleted || result.Output != "offline answer" {
		t.Fatalf("result=%+v", result)
	}
}
