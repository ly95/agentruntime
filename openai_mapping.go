package agentruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func buildOpenAIInputItems(items []ModelInputItem) ([]responses.ResponseInputItemUnionParam, error) {
	if len(items) == 0 {
		return nil, errors.New("agent: model request requires at least one input item")
	}
	out := make([]responses.ResponseInputItemUnionParam, 0, len(items))
	for i, item := range items {
		if len(item.Attachments) > 0 && item.Type != ModelInputUserMessage {
			return nil, fmt.Errorf("agent: model input item %d type %q cannot contain attachments", i, item.Type)
		}
		switch item.Type {
		case ModelInputUserMessage:
			if strings.TrimSpace(item.Text) == "" {
				return nil, fmt.Errorf("agent: user input item %d text is required", i)
			}
			content := responses.EasyInputMessageContentUnionParam{OfString: openai.String(item.Text)}
			if len(item.Attachments) > 0 {
				parts := make(responses.ResponseInputMessageContentListParam, 0, len(item.Attachments)+1)
				parts = append(parts, responses.ResponseInputContentUnionParam{
					OfInputText: &responses.ResponseInputTextParam{Text: item.Text},
				})
				for attachmentIndex, attachment := range item.Attachments {
					attachment = NormalizeModelInputAttachment(attachment)
					if err := ValidateModelInputAttachment(attachment); err != nil {
						return nil, fmt.Errorf("agent: user input item %d attachment %d: %w", i, attachmentIndex, err)
					}
					switch attachment.Kind {
					case ModelInputAttachmentImage:
						parts = append(parts, responses.ResponseInputContentUnionParam{
							OfInputImage: &responses.ResponseInputImageParam{ImageURL: openai.String(attachment.URL)},
						})
					case ModelInputAttachmentText:
						text, err := RenderTextAttachment(attachment)
						if err != nil {
							return nil, fmt.Errorf("agent: render user input item %d attachment %d: %w", i, attachmentIndex, err)
						}
						parts = append(parts, responses.ResponseInputContentUnionParam{
							OfInputText: &responses.ResponseInputTextParam{Text: text},
						})
					default:
						return nil, fmt.Errorf("agent: unsupported attachment kind %q", attachment.Kind)
					}
				}
				content = responses.EasyInputMessageContentUnionParam{OfInputItemContentList: parts}
			}
			out = append(out, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRoleUser,
					Content: content,
				},
			})
		case ModelInputAssistantOutput:
			replayed, err := buildOpenAIReplayItem(item)
			if err != nil {
				return nil, fmt.Errorf("agent: assistant output item %d: %w", i, err)
			}
			out = append(out, replayed)
		case ModelInputToolResult:
			if strings.TrimSpace(item.CallID) == "" {
				return nil, fmt.Errorf("agent: tool result item %d call_id is required", i)
			}
			if len(item.Output) == 0 || !json.Valid(item.Output) {
				return nil, fmt.Errorf("agent: tool result item %d output must be valid JSON", i)
			}
			out = append(out, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: item.CallID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(string(item.Output)),
					},
				},
			})
		default:
			return nil, fmt.Errorf("agent: unsupported model input item type %q", item.Type)
		}
	}
	return out, nil
}

func buildOpenAIReplayItem(item ModelInputItem) (responses.ResponseInputItemUnionParam, error) {
	if len(item.Raw) == 0 || !json.Valid(item.Raw) {
		return responses.ResponseInputItemUnionParam{}, errors.New("raw output must be valid JSON")
	}
	var envelope struct {
		Type ModelOutputItemType `json:"type"`
	}
	if err := json.Unmarshal(item.Raw, &envelope); err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	if envelope.Type != item.OutputType {
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("output type %q does not match raw type %q", item.OutputType, envelope.Type)
	}
	switch item.OutputType {
	case ModelOutputMessage:
		var message responses.ResponseOutputMessageParam
		if err := json.Unmarshal(item.Raw, &message); err != nil {
			return responses.ResponseInputItemUnionParam{}, err
		}
		return responses.ResponseInputItemUnionParam{OfOutputMessage: &message}, nil
	case ModelOutputReasoning:
		var reasoning responses.ResponseReasoningItemParam
		if err := json.Unmarshal(item.Raw, &reasoning); err != nil {
			return responses.ResponseInputItemUnionParam{}, err
		}
		return responses.ResponseInputItemUnionParam{OfReasoning: &reasoning}, nil
	case ModelOutputFunctionCall:
		var call responses.ResponseFunctionToolCallParam
		if err := json.Unmarshal(item.Raw, &call); err != nil {
			return responses.ResponseInputItemUnionParam{}, err
		}
		return responses.ResponseInputItemUnionParam{OfFunctionCall: &call}, nil
	default:
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("unsupported output type %q", item.OutputType)
	}
}

