package agentruntime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// C. Response-level validators
// ---------------------------------------------------------------------------

func validateOpenAIImmutableResponseFieldTypes(eventType string, response responses.Response, fields map[string]json.RawMessage) error {
	return validateOpenAIResponseFields(eventType, response, fields)
}

func validateOpenAIResponseTerminalAuthority(eventType string, response responses.Response, fields map[string]json.RawMessage) error {
	return validateOpenAIResponseFields(eventType, response, fields)
}

func validateOpenAIResponseFields(eventType string, response responses.Response, fields map[string]json.RawMessage) error {
	label := fmt.Sprintf("response %q", response.ID)
	_ = eventType

	simpleKinds := map[string]string{
		"object":                 "string",
		"created_at":             "integer",
		"model":                  "string",
		"parallel_tool_calls":    "boolean",
		"temperature":            "number",
		"top_p":                  "number",
		"background":             "boolean",
		"max_output_tokens":      "integer",
		"max_tool_calls":         "integer",
		"prompt_cache_key":       "string",
		"prompt_cache_retention": "string",
		"safety_identifier":      "string",
		"service_tier":           "string",
		"top_logprobs":           "integer",
		"truncation":             "string",
		"user":                   "string",
		"completed_at":           "integer",
	}
	for _, name := range openAIImmutableResponseFields {
		if kind, ok := simpleKinds[name]; ok {
			if err := openAIRequireKind(fields, name, kind, label); err != nil {
				return err
			}
		}
		switch name {
		case "instructions":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if !openAIRawHasJSONKind(raw, "string-or-array") {
					return fmt.Errorf("%w: OpenAI %s.instructions must be a string or an array", ErrInvalidModelOutput, label)
				}
				if err := validateOpenAIResponseInstructions(eventType, raw, response.Instructions); err != nil {
					return err
				}
			}
		case "metadata":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponseMetadata(eventType, raw); err != nil {
					return err
				}
			}
		case "tool_choice":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if !openAIRawHasJSONKind(raw, "string-or-object") {
					return fmt.Errorf("%w: OpenAI %s.tool_choice must be a string or an object", ErrInvalidModelOutput, label)
				}
				if err := validateOpenAIResponseToolChoice(eventType, raw, response.ToolChoice); err != nil {
					return err
				}
			}
		case "tools":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponseTools(eventType, raw, response.Tools); err != nil {
					return err
				}
			}
		case "conversation":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponseConversation(eventType, raw, response.Conversation); err != nil {
					return err
				}
			}
		case "previous_response_id":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAICanonicalIdentity(raw, label+".previous_response_id"); err != nil {
					return err
				}
			}
		case "prompt":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponsePrompt(eventType, raw, response.Prompt); err != nil {
					return err
				}
			}
		case "reasoning":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponseReasoning(eventType, raw, response.Reasoning); err != nil {
					return err
				}
			}
		case "text":
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if err := validateOpenAIResponseText(eventType, raw, response.Text); err != nil {
					return err
				}
			}
		}
	}

	// Accounting fields that live outside openAIImmutableResponseFields.
	if raw, present := fields["error"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIResponseError(raw, label+".error"); err != nil {
			return err
		}
	}
	if raw, present := fields["incomplete_details"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIResponseIncompleteDetails(raw, label+".incomplete_details"); err != nil {
			return err
		}
	}
	if raw, present := fields["usage"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIResponseUsage(raw, label+".usage"); err != nil {
			return err
		}
	}
	if string(response.Status) == "failed" {
		if _, present := fields["error"]; !present || !openAINonNullRaw(fields["error"]) {
			return fmt.Errorf("%w: OpenAI response %q failed is missing its error", ErrInvalidModelOutput, response.ID)
		}
	}
	if string(response.Status) == "incomplete" {
		if _, present := fields["incomplete_details"]; !present || !openAINonNullRaw(fields["incomplete_details"]) {
			return fmt.Errorf("%w: OpenAI response %q incomplete is missing its incomplete_details", ErrInvalidModelOutput, response.ID)
		}
	}
	return nil
}

func validateOpenAIResponseError(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "code", "message") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	codeRaw, present := fields["code"]
	if !present || !openAINonNullRaw(codeRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its code", ErrInvalidModelOutput, label)
	}
	code, err := openAIRawString(codeRaw, label+".code")
	if err != nil {
		return err
	}
	if !validOpenAIResponseErrorCode(code) {
		return fmt.Errorf("%w: OpenAI %s.code %q is unsupported", ErrInvalidModelOutput, label, code)
	}
	messageRaw, present := fields["message"]
	if !present || !openAINonNullRaw(messageRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its message", ErrInvalidModelOutput, label)
	}
	message, err := openAIRawString(messageRaw, label+".message")
	if err != nil {
		return err
	}
	if message == "" {
		return fmt.Errorf("%w: OpenAI %s.message must not be empty", ErrInvalidModelOutput, label)
	}
	return nil
}

func validOpenAIResponseErrorCode(code string) bool {
	switch code {
	case "server_error", "rate_limit_exceeded", "invalid_prompt", "vector_store_timeout",
		"invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
		"image_too_large", "image_too_small", "image_parse_error",
		"image_content_policy_violation", "invalid_image_mode", "image_file_too_large",
		"unsupported_image_media_type", "empty_image_file", "failed_to_download_image",
		"image_file_not_found":
		return true
	default:
		return false
	}
}

