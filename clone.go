package agentruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func appendModelOutputItems(transcript []ModelInputItem, items []ModelOutputItem, responseID string) ([]ModelInputItem, error) {
	if responseID != "" {
		if err := requireCanonicalIdentity(responseID, "model response id"); err != nil {
			return nil, fmt.Errorf("%w: model output cannot be replayed: %v", ErrInvalidModelOutput, err)
		}
	}
	if err := validateModelOutputItemAggregateIdentity(items); err != nil {
		return nil, fmt.Errorf("%w: model output cannot be replayed: %v", ErrInvalidModelOutput, err)
	}
	for _, item := range items {
		inputItem := ModelInputItem{
			Type:       ModelInputAssistantOutput,
			ResponseID: responseID,
			OutputType: item.Type,
			Raw:        append(json.RawMessage(nil), item.Raw...),
		}
		if item.Type == ModelOutputFunctionCall && item.Call != nil {
			inputItem.CallID = item.Call.ID
		}
		transcript = append(transcript, inputItem)
	}
	return transcript, nil
}

func validateModelOutputItem(item ModelOutputItem) error {
	if err := validateUTF8Boundary("model output item", item); err != nil {
		return err
	}
	object, err := decodeModelOutputObject(item.Raw, item.Type)
	if err != nil {
		return err
	}
	if err := validateModelOutputItemID(object, item.ID); err != nil {
		return err
	}
	switch item.Type {
	case ModelOutputMessage:
		if item.Call != nil {
			return fmt.Errorf("output type %q cannot contain a function call", item.Type)
		}
		return validateReplayMessageObject(object, item.Text)
	case ModelOutputReasoning:
		if item.Call != nil {
			return fmt.Errorf("output type %q cannot contain a function call", item.Type)
		}
		return validateReplayReasoningObject(object)
	case ModelOutputFunctionCall:
		if item.Call == nil {
			return fmt.Errorf("function call is missing its structured call id")
		}
		if err := requireCanonicalIdentity(item.Call.ID, "structured function call id"); err != nil {
			return err
		}
		if err := requireCanonicalIdentity(item.Call.Name, "structured function call name"); err != nil {
			return err
		}
		if err := validateReplayFunctionCallObject(object, item.Call, item.Call.ID); err != nil {
			return fmt.Errorf("cannot be projected before execution: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported output type %q", item.Type)
	}
}

func decodeModelOutputObject(raw json.RawMessage, outputType ModelOutputItemType) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("raw payload is required")
	}
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("raw payload is ambiguous or invalid: %v", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("raw payload must be a JSON object")
	}
	rawType, ok := object["type"].(string)
	if !ok || strings.TrimSpace(rawType) == "" {
		return nil, fmt.Errorf("raw payload type is required")
	}
	if ModelOutputItemType(rawType) != outputType {
		return nil, fmt.Errorf("cannot be projected before execution: declared type %q does not match raw type %q", outputType, rawType)
	}
	return object, nil
}

func validateModelOutputItemID(object map[string]any, itemID string) error {
	if itemID != "" {
		if err := requireCanonicalIdentity(itemID, "structured item id"); err != nil {
			return err
		}
	}
	rawID, exists := object["id"]
	if !exists {
		if itemID != "" {
			return fmt.Errorf("raw payload omits structured item id %q", itemID)
		}
		return nil
	}
	id, ok := rawID.(string)
	if !ok {
		return fmt.Errorf("raw payload item id must be a non-empty string")
	}
	if err := requireCanonicalIdentity(id, "raw payload item id"); err != nil {
		return err
	}
	if itemID != "" && id != itemID {
		return fmt.Errorf("raw payload item id %q does not match structured item id %q", id, itemID)
	}
	return nil
}

type replayMessageSemantics struct {
	text      string
	refusal   string
	aggregate bool
}

func validateReplayMessageObject(object map[string]any, structuredText string) error {
	semantics, err := extractReplayMessageSemantics(object)
	if err != nil {
		return err
	}
	if structuredText != "" && semantics.text != structuredText {
		return fmt.Errorf("message raw text does not match structured text")
	}
	return nil
}

