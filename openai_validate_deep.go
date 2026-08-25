package agentruntime

import (
	"encoding/json"
	"fmt"
)

// This file owns the per-type semantic validators for provider-owned JSON. The
// closed-schema reflection machinery lives in openai_validate.go; the stream
// state machine lives in openai_stream.go.

// ---------------------------------------------------------------------------
// A. Identity helpers (called by the closed schema)
// ---------------------------------------------------------------------------

// openAIIdentityField reports whether a present JSON field name must carry a
// canonical identity string (non-empty, no surrounding whitespace, valid UTF-8).
func openAIIdentityField(name string) bool {
	switch name {
	case "id", "call_id", "item_id", "run_id", "response_id", "conversation_id",
		"prompt_id", "skill_id", "file_id", "vector_store_id", "connector_id",
		"server_label", "attempt_id":
		return true
	default:
		return false
	}
}

// openAIIdentityCollectionField reports whether a present JSON field name must
// carry a canonical string array (non-empty elements, no padding, no dupes).
func openAIIdentityCollectionField(name string) bool {
	switch name {
	case "vector_store_ids", "file_ids", "allowed_tools", "tool_names", "tags":
		return true
	default:
		return false
	}
}

// validateOpenAICanonicalIdentity requires raw to be a JSON string whose decoded
// value is a canonical identity (non-empty, no surrounding whitespace).
func validateOpenAICanonicalIdentity(raw json.RawMessage, label string) error {
	if !openAIRawHasJSONKind(raw, "string") {
		return fmt.Errorf("%w: OpenAI %s must be a non-empty canonical string", ErrInvalidModelOutput, label)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("%w: OpenAI %s is not a valid string: %v", ErrInvalidModelOutput, label, err)
	}
	if err := requireCanonicalIdentity(text, label); err != nil {
		return fmt.Errorf("%w: OpenAI %v", ErrInvalidModelOutput, err)
	}
	return nil
}

// validateOpenAICanonicalStringArray requires raw to be a JSON array of
// canonical identity strings. Elements must be non-empty and unpadded and the
// array must not repeat a value. When requireNonEmpty is set, the array itself
// must contain at least one element.
func validateOpenAICanonicalStringArray(raw json.RawMessage, label string, requireNonEmpty bool) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	values, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s must be an array of strings", ErrInvalidModelOutput, label)
	}
	if requireNonEmpty && len(values) == 0 {
		return fmt.Errorf("%w: OpenAI %s must not be empty", ErrInvalidModelOutput, label)
	}
	seen := make(map[string]struct{}, len(values))
	for index, item := range values {
		text, ok := item.(string)
		if !ok {
			return fmt.Errorf("%w: OpenAI %s[%d] must be a string", ErrInvalidModelOutput, label, index)
		}
		if err := requireCanonicalIdentity(text, fmt.Sprintf("%s[%d]", label, index)); err != nil {
			return fmt.Errorf("%w: OpenAI %v", ErrInvalidModelOutput, err)
		}
		if _, duplicate := seen[text]; duplicate {
			return fmt.Errorf("%w: OpenAI %s contains duplicate value %q", ErrInvalidModelOutput, label, text)
		}
		seen[text] = struct{}{}
	}
	return nil
}

// validateOpenAIStringArray requires raw to be a JSON array of strings.
func validateOpenAIStringArray(raw json.RawMessage, label string) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	values, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: OpenAI %s must be an array of strings", ErrInvalidModelOutput, label)
	}
	for index, item := range values {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("%w: OpenAI %s[%d] must be a string", ErrInvalidModelOutput, label, index)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
