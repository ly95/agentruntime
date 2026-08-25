package agentruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodeArguments converts schema-validated operation arguments into a typed
// host struct without lossy float64 coercion. Unknown object fields and trailing
// JSON values are rejected so the Go type cannot silently lag the operation
// schema.
func DecodeArguments[T any](arguments any) (T, error) {
	var result T
	normalized, err := normalizeExactJSONHostValue("operation arguments", arguments)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return result, fmt.Errorf("agent: marshal operation arguments: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("agent: decode operation arguments: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return result, fmt.Errorf("agent: decode operation arguments: %w", err)
	}
	return result, nil
}
