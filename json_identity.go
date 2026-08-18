package agentruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"
)

func jsonSemanticallyEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	leftValue, leftErr := decodeExactJSON(left)
	rightValue, rightErr := decodeExactJSON(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return exactJSONValuesEqual(leftValue, rightValue)
}

func decodeExactJSON(raw json.RawMessage) (any, error) {
	if err := validateExactJSONText(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeExactJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func validateExactJSONText(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON text contains invalid UTF-8")
	}
	inString := false
	for index := 0; index < len(raw); {
		current := raw[index]
		if !inString {
			if current == '"' {
				inString = true
			}
			index++
			continue
		}
		switch current {
		case '"':
			inString = false
			index++
		case '\\':
			if index+1 >= len(raw) {
				return errors.New("JSON string ends with an incomplete escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := decodeJSONHexQuad(raw, index+2)
			if !ok {
				return errors.New("JSON string contains an invalid Unicode escape")
			}
			switch {
			case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
				if index+12 > len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return errors.New("JSON string contains an unpaired high surrogate")
				}
				low, valid := decodeJSONHexQuad(raw, index+8)
				if !valid || low < 0xdc00 || low > 0xdfff {
					return errors.New("JSON string contains an unpaired high surrogate")
				}
				index += 12
			case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
				return errors.New("JSON string contains an unpaired low surrogate")
			default:
				index += 6
			}
		default:
			index++
		}
	}
	return nil
}

func decodeJSONHexQuad(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, item := range raw[start : start+4] {
		value <<= 4
		switch {
		case item >= '0' && item <= '9':
			value |= uint16(item - '0')
		case item >= 'a' && item <= 'f':
			value |= uint16(item-'a') + 10
		case item >= 'A' && item <= 'F':
			value |= uint16(item-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func decodeExactJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("JSON object contains duplicate key %q", key)
			}
			value, err := decodeExactJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("JSON object is not closed")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeExactJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("JSON array is not closed")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func exactJSONValuesEqual(left, right any) bool {
	switch leftValue := left.(type) {
	case nil:
		return right == nil
	case bool:
		rightValue, ok := right.(bool)
		return ok && leftValue == rightValue
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue
	case json.Number:
		rightValue, ok := right.(json.Number)
		if !ok {
			return false
		}
		return exactJSONNumbersEqual(leftValue, rightValue)
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for index := range leftValue {
			if !exactJSONValuesEqual(leftValue[index], rightValue[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for key, value := range leftValue {
			rightItem, exists := rightValue[key]
			if !exists || !exactJSONValuesEqual(value, rightItem) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

type canonicalJSONNumber struct {
	negative bool
	digits   string
	exponent *big.Int
}

func exactJSONNumbersEqual(left, right json.Number) bool {
	leftNumber, leftOK := canonicalizeJSONNumber(left.String())
	rightNumber, rightOK := canonicalizeJSONNumber(right.String())
	if !leftOK || !rightOK {
		return false
	}
	return leftNumber.negative == rightNumber.negative && leftNumber.digits == rightNumber.digits &&
		leftNumber.exponent.Cmp(rightNumber.exponent) == 0
}

func canonicalizeJSONNumber(value string) (canonicalJSONNumber, bool) {
	out := canonicalJSONNumber{exponent: new(big.Int)}
	if strings.HasPrefix(value, "-") {
		out.negative = true
		value = value[1:]
	}
	mantissa := value
	if exponentIndex := strings.IndexAny(value, "eE"); exponentIndex >= 0 {
		mantissa = value[:exponentIndex]
		if _, ok := out.exponent.SetString(value[exponentIndex+1:], 10); !ok {
			return canonicalJSONNumber{}, false
		}
	}
	integer := mantissa
	fraction := ""
	if decimalIndex := strings.IndexByte(mantissa, '.'); decimalIndex >= 0 {
		integer = mantissa[:decimalIndex]
		fraction = mantissa[decimalIndex+1:]
	}
	digits := strings.TrimLeft(integer+fraction, "0")
	if digits == "" {
		// Zero in any spelling (-0, 0.0, 0e3) normalizes to a single unsigned
		// zero: JSON number identity ignores the sign and exponent of zero.
		return canonicalJSONNumber{digits: "0", exponent: new(big.Int)}, true
	}
	trailingZeros := len(digits) - len(strings.TrimRight(digits, "0"))
	out.digits = digits[:len(digits)-trailingZeros]
	out.exponent.Sub(out.exponent, big.NewInt(int64(len(fraction))))
	out.exponent.Add(out.exponent, big.NewInt(int64(trailingZeros)))
	return out, true
}

func canonicalJSONIdentity(raw json.RawMessage) ([]byte, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonical JSON identity: %w", err)
	}
	var out bytes.Buffer
	if err := appendCanonicalJSONIdentity(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonicalJSONIdentity(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		out.Write(encoded)
	case json.Number:
		number, ok := canonicalizeJSONNumber(typed.String())
		if !ok {
			return fmt.Errorf("canonical JSON identity: invalid number %q", typed)
		}
		if number.digits == "0" {
			out.WriteByte('0')
			return nil
		}
		if number.negative {
			out.WriteByte('-')
		}
		out.WriteString(number.digits)
		if number.exponent.Sign() != 0 {
			out.WriteByte('e')
			out.WriteString(number.exponent.String())
		}
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSONIdentity(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			out.Write(encodedKey)
			out.WriteByte(':')
			if err := appendCanonicalJSONIdentity(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON identity: unsupported value %T", value)
	}
	return nil
}
