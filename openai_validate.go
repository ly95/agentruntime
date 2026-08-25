package agentruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3/responses"
)

// This file owns the closed-schema reflection machinery shared by every
// provider payload validator. Semantic per-type rules live in
// openai_validate_deep.go; the stream state machine lives in openai_stream.go.

var openAIImmutableResponseFields = [...]string{
	"object", "created_at", "model", "instructions", "metadata", "parallel_tool_calls",
	"temperature", "tool_choice", "tools", "top_p", "background", "conversation",
	"max_output_tokens", "max_tool_calls", "previous_response_id", "prompt",
	"prompt_cache_key", "prompt_cache_retention", "reasoning", "safety_identifier",
	"service_tier", "text", "top_logprobs", "truncation", "user",
}

func decodeOpenAIRawObject(raw json.RawMessage, label string) (map[string]json.RawMessage, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	return decodeOpenAIRawObjectChecked(raw, label, value)
}

// decodeOpenAIRawObjectChecked skips the exact decode when a caller already
// decoded raw via decodeExactJSON, so each payload node is parsed exactly once.
func decodeOpenAIRawObjectChecked(raw json.RawMessage, label string, value any) (map[string]json.RawMessage, error) {
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%w: OpenAI %s must be a JSON object", ErrInvalidModelOutput, label)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%w: OpenAI %s JSON is invalid: %v", ErrInvalidModelOutput, label, err)
	}
	return fields, nil
}

var openAIStringTypeDomains = map[string][]string{
	"ComputerUsePreviewToolEnvironment":          {"windows", "mac", "linux", "ubuntu", "browser"},
	"EasyInputMessagePhase":                      {"commentary", "final_answer"},
	"EasyInputMessageRole":                       {"user", "assistant", "system", "developer"},
	"ResponseApplyPatchToolCallOutputStatus":     {"completed", "failed"},
	"ResponseApplyPatchToolCallStatus":           {"in_progress", "completed"},
	"ResponseCodeInterpreterToolCallStatus":      {"in_progress", "completed", "incomplete", "interpreting", "failed"},
	"ResponseComputerToolCallOutputItemStatus":   {"in_progress", "completed", "incomplete", "failed"},
	"ResponseComputerToolCallStatus":             {"in_progress", "completed", "incomplete"},
	"ResponseComputerToolCallType":               {"computer_call"},
	"ResponseFileSearchToolCallStatus":           {"in_progress", "searching", "completed", "incomplete", "failed"},
	"ResponseFunctionShellToolCallOutputStatus":  {"in_progress", "completed", "incomplete"},
	"ResponseFunctionShellToolCallStatus":        {"in_progress", "completed", "incomplete"},
	"ResponseFunctionToolCallOutputItemStatus":   {"in_progress", "completed", "incomplete"},
	"ResponseFunctionToolCallStatus":             {"in_progress", "completed", "incomplete"},
	"ResponseFunctionWebSearchStatus":            {"in_progress", "searching", "completed", "failed"},
	"ResponseInputMessageItemRole":               {"user", "system", "developer"},
	"ResponseInputMessageItemStatus":             {"in_progress", "completed", "incomplete"},
	"ResponseOutputMessagePhase":                 {"commentary", "final_answer"},
	"ResponseOutputMessageStatus":                {"in_progress", "completed", "incomplete"},
	"ResponseReasoningItemStatus":                {"in_progress", "completed", "incomplete"},
	"ResponseToolSearchCallExecution":            {"server", "client"},
	"ResponseToolSearchCallStatus":               {"in_progress", "completed", "incomplete"},
	"ResponseToolSearchOutputItemExecution":      {"server", "client"},
	"ResponseToolSearchOutputItemParamExecution": {"server", "client"},
	"ResponseToolSearchOutputItemParamStatus":    {"in_progress", "completed", "incomplete"},
	"ResponseToolSearchOutputItemStatus":         {"in_progress", "completed", "incomplete"},
	"ToolSearchToolExecution":                    {"server", "client"},
	"WebSearchPreviewToolSearchContextSize":      {"low", "medium", "high"},
	"WebSearchPreviewToolType":                   {"web_search_preview", "web_search_preview_2025_03_11"},
	"WebSearchToolSearchContextSize":             {"low", "medium", "high"},
	"WebSearchToolType":                          {"web_search", "web_search_2025_08_26"},
}