func extractReplayMessageSemantics(object map[string]any) (replayMessageSemantics, error) {
	content, hasContent := object["content"]
	if hasContent {
		parts, ok := content.([]any)
		if !ok {
			return replayMessageSemantics{}, fmt.Errorf("message raw content must be an array")
		}
		if err := requireReplayString(object, "id"); err != nil {
			return replayMessageSemantics{}, fmt.Errorf("message raw %w", err)
		}
		if role, ok := object["role"].(string); !ok || role != "assistant" {
			return replayMessageSemantics{}, fmt.Errorf("message raw role must be \"assistant\"")
		}
		if err := requireReplayStatus(object, "message"); err != nil {
			return replayMessageSemantics{}, err
		}
		aggregate := true
		if phase, exists := object["phase"]; exists {
			phaseName, ok := phase.(string)
			if !ok {
				return replayMessageSemantics{}, fmt.Errorf("message raw phase must be a string")
			}
			switch phaseName {
			case "", "final_answer":
			case "commentary":
				aggregate = false
			default:
				return replayMessageSemantics{}, fmt.Errorf("message raw phase %q is unsupported", phaseName)
			}
		}
		var text strings.Builder
		var refusal strings.Builder
		for index, value := range parts {
			part, ok := value.(map[string]any)
			if !ok {
				return replayMessageSemantics{}, fmt.Errorf("message raw content item %d must be an object", index)
			}
			partType, ok := part["type"].(string)
			if !ok {
				return replayMessageSemantics{}, fmt.Errorf("message raw content item %d type is required", index)
			}
			switch partType {
			case "output_text":
				partText, ok := part["text"].(string)
				if !ok {
					return replayMessageSemantics{}, fmt.Errorf("message raw output_text item %d text must be a string", index)
				}
				text.WriteString(partText)
			case "refusal":
				partRefusal, ok := part["refusal"].(string)
				if !ok {
					return replayMessageSemantics{}, fmt.Errorf("message raw refusal item %d refusal must be a string", index)
				}
				refusal.WriteString(partRefusal)
			default:
				return replayMessageSemantics{}, fmt.Errorf("message raw content item %d has unsupported type %q", index, partType)
			}
		}
		return replayMessageSemantics{text: text.String(), refusal: refusal.String(), aggregate: aggregate}, nil
	}
	text, ok := object["text"].(string)
	if !ok {
		return replayMessageSemantics{}, fmt.Errorf("message raw requires an exact content array or native text string")
	}
	return replayMessageSemantics{text: text, aggregate: true}, nil
}

func validateModelResponseReplayIdentity(response *ModelResponse) error {
	if response == nil {
		return errors.New("model response is nil")
	}
	if err := requireCanonicalIdentity(response.ID, "model response id"); err != nil {
		return err
	}
	if err := validateModelOutputItemAggregateIdentity(response.Items); err != nil {
		return err
	}
	var outputText strings.Builder
	var refusal strings.Builder
	for index, item := range response.Items {
		if item.Type != ModelOutputMessage {
			continue
		}
		object, err := decodeModelOutputObject(item.Raw, item.Type)
		if err != nil {
			return fmt.Errorf("response message item %d: %w", index, err)
		}
		semantics, err := extractReplayMessageSemantics(object)
		if err != nil {
			return fmt.Errorf("response message item %d: %w", index, err)
		}
		if !semantics.aggregate {
			continue
		}
		outputText.WriteString(semantics.text)
		refusal.WriteString(semantics.refusal)
	}
	if response.OutputText != outputText.String() {
		return fmt.Errorf("response output_text does not match replay message content")
	}
	if response.Refusal != refusal.String() {
		return fmt.Errorf("response refusal does not match replay message content")
	}
	return nil
}

func validateModelOutputItemAggregateIdentity(items []ModelOutputItem) error {
	seenItemIDs := make(map[string]struct{})
	for index, item := range items {
		if err := validateModelOutputItem(item); err != nil {
			return fmt.Errorf("output item %d cannot be replayed: %w", index, err)
		}
		itemID, err := replayProviderItemID(item.Raw)
		if err != nil {
			return fmt.Errorf("output item %d cannot be replayed: %w", index, err)
		}
		if item.ID != "" {
			itemID = item.ID
		}
		if itemID == "" {
			continue
		}
		if _, exists := seenItemIDs[itemID]; exists {
			return fmt.Errorf("duplicate provider item id %q", itemID)
		}
		seenItemIDs[itemID] = struct{}{}
	}
	return nil
}