func validateOpenAIResponseIncompleteDetails(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if name != "reason" {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	reasonRaw, present := fields["reason"]
	if !present || !openAINonNullRaw(reasonRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its reason", ErrInvalidModelOutput, label)
	}
	reason, err := openAIRawString(reasonRaw, label+".reason")
	if err != nil {
		return err
	}
	if !openAIStringInSet(reason, "max_output_tokens", "content_filter") {
		return fmt.Errorf("%w: OpenAI %s.reason %q is unsupported", ErrInvalidModelOutput, label, reason)
	}
	return nil
}

func validateOpenAIResponseUsage(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	var inputTokens, outputTokens, totalTokens int64
	if raw, present := fields["input_tokens"]; present && openAINonNullRaw(raw) {
		if inputTokens, err = openAIRawInt(raw, label+".input_tokens"); err != nil {
			return err
		}
		if inputTokens < 0 {
			return fmt.Errorf("%w: OpenAI %s.input_tokens must not be negative", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["output_tokens"]; present && openAINonNullRaw(raw) {
		if outputTokens, err = openAIRawInt(raw, label+".output_tokens"); err != nil {
			return err
		}
		if outputTokens < 0 {
			return fmt.Errorf("%w: OpenAI %s.output_tokens must not be negative", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["total_tokens"]; present && openAINonNullRaw(raw) {
		if totalTokens, err = openAIRawInt(raw, label+".total_tokens"); err != nil {
			return err
		}
		if totalTokens < 0 {
			return fmt.Errorf("%w: OpenAI %s.total_tokens must not be negative", ErrInvalidModelOutput, label)
		}
	}
	if _, inputPresent := fields["input_tokens"]; inputPresent {
		if _, outputPresent := fields["output_tokens"]; outputPresent {
			if _, totalPresent := fields["total_tokens"]; totalPresent {
				if totalTokens != inputTokens+outputTokens {
					return fmt.Errorf("%w: OpenAI %s.total_tokens does not equal input_tokens plus output_tokens", ErrInvalidModelOutput, label)
				}
			}
		}
	}
	if raw, present := fields["input_tokens_details"]; present && openAINonNullRaw(raw) {
		details, err := decodeOpenAIRawObject(raw, label+".input_tokens_details")
		if err != nil {
			return err
		}
		for name := range details {
			if name != "cached_tokens" {
				return fmt.Errorf("%w: OpenAI %s.input_tokens_details has unsupported field %s", ErrInvalidModelOutput, label, name)
			}
		}
		if cachedRaw, present := details["cached_tokens"]; present && openAINonNullRaw(cachedRaw) {
			cached, err := openAIRawInt(cachedRaw, label+".input_tokens_details.cached_tokens")
			if err != nil {
				return err
			}
			if cached < 0 {
				return fmt.Errorf("%w: OpenAI %s.input_tokens_details.cached_tokens must not be negative", ErrInvalidModelOutput, label)
			}
			if _, inputPresent := fields["input_tokens"]; inputPresent && cached > inputTokens {
				return fmt.Errorf("%w: OpenAI %s.input_tokens_details.cached_tokens exceeds input_tokens", ErrInvalidModelOutput, label)
			}
		}
	}
	if raw, present := fields["output_tokens_details"]; present && openAINonNullRaw(raw) {
		details, err := decodeOpenAIRawObject(raw, label+".output_tokens_details")
		if err != nil {
			return err
		}
		for name := range details {
			if name != "reasoning_tokens" {
				return fmt.Errorf("%w: OpenAI %s.output_tokens_details has unsupported field %s", ErrInvalidModelOutput, label, name)
			}
		}
		if reasoningRaw, present := details["reasoning_tokens"]; present && openAINonNullRaw(reasoningRaw) {
			if _, err := openAIRawInt(reasoningRaw, label+".output_tokens_details.reasoning_tokens"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIResponseScalarValues(eventType string, fields map[string]json.RawMessage, response responses.Response) error {
	label := fmt.Sprintf("response %q", response.ID)
	_ = eventType

	if raw, present := fields["object"]; present && openAINonNullRaw(raw) {
		object, err := openAIRawString(raw, label+".object")
		if err != nil {
			return err
		}
		if object != "response" {
			return fmt.Errorf("%w: OpenAI %s.object %q is unsupported", ErrInvalidModelOutput, label, object)
		}
	}
	if raw, present := fields["temperature"]; present && openAINonNullRaw(raw) {
		value, err := openAIRawNumber(raw, label+".temperature")
		if err != nil {
			return err
		}
		if value < 0 || value > 2 {
			return fmt.Errorf("%w: OpenAI %s.temperature %v is out of range [0,2]", ErrInvalidModelOutput, label, value)
		}
	}
	if raw, present := fields["top_p"]; present && openAINonNullRaw(raw) {
		value, err := openAIRawNumber(raw, label+".top_p")
		if err != nil {
			return err
		}
		if value < 0 || value > 1 {
			return fmt.Errorf("%w: OpenAI %s.top_p %v is out of range [0,1]", ErrInvalidModelOutput, label, value)
		}
	}
	if raw, present := fields["top_logprobs"]; present && openAINonNullRaw(raw) {
		value, err := openAIRawInt(raw, label+".top_logprobs")
		if err != nil {
			return err
		}
		if value < 0 || value > 20 {
			return fmt.Errorf("%w: OpenAI %s.top_logprobs %d is out of range [0,20]", ErrInvalidModelOutput, label, value)
		}
	}
	for _, name := range []string{"max_output_tokens", "max_tool_calls"} {
		if raw, present := fields[name]; present && openAINonNullRaw(raw) {
			value, err := openAIRawInt(raw, label+"."+name)
			if err != nil {
				return err
			}
			if value < 0 {
				return fmt.Errorf("%w: OpenAI %s.%s must not be negative", ErrInvalidModelOutput, label, name)
			}
		}
	}
	if raw, present := fields["previous_response_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".previous_response_id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "prompt_cache_retention", []string{"in-memory", "24h"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "service_tier", []string{"auto", "default", "flex", "scale", "priority"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "truncation", []string{"auto", "disabled"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIResponseMetadata(eventType string, raw json.RawMessage) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s metadata JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, eventType, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s metadata must be an object", ErrInvalidModelOutput, eventType)
	}
	for key, item := range object {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("%w: OpenAI %s metadata value %q must be a string", ErrInvalidModelOutput, eventType, key)
		}
	}
	return nil
}

func validateOpenAIResponseConversation(eventType string, raw json.RawMessage, conversation responses.ResponseConversation) error {
	_ = conversation
	fields, err := decodeOpenAIRawObject(raw, "response "+eventType+" conversation")
	if err != nil {
		return err
	}
	for name := range fields {
		if name != "id" {
			return fmt.Errorf("%w: OpenAI response %s conversation has unsupported field %s", ErrInvalidModelOutput, eventType, name)
		}
	}
	idRaw, present := fields["id"]
	if !present || !openAINonNullRaw(idRaw) {
		return fmt.Errorf("%w: OpenAI response %s conversation is missing its id", ErrInvalidModelOutput, eventType)
	}
	return validateOpenAICanonicalIdentity(idRaw, "response "+eventType+" conversation.id")
}

func validateOpenAIResponsePrompt(eventType string, raw json.RawMessage, prompt responses.ResponsePrompt) error {
	_ = prompt
	label := "response " + eventType + " prompt"
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "id", "variables", "version") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	idRaw, present := fields["id"]
	if !present || !openAINonNullRaw(idRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its id", ErrInvalidModelOutput, label)
	}
	if err := validateOpenAICanonicalIdentity(idRaw, label+".id"); err != nil {
		return err
	}
	if raw, present := fields["variables"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIResponsePromptVariables(eventType, raw); err != nil {
			return err
		}
	}
	if raw, present := fields["version"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.version must be a string", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validateOpenAIResponsePromptVariables(eventType string, raw json.RawMessage) error {
	label := "response " + eventType + " prompt.variables"
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name, variableRaw := range fields {
		if err := validateOpenAIPromptVariableObject(variableRaw, label+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIPromptVariableObject(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	switch value.(type) {
	case string:
		return nil
	case map[string]any:
	default:
		return fmt.Errorf("%w: OpenAI %s must be a string or an object", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	typeRaw, present := fields["type"]
	if !present || !openAINonNullRaw(typeRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its type", ErrInvalidModelOutput, label)
	}
	variableType, err := openAIRawString(typeRaw, label+".type")
	if err != nil {
		return err
	}
	switch variableType {
	case "input_text":
		if err := openAIRequireKnownFields(fields, label, "type", "text"); err != nil {
			return err
		}
		if !openAIRawHasJSONKind(fields["text"], "string") {
			return fmt.Errorf("%w: OpenAI %s.text must be a string", ErrInvalidModelOutput, label)
		}
	case "input_image":
		if err := openAIRequireKnownFields(fields, label, "type", "detail", "file_id", "image_url"); err != nil {
			return err
		}
		if err := openAIRequireDomain(fields, "detail", []string{"low", "high", "auto", "original"}, label); err != nil {
			return err
		}
		if raw, present := fields["file_id"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAICanonicalIdentity(raw, label+".file_id"); err != nil {
				return err
			}
		}
		if raw, present := fields["image_url"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s.image_url must be a string", ErrInvalidModelOutput, label)
			}
		}
	case "input_file":
		if err := openAIRequireKnownFields(fields, label, "type", "detail", "file_data", "file_id", "file_url", "filename"); err != nil {
			return err
		}
		if err := openAIRequireDomain(fields, "detail", []string{"low", "high"}, label); err != nil {
			return err
		}
		if raw, present := fields["file_data"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAIFileData(raw, label+".file_data"); err != nil {
				return err
			}
		}
		if raw, present := fields["file_id"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAICanonicalIdentity(raw, label+".file_id"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, variableType)
	}
	return nil
}

func validateOpenAIFileData(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "type", "file_id", "filename", "file_url", "detail") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	if raw, present := fields["file_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".file_id"); err != nil {
			return err
		}
	}
	return nil
}

func openAIRequireKnownFields(fields map[string]json.RawMessage, label string, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowedSet[name]; !ok {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	return nil
}

func validateOpenAIResponseReasoning(eventType string, raw json.RawMessage, reasoning shared.Reasoning) error {
	_ = reasoning
	label := "response " + eventType + " reasoning"
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "effort", "generate_summary", "summary") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	if err := openAIRequireDomain(fields, "effort", []string{"none", "minimal", "low", "medium", "high", "xhigh"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "summary", []string{"auto", "concise", "detailed"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "generate_summary", []string{"auto", "concise", "detailed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIResponseText(eventType string, raw json.RawMessage, text responses.ResponseTextConfig) error {
	_ = text
	label := "response " + eventType + " text"
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	for name := range fields {
		if !openAIStringInSet(name, "format", "verbosity") {
			return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
		}
	}
	if raw, present := fields["format"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIResponseFormat(raw, label+".format"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "verbosity", []string{"low", "medium", "high"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIResponseFormat(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	typeRaw, present := fields["type"]
	if !present || !openAINonNullRaw(typeRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its type", ErrInvalidModelOutput, label)
	}
	formatType, err := openAIRawString(typeRaw, label+".type")
	if err != nil {
		return err
	}
	switch formatType {
	case "text":
		if err := openAIRequireKnownFields(fields, label, "type"); err != nil {
			return err
		}
	case "json_schema":
		if err := openAIRequireKnownFields(fields, label, "type", "name", "schema", "description", "strict"); err != nil {
			return err
		}
		nameRaw, present := fields["name"]
		if !present || !openAINonNullRaw(nameRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its name", ErrInvalidModelOutput, label)
		}
		name, err := openAIRawString(nameRaw, label+".name")
		if err != nil {
			return err
		}
		if err := validateOpenAIJSONSchemaFormatName(name, label+".name"); err != nil {
			return err
		}
		schemaRaw, present := fields["schema"]
		if !present || !openAINonNullRaw(schemaRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its schema", ErrInvalidModelOutput, label)
		}
		if !openAIRawHasJSONKind(schemaRaw, "object") {
			return fmt.Errorf("%w: OpenAI %s.schema must be an object", ErrInvalidModelOutput, label)
		}
		if raw, present := fields["description"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s.description must be a string", ErrInvalidModelOutput, label)
			}
		}
		if raw, present := fields["strict"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "boolean") {
				return fmt.Errorf("%w: OpenAI %s.strict must be a boolean", ErrInvalidModelOutput, label)
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, formatType)
	}
	return nil
}

func validateOpenAIJSONSchemaFormatName(name, label string) error {
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("%w: OpenAI %s must be between 1 and 64 characters", ErrInvalidModelOutput, label)
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_':
		default:
			return fmt.Errorf("%w: OpenAI %s must contain only letters, digits, underscores, and hyphens", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// C2. Instructions
// ---------------------------------------------------------------------------

func validateOpenAIResponseInstructions(eventType string, raw json.RawMessage, instructions responses.ResponseInstructionsUnion) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s instructions JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, eventType, err)
	}
	switch value.(type) {
	case string:
		return nil
	case []any:
	default:
		return fmt.Errorf("%w: OpenAI %s instructions must be a string or an array", ErrInvalidModelOutput, eventType)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil {
		return fmt.Errorf("%w: OpenAI %s instructions has invalid array JSON: %v", ErrInvalidModelOutput, eventType, err)
	}
	decoded := instructions.AsInputItemList()
	for index, rawItem := range rawItems {
		label := fmt.Sprintf("response %s instructions[%d]", eventType, index)
		var item responses.ResponseInputItemUnion
		if index < len(decoded) {
			item = decoded[index]
		}
		if err := validateOpenAIInstructionItem(rawItem, label, item); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIInstructionItem(raw json.RawMessage, label string, item responses.ResponseInputItemUnion) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	instructionType := item.Type
	if instructionType == "" {
		instructionType, err = openAIStringField(fields, "type")
		if err != nil || instructionType == "" {
			return fmt.Errorf("%w: OpenAI %s is missing its type", ErrInvalidModelOutput, label)
		}
	}
	switch instructionType {
	case "message":
		return validateOpenAIInstructionMessage(raw, label)
	case "function_call":
		return validateOpenAIInstructionFunctionCall(fields, label)
	case "function_call_output":
		return validateOpenAIInstructionFunctionCallOutput(fields, label)
	case "file_search_call":
		return validateOpenAIInstructionFileSearchCall(fields, label)
	case "mcp_call":
		return validateOpenAIInstructionMcpCall(fields, label)
	case "code_interpreter_call":
		return validateOpenAIInstructionCodeInterpreterCall(fields, label)
	case "custom_tool_call":
		return validateOpenAIInstructionCustomToolCall(fields, label)
	case "image_generation_call":
		return validateOpenAIInstructionImageGenerationCall(fields, label)
	case "tool_search_call":
		return validateOpenAIInstructionToolSearchCall(fields, label)
	case "tool_search_output":
		return validateOpenAIInstructionToolSearchOutput(raw, fields, label)
	case "apply_patch_call":
		return validateOpenAIInstructionApplyPatchCall(fields, label)
	case "apply_patch_call_output":
		return validateOpenAIInstructionApplyPatchCallOutput(fields, label)
	case "compaction":
		return validateOpenAIInstructionCompaction(fields, label)
	default:
		// Fall back to the closed schema against the concrete variant, which
		// covers identity, unknown fields, and union recursion for the remaining
		// (untested) instruction item kinds.
		variant := item.AsAny()
		if variant == nil {
			return fmt.Errorf("%w: OpenAI %s has no supported schema", ErrInvalidModelOutput, label)
		}
		return validateOpenAIClosedSchema(raw, label, reflect.TypeOf(variant))
	}
}

func validateOpenAIInstructionMessage(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "role", "content", "status", "phase", "id"); err != nil {
		return err
	}
	role, err := openAIStringField(fields, "role")
	if err != nil {
		return err
	}
	if role == "" {
		return fmt.Errorf("%w: OpenAI %s is missing its role", ErrInvalidModelOutput, label)
	}
	switch role {
	case "user", "system", "developer":
		if phase, present := fields["phase"]; present && openAINonNullRaw(phase) {
			return fmt.Errorf("%w: OpenAI %s.phase is unsupported for role %q", ErrInvalidModelOutput, label, role)
		}
		if err := validateOpenAIInputMessageContent(fields["content"], label+".content"); err != nil {
			return err
		}
	case "assistant":
		// A completed (or otherwise statused) assistant output must carry an
		// id so it can be replayed; a bare assistant context message does not.
		if status, present := fields["status"]; present && openAINonNullRaw(status) {
			idRaw, present := fields["id"]
			if !present || !openAINonNullRaw(idRaw) {
				return fmt.Errorf("%w: OpenAI %s assistant output is missing its id", ErrInvalidModelOutput, label)
			}
			if err := validateOpenAICanonicalIdentity(idRaw, label+".id"); err != nil {
				return err
			}
		} else if idRaw, present := fields["id"]; present && openAINonNullRaw(idRaw) {
			if err := validateOpenAICanonicalIdentity(idRaw, label+".id"); err != nil {
				return err
			}
		}
		if err := openAIRequireDomain(fields, "phase", []string{"commentary", "final_answer"}, label); err != nil {
			return err
		}
		if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
			return err
		}
		if err := validateOpenAIOutputMessageContent(fields["content"], label+".content"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: OpenAI %s.role %q is unsupported", ErrInvalidModelOutput, label, role)
	}
	return nil
}

func validateOpenAIInputMessageContent(raw json.RawMessage, label string) error {
	if !openAINonNullRaw(raw) {
		return fmt.Errorf("%w: OpenAI %s is missing", ErrInvalidModelOutput, label)
	}
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s must be a string or an array", ErrInvalidModelOutput, label)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, rawItem := range rawItems {
		if err := validateOpenAIInputContentItem(rawItem, fmt.Sprintf("%s[%d]", label, index)); err != nil {
			return err
		}
	}
	_ = items
	return nil
}

func validateOpenAIOutputMessageContent(raw json.RawMessage, label string) error {
	if !openAINonNullRaw(raw) {
		return fmt.Errorf("%w: OpenAI %s is missing", ErrInvalidModelOutput, label)
	}
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s must be a string or an array", ErrInvalidModelOutput, label)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, rawItem := range rawItems {
		if err := validateOpenAIOutputContentItem(rawItem, fmt.Sprintf("%s[%d]", label, index)); err != nil {
			return err
		}
	}
	_ = items
	return nil
}

func validateOpenAIInputContentItem(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	itemType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	switch itemType {
	case "input_text":
		if err := openAIRequireKnownFields(fields, label, "type", "text"); err != nil {
			return err
		}
		if !openAIRawHasJSONKind(fields["text"], "string") {
			return fmt.Errorf("%w: OpenAI %s.text must be a string", ErrInvalidModelOutput, label)
		}
	case "input_image":
		if err := openAIRequireKnownFields(fields, label, "type", "detail", "file_id", "image_url"); err != nil {
			return err
		}
		if err := openAIRequireDomain(fields, "detail", []string{"low", "high", "auto", "original"}, label); err != nil {
			return err
		}
		if raw, present := fields["file_id"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAICanonicalIdentity(raw, label+".file_id"); err != nil {
				return err
			}
		}
		if raw, present := fields["image_url"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s.image_url must be a string", ErrInvalidModelOutput, label)
			}
		}
	case "input_file":
		if err := openAIRequireKnownFields(fields, label, "type", "detail", "file_data", "file_id", "file_url", "filename"); err != nil {
			return err
		}
		if raw, present := fields["file_id"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAICanonicalIdentity(raw, label+".file_id"); err != nil {
				return err
			}
		}
		if raw, present := fields["file_data"]; present && openAINonNullRaw(raw) {
			if err := validateOpenAIFileData(raw, label+".file_data"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s has unsupported input content type %q", ErrInvalidModelOutput, label, itemType)
	}
	return nil
}

func validateOpenAIOutputContentItem(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	itemType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	switch itemType {
	case "output_text":
		if err := openAIRequireKnownFields(fields, label, "type", "text", "annotations", "logprobs"); err != nil {
			return err
		}
		if !openAIRawHasJSONKind(fields["text"], "string") {
			return fmt.Errorf("%w: OpenAI %s.text must be a string", ErrInvalidModelOutput, label)
		}
		if err := validateOpenAIOutputTextAnnotations(fields["annotations"], label+".annotations"); err != nil {
			return err
		}
		if raw, present := fields["logprobs"]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "array") {
				return fmt.Errorf("%w: OpenAI %s.logprobs must be an array", ErrInvalidModelOutput, label)
			}
			if err := validateOpenAIRawLogprobs(raw, label+".logprobs", true); err != nil {
				return err
			}
		}
	case "refusal":
		if err := openAIRequireKnownFields(fields, label, "type", "refusal"); err != nil {
			return err
		}
		if !openAIRawHasJSONKind(fields["refusal"], "string") {
			return fmt.Errorf("%w: OpenAI %s.refusal must be a string", ErrInvalidModelOutput, label)
		}
	default:
		return fmt.Errorf("%w: OpenAI %s has unsupported output content type %q", ErrInvalidModelOutput, label, itemType)
	}
	return nil
}

func validateOpenAIOutputTextAnnotations(raw json.RawMessage, label string) error {
	if !openAINonNullRaw(raw) {
		return fmt.Errorf("%w: OpenAI %s is missing", ErrInvalidModelOutput, label)
	}
	if !openAIRawHasJSONKind(raw, "array") {
		return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, rawItem := range rawItems {
		var annotation responses.ResponseOutputTextAnnotationUnion
		if err := json.Unmarshal(rawItem, &annotation); err != nil {
			return fmt.Errorf("%w: OpenAI %s[%d] is invalid: %v", ErrInvalidModelOutput, label, index, err)
		}
		if err := validateOpenAIAnnotation(annotation, fmt.Sprintf("%s[%d]", label, index)); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIInstructionFunctionCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "call_id", "name", "arguments", "status", "id"); err != nil {
		return err
	}
	for _, name := range []string{"call_id", "name"} {
		raw, present := fields[name]
		if !present || !openAINonNullRaw(raw) {
			return fmt.Errorf("%w: OpenAI %s is missing its %s", ErrInvalidModelOutput, label, name)
		}
		if err := validateOpenAICanonicalIdentity(raw, label+"."+name); err != nil {
			return err
		}
	}
	if raw, present := fields["arguments"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIExactJSONString(raw, label+".arguments"); err != nil {
			return err
		}
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionFunctionCallOutput(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "call_id", "output", "id", "status"); err != nil {
		return err
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionFileSearchCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "id", "queries", "status", "results"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["queries"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIStringArray(raw, label+".queries"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "searching", "completed", "incomplete", "failed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionMcpCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "id", "arguments", "name", "server_label", "approval_request_id", "error", "output", "status"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["server_label"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".server_label"); err != nil {
			return err
		}
	}
	if raw, present := fields["name"]; present && openAINonNullRaw(raw) {
		name, err := openAIRawString(raw, label+".name")
		if err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["arguments"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIExactJSONString(raw, label+".arguments"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete", "calling", "failed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionCodeInterpreterCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "id", "code", "container_id", "outputs", "status"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete", "interpreting", "failed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionCustomToolCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "call_id", "name", "input", "id"); err != nil {
		return err
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	if raw, present := fields["name"]; present && openAINonNullRaw(raw) {
		name, err := openAIRawString(raw, label+".name")
		if err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["input"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.input must be a string", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validateOpenAIInstructionImageGenerationCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "id", "result", "status"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "generating", "failed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionToolSearchCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "arguments", "id", "call_id", "execution", "status"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "execution", []string{"server", "client"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIInstructionToolSearchOutput(raw json.RawMessage, fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "tools", "id", "call_id", "execution", "status"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "execution", []string{"server", "client"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed", "incomplete"}, label); err != nil {
		return err
	}
	if toolsRaw, present := fields["tools"]; present && openAINonNullRaw(toolsRaw) {
		if err := validateOpenAIToolsRaw(toolsRaw, label+".tools"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIInstructionApplyPatchCall(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "call_id", "operation", "status", "id"); err != nil {
		return err
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"in_progress", "completed"}, label); err != nil {
		return err
	}
	if raw, present := fields["operation"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIApplyPatchOperation(raw, label+".operation"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIInstructionApplyPatchCallOutput(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "call_id", "status", "id", "output"); err != nil {
		return err
	}
	if raw, present := fields["call_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".call_id"); err != nil {
			return err
		}
	}
	if err := openAIRequireDomain(fields, "status", []string{"completed", "failed"}, label); err != nil {
		return err
	}
	return nil
}

func validateOpenAIInstructionCompaction(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireKnownFields(fields, label, "type", "encrypted_content", "id"); err != nil {
		return err
	}
	if raw, present := fields["id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".id"); err != nil {
			return err
		}
	}
	if raw, present := fields["encrypted_content"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.encrypted_content must be a string", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validateOpenAIApplyPatchOperation(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "diff", "path"); err != nil {
		return err
	}
	operationType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	switch operationType {
	case "create_file", "delete_file", "update_file":
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, operationType)
	}
	return nil
}

// ---------------------------------------------------------------------------
// C3. Tool choice
// ---------------------------------------------------------------------------

func validateOpenAIResponseToolChoice(eventType string, raw json.RawMessage, choice responses.ResponseToolChoiceUnion) error {
	_ = choice
	label := "response " + eventType + " tool_choice"
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	switch value := value.(type) {
	case string:
		if !openAIStringInSet(value, "auto", "required", "none") {
			return fmt.Errorf("%w: OpenAI %s mode %q is unsupported", ErrInvalidModelOutput, label, value)
		}
		return nil
	case map[string]any:
	default:
		return fmt.Errorf("%w: OpenAI %s must be a string or an object", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	choiceType, err := openAIStringField(fields, "type")
	if err != nil || choiceType == "" {
		return fmt.Errorf("%w: OpenAI %s is missing its type", ErrInvalidModelOutput, label)
	}
	if err := validateOpenAIToolChoiceVariantFields(eventType, choiceType, fields); err != nil {
		return err
	}
	switch choiceType {
	case "function", "custom":
		nameRaw, present := fields["name"]
		if !present || !openAINonNullRaw(nameRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its name", ErrInvalidModelOutput, label)
		}
		if err := validateOpenAICanonicalIdentity(nameRaw, label+".name"); err != nil {
			return err
		}
	case "mcp":
		serverRaw, present := fields["server_label"]
		if !present || !openAINonNullRaw(serverRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its server_label", ErrInvalidModelOutput, label)
		}
		if err := validateOpenAICanonicalIdentity(serverRaw, label+".server_label"); err != nil {
			return err
		}
		if nameRaw, present := fields["name"]; present && openAINonNullRaw(nameRaw) {
			name, err := openAIRawString(nameRaw, label+".name")
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
			}
		}
	case "allowed_tools":
		modeRaw, present := fields["mode"]
		if !present || !openAINonNullRaw(modeRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its mode", ErrInvalidModelOutput, label)
		}
		mode, err := openAIRawString(modeRaw, label+".mode")
		if err != nil {
			return err
		}
		if !openAIStringInSet(mode, "auto", "required") {
			return fmt.Errorf("%w: OpenAI %s.mode %q is unsupported", ErrInvalidModelOutput, label, mode)
		}
		toolsRaw, present := fields["tools"]
		if !present || !openAINonNullRaw(toolsRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its tools", ErrInvalidModelOutput, label)
		}
		if !openAIRawHasJSONKind(toolsRaw, "array") {
			return fmt.Errorf("%w: OpenAI %s.tools must be an array", ErrInvalidModelOutput, label)
		}
		var rawSelectors []json.RawMessage
		if err := json.Unmarshal(toolsRaw, &rawSelectors); err != nil {
			return fmt.Errorf("%w: OpenAI %s.tools has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, rawSelector := range rawSelectors {
			if err := validateOpenAIAllowedToolSelector(rawSelector, fmt.Sprintf("%s.tools[%d]", label, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIToolChoiceVariantFields(eventType string, choiceType string, fields map[string]json.RawMessage) error {
	label := "response " + eventType + " tool_choice"
	var allowed []string
	switch choiceType {
	case "function", "custom":
		allowed = []string{"type", "name"}
	case "mcp":
		allowed = []string{"type", "server_label", "name"}
	case "allowed_tools":
		allowed = []string{"type", "mode", "tools"}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, choiceType)
	}
	return openAIRequireKnownFields(fields, label, allowed...)
}

func validateOpenAIAllowedToolSelector(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	selectorType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	switch selectorType {
	case "function", "custom":
		if err := openAIRequireKnownFields(fields, label, "type", "name"); err != nil {
			return err
		}
		nameRaw, present := fields["name"]
		if !present || !openAINonNullRaw(nameRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its name", ErrInvalidModelOutput, label)
		}
		if err := validateOpenAICanonicalIdentity(nameRaw, label+".name"); err != nil {
			return err
		}
	case "mcp":
		if err := openAIRequireKnownFields(fields, label, "type", "server_label", "name"); err != nil {
			return err
		}
		serverRaw, present := fields["server_label"]
		if !present || !openAINonNullRaw(serverRaw) {
			return fmt.Errorf("%w: OpenAI %s is missing its server_label", ErrInvalidModelOutput, label)
		}
		if err := validateOpenAICanonicalIdentity(serverRaw, label+".server_label"); err != nil {
			return err
		}
		if nameRaw, present := fields["name"]; present && openAINonNullRaw(nameRaw) {
			name, err := openAIRawString(nameRaw, label+".name")
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, selectorType)
	}
	return nil
}

// ---------------------------------------------------------------------------
// C4. Tools
// ---------------------------------------------------------------------------

func validateOpenAIResponseTools(eventType string, raw json.RawMessage, tools []responses.ToolUnion) error {
	_ = tools
	return validateOpenAIToolsRaw(raw, "response "+eventType+" tools")
}

func validateOpenAIToolsRaw(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.([]any); !ok {
		return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
	}
	var rawTools []json.RawMessage
	if err := json.Unmarshal(raw, &rawTools); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, rawTool := range rawTools {
		if err := validateOpenAITool(rawTool, fmt.Sprintf("%s[%d]", label, index), index); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAITool(raw json.RawMessage, label string, index int) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	toolType, err := openAIStringField(fields, "type")
	if err != nil || toolType == "" {
		return fmt.Errorf("%w: OpenAI %s is missing its type", ErrInvalidModelOutput, label)
	}
	var tool responses.ToolUnion
	_ = json.Unmarshal(raw, &tool)
	if err := validateOpenAIToolVariantFields("", index, toolType, fields); err != nil {
		return err
	}
	if err := validateOpenAIToolDomains("", index, tool, fields); err != nil {
		return err
	}
	return validateOpenAIToolSemantics(toolType, tool, raw, fields, label)
}

func validateOpenAIToolVariantFields(eventType string, index int, toolType string, fields map[string]json.RawMessage) error {
	label := fmt.Sprintf("tools[%d]", index)
	var allowed []string
	switch toolType {
	case "function":
		allowed = []string{"type", "name", "parameters", "strict", "defer_loading", "description"}
	case "file_search":
		allowed = []string{"type", "vector_store_ids", "filters", "max_num_results", "ranking_options"}
	case "mcp":
		allowed = []string{"type", "server_label", "allowed_tools", "authorization", "connector_id", "defer_loading", "headers", "require_approval", "server_description", "server_url"}
	case "code_interpreter":
		allowed = []string{"type", "container"}
	case "computer_use_preview":
		allowed = []string{"type", "display_height", "display_width", "environment"}
	case "web_search_preview":
		allowed = []string{"type", "search_content_types", "search_context_size", "user_location"}
	case "web_search":
		allowed = []string{"type", "filters", "search_context_size", "user_location"}
	case "custom":
		allowed = []string{"type", "name", "defer_loading", "description", "format"}
	case "namespace":
		allowed = []string{"type", "description", "name", "tools"}
	case "shell":
		allowed = []string{"type", "environment"}
	case "local_shell":
		allowed = []string{"type"}
	case "image_generation":
		allowed = []string{"type", "action", "background", "input_fidelity", "input_image_mask", "model", "moderation", "output_compression", "output_format", "partial_images", "quality", "size"}
	case "tool_search":
		allowed = []string{"type", "description", "execution", "parameters"}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, toolType)
	}
	return openAIRequireKnownFields(fields, label, allowed...)
}

func validateOpenAIToolDomains(eventType string, index int, tool responses.ToolUnion, fields map[string]json.RawMessage) error {
	label := fmt.Sprintf("tools[%d]", index)
	toolType, _ := openAIStringField(fields, "type")
	switch toolType {
	case "computer_use_preview":
		return openAIRequireDomain(fields, "environment", openAIStringTypeDomains["ComputerUsePreviewToolEnvironment"], label)
	case "web_search_preview":
		return openAIRequireDomain(fields, "search_context_size", openAIStringTypeDomains["WebSearchPreviewToolSearchContextSize"], label)
	case "web_search":
		return openAIRequireDomain(fields, "search_context_size", openAIStringTypeDomains["WebSearchToolSearchContextSize"], label)
	case "tool_search":
		return openAIRequireDomain(fields, "execution", openAIStringTypeDomains["ToolSearchToolExecution"], label)
	}
	return nil
}

func validateOpenAIToolSemantics(toolType string, tool responses.ToolUnion, raw json.RawMessage, fields map[string]json.RawMessage, label string) error {
	_ = tool
	switch toolType {
	case "function":
		return validateOpenAIFunctionTool(fields, label)
	case "file_search":
		return validateOpenAIFileSearchTool(fields, label)
	case "mcp":
		return validateOpenAIMCPTool(fields, label)
	case "code_interpreter":
		return validateOpenAICodeInterpreterTool(fields, label)
	case "computer_use_preview":
		return validateOpenAIComputerUsePreviewTool(fields, label)
	case "web_search_preview", "web_search":
		return validateOpenAIWebSearchTool(fields, label)
	case "custom":
		return validateOpenAICustomTool(fields, label)
	case "namespace":
		return validateOpenAINamespaceTool(raw, label)
	case "shell":
		return validateOpenAIShellTool(fields, label)
	case "local_shell":
		return nil
	case "image_generation":
		return validateOpenAIImageGenerationTool(fields, label)
	case "tool_search":
		return nil
	}
	return nil
}

func validateOpenAIComputerUsePreviewTool(fields map[string]json.RawMessage, label string) error {
	for _, name := range []string{"display_height", "display_width"} {
		if raw, present := fields[name]; present && openAINonNullRaw(raw) {
			value, err := openAIRawInt(raw, label+"."+name)
			if err != nil {
				return err
			}
			if value <= 0 {
				return fmt.Errorf("%w: OpenAI %s.%s must be positive", ErrInvalidModelOutput, label, name)
			}
		}
	}
	return nil
}

func validateOpenAIFunctionTool(fields map[string]json.RawMessage, label string) error {
	nameRaw, present := fields["name"]
	if !present || !openAINonNullRaw(nameRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its name", ErrInvalidModelOutput, label)
	}
	name, err := openAIRawString(nameRaw, label+".name")
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
	}
	for _, name := range []string{"parameters", "strict"} {
		if _, present := fields[name]; !present {
			return fmt.Errorf("%w: OpenAI %s is missing its %s", ErrInvalidModelOutput, label, name)
		}
	}
	if raw, present := fields["parameters"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "object") {
			return fmt.Errorf("%w: OpenAI %s.parameters must be an object", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["strict"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.strict must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["description"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.description must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["defer_loading"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.defer_loading must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validateOpenAIFileSearchTool(fields map[string]json.RawMessage, label string) error {
	storesRaw, present := fields["vector_store_ids"]
	if !present || !openAINonNullRaw(storesRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its vector_store_ids", ErrInvalidModelOutput, label)
	}
	if err := validateOpenAICanonicalStringArray(storesRaw, label+".vector_store_ids", true); err != nil {
		return err
	}
	if raw, present := fields["max_num_results"]; present && openAINonNullRaw(raw) {
		value, err := openAIRawInt(raw, label+".max_num_results")
		if err != nil {
			return err
		}
		if value < 1 || value > 50 {
			return fmt.Errorf("%w: OpenAI %s.max_num_results %d is out of range [1,50]", ErrInvalidModelOutput, label, value)
		}
	}
	if raw, present := fields["ranking_options"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIFileSearchRankingOptions(raw, label+".ranking_options"); err != nil {
			return err
		}
	}
	if raw, present := fields["filters"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIFileSearchFilter(raw, label+".filters"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIFileSearchRankingOptions(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "hybrid_search", "ranker", "score_threshold"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "ranker", []string{"auto", "default_2024_08_21"}, label); err != nil {
		return err
	}
	if raw, present := fields["score_threshold"]; present && openAINonNullRaw(raw) {
		if _, err := openAIRawNumber(raw, label+".score_threshold"); err != nil {
			return err
		}
	}
	if raw, present := fields["hybrid_search"]; present && openAINonNullRaw(raw) {
		hybrid, err := decodeOpenAIRawObject(raw, label+".hybrid_search")
		if err != nil {
			return err
		}
		if err := openAIRequireKnownFields(hybrid, label+".hybrid_search", "embedding_weight", "text_weight"); err != nil {
			return err
		}
		for _, name := range []string{"embedding_weight", "text_weight"} {
			if raw, present := hybrid[name]; present && openAINonNullRaw(raw) {
				if _, err := openAIRawNumber(raw, label+".hybrid_search."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateOpenAIFileSearchFilter(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "key", "type", "value", "filters"); err != nil {
		return err
	}
	filterType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	if !openAIStringInSet(filterType, "eq", "ne", "and", "or") {
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, filterType)
	}
	if raw, present := fields["key"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.key must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["value"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string-or-array") {
			return fmt.Errorf("%w: OpenAI %s.value must be a string or an array", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["filters"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "array") {
			return fmt.Errorf("%w: OpenAI %s.filters must be an array", ErrInvalidModelOutput, label)
		}
		var rawFilters []json.RawMessage
		if err := json.Unmarshal(raw, &rawFilters); err != nil {
			return fmt.Errorf("%w: OpenAI %s.filters has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, rawFilter := range rawFilters {
			if err := validateOpenAIFileSearchFilter(rawFilter, fmt.Sprintf("%s.filters[%d]", label, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIMCPTool(fields map[string]json.RawMessage, label string) error {
	if raw, present := fields["server_label"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".server_label"); err != nil {
			return err
		}
	}
	if _, present := fields["authorization"]; present && openAINonNullRaw(fields["authorization"]) {
		return fmt.Errorf("%w: OpenAI %s.authorization is unsupported because the runtime cannot preserve authorization authority", ErrInvalidModelOutput, label)
	}
	serverURL, hasURL := fields["server_url"]
	connectorID, hasConnector := fields["connector_id"]
	hasURL = hasURL && openAINonNullRaw(serverURL)
	hasConnector = hasConnector && openAINonNullRaw(connectorID)
	if hasURL && hasConnector {
		return fmt.Errorf("%w: OpenAI %s cannot specify both server_url and connector_id", ErrInvalidModelOutput, label)
	}
	if !hasURL && !hasConnector {
		return fmt.Errorf("%w: OpenAI %s must specify either server_url or connector_id", ErrInvalidModelOutput, label)
	}
	if hasURL {
		urlValue, err := openAIRawString(serverURL, label+".server_url")
		if err != nil {
			return err
		}
		if err := validateOpenAIMCPServerURL(urlValue, label+".server_url"); err != nil {
			return err
		}
	}
	if hasConnector {
		connector, err := openAIRawString(connectorID, label+".connector_id")
		if err != nil {
			return err
		}
		if !validOpenAIConnectorID(connector) {
			return fmt.Errorf("%w: OpenAI %s.connector_id %q is unsupported", ErrInvalidModelOutput, label, connector)
		}
	}
	if raw, present := fields["headers"]; present && openAINonNullRaw(raw) {
		value, err := decodeExactJSON(raw)
		if err != nil {
			return fmt.Errorf("%w: OpenAI %s.headers JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
		}
		headers, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: OpenAI %s.headers must be an object", ErrInvalidModelOutput, label)
		}
		for key, item := range headers {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("%w: OpenAI %s.headers value %q must be a string", ErrInvalidModelOutput, label, key)
			}
		}
	}
	if raw, present := fields["allowed_tools"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIMCPToolFilter(raw, label+".allowed_tools"); err != nil {
			return err
		}
	}
	if raw, present := fields["require_approval"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIMCPApproval(raw, label+".require_approval"); err != nil {
			return err
		}
	}
	if raw, present := fields["server_description"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.server_description must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["defer_loading"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.defer_loading must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validOpenAIConnectorID(connector string) bool {
	switch connector {
	case "connector_dropbox", "connector_gmail", "connector_googlecalendar",
		"connector_googledrive", "connector_microsoftteams", "connector_outlookcalendar",
		"connector_outlookemail", "connector_sharepoint":
		return true
	default:
		return false
	}
}

func validateOpenAIMCPServerURL(value, label string) error {
	if value == "" {
		return fmt.Errorf("%w: OpenAI %s must not be empty", ErrInvalidModelOutput, label)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s is not a valid URL: %v", ErrInvalidModelOutput, label, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("%w: OpenAI %s must use https or http", ErrInvalidModelOutput, label)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: OpenAI %s must include a host", ErrInvalidModelOutput, label)
	}
	if parsed.Host != strings.ToLower(parsed.Host) {
		return fmt.Errorf("%w: OpenAI %s host must be lowercase", ErrInvalidModelOutput, label)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: OpenAI %s must not contain userinfo", ErrInvalidModelOutput, label)
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("%w: OpenAI %s port %q is out of range", ErrInvalidModelOutput, label, port)
		}
		if (parsed.Scheme == "https" && number == 443) || (parsed.Scheme == "http" && number == 80) {
			return fmt.Errorf("%w: OpenAI %s must not specify the default port", ErrInvalidModelOutput, label)
		}
	}
	if parsed.ForceQuery && parsed.RawQuery == "" {
		return fmt.Errorf("%w: OpenAI %s must not end with an empty query", ErrInvalidModelOutput, label)
	}
	if openAIURLHasEscapedUnreserved(value) {
		return fmt.Errorf("%w: OpenAI %s contains percent-escaped unreserved or separator characters", ErrInvalidModelOutput, label)
	}
	return nil
}

func openAIURLHasEscapedUnreserved(value string) bool {
	for index := 0; index+2 < len(value); index++ {
		if value[index] != '%' {
			continue
		}
		high, highOK := openAIHexDigit(value[index+1])
		low, lowOK := openAIHexDigit(value[index+2])
		if !highOK || !lowOK {
			continue
		}
		decoded := high<<4 | low
		switch {
		case decoded >= 'a' && decoded <= 'z':
			return true
		case decoded >= 'A' && decoded <= 'Z':
			return true
		case decoded >= '0' && decoded <= '9':
			return true
		case decoded == '-', decoded == '.', decoded == '_', decoded == '~':
			return true
		case decoded == '/':
			return true
		}
	}
	return false
}

func openAIHexDigit(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}

func validateOpenAIMCPToolFilter(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.([]any); ok {
		return validateOpenAICanonicalStringArray(raw, label, true)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "read_only", "tool_names"); err != nil {
		return err
	}
	if raw, present := fields["read_only"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.read_only must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["tool_names"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalStringArray(raw, label+".tool_names", false); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIMCPApprovalFilter(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "read_only", "tool_names"); err != nil {
		return err
	}
	if raw, present := fields["read_only"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.read_only must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["tool_names"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalStringArray(raw, label+".tool_names", false); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIMCPApproval(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.(string); ok {
		var setting string
		if err := json.Unmarshal(raw, &setting); err == nil {
			if openAIStringInSet(setting, "always", "never") {
				return nil
			}
		}
		return fmt.Errorf("%w: OpenAI %s must be a supported approval setting", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "always", "never"); err != nil {
		return err
	}
	if raw, present := fields["always"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIMCPApprovalFilter(raw, label+".always"); err != nil {
			return err
		}
	}
	if raw, present := fields["never"]; present && openAINonNullRaw(raw) {
		neverFields, err := decodeOpenAIRawObject(raw, label+".never")
		if err != nil {
			return err
		}
		if len(neverFields) != 0 {
			return fmt.Errorf("%w: OpenAI %s.never must be empty", ErrInvalidModelOutput, label)
		}
	}
	return nil
}

func validateOpenAICodeInterpreterTool(fields map[string]json.RawMessage, label string) error {
	containerRaw, present := fields["container"]
	if !present || !openAINonNullRaw(containerRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its container", ErrInvalidModelOutput, label)
	}
	return validateOpenAICodeInterpreterContainer(containerRaw, label+".container")
}

func validateOpenAICodeInterpreterContainer(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.(string); ok {
		return validateOpenAICanonicalIdentity(raw, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "file_ids", "memory_limit", "network_policy"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "type", []string{"auto"}, label); err != nil {
		return err
	}
	if raw, present := fields["file_ids"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalStringArray(raw, label+".file_ids", false); err != nil {
			return err
		}
	}
	if raw, present := fields["memory_limit"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.memory_limit must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["network_policy"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAINetworkPolicy(raw, label+".network_policy"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAINetworkPolicy(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "allowed_domains", "domain_secrets"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "type", []string{"allowlist"}, label); err != nil {
		return err
	}
	allowed := make(map[string]struct{})
	if raw, present := fields["allowed_domains"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalDomains(raw, label+".allowed_domains"); err != nil {
			return err
		}
		domains, err := openAIStringArrayValues(raw)
		if err != nil {
			return err
		}
		for _, domain := range domains {
			allowed[domain] = struct{}{}
		}
	}
	if raw, present := fields["domain_secrets"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "array") {
			return fmt.Errorf("%w: OpenAI %s.domain_secrets must be an array", ErrInvalidModelOutput, label)
		}
		var rawSecrets []json.RawMessage
		if err := json.Unmarshal(raw, &rawSecrets); err != nil {
			return fmt.Errorf("%w: OpenAI %s.domain_secrets has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		seen := make(map[string]struct{}, len(rawSecrets))
		for index, rawSecret := range rawSecrets {
			secretLabel := fmt.Sprintf("%s.domain_secrets[%d]", label, index)
			secretFields, err := decodeOpenAIRawObject(rawSecret, secretLabel)
			if err != nil {
				return err
			}
			if err := openAIRequireKnownFields(secretFields, secretLabel, "domain", "name", "value"); err != nil {
				return err
			}
			domain, err := openAIStringField(secretFields, "domain")
			if err != nil {
				return err
			}
			if err := validateOpenAICanonicalDomain(domain, secretLabel+".domain"); err != nil {
				return err
			}
			if _, ok := allowed[domain]; !ok {
				return fmt.Errorf("%w: OpenAI %s.domain %q is not in the allowed domain set", ErrInvalidModelOutput, secretLabel, domain)
			}
			name, err := openAIStringField(secretFields, "name")
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, secretLabel)
			}
			if _, present := secretFields["value"]; !present || !openAINonNullRaw(secretFields["value"]) {
				return fmt.Errorf("%w: OpenAI %s is missing its value", ErrInvalidModelOutput, secretLabel)
			}
			if !openAIRawHasJSONKind(secretFields["value"], "string") {
				return fmt.Errorf("%w: OpenAI %s.value must be a string", ErrInvalidModelOutput, secretLabel)
			}
			key := domain + "\x00" + name
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: OpenAI %s repeats domain %q name %q", ErrInvalidModelOutput, secretLabel, domain, name)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func openAIStringArrayValues(raw json.RawMessage) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func validateOpenAICanonicalDomains(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
	}
	for index, item := range items {
		domain, ok := item.(string)
		if !ok {
			return fmt.Errorf("%w: OpenAI %s[%d] must be a string", ErrInvalidModelOutput, label, index)
		}
		if err := validateOpenAICanonicalDomain(domain, fmt.Sprintf("%s[%d]", label, index)); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAICanonicalDomain(domain, label string) error {
	if domain == "" {
		return fmt.Errorf("%w: OpenAI %s must not be empty", ErrInvalidModelOutput, label)
	}
	if domain != strings.TrimSpace(domain) {
		return fmt.Errorf("%w: OpenAI %s must be canonical without surrounding whitespace", ErrInvalidModelOutput, label)
	}
	if domain != strings.ToLower(domain) {
		return fmt.Errorf("%w: OpenAI %s must be lowercase", ErrInvalidModelOutput, label)
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return fmt.Errorf("%w: OpenAI %s must not start or end with a dot", ErrInvalidModelOutput, label)
	}
	if strings.Contains(domain, "*") {
		return fmt.Errorf("%w: OpenAI %s must not contain a wildcard", ErrInvalidModelOutput, label)
	}
	return nil
}

func validateOpenAICustomTool(fields map[string]json.RawMessage, label string) error {
	nameRaw, present := fields["name"]
	if !present || !openAINonNullRaw(nameRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its name", ErrInvalidModelOutput, label)
	}
	name, err := openAIRawString(nameRaw, label+".name")
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
	}
	if raw, present := fields["description"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.description must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["defer_loading"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "boolean") {
			return fmt.Errorf("%w: OpenAI %s.defer_loading must be a boolean", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["format"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICustomToolFormat(raw, label+".format"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAICustomToolFormat(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("%w: OpenAI %s must be an object", ErrInvalidModelOutput, label)
	}
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	formatType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	switch formatType {
	case "text":
		if err := openAIRequireKnownFields(fields, label, "type"); err != nil {
			return err
		}
	case "grammar":
		if err := openAIRequireKnownFields(fields, label, "type", "definition", "syntax"); err != nil {
			return err
		}
		for _, name := range []string{"definition", "syntax"} {
			if raw, present := fields[name]; present && openAINonNullRaw(raw) {
				if !openAIRawHasJSONKind(raw, "string") {
					return fmt.Errorf("%w: OpenAI %s.%s must be a string", ErrInvalidModelOutput, label, name)
				}
			}
		}
	default:
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, formatType)
	}
	return nil
}

func validateOpenAINamespaceTool(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "description", "name", "tools"); err != nil {
		return err
	}
	if raw, present := fields["name"]; present && openAINonNullRaw(raw) {
		name, err := openAIRawString(raw, label+".name")
		if err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("%w: OpenAI %s.name must not be empty", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["description"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "string") {
			return fmt.Errorf("%w: OpenAI %s.description must be a string", ErrInvalidModelOutput, label)
		}
	}
	if raw, present := fields["tools"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAINamespaceTools(raw, label+".tools"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAINamespaceTools(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	if _, ok := value.([]any); !ok {
		return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
	}
	var rawTools []json.RawMessage
	if err := json.Unmarshal(raw, &rawTools); err != nil {
		return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
	}
	for index, rawTool := range rawTools {
		fields, err := decodeOpenAIRawObject(rawTool, fmt.Sprintf("%s[%d]", label, index))
		if err != nil {
			return err
		}
		toolType, err := openAIStringField(fields, "type")
		if err != nil {
			return err
		}
		switch toolType {
		case "function":
			if err := openAIRequireKnownFields(fields, label, "type", "name", "parameters", "strict", "defer_loading", "description"); err != nil {
				return err
			}
			nameRaw, present := fields["name"]
			if !present || !openAINonNullRaw(nameRaw) {
				return fmt.Errorf("%w: OpenAI %s[%d] is missing its name", ErrInvalidModelOutput, label, index)
			}
			name, err := openAIRawString(nameRaw, fmt.Sprintf("%s[%d].name", label, index))
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("%w: OpenAI %s[%d].name must not be empty", ErrInvalidModelOutput, label, index)
			}
		case "custom":
			if err := openAIRequireKnownFields(fields, label, "type", "name", "defer_loading", "description", "format"); err != nil {
				return err
			}
			nameRaw, present := fields["name"]
			if !present || !openAINonNullRaw(nameRaw) {
				return fmt.Errorf("%w: OpenAI %s[%d] is missing its name", ErrInvalidModelOutput, label, index)
			}
			name, err := openAIRawString(nameRaw, fmt.Sprintf("%s[%d].name", label, index))
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("%w: OpenAI %s[%d].name must not be empty", ErrInvalidModelOutput, label, index)
			}
		default:
			return fmt.Errorf("%w: OpenAI %s[%d].type %q is unsupported", ErrInvalidModelOutput, label, index, toolType)
		}
	}
	return nil
}

func validateOpenAIShellTool(fields map[string]json.RawMessage, label string) error {
	environmentRaw, present := fields["environment"]
	if !present || !openAINonNullRaw(environmentRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its environment", ErrInvalidModelOutput, label)
	}
	return validateOpenAIShellEnvironment(environmentRaw, label+".environment")
}

func validateOpenAIShellEnvironment(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "file_ids", "memory_limit", "network_policy", "skills", "container_id"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "type", []string{"container_auto"}, label); err != nil {
		return err
	}
	if raw, present := fields["file_ids"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalStringArray(raw, label+".file_ids", false); err != nil {
			return err
		}
	}
	if raw, present := fields["container_id"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAICanonicalIdentity(raw, label+".container_id"); err != nil {
			return err
		}
	}
	if raw, present := fields["network_policy"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAINetworkPolicy(raw, label+".network_policy"); err != nil {
			return err
		}
	}
	if raw, present := fields["skills"]; present && openAINonNullRaw(raw) {
		if !openAIRawHasJSONKind(raw, "array") {
			return fmt.Errorf("%w: OpenAI %s.skills must be an array", ErrInvalidModelOutput, label)
		}
		var rawSkills []json.RawMessage
		if err := json.Unmarshal(raw, &rawSkills); err != nil {
			return fmt.Errorf("%w: OpenAI %s.skills has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, rawSkill := range rawSkills {
			if err := validateOpenAIContainerSkill(rawSkill, fmt.Sprintf("%s.skills[%d]", label, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIContainerSkill(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "skill_id", "version", "name", "description", "source"); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "type", []string{"skill_reference"}, label); err != nil {
		return err
	}
	skillRaw, present := fields["skill_id"]
	if !present || !openAINonNullRaw(skillRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its skill_id", ErrInvalidModelOutput, label)
	}
	if err := validateOpenAICanonicalIdentity(skillRaw, label+".skill_id"); err != nil {
		return err
	}
	versionRaw, present := fields["version"]
	if !present || !openAINonNullRaw(versionRaw) {
		return fmt.Errorf("%w: OpenAI %s is missing its version", ErrInvalidModelOutput, label)
	}
	version, err := openAIRawString(versionRaw, label+".version")
	if err != nil {
		return err
	}
	if !validOpenAISkillVersion(version) {
		return fmt.Errorf("%w: OpenAI %s.version %q is unsupported", ErrInvalidModelOutput, label, version)
	}
	return nil
}

func validOpenAISkillVersion(version string) bool {
	if version == "latest" {
		return true
	}
	number, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return false
	}
	if number <= 0 {
		return false
	}
	return strconv.FormatInt(number, 10) == version
}

func validateOpenAIImageGenerationTool(fields map[string]json.RawMessage, label string) error {
	if err := openAIRequireDomain(fields, "action", []string{"generate", "edit", "auto"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "background", []string{"transparent", "opaque", "auto"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "input_fidelity", []string{"high", "low"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "moderation", []string{"auto", "low"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "output_format", []string{"png", "webp", "jpeg"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "quality", []string{"low", "medium", "high", "auto"}, label); err != nil {
		return err
	}
	if err := openAIRequireDomain(fields, "size", []string{"1024x1024", "1024x1536", "1536x1024", "auto"}, label); err != nil {
		return err
	}
	if raw, present := fields["partial_images"]; present && openAINonNullRaw(raw) {
		value, err := openAIRawInt(raw, label+".partial_images")
		if err != nil {
			return err
		}
		if value < 0 || value > 3 {
			return fmt.Errorf("%w: OpenAI %s.partial_images %d is out of range [0,3]", ErrInvalidModelOutput, label, value)
		}
	}
	if raw, present := fields["output_compression"]; present && openAINonNullRaw(raw) {
		if _, err := openAIRawInt(raw, label+".output_compression"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIWebSearchTool(fields map[string]json.RawMessage, label string) error {
	if raw, present := fields["search_content_types"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIStringArray(raw, label+".search_content_types"); err != nil {
			return err
		}
	}
	if raw, present := fields["user_location"]; present && openAINonNullRaw(raw) {
		if err := validateOpenAIUserLocation(raw, label+".user_location"); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIUserLocation(raw json.RawMessage, label string) error {
	fields, err := decodeOpenAIRawObject(raw, label)
	if err != nil {
		return err
	}
	if err := openAIRequireKnownFields(fields, label, "type", "city", "country", "region", "timezone"); err != nil {
		return err
	}
	locationType, err := openAIStringField(fields, "type")
	if err != nil {
		return err
	}
	if locationType != "approximate" {
		return fmt.Errorf("%w: OpenAI %s.type %q is unsupported", ErrInvalidModelOutput, label, locationType)
	}
	for _, name := range []string{"city", "country", "region", "timezone"} {
		if raw, present := fields[name]; present && openAINonNullRaw(raw) {
			if !openAIRawHasJSONKind(raw, "string") {
				return fmt.Errorf("%w: OpenAI %s.%s must be a string", ErrInvalidModelOutput, label, name)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