// openAISchemaInfo caches per-type JSON schema metadata so per-event validation
// does not re-walk reflect fields and json tags for every payload.
type openAISchemaInfo struct {
	jsonFields  map[string]struct{}
	fields      []openAISchemaField
	inlineTypes []reflect.Type
}

type openAISchemaField struct {
	jsonName string
	typ      reflect.Type
}

var openAISchemaCache sync.Map // reflect.Type -> *openAISchemaInfo

func openAISchemaInfoFor(valueType reflect.Type) *openAISchemaInfo {
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	if cached, ok := openAISchemaCache.Load(valueType); ok {
		return cached.(*openAISchemaInfo)
	}
	info := buildOpenAISchemaInfo(valueType)
	actual, _ := openAISchemaCache.LoadOrStore(valueType, info)
	return actual.(*openAISchemaInfo)
}

func buildOpenAISchemaInfo(valueType reflect.Type) *openAISchemaInfo {
	info := &openAISchemaInfo{jsonFields: make(map[string]struct{})}
	if valueType.Kind() != reflect.Struct {
		return info
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		// Inline union members have no JSON name of their own; record them
		// before the name checks so closed-schema validation can descend into
		// heterogeneous inline variants.
		if strings.Contains(tag, "inline") {
			info.inlineTypes = append(info.inlineTypes, field.Type)
		}
		jsonName := strings.Split(tag, ",")[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		info.jsonFields[jsonName] = struct{}{}
		info.fields = append(info.fields, openAISchemaField{jsonName: jsonName, typ: field.Type})
	}
	return info
}

// validateOpenAIClosedSchema proves that provider-owned JSON uses only fields
// declared by the selected SDK schema. It deliberately leaves interface values
// and map keys open: those are the documented extension points used by metadata,
// headers, function parameters, and JSON Schema. Map values still recurse when
// their schema is typed (for example prompt variables).
func validateOpenAIClosedSchema(raw json.RawMessage, label string, valueType reflect.Type) error {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s JSON is ambiguous or invalid: %v", ErrInvalidModelOutput, label, err)
	}
	return validateOpenAIClosedSchemaValue(value, raw, label, valueType)
}