func validateReplayReasoningObject(object map[string]any) error {
	if err := requireReplayString(object, "id"); err != nil {
		return fmt.Errorf("reasoning raw %w", err)
	}
	summary, ok := object["summary"].([]any)
	if !ok {
		return fmt.Errorf("reasoning raw summary must be an array")
	}
	for index, value := range summary {
		part, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("reasoning raw summary item %d must be an object", index)
		}
		if partType, ok := part["type"].(string); !ok || partType != "summary_text" {
			return fmt.Errorf("reasoning raw summary item %d type must be \"summary_text\"", index)
		}
		if _, ok := part["text"].(string); !ok {
			return fmt.Errorf("reasoning raw summary item %d text must be a string", index)
		}
	}
	if value, exists := object["content"]; exists {
		content, ok := value.([]any)
		if !ok {
			return fmt.Errorf("reasoning raw content must be an array")
		}
		for index, value := range content {
			part, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("reasoning raw content item %d must be an object", index)
			}
			if partType, ok := part["type"].(string); !ok || partType != "reasoning_text" {
				return fmt.Errorf("reasoning raw content item %d type must be \"reasoning_text\"", index)
			}
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("reasoning raw content item %d text must be a string", index)
			}
		}
	}
	if encrypted, exists := object["encrypted_content"]; exists && encrypted != nil {
		if _, ok := encrypted.(string); !ok {
			return fmt.Errorf("reasoning raw encrypted_content must be a string or null")
		}
	}
	if _, exists := object["status"]; exists {
		if err := requireReplayStatus(object, "reasoning"); err != nil {
			return err
		}
	}
	return nil
}

func validateReplayFunctionCallObject(object map[string]any, structured *ToolCall, expectedCallID string) error {
	if expectedCallID != "" {
		if err := requireCanonicalIdentity(expectedCallID, "paired function call id"); err != nil {
			return err
		}
	}
	if structured != nil {
		if err := requireCanonicalIdentity(structured.ID, "structured function call id"); err != nil {
			return err
		}
		if err := requireCanonicalIdentity(structured.Name, "structured function call name"); err != nil {
			return err
		}
		if expectedCallID != "" && structured.ID != expectedCallID {
			return fmt.Errorf("structured function call id %q does not match paired call id %q", structured.ID, expectedCallID)
		}
	}
	if _, exists := object["id"]; exists {
		if err := requireReplayString(object, "id"); err != nil {
			return fmt.Errorf("function call raw %w", err)
		}
	}
	if _, exists := object["status"]; exists {
		if err := requireReplayFunctionCallStatus(object, "function call"); err != nil {
			return err
		}
	}
	top, hasTop, err := parseReplayFunctionCallRepresentation(object, false)
	if err != nil {
		return fmt.Errorf("function call raw %w", err)
	}
	var nested replayFunctionCallRepresentation
	hasNested := false
	if value, exists := object["call"]; exists {
		nestedObject, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("function call raw call must be an object")
		}
		if _, exists := nestedObject["status"]; exists {
			if err := requireReplayFunctionCallStatus(nestedObject, "function call nested"); err != nil {
				return err
			}
		}
		nested, hasNested, err = parseReplayFunctionCallRepresentation(nestedObject, true)
		if err != nil {
			return fmt.Errorf("function call nested raw %w", err)
		}
		if !hasNested {
			return fmt.Errorf("function call nested raw representation is empty")
		}
		if topStatus, topHasStatus, statusErr := optionalReplayString(object, "status"); statusErr != nil {
			return fmt.Errorf("function call raw %w", statusErr)
		} else if nestedStatus, nestedHasStatus, nestedStatusErr := optionalReplayString(nestedObject, "status"); nestedStatusErr != nil {
			return fmt.Errorf("function call nested raw %w", nestedStatusErr)
		} else if topHasStatus && nestedHasStatus && topStatus != nestedStatus {
			return fmt.Errorf("function call raw payload contains conflicting statuses")
		}
	}
	if !hasTop && !hasNested {
		return fmt.Errorf("function call raw payload omits a complete call representation")
	}
	call := top
	if !hasTop {
		call = nested
	}
	if hasTop && hasNested {
		if top.callID != nested.callID {
			return fmt.Errorf("function call raw payload contains conflicting call ids")
		}
		if top.name != nested.name {
			return fmt.Errorf("function call raw payload contains conflicting names")
		}
		if !jsonSemanticallyEqual(top.arguments, nested.arguments) {
			return fmt.Errorf("function call raw payload contains conflicting arguments")
		}
	}
	if expectedCallID != "" && call.callID != expectedCallID {
		return fmt.Errorf("function call raw payload does not identify the paired call %q", expectedCallID)
	}
	if structured == nil {
		return nil
	}
	if call.callID != structured.ID {
		return fmt.Errorf("function call raw id %q does not match structured id %q", call.callID, structured.ID)
	}
	if call.name != structured.Name {
		return fmt.Errorf("function call raw name %q does not match structured name %q", call.name, structured.Name)
	}
	if !jsonSemanticallyEqual(call.arguments, structured.Input) {
		return fmt.Errorf("function call raw arguments do not match structured input")
	}
	return nil
}

