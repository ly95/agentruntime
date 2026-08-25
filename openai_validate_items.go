package agentruntime

import (
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/responses"
)

// D. Item / annotation / logprob validators
// ---------------------------------------------------------------------------

func validateOpenAIStreamItem(item responses.ResponseOutputItemUnion, expectedStatus responses.ResponseStatus, eventType string) error {
	label := fmt.Sprintf("%s item %q", eventType, item.ID)
	raw := json.RawMessage(item.RawJSON())
	if len(raw) == 0 {
		return fmt.Errorf("%w: OpenAI %s is missing its raw JSON", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	_ = expectedStatus
	switch item.Type {
	case "message":
		if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
			return err
		}
		role, err := openAIStringField(fields, "role")
		if err != nil {
			return err
		}
		if role != "assistant" {
			return fmt.Errorf("%w: OpenAI %s role %q is unsupported", ErrInvalidModelOutput, label, role)
		}
		if !openAINonNullRaw(fields["role"]) {
			return fmt.Errorf("%w: OpenAI %s is missing its role", ErrInvalidModelOutput, label)
		}
		if err := openAIRequireDomain(fields, "phase", []string{"commentary", "final_answer"}, label); err != nil {
			return err
		}
		if !openAINonNullRaw(fields["content"]) {
			return fmt.Errorf("%w: OpenAI %s is missing its content", ErrInvalidModelOutput, label)
		}
		if !openAIRawHasJSONKind(fields["content"], "array") {
			return fmt.Errorf("%w: OpenAI %s content must be an array", ErrInvalidModelOutput, label)
		}
		var rawParts []json.RawMessage
		if err := json.Unmarshal(fields["content"], &rawParts); err != nil {
			return fmt.Errorf("%w: OpenAI %s content has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, rawPart := range rawParts {
			if err := validateOpenAIOutputContentItem(rawPart, fmt.Sprintf("%s content[%d]", label, index)); err != nil {
				return err
			}
		}
	case "reasoning":
		if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
			return err
		}
		if !openAINonNullRaw(fields["summary"]) {
			return fmt.Errorf("%w: OpenAI %s is missing its summary", ErrInvalidModelOutput, label)
		}
		if !openAIRawHasJSONKind(fields["summary"], "array") {
			return fmt.Errorf("%w: OpenAI %s summary must be an array", ErrInvalidModelOutput, label)
		}
		var rawSummary []json.RawMessage
		if err := json.Unmarshal(fields["summary"], &rawSummary); err != nil {
			return fmt.Errorf("%w: OpenAI %s summary has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, rawPart := range rawSummary {
			if err := validateOpenAIReasoningSummaryPart(rawPart, fmt.Sprintf("%s summary[%d]", label, index)); err != nil {
				return err
			}
		}
		if raw, present := fields["content"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "array") {
				return fmt.Errorf("%w: OpenAI %s content must be an array", ErrInvalidModelOutput, label)
			}
			var rawContent []json.RawMessage
			if err := json.Unmarshal(raw, &rawContent); err != nil {
				return fmt.Errorf("%w: OpenAI %s content has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
			}
			for index, rawPart := range rawContent {
				if err := validateOpenAIReasoningContentPart(rawPart, fmt.Sprintf("%s content[%d]", label, index)); err != nil {
					return err
				}
			}
		}
		if raw, present := fields["encrypted_content"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s encrypted_content must be a string", ErrInvalidModelOutput, label)
			}
		}
	case "function_call":
		if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
			return err
		}
		for _, name := range []string{"call_id", "name", "arguments"} {
			raw, present := fields[name]
			if !present || !openAINonNullRaw(raw) {
				return fmt.Errorf("%w: OpenAI %s is missing its %s", ErrInvalidModelOutput, label, name)
			}
		}
		if err := validateOpenAICanonicalIdentity(fields["call_id"], label+".call_id"); err != nil {
			return err
		}
		name, err := openAIRawString(fields["name"], label+".name")
		if err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
		}
		arguments, err := openAIRawString(fields["arguments"], label+".arguments")
		if err != nil {
			return err
		}
		if arguments != "" {
			if _, err := decodeExactJSON(json.RawMessage(arguments)); err != nil {
				return fmt.Errorf("%w: OpenAI %s.arguments are ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s has unsupported item type %q", ErrInvalidModelOutput, label, item.Type)
	}
	return nil
}

func validateOpenAIReasoningSummaryPart(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "text"); err != nil {
		return err
	}
	partType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	if partType != "summary_text" {
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, partType)
	}
	if !openAIRawHasJSONKind(fields["text"], "string") {
		return fmt.Errorf("%w: OpenAI %s.text must be a string", ErrInvalidModelOutput, label)
	}
	return nil
}

func validateOpenAIReasoningContentPart(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "text"); err != nil {
		return err
	}
	partType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	if partType != "reasoning_text" {
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, partType)
	}
	if !openAIRawHasJSONKind(fields["text"], "string") {
		return fmt.Errorf("%w: OpenAI %s.text must be a string", ErrInvalidModelOutput, label)
	}
	return nil
}