func validateOpenAIClosedSchemaValue(value any, raw json.RawMessage, label string, valueType reflect.Type) error {
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	if value == nil {
		return nil
	}
	if valueType.PkgPath() == "encoding/json" && valueType.Name() == "RawMessage" {
		return nil
	}
	switch valueType.Kind() {
	case reflect.Interface:
		return nil
	case reflect.String:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%w: OpenAI %s must be a string", ErrInvalidModelOutput, label)
		}
		return nil
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%w: OpenAI %s must be a boolean", ErrInvalidModelOutput, label)
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%w: OpenAI %s must be an integer", ErrInvalidModelOutput, label)
		}
		if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
			return fmt.Errorf("%w: OpenAI %s must be an integer", ErrInvalidModelOutput, label)
		}
		return nil
	case reflect.Float32, reflect.Float64:
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%w: OpenAI %s must be a number", ErrInvalidModelOutput, label)
		}
		return nil
	case reflect.Slice, reflect.Array:
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%w: OpenAI %s must be an array", ErrInvalidModelOutput, label)
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return fmt.Errorf("%w: OpenAI %s has invalid array JSON: %v", ErrInvalidModelOutput, label, err)
		}
		for index, item := range values {
			if err := validateOpenAIClosedSchema(item, fmt.Sprintf("%s[%d]", label, index), valueType.Elem()); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if valueType.Key().Kind() != reflect.String {
			return fmt.Errorf("%w: OpenAI %s must be an object with string keys", ErrInvalidModelOutput, label)
		}
		fields, err := decodeOpenAIRawObjectChecked(raw, label, value)
		if err != nil {
			return err
		}
		for name, fieldValue := range fields {
			if err := validateOpenAIClosedSchema(fieldValue, fmt.Sprintf("%s.%s", label, name), valueType.Elem()); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		info := openAISchemaInfoFor(valueType)
		if _, isObject := value.(map[string]any); !isObject {
			for _, inlineType := range info.inlineTypes {
				if validateOpenAIClosedSchemaValue(value, raw, label, inlineType) == nil {
					return nil
				}
			}
			if strings.HasSuffix(valueType.Name(), "Union") {
				return nil
			}
			return fmt.Errorf("%w: OpenAI %s must be an object", ErrInvalidModelOutput, label)
		}
		if strings.HasSuffix(valueType.Name(), "Union") {
			holder := reflect.New(valueType)
			if err := json.Unmarshal(raw, holder.Interface()); err != nil {
				return fmt.Errorf("%w: OpenAI %s has invalid union JSON: %v", ErrInvalidModelOutput, label, err)
			}
			method := holder.Elem().MethodByName("AsAny")
			if !method.IsValid() {
				method = holder.MethodByName("AsAny")
			}
			if method.IsValid() {
				results := method.Call(nil)
				if len(results) != 1 || ((results[0].Kind() == reflect.Interface || results[0].Kind() == reflect.Pointer) && results[0].IsNil()) {
					return fmt.Errorf("%w: OpenAI %s has an unsupported union discriminator", ErrInvalidModelOutput, label)
				}
				variantType := reflect.TypeOf(results[0].Interface())
				if variantType != nil && variantType != valueType {
					return validateOpenAIClosedSchemaValue(value, raw, label, variantType)
				}
			}
		}
		fields, err := decodeOpenAIRawObjectChecked(raw, label, value)
		if err != nil {
			return err
		}
		for name := range fields {
			if _, exists := info.jsonFields[name]; !exists {
				return fmt.Errorf("%w: OpenAI %s has unsupported field %s", ErrInvalidModelOutput, label, name)
			}
		}
		for _, field := range info.fields {
			fieldRaw, exists := fields[field.jsonName]
			if !exists {
				continue
			}
			if field.jsonName == "namespace" {
				return fmt.Errorf("%w: OpenAI %s.namespace is unsupported because the runtime cannot preserve namespace authority", ErrInvalidModelOutput, label)
			}
			if bytes.Equal(bytes.TrimSpace(fieldRaw), []byte("null")) {
				continue
			}
			if openAIIdentityField(field.jsonName) {
				if err := validateOpenAICanonicalIdentity(fieldRaw, fmt.Sprintf("%s.%s", label, field.jsonName)); err != nil {
					return err
				}
			}
			if openAIIdentityCollectionField(field.jsonName) {
				if err := validateOpenAICanonicalStringArray(fieldRaw, fmt.Sprintf("%s.%s", label, field.jsonName), false); err != nil {
					return err
				}
			}
			if err := validateOpenAIClosedSchema(fieldRaw, fmt.Sprintf("%s.%s", label, field.jsonName), field.typ); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIExactJSONString(raw json.RawMessage, label string) error {
	if !openAIRawHasJSONKind(raw, "string") {
		return fmt.Errorf("%w: OpenAI %s must be a JSON string", ErrInvalidModelOutput, label)
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return fmt.Errorf("%w: OpenAI %s is not a valid string: %v", ErrInvalidModelOutput, label, err)
	}
	if _, err := decodeExactJSON([]byte(encoded)); err != nil {
		return fmt.Errorf("%w: OpenAI %s contains ambiguous or invalid JSON: %v", ErrInvalidModelOutput, label, err)
	}
	return nil
}

func openAIRawHasJSONKind(raw json.RawMessage, kind string) bool {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return false
	}
	return openAIValueHasJSONKind(value, kind)
}

func openAIValueHasJSONKind(value any, kind string) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(number.String(), 10, 64)
		return err == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string-or-array":
		if _, ok := value.(string); ok {
			return true
		}
		_, ok := value.([]any)
		return ok
	case "string-or-object":
		if _, ok := value.(string); ok {
			return true
		}
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

func openAINonNullRaw(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func openAIStringInSet(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// openAIStreamEventField returns the exact raw JSON of one named top-level
// field of a stream event, or nil when the field is absent.
func openAIStreamEventField(event responses.ResponseStreamEventUnion, name string) (json.RawMessage, error) {
	fields, err := decodeOpenAIRawObject(json.RawMessage(event.RawJSON()), "stream event "+event.Type)
	if err != nil {
		return nil, err
	}
	return fields[name], nil
}