func requireReplayFunctionCallStatus(object map[string]any, kind string) error {
	status, ok := object["status"].(string)
	if !ok {
		return fmt.Errorf("%s raw status must be a string", kind)
	}
	if status != "completed" {
		return fmt.Errorf("%s raw status %q is unsupported", kind, status)
	}
	return nil
}

type replayFunctionCallRepresentation struct {
	callID    string
	name      string
	arguments json.RawMessage
}

func parseReplayFunctionCallRepresentation(object map[string]any, nested bool) (replayFunctionCallRepresentation, bool, error) {
	callIDField := "call_id"
	if nested {
		callIDField = "id"
	}
	_, hasCallID := object[callIDField]
	_, hasName := object["name"]
	_, hasArguments := object["arguments"]
	if !hasCallID && !hasName && !hasArguments {
		return replayFunctionCallRepresentation{}, false, nil
	}
	if !hasCallID || !hasName || !hasArguments {
		return replayFunctionCallRepresentation{}, true, fmt.Errorf("representation requires %s, name, and arguments", callIDField)
	}
	callID, _, err := optionalReplayString(object, callIDField)
	if err != nil {
		return replayFunctionCallRepresentation{}, true, err
	}
	if err := requireCanonicalIdentity(callID, callIDField); err != nil {
		return replayFunctionCallRepresentation{}, true, err
	}
	name, _, err := optionalReplayString(object, "name")
	if err != nil {
		return replayFunctionCallRepresentation{}, true, err
	}
	if err := requireCanonicalIdentity(name, "name"); err != nil {
		return replayFunctionCallRepresentation{}, true, err
	}
	var arguments json.RawMessage
	if nested {
		arguments, err = json.Marshal(object["arguments"])
		if err != nil {
			return replayFunctionCallRepresentation{}, true, fmt.Errorf("arguments are invalid: %v", err)
		}
	} else {
		text, ok := object["arguments"].(string)
		if !ok {
			return replayFunctionCallRepresentation{}, true, fmt.Errorf("arguments must be a JSON string")
		}
		arguments = json.RawMessage(text)
	}
	if _, err := decodeExactJSON(arguments); err != nil {
		return replayFunctionCallRepresentation{}, true, fmt.Errorf("arguments are ambiguous or invalid: %v", err)
	}
	return replayFunctionCallRepresentation{callID: callID, name: name, arguments: arguments}, true, nil
}

func optionalReplayString(object map[string]any, field string) (string, bool, error) {
	value, exists := object[field]
	if !exists {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", field)
	}
	return text, true, nil
}

func requireReplayString(object map[string]any, field string) error {
	value, ok := object[field].(string)
	if !ok {
		return fmt.Errorf("%s must be a non-empty string", field)
	}
	return requireCanonicalIdentity(value, field)
}

func requireCanonicalIdentity(value string, field string) error {
	if value == "" {
		return fmt.Errorf("%s must be a non-empty string", field)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be canonical without surrounding whitespace", field)
	}
	return nil
}

