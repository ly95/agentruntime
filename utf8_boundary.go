package agentruntime

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"
)

// validateUTF8Boundary rejects non-injective host strings before encoding/json
// can replace invalid bytes with U+FFFD. It follows the exported, JSON-visible
// shape of arbitrary host values, including string map keys and nested values.
func validateUTF8Boundary(label string, value any) error {
	active := make(map[utf8Visit]struct{})
	if err := validateUTF8Reflect(reflect.ValueOf(value), label, active); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

type utf8Visit struct {
	typ  reflect.Type
	ptr  uintptr
	kind reflect.Kind
	len  int
	cap  int
}

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	rawMessageType    = reflect.TypeOf(json.RawMessage{})
)

// normalizeExactJSONHostValue converts an arbitrary host value into the native
// JSON data model only after proving encoding/json cannot invoke an unchecked
// custom encoder. RawMessage is the one supported encoder-backed value because
// its complete output is validated by the exact decoder before use.
func normalizeExactJSONHostValue(label string, value any) (any, error) {
	if err := validateUTF8Boundary(label, value); err != nil {
		return nil, err
	}
	active := make(map[utf8Visit]struct{})
	if err := validateExactJSONHostEncoding(reflect.ValueOf(value), label, active); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("agent: encode %s: %w", label, err)
	}
	normalized, err := decodeExactJSON(encoded)
	if err != nil {
		return nil, fmt.Errorf("agent: %s does not encode as exact JSON: %w", label, err)
	}
	return normalized, nil
}

func validateExactJSONHostEncoding(value reflect.Value, path string, active map[utf8Visit]struct{}) error {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Map || value.Kind() == reflect.Slice) && value.IsNil() {
		return nil
	}
	if value.Type() == rawMessageType {
		if _, err := decodeExactJSON(json.RawMessage(value.Bytes())); err != nil {
			return fmt.Errorf("%s RawMessage is ambiguous or invalid: %w", path, err)
		}
		return nil
	}
	if hasUnsupportedJSONEncodingMethod(value.Type()) {
		return fmt.Errorf("%s uses unsupported custom JSON or text encoding type %s", path, value.Type())
	}

	switch value.Kind() {
	case reflect.Pointer:
		visit := utf8Visit{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		return validateExactJSONHostEncoding(value.Elem(), path, active)
	case reflect.Map:
		keyType := value.Type().Key()
		if hasUnsupportedJSONEncodingMethod(keyType) {
			return fmt.Errorf("%s map key uses unsupported custom JSON or text encoding type %s", path, keyType)
		}
		if keyType.Kind() != reflect.String {
			return fmt.Errorf("%s uses non-string JSON object key type %s", path, keyType)
		}
		visit := utf8Visit{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind(), len: value.Len()}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateExactJSONHostEncoding(iterator.Key(), path+" map key", active); err != nil {
				return err
			}
			if err := validateExactJSONHostEncoding(iterator.Value(), path+" map value", active); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		visit := utf8Visit{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind(), len: value.Len(), cap: value.Cap()}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		for index := 0; index < value.Len(); index++ {
			if err := validateExactJSONHostEncoding(value.Index(index), fmt.Sprintf("%s[%d]", path, index), active); err != nil {
				return err
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateExactJSONHostEncoding(value.Index(index), fmt.Sprintf("%s[%d]", path, index), active); err != nil {
				return err
			}
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for index := 0; index < value.NumField(); index++ {
			field := typeOfValue.Field(index)
			// encoding/json promotes exported fields through an unexported
			// anonymous struct. Follow that container so prevalidation sees the
			// same strings and method sets that the encoder can expose.
			if field.PkgPath != "" && !field.Anonymous {
				continue
			}
			if err := validateExactJSONHostEncoding(value.Field(index), path+"."+field.Name, active); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasUnsupportedJSONEncodingMethod(valueType reflect.Type) bool {
	if valueType.Implements(jsonMarshalerType) || valueType.Implements(textMarshalerType) {
		return true
	}
	// encoding/json can take the address of values in several containers and
	// then invoke pointer-receiver methods. Rejecting the pointer method set for
	// every non-pointer value is deliberately conservative and keeps the host
	// boundary injective without depending on subtle addressability changes.
	return valueType.Kind() != reflect.Pointer &&
		(reflect.PointerTo(valueType).Implements(jsonMarshalerType) ||
			reflect.PointerTo(valueType).Implements(textMarshalerType))
}

func validateUTF8Reflect(value reflect.Value, path string, active map[utf8Visit]struct{}) error {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("%s contains invalid UTF-8", path)
		}
		return nil
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		visit := utf8Visit{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		return validateUTF8Reflect(value.Elem(), path, active)
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		visit := utf8Visit{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind(), len: value.Len()}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateUTF8Reflect(iterator.Key(), path+" map key", active); err != nil {
				return err
			}
			if err := validateUTF8Reflect(iterator.Value(), path+" map value", active); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if value.IsNil() || value.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		visit := utf8Visit{
			typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind(),
			len: value.Len(), cap: value.Cap(),
		}
		if _, seen := active[visit]; seen {
			return nil
		}
		active[visit] = struct{}{}
		defer delete(active, visit)
		for index := 0; index < value.Len(); index++ {
			if err := validateUTF8Reflect(value.Index(index), fmt.Sprintf("%s[%d]", path, index), active); err != nil {
				return err
			}
		}
		return nil
	case reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for index := 0; index < value.Len(); index++ {
			if err := validateUTF8Reflect(value.Index(index), fmt.Sprintf("%s[%d]", path, index), active); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		typeOfValue := value.Type()
		for index := 0; index < value.NumField(); index++ {
			field := typeOfValue.Field(index)
			if field.PkgPath != "" && !field.Anonymous {
				continue
			}
			if err := validateUTF8Reflect(value.Field(index), path+"."+field.Name, active); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

type invalidUTF8Error struct {
	label string
	cause error
}

func (err invalidUTF8Error) Error() string {
	return "agent: " + err.label + " returned an error containing invalid UTF-8"
}

func (err invalidUTF8Error) Unwrap() error {
	return err.cause
}

func validateUTF8Error(label string, cause error) error {
	if cause == nil || utf8.ValidString(cause.Error()) {
		return cause
	}
	return invalidUTF8Error{label: label, cause: cause}
}
