package agentruntime

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// B. Deep type comparator
// ---------------------------------------------------------------------------

func openAIKindLabel(kind string) string {
	switch kind {
	case "string":
		return "a string"
	case "number":
		return "a number"
	case "integer":
		return "an integer"
	case "boolean":
		return "a boolean"
	case "object":
		return "an object"
	case "array":
		return "an array"
	default:
		return kind
	}
}

// ---------------------------------------------------------------------------
// Raw extraction helpers
// ---------------------------------------------------------------------------

func openAIRawString(raw json.RawMessage, label string) (string, error) {
	if !openAIRawHasJSONKind(raw, "string") {
		return "", fmt.Errorf("%w: OpenAI %s must be a string", ErrInvalidModelOutput, label)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", fmt.Errorf("%w: OpenAI %s is not a valid string: %v", ErrInvalidModelOutput, label, err)
	}
	return text, nil
}

func openAIRawNumber(raw json.RawMessage, label string) (float64, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%w: OpenAI %s must be a number", ErrInvalidModelOutput, label)
	}
	result, err := number.Float64()
	if err != nil {
		return 0, fmt.Errorf("%w: OpenAI %s must be a number: %v", ErrInvalidModelOutput, label, err)
	}
	return result, nil
}

func openAIRawInt(raw json.RawMessage, label string) (int64, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%w: OpenAI %s must be an integer", ErrInvalidModelOutput, label)
	}
	result, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: OpenAI %s must be an integer", ErrInvalidModelOutput, label)
	}
	return result, nil
}

func openAIStringField(fields map[string]json.RawMessage, name string) (string, error) {
	raw, present := fields[name]
	if !present || !openAINonNullRaw(raw) {
		return "", nil
	}
	return openAIRawString(raw, name)
}

func openAIRequireDomain(fields map[string]json.RawMessage, name string, domain []string, label string) error {
	raw, present := fields[name]
	if !present || !openAINonNullRaw(raw) {
		return nil
	}
	text, err := openAIRawString(raw, label+"."+name)
	if err != nil {
		return err
	}
	if !openAIStringInSet(text, domain...) {
		return fmt.Errorf("%w: OpenAI %s.%s %q is unsupported", ErrInvalidModelOutput, label, name, text)
	}
	return nil
}

func openAIRequireKind(fields map[string]json.RawMessage, name, kind, label string) error {
	raw, present := fields[name]
	if !present || !openAINonNullRaw(raw) {
		return nil
	}
	if !openAIRawHasJSONKind(raw, kind) {
		return fmt.Errorf("%w: OpenAI %s.%s must be %s", ErrInvalidModelOutput, label, name, openAIKindLabel(kind))
	}
	return nil
}

// ---------------------------------------------------------------------------