func validateOpenAIAnnotation(annotation responses.ResponseOutputTextAnnotationUnion, label string) error {
	raw := json.RawMessage(annotation.RawJSON())
	if len(raw) == 0 {
		return fmt.Errorf("%w: OpenAI %s is missing its raw JSON", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	switch annotation.Type {
	case "url_citation":
		if err := openAIRequireKnownFields(fields, label, "type", "start_index", "end_index", "title", "url"); err != nil {
			return err
		}
		start, err := openAIRawInt(fields["start_index"], label+".start_index")
		if err != nil {
			return err
		}
		end, err := openAIRawInt(fields["end_index"], label+".end_index")
		if err != nil {
			return err
		}
		if start < 0 || end < 0 || end < start {
			return fmt.Errorf("%w: OpenAI %s indices are out of order or negative", ErrInvalidModelOutput, label)
		}
		for _, name := range []string{"title", "url"} {
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if !openAIRawHasJSONKind(raw, "string") {
					return fmt.Errorf("%w: OpenAI %s.%s must be a string", ErrInvalidModelOutput, label, name)
				}
			}
		}
	case "file_citation":
		if err := openAIRequireKnownFields(fields, label, "type", "file_id", "filename", "index"); err != nil {
			return err
		}
		if err := validateOpenAICanonicalIdentity(fields["file_id"], label+".file_id"); err != nil {
			return err
		}
		if raw, present := fields["filename"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s.filename must be a string", ErrInvalidModelOutput, label)
			}
		}
		if raw, present := fields["index"]; present && openAINonNullRaw(raw) {
			if _, err := openAIRawInt(raw, label+".index"); err != nil {
				return err
			}
		}
	case "file_path":
		if err := openAIRequireKnownFields(fields, label, "type", "file_id", "index"); err != nil {
			return err
		}
		if err := validateOpenAICanonicalIdentity(fields["file_id"], label+".file_id"); err != nil {
			return err
		}
		if raw, present := fields["index"]; present && openAINonNullRaw(raw) {
			if _, err := openAIRawInt(raw, label+".index"); err != nil {
				return err
			}
		}
	case "container_file_citation":
		if err := openAIRequireKnownFields(fields, label, "type", "container_id", "end_index", "file_id", "filename", "start_index"); err != nil {
			return err
		}
		if err := validateOpenAICanonicalIdentity(fields["file_id"], label+".file_id"); err != nil {
			return err
		}
		if raw, present := fields["container_id"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAICanonicalIdentity(raw, label+".container_id"); err != nil {
				return err
			}
		}
		for _, name := range []string{"start_index", "end_index"} {
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if _, err := openAIRawInt(raw, label+"."+name); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s has unsupported annotation type %q", ErrInvalidModelOutput, label, annotation.Type)
	}
	return nil
}

func validateOpenAIRawLogprobs(raw json.RawMessage, label string, withBytes bool) error {
	if !openAIRawHasJSONKind(raw, "array") {
		return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, item := range items {
		fields, err := decodeOpenAIRawObject(item, fmt.Sprintf("%s[%d]", label, index))
		if err != nil {
			return err
		}
		var allowed []string
		if withBytes {
			allowed = []string{"token", "bytes", "logprob", "top_logprobs"}
		} else {
			allowed = []string{"token", "logprob", "top_logprobs"}
		}
		if err := openAIRequireKnownFields(fields, fmt.Sprintf("%s[%d]", label, index), allowed...); err != nil {
			return err
		}
		if !openAIRawHasJSONKind(fields["token"], "string") {
			return fmt.Errorf("%w: OpenAI %s[%d].token must be a string", ErrInvalidModelOutput, label, index)
		}
		if !openAIRawHasJSONKind(fields["logprob"], "number") {
			return fmt.Errorf("%w: OpenAI %s[%d].logprob must be a number", ErrInvalidModelOutput, label, index)
		}
		if withBytes {
			if raw, present := fields["bytes"]; present && openAINonNullRaw(raw) {
				if !openAIRawHasJSONKind(raw, "array") {
					return fmt.Errorf("%w: OpenAI %s[%d].bytes must be an array", ErrInvalidModelOutput, label, index)
				}
			}
		}
		if !openAIRawHasJSONKind(fields["top_logprobs"], "array") {
			return fmt.Errorf("%w: OpenAI %s[%d].top_logprobs must be an array", ErrInvalidModelOutput, label, index)
		}
		var rawTop []json.RawMessage
		if err := json.Unmarshal(fields["top_logprobs"], &rawTop); err != nil {
			return fmt.Errorf("%w: OpenAI %s[%d].top_logprobs has invalid array JSON: %v", ErrInvalidModelOutput, label, index, err)
		}
		for topIndex, rawTopItem := range rawTop {
			topFields, err := decodeOpenAIRawObject(rawTopItem, fmt.Sprintf("%s[%d].top_logprobs[%d]", label, index, topIndex))
			if err != nil {
				return err
			}
			if !openAIRawHasJSONKind(topFields["token"], "string") {
				return fmt.Errorf("%w: OpenAI %s[%d].top_logprobs[%d].token must be a string", ErrInvalidModelOutput, label, index, topIndex)
			}
			if !openAIRawHasJSONKind(topFields["logprob"], "number") {
				return fmt.Errorf("%w: OpenAI %s[%d].top_logprobs[%d].logprob must be a number", ErrInvalidModelOutput, label, index, topIndex)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