func mapOpenAITools(tools []ToolDefinition) ([]responses.ToolUnionParam, error) {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		tool.Name = strings.TrimSpace(tool.Name)
		if tool.Name == "" {
			return nil, errors.New("agent: tool name is required")
		}
		params, err := strictOpenAIParameters(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("agent: tool %q input schema: %w", tool.Name, err)
		}
		fn := &responses.FunctionToolParam{
			Name:       tool.Name,
			Parameters: params,
			Strict:     openai.Bool(true),
		}
		if tool.Description != "" {
			fn.Description = openai.String(tool.Description)
		}
		out = append(out, responses.ToolUnionParam{OfFunction: fn})
	}
	return out, nil
}

func parseOpenAIResponse(raw *responses.Response) (*ModelResponse, error) {
	return parseOpenAIResponseWithFinalizedCalls(raw, nil)
}

func parseOpenAIResponseWithFinalizedCalls(
	raw *responses.Response,
	finalizedCalls map[string]openAIStreamFunctionCall,
) (*ModelResponse, error) {
	outputText, refusal, err := openAIFinalMessageText(raw)
	if err != nil {
		return nil, err
	}
	out := &ModelResponse{
		ID:           raw.ID,
		OutputText:   outputText,
		Refusal:      refusal,
		FinishReason: string(raw.Status),
		Usage: Usage{
			InputTokens:  int(raw.Usage.InputTokens),
			OutputTokens: int(raw.Usage.OutputTokens),
			TotalTokens:  int(raw.Usage.TotalTokens),
		},
	}
	for _, item := range raw.Output {
		switch ModelOutputItemType(item.Type) {
		case ModelOutputMessage, ModelOutputReasoning, ModelOutputFunctionCall:
		default:
			return nil, fmt.Errorf("agent: unsupported OpenAI output item type %q", item.Type)
		}
		data := json.RawMessage(item.RawJSON())
		if len(data) == 0 || !json.Valid(data) {
			return nil, fmt.Errorf("agent: OpenAI output item %q is missing its raw JSON", item.ID)
		}
		parsed := ModelOutputItem{ID: item.ID, Type: ModelOutputItemType(item.Type), Raw: data}
		if item.Type == string(ModelOutputReasoning) {
			out.HadReasoning = true
		}
		if item.Type == string(ModelOutputFunctionCall) {
			parsed.Call, parsed.Raw, err = parseOpenAIFunctionCall(
				item.ID, item.AsFunctionCall(), data, finalizedCalls,
			)
			if err != nil {
				return nil, err
			}
		}
		out.Items = append(out.Items, parsed)
	}
	return out, nil
}

func parseOpenAIFunctionCall(
	itemID string,
	call responses.ResponseFunctionToolCall,
	raw json.RawMessage,
	finalizedCalls map[string]openAIStreamFunctionCall,
) (*ToolCall, json.RawMessage, error) {
	callID := strings.TrimSpace(call.CallID)
	name := strings.TrimSpace(call.Name)
	arguments := call.Arguments
	if callID == "" || name == "" {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q is missing its call id or name",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if arguments != "" && json.Valid([]byte(arguments)) {
		return &ToolCall{
			ID: callID, Name: name, Input: json.RawMessage(arguments),
		}, raw, nil
	}
	finalized, ok := finalizedCalls[itemID]
	if !ok || !finalized.Finalized || finalized.Arguments == "" ||
		!json.Valid([]byte(finalized.Arguments)) {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q has invalid JSON arguments",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if finalized.CallID != "" && finalized.CallID != callID {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q call id conflicts with its finalized stream item",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if finalized.Name != "" && finalized.Name != name {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q name conflicts with its finalized stream item",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	raw, err := replaceOpenAIFunctionArguments(raw, finalized.Arguments)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"%w: repair OpenAI function call %q replay payload: %v",
			ErrInvalidModelOutput,
			itemID,
			err,
		)
	}
	return &ToolCall{
		ID: callID, Name: name, Input: json.RawMessage(finalized.Arguments),
	}, raw, nil
}

