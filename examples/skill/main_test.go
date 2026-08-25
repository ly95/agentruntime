package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ly95/agentruntime"
)

type offlineSkillModel struct {
	turn int
}

func (model *offlineSkillModel) Complete(_ context.Context, request agentruntime.ModelRequest) (*agentruntime.ModelResponse, error) {
	model.turn++
	if !strings.Contains(request.Instructions, "text-analysis") || !strings.Contains(request.Instructions, "text_analyze") {
		return nil, errors.New("mounted Skill instructions are missing")
	}
	if model.turn == 1 {
		arguments := json.RawMessage(`{"text":"hello offline world"}`)
		return &agentruntime.ModelResponse{ID: "offline-skill-call", Items: []agentruntime.ModelOutputItem{{
			ID: "offline-skill-call-item", Type: agentruntime.ModelOutputFunctionCall,
			Call: &agentruntime.ToolCall{ID: "offline-call", Name: "text_analyze", Input: arguments},
			Raw:  json.RawMessage(`{"id":"offline-skill-call-item","type":"function_call","status":"completed","call_id":"offline-call","name":"text_analyze","arguments":"{\"text\":\"hello offline world\"}"}`),
		}}}, nil
	}
	if model.turn != 2 {
		return nil, errors.New("unexpected extra model turn")
	}
	foundToolResult := false
	for _, item := range request.Input {
		if item.Type == agentruntime.ModelInputToolResult && item.CallID == "offline-call" {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		return nil, errors.New("text_analyze result was not returned to the model")
	}
	return &agentruntime.ModelResponse{
		ID: "offline-skill-answer", OutputText: "3 words",
		Items: []agentruntime.ModelOutputItem{{
			ID: "offline-skill-answer-message", Type: agentruntime.ModelOutputMessage, Text: "3 words",
			Raw: json.RawMessage(`{"id":"offline-skill-answer-message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"3 words","annotations":[]}]}`),
		}},
	}, nil
}

func TestSkillExampleRunsOffline(t *testing.T) {
	directory, err := filepath.Abs("textskill")
	if err != nil {
		t.Fatal(err)
	}
	model := &offlineSkillModel{}
	result, err := runWithModel(t.Context(), model, directory, "analyze offline text")
	if err != nil {
		t.Fatal(err)
	}
	if model.turn != 2 || result.Status != agentruntime.RunStatusCompleted || result.Output != "3 words" {
		t.Fatalf("turns=%d result=%+v", model.turn, result)
	}
}