func requireReplayStatus(object map[string]any, kind string) error {
	status, ok := object["status"].(string)
	if !ok {
		return fmt.Errorf("%s raw status must be a string", kind)
	}
	switch status {
	case "in_progress", "completed", "incomplete":
		return nil
	default:
		return fmt.Errorf("%s raw status %q is unsupported", kind, status)
	}
}

func validateModelInputItemsForReplay(items []ModelInputItem) error {
	seenItemIDs := make(map[string]struct{})
	seenResponseIDs := make(map[string]struct{})
	activeResponseID := ""
	for index, item := range items {
		if err := validateModelInputItemForReplay(item); err != nil {
			return fmt.Errorf("model input item %d cannot be replayed: %w", index, err)
		}
		if item.Type == ModelInputAssistantOutput {
			if item.ResponseID == "" {
				activeResponseID = ""
			} else if item.ResponseID != activeResponseID {
				if _, exists := seenResponseIDs[item.ResponseID]; exists {
					return fmt.Errorf("model input item %d repeats model response id %q", index, item.ResponseID)
				}
				seenResponseIDs[item.ResponseID] = struct{}{}
				activeResponseID = item.ResponseID
			}
			itemID, err := replayProviderItemID(item.Raw)
			if err != nil {
				return fmt.Errorf("model input item %d cannot be replayed: %w", index, err)
			}
			if itemID != "" {
				if _, exists := seenItemIDs[itemID]; exists {
					return fmt.Errorf("model input item %d repeats provider item id %q", index, itemID)
				}
				seenItemIDs[itemID] = struct{}{}
			}
		} else {
			activeResponseID = ""
		}
	}
	return nil
}

func validatePersistedModelInputItems(items []ModelInputItem) error {
	seenItemIDs := make(map[string]struct{})
	seenResponseIDs := make(map[string]struct{})
	activeResponseID := ""
	for index, item := range items {
		if err := validateModelInputItemForReplay(item); err != nil {
			return fmt.Errorf("persisted model input item %d cannot be replayed: %w", index, err)
		}
		if item.Type == ModelInputAssistantOutput {
			if item.ResponseID == "" {
				activeResponseID = ""
			} else if item.ResponseID != activeResponseID {
				if _, exists := seenResponseIDs[item.ResponseID]; exists {
					return fmt.Errorf("persisted model input item %d repeats model response id %q", index, item.ResponseID)
				}
				seenResponseIDs[item.ResponseID] = struct{}{}
				activeResponseID = item.ResponseID
			}
			itemID, err := replayProviderItemID(item.Raw)
			if err != nil {
				return fmt.Errorf("persisted model input item %d cannot be replayed: %w", index, err)
			}
			if itemID != "" {
				if _, exists := seenItemIDs[itemID]; exists {
					return fmt.Errorf("persisted model input item %d repeats provider item id %q", index, itemID)
				}
				seenItemIDs[itemID] = struct{}{}
			}
		} else {
			activeResponseID = ""
		}
		if item.Type != ModelInputUserMessage {
			continue
		}
		for attachmentIndex, attachment := range item.Attachments {
			if err := validatePersistedModelInputAttachment(attachment); err != nil {
				return fmt.Errorf("persisted model input item %d attachment %d is invalid: %w", index, attachmentIndex, err)
			}
		}
	}
	return nil
}

func replayProviderItemID(raw json.RawMessage) (string, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return "", fmt.Errorf("raw payload is ambiguous or invalid: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", errors.New("raw payload must be a JSON object")
	}
	rawID, exists := object["id"]
	if !exists {
		return "", nil
	}
	itemID, ok := rawID.(string)
	if !ok {
		return "", errors.New("raw payload item id must be a non-empty string")
	}
	if err := requireCanonicalIdentity(itemID, "raw payload item id"); err != nil {
		return "", err
	}
	return itemID, nil
}

func validatePersistedModelInputAttachment(attachment ModelInputAttachment) error {
	if attachment.URL != "" || attachment.CurrentRun {
		return errors.New("persisted attachment contains transient URL or current-run authority")
	}
	normalized := NormalizeModelInputAttachment(attachment)
	if normalized != attachment {
		return errors.New("persisted attachment metadata is not canonical")
	}
	if attachment.Kind != ModelInputAttachmentImage {
		return ValidateModelInputAttachment(attachment)
	}
	if attachment.StorageKey == "" || attachment.ExpiresAt.IsZero() {
		return errors.New("persisted image attachment requires stable storage metadata")
	}
	probe := attachment
	probe.URL = "https://persistent.invalid/image"
	if err := ValidateImageAttachment(probe); err != nil {
		return err
	}
	return nil
}

