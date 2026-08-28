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
			if err := requireCanonicalIdentity(item.CallID, "tool result call_id"); err != nil {
				return nil, fmt.Errorf("agent: tool result item %d: %v", i, err)
			}
			if len(item.Output) == 0 {
				return nil, fmt.Errorf("agent: tool result item %d output must be valid JSON", i)
			}
			if _, err := decodeExactJSON(item.Output); err != nil {
				return nil, fmt.Errorf("agent: tool result item %d output must be unambiguous valid JSON: %v", i, err)
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
	if err := validateModelInputItemForReplay(item); err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	object, err := decodeModelOutputObject(item.Raw, item.OutputType)
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	switch item.OutputType {
	case ModelOutputMessage:
		if _, ok := object["content"].([]any); !ok {
			return responses.ResponseInputItemUnionParam{}, errors.New("OpenAI message raw content must be an array")
		}
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
		name, nameOK := object["name"].(string)
		arguments, argumentsOK := object["arguments"].(string)
		if !nameOK || strings.TrimSpace(name) == "" || !argumentsOK {
			return responses.ResponseInputItemUnionParam{}, errors.New("OpenAI function call raw name and arguments are required")
		}
		if _, err := decodeExactJSON(json.RawMessage(arguments)); err != nil {
			return responses.ResponseInputItemUnionParam{}, fmt.Errorf("OpenAI function call raw arguments are ambiguous or invalid: %v", err)
		}
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
	if err := validateOpenAIResponseEnvelope(raw); err != nil {
		return nil, err
	}
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
		if _, err := decodeExactJSON(data); err != nil {
			return nil, fmt.Errorf("agent: OpenAI output item %q has ambiguous or invalid raw JSON: %w", item.ID, err)
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
		if err := validateModelOutputItem(parsed); err != nil {
			return nil, fmt.Errorf("agent: OpenAI output item %q cannot be replayed: %w", item.ID, err)
		}
		out.Items = append(out.Items, parsed)
	}
	if err := validateModelResponseReplayIdentity(out); err != nil {
		return nil, fmt.Errorf("%w: OpenAI response identity is invalid: %v", ErrInvalidModelOutput, err)
	}
	return out, nil
}

func validateOpenAIResponseEnvelope(raw *responses.Response) error {
	if raw == nil {
		return fmt.Errorf("%w: OpenAI response is nil", ErrInvalidModelOutput)
	}
	if err := requireCanonicalIdentity(raw.ID, "OpenAI response id"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidModelOutput, err)
	}
	if raw.Status != responses.ResponseStatusCompleted {
		return fmt.Errorf("%w: OpenAI response %q has unsupported status %q", ErrInvalidModelOutput, raw.ID, raw.Status)
	}
	seenItemIDs := make(map[string]struct{}, len(raw.Output))
	for index, item := range raw.Output {
		if err := requireCanonicalIdentity(item.ID, "OpenAI output item id"); err != nil {
			return fmt.Errorf("%w: OpenAI output item %d: %v", ErrInvalidModelOutput, index, err)
		}
		if _, exists := seenItemIDs[item.ID]; exists {
			return fmt.Errorf("%w: OpenAI response repeats output item id %q", ErrInvalidModelOutput, item.ID)
		}
		seenItemIDs[item.ID] = struct{}{}
		if err := validateOpenAICompletedOutputItemStatus(item); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAICompletedOutputItemStatus(item responses.ResponseOutputItemUnion) error {
	status := ""
	present := false
	required := false
	switch item.Type {
	case string(ModelOutputMessage):
		message := item.AsMessage()
		status = string(message.Status)
		present = message.JSON.Status.Valid()
		required = true
	case string(ModelOutputReasoning):
		reasoning := item.AsReasoning()
		status = string(reasoning.Status)
		present = reasoning.JSON.Status.Valid()
	case string(ModelOutputFunctionCall):
		call := item.AsFunctionCall()
		status = string(call.Status)
		present = call.JSON.Status.Valid()
	default:
		return nil
	}
	if required && !present || present && status != string(responses.ResponseStatusCompleted) {
		return fmt.Errorf("%w: OpenAI output item %q has unsupported status %q", ErrInvalidModelOutput, item.ID, status)
	}
	return nil
}

func parseOpenAIFunctionCall(
	itemID string,
	call responses.ResponseFunctionToolCall,
	raw json.RawMessage,
	finalizedCalls map[string]openAIStreamFunctionCall,
) (*ToolCall, json.RawMessage, error) {
	callID := call.CallID
	name := call.Name
	arguments := call.Arguments
	if err := requireCanonicalIdentity(callID, "OpenAI function call id"); err != nil {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q: %v",
			ErrInvalidModelOutput,
			itemID,
			err,
		)
	}
	if err := requireCanonicalIdentity(name, "OpenAI function call name"); err != nil {
		return nil, nil, fmt.Errorf("%w: OpenAI function call %q: %v", ErrInvalidModelOutput, itemID, err)
	}
	finalized, ok := finalizedCalls[itemID]
	if !ok {
		if arguments != "" && json.Valid([]byte(arguments)) {
			return &ToolCall{
				ID: callID, Name: name, Input: json.RawMessage(arguments),
			}, raw, nil
		}
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q has invalid JSON arguments",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if !finalized.Finalized || finalized.Arguments == "" || !json.Valid([]byte(finalized.Arguments)) {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q has invalid JSON arguments",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if call.JSON.Status.Valid() && call.Status != responses.ResponseFunctionToolCallStatusCompleted {
		return nil, nil, fmt.Errorf(
			"%w: OpenAI function call %q cannot use finalized stream arguments with status %q",
			ErrInvalidModelOutput,
			itemID,
			call.Status,
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
	if arguments != "" {
		if !json.Valid([]byte(arguments)) {
			return nil, nil, fmt.Errorf("%w: OpenAI function call %q has invalid JSON arguments", ErrInvalidModelOutput, itemID)
		}
		if !jsonSemanticallyEqual(json.RawMessage(arguments), json.RawMessage(finalized.Arguments)) {
			return nil, nil, fmt.Errorf("%w: OpenAI function call %q arguments conflict with its finalized stream item", ErrInvalidModelOutput, itemID)
		}
		return &ToolCall{ID: callID, Name: name, Input: json.RawMessage(arguments)}, raw, nil
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
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("OpenAI function call raw payload must be an object")
	}
	object["arguments"] = arguments
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