func replaceOpenAIFunctionArguments(
	raw json.RawMessage,
	arguments string,
) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	object["arguments"] = encodedArguments
	return json.Marshal(object)
}

func openAIFinalMessageText(raw *responses.Response) (string, string, error) {
	var outputText strings.Builder
	var refusal strings.Builder
	for _, item := range raw.Output {
		if item.Type != string(ModelOutputMessage) {
			continue
		}
		switch item.Phase {
		case "", responses.ResponseOutputMessagePhaseFinalAnswer:
		case responses.ResponseOutputMessagePhaseCommentary:
			continue
		default:
			return "", "", fmt.Errorf("agent: OpenAI output message %q has unsupported phase %q", item.ID, item.Phase)
		}
		for _, content := range item.Content {
			switch content.Type {
			case "output_text":
				outputText.WriteString(content.Text)
			case "refusal":
				refusal.WriteString(content.Refusal)
			default:
				return "", "", fmt.Errorf("agent: OpenAI output message %q has unsupported content type %q", item.ID, content.Type)
			}
		}
	}
	return outputText.String(), refusal.String(), nil
}

func strictOpenAIParameters(raw json.RawMessage) (map[string]any, error) {
	params, err := unmarshalOpenAISchema(raw)
	if err != nil {
		return nil, err
	}
	normalizeOpenAISchemaForStrict(params)
	return params, nil
}

func unmarshalOpenAISchema(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty schema")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("schema must be a JSON object")
	}
	return out, nil
}

func normalizeOpenAISchemaForStrict(node map[string]any) {
	props, hasProps := node["properties"].(map[string]any)
	typeName, _ := node["type"].(string)
	typeNames, _ := node["type"].([]any)
	hasObjectType := typeName == "object" || openAIContainsString(typeNames, "object")
	if hasObjectType || hasProps {
		if !hasObjectType {
			node["type"] = "object"
		}
		node["additionalProperties"] = false
		if !hasProps {
			props = make(map[string]any)
			node["properties"] = props
		}
		originalRequired := openAIRequiredSet(node["required"])
		required := make([]string, 0, len(props))
		for name, rawProp := range props {
			required = append(required, name)
			if prop, ok := rawProp.(map[string]any); ok {
				normalizeOpenAISchemaForStrict(prop)
				if !originalRequired[name] {
					makeOpenAISchemaNullable(prop)
				}
			}
		}
		sort.Strings(required)
		requiredAny := make([]any, len(required))
		for i, name := range required {
			requiredAny[i] = name
		}
		node["required"] = requiredAny
	}
	if items, ok := node["items"].(map[string]any); ok {
		normalizeOpenAISchemaForStrict(items)
	}
	for _, key := range []string{"$defs", "definitions"} {
		if definitions, ok := node[key].(map[string]any); ok {
			for _, rawDefinition := range definitions {
				if definition, ok := rawDefinition.(map[string]any); ok {
					normalizeOpenAISchemaForStrict(definition)
				}
			}
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if arr, ok := node[key].([]any); ok {
			for _, rawChild := range arr {
				if child, ok := rawChild.(map[string]any); ok {
					normalizeOpenAISchemaForStrict(child)
				}
			}
		}
	}
}

func openAIRequiredSet(raw any) map[string]bool {
	out := make(map[string]bool)
	for _, value := range openAIAnySlice(raw) {
		if name, ok := value.(string); ok {
			out[name] = true
		}
	}
	return out
}

func makeOpenAISchemaNullable(node map[string]any) {
	original := cloneOpenAIJSON(node)
	for key := range node {
		delete(node, key)
	}
	node["anyOf"] = []any{original, map[string]any{"type": "null"}}
}

func cloneOpenAIJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = cloneOpenAIJSON(child)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = cloneOpenAIJSON(child)
		}
		return out
	default:
		return value
	}
}

func openAIAnySlice(raw any) []any {
	switch values := raw.(type) {
	case []any:
		return values
	case []string:
		out := make([]any, len(values))
		for i, value := range values {
			out[i] = value
		}
		return out
	default:
		return nil
	}
}

func openAIContainsString(values []any, want string) bool {
	for _, value := range values {
		if s, ok := value.(string); ok && s == want {
			return true
		}
	}
	return false
}