func validateModelInputItemForReplay(item ModelInputItem) error {
	if err := validateUTF8Boundary("model input replay item", item); err != nil {
		return err
	}
	switch item.Type {
	case ModelInputUserMessage:
		if strings.TrimSpace(item.Text) == "" {
			return fmt.Errorf("user message text is required")
		}
		if item.ResponseID != "" || item.OutputType != "" || len(item.Raw) != 0 || item.CallID != "" || len(item.Output) != 0 {
			return fmt.Errorf("user message contains assistant or tool-result fields")
		}
		return nil
	case ModelInputAssistantOutput:
		if item.ResponseID != "" {
			if err := requireCanonicalIdentity(item.ResponseID, "assistant response id"); err != nil {
				return err
			}
		}
		if len(item.Attachments) != 0 || len(item.Output) != 0 {
			return fmt.Errorf("assistant output contains attachments or tool-result output")
		}
		if item.OutputType != ModelOutputFunctionCall && item.CallID != "" {
			return fmt.Errorf("non-function assistant output contains a call id")
		}
		if item.OutputType == ModelOutputFunctionCall {
			if err := requireCanonicalIdentity(item.CallID, "assistant function call id"); err != nil {
				return err
			}
		}
		object, legacy, err := decodeModelInputReplayObject(item)
		if err != nil {
			return err
		}
		if legacy {
			return nil
		}
		switch item.OutputType {
		case ModelOutputMessage:
			return validateReplayMessageObject(object, item.Text)
		case ModelOutputReasoning:
			return validateReplayReasoningObject(object)
		case ModelOutputFunctionCall:
			return validateReplayFunctionCallObject(object, nil, item.CallID)
		default:
			return fmt.Errorf("unsupported output type %q", item.OutputType)
		}
	case ModelInputToolResult:
		if item.Text != "" || len(item.Attachments) != 0 || item.ResponseID != "" || item.OutputType != "" || len(item.Raw) != 0 {
			return fmt.Errorf("tool result contains user or assistant fields")
		}
		if err := requireCanonicalIdentity(item.CallID, "tool result call id"); err != nil {
			return err
		}
		if len(item.Output) == 0 {
			return fmt.Errorf("tool result output is required")
		}
		if _, err := decodeExactJSON(item.Output); err != nil {
			return fmt.Errorf("tool result output is ambiguous or invalid: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported model input item type %q", item.Type)
	}
}

func decodeModelInputReplayObject(item ModelInputItem) (map[string]any, bool, error) {
	value, err := decodeExactJSON(item.Raw)
	if err != nil {
		return nil, false, fmt.Errorf("raw payload is ambiguous or invalid: %v", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("raw payload must be a JSON object")
	}
	if _, hasType := object["type"]; hasType {
		strict, err := decodeModelOutputObject(item.Raw, item.OutputType)
		return strict, false, err
	}
	// Legacy native transcripts stored only the provider item ID alongside
	// structured ModelInputItem fields. Preserve that narrow, demonstrable
	// compatibility shape for generic models; adapters such as OpenAI still
	// apply their stricter replay mapping before any network request.
	if len(object) != 1 {
		return nil, false, fmt.Errorf("legacy raw payload may contain only its item id")
	}
	if err := requireReplayString(object, "id"); err != nil {
		return nil, false, fmt.Errorf("legacy raw %w", err)
	}
	switch item.OutputType {
	case ModelOutputMessage:
		if item.Text == "" {
			return nil, false, fmt.Errorf("legacy message requires structured text")
		}
	case ModelOutputReasoning:
	case ModelOutputFunctionCall:
		if err := requireCanonicalIdentity(item.CallID, "legacy function call structured call id"); err != nil {
			return nil, false, err
		}
	default:
		return nil, false, fmt.Errorf("unsupported output type %q", item.OutputType)
	}
	return object, true, nil
}

func cloneModelInputItems(items []ModelInputItem) []ModelInputItem {
	out := make([]ModelInputItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].Raw = append(json.RawMessage(nil), items[i].Raw...)
		out[i].Output = append(json.RawMessage(nil), items[i].Output...)
		out[i].Attachments = cloneModelInputAttachments(items[i].Attachments)
	}
	return out
}

func clonePersistentModelInputItems(items []ModelInputItem) []ModelInputItem {
	out := cloneModelInputItems(items)
	for itemIndex := range out {
		for attachmentIndex := range out[itemIndex].Attachments {
			if out[itemIndex].Attachments[attachmentIndex].Kind == ModelInputAttachmentImage {
				out[itemIndex].Attachments[attachmentIndex].URL = ""
			}
			out[itemIndex].Attachments[attachmentIndex].CurrentRun = false
		}
	}
	return out
}

func cloneModelInputAttachments(attachments []ModelInputAttachment) []ModelInputAttachment {
	return append([]ModelInputAttachment(nil), attachments...)
}

func cloneModelOutputItems(items []ModelOutputItem) []ModelOutputItem {
	out := make([]ModelOutputItem, len(items))
	for i := range items {
		out[i] = items[i]
		out[i].Raw = append(json.RawMessage(nil), items[i].Raw...)
		if items[i].Call != nil {
			call := *items[i].Call
			call.Input = append(json.RawMessage(nil), items[i].Call.Input...)
			out[i].Call = &call
		}
	}
	return out
}

func cloneStoredItemRecord(item ItemRecord) ItemRecord {
	item.Data = append(json.RawMessage(nil), item.Data...)
	return item
}

func cloneStoredSessionState(session SessionState) SessionState {
	session.Transcript = cloneModelInputItems(session.Transcript)
	session.Checkpoint = cloneContextCheckpoint(session.Checkpoint)
	session.SeenCallIDs = cloneStringsPreserveNil(session.SeenCallIDs)
	return session
}

func cloneStoredOperationPlanBatch(batch OperationPlanBatch) OperationPlanBatch {
	batch.Steps = append([]OperationPlanStep(nil), batch.Steps...)
	for index := range batch.Steps {
		batch.Steps[index].Arguments = append(json.RawMessage(nil), batch.Steps[index].Arguments...)
	}
	return batch
}

func cloneStoredOperationExecution(execution OperationExecutionRecord) OperationExecutionRecord {
	execution.Arguments = append(json.RawMessage(nil), execution.Arguments...)
	execution.Result = cloneOperationResult(execution.Result)
	if execution.Verification != nil {
		verification := cloneVerificationResult(*execution.Verification)
		execution.Verification = &verification
	}
	return execution
}

func cloneStoredOperationTransition(transition OperationExecutionTransition) OperationExecutionTransition {
	transition.Result = cloneOperationResult(transition.Result)
	if transition.Verification != nil {
		verification := cloneVerificationResult(*transition.Verification)
		transition.Verification = &verification
	}
	transition.Evidence = append(json.RawMessage(nil), transition.Evidence...)
	return transition
}

func cloneStoredPendingApproval(pending PendingApprovalCommit) (PendingApprovalCommit, error) {
	request := pending.Request
	input, err := cloneOperationInput(request.Operation.Input)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	request.Operation.Input = input
	request.Operation.Operation = cloneOperationSummaries([]OperationSummary{request.Operation.Operation})[0]
	request.Operation.Call.Input = append(json.RawMessage(nil), request.Operation.Call.Input...)
	arguments, err := json.Marshal(request.Operation.Arguments)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	request.Operation.Arguments, err = decodeExactJSON(arguments)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	request.ModelOutput = cloneModelOutputItems(request.ModelOutput)
	request.Preview = append(json.RawMessage(nil), request.Preview...)
	request.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, true)
	return PendingApprovalCommit{
		AuthorityVersion: pending.AuthorityVersion,
		Request:          request,
		Decision:         pending.Decision,
		Audit:            cloneStoredItemRecord(pending.Audit),
		Digest:           pending.Digest,
	}, nil
}

func randomID() (string, error) {
	return randomIDFrom(rand.Reader)
}

func randomIDFrom(reader io.Reader) (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
