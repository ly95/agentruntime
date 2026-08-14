// Package textskill demonstrates host-authorized operation contracts and
// execution behavior used beside a separately mounted SKILL.md.
package textskill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ly95/agentruntime"
)

const OperationName = "text_analyze"

type Skill struct{}

func New() Skill {
	return Skill{}
}

func (Skill) Register(registry *agentruntime.OperationRegistry) error {
	if registry == nil {
		return errors.New("text skill requires an operation registry")
	}
	return registry.Register(agentruntime.Operation{
		Name:        OperationName,
		Description: "Count Unicode characters, words, and lines in supplied text.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"text":{"type":"string","minLength":1,"maxLength":10000}},
			"required":["text"],
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"characters":{"type":"integer","minimum":1},
				"words":{"type":"integer","minimum":0},
				"lines":{"type":"integer","minimum":1}
			},
			"required":["characters","words","lines"],
			"additionalProperties":false
		}`),
		Effect:       agentruntime.OperationEffectRead,
		Capabilities: []string{"text_analysis"},
		Confirmation: agentruntime.ConfirmationSpec{Mode: agentruntime.ConfirmationNone},
	})
}

func (Skill) Execute(_ context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
	if request.Operation.Name != OperationName {
		return agentruntime.OperationResult{}, fmt.Errorf("text skill does not implement %q", request.Operation.Name)
	}
	arguments, ok := request.Arguments.(map[string]any)
	if !ok {
		return agentruntime.OperationResult{}, errors.New("text_analyze arguments must be an object")
	}
	text, ok := arguments["text"].(string)
	if !ok || text == "" {
		return agentruntime.OperationResult{}, errors.New("text_analyze argument text must be a non-empty string")
	}
	output, err := json.Marshal(struct {
		Characters int `json:"characters"`
		Words      int `json:"words"`
		Lines      int `json:"lines"`
	}{
		Characters: utf8.RuneCountInString(text),
		Words:      len(strings.Fields(text)),
		Lines:      strings.Count(text, "\n") + 1,
	})
	if err != nil {
		return agentruntime.OperationResult{}, fmt.Errorf("encode text_analyze output: %w", err)
	}
	return agentruntime.OperationResult{Output: output}, nil
}
