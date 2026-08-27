package mcpadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxWireJSONDepth = 256

// JSON-number conversion is deliberately bounded so a compact exponent cannot
// force construction of an enormous power of ten. Canonicalization does not
// materialize powers of ten and therefore is not subject to these limits.
const (
	maxJSONNumberConversionDigits   = 4096
	maxJSONNumberConversionExponent = maxJSONNumberConversionDigits - 1
)

func decodeExactJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty JSON")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("JSON contains invalid UTF-8")
	}
	if err := validateJSONEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeExactJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values are not allowed")
		}
		return nil, err
	}
	return value, nil
}

func decodeExactJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxWireJSONDepth {
		return nil, errors.New("JSON nesting exceeds the wire limit")
	}
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
				return nil, errors.New("JSON object contains a duplicate member")
			}
			value, err := decodeExactJSONValue(decoder, depth+1)
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
			value, err := decodeExactJSONValue(decoder, depth+1)
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

func validateJSONEscapes(raw []byte) error {
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

func canonicalJSONValue(value any) (json.RawMessage, error) {
	var out bytes.Buffer
	if err := appendCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return json.RawMessage(out.Bytes()), nil
}

type jsonNumberLexeme struct {
	negative bool
	integer  string
	fraction string
	exponent string
}

// validJSONNumberLexeme reports whether value matches the JSON number grammar
// exactly, with no surrounding whitespace or non-JSON numeric extensions.
func validJSONNumberLexeme(value string) bool {
	_, ok := parseJSONNumberLexeme(value)
	return ok
}

func parseJSONNumberLexeme(value string) (jsonNumberLexeme, bool) {
	var out jsonNumberLexeme
	if value == "" {
		return jsonNumberLexeme{}, false
	}

	index := 0
	if value[index] == '-' {
		out.negative = true
		index++
		if index == len(value) {
			return jsonNumberLexeme{}, false
		}
	}

	integerStart := index
	switch {
	case value[index] == '0':
		index++
		if index < len(value) && isJSONDigit(value[index]) {
			return jsonNumberLexeme{}, false
		}
	case value[index] >= '1' && value[index] <= '9':
		for index < len(value) && isJSONDigit(value[index]) {
			index++
		}
	default:
		return jsonNumberLexeme{}, false
	}
	out.integer = value[integerStart:index]

	if index < len(value) && value[index] == '.' {
		index++
		fractionStart := index
		for index < len(value) && isJSONDigit(value[index]) {
			index++
		}
		if index == fractionStart {
			return jsonNumberLexeme{}, false
		}
		out.fraction = value[fractionStart:index]
	}

	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		index++
		exponentStart := index
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			index++
		}
		exponentDigitsStart := index
		for index < len(value) && isJSONDigit(value[index]) {
			index++
		}
		if index == exponentDigitsStart {
			return jsonNumberLexeme{}, false
		}
		out.exponent = value[exponentStart:index]
	}

	if index != len(value) {
		return jsonNumberLexeme{}, false
	}
	return out, true
}

func isJSONDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

type canonicalNumber struct {
	negative bool
	digits   string
	exponent *big.Int
}

func normalizeJSONNumber(value string) (canonicalNumber, bool) {
	lexeme, ok := parseJSONNumberLexeme(value)
	if !ok {
		return canonicalNumber{}, false
	}

	out := canonicalNumber{
		negative: lexeme.negative,
		exponent: new(big.Int),
	}
	if lexeme.exponent != "" {
		if _, ok := out.exponent.SetString(lexeme.exponent, 10); !ok {
			return canonicalNumber{}, false
		}
	}
	digits := strings.TrimLeft(lexeme.integer+lexeme.fraction, "0")
	if digits == "" {
		return canonicalNumber{digits: "0", exponent: new(big.Int)}, true
	}
	trailingZeros := len(digits) - len(strings.TrimRight(digits, "0"))
	out.digits = digits[:len(digits)-trailingZeros]
	out.exponent.Sub(out.exponent, big.NewInt(int64(len(lexeme.fraction))))
	out.exponent.Add(out.exponent, big.NewInt(int64(trailingZeros)))
	return out, true
}

type boundedJSONNumber struct {
	negative bool
	digits   string
	exponent int
}

func normalizeBoundedJSONNumber(number json.Number) (boundedJSONNumber, bool) {
	lexeme, ok := parseJSONNumberLexeme(number.String())
	if !ok || len(lexeme.integer) > maxJSONNumberConversionDigits ||
		len(lexeme.fraction) > maxJSONNumberConversionDigits-len(lexeme.integer) {
		return boundedJSONNumber{}, false
	}
	exponent, ok := parseBoundedJSONExponent(lexeme.exponent)
	if !ok {
		return boundedJSONNumber{}, false
	}

	digits := strings.TrimLeft(lexeme.integer+lexeme.fraction, "0")
	if digits == "" {
		return boundedJSONNumber{digits: "0"}, true
	}
	trailingZeros := len(digits) - len(strings.TrimRight(digits, "0"))
	digits = digits[:len(digits)-trailingZeros]
	exponent -= len(lexeme.fraction)
	exponent += trailingZeros
	if exponent < -maxJSONNumberConversionExponent || exponent > maxJSONNumberConversionExponent {
		return boundedJSONNumber{}, false
	}
	return boundedJSONNumber{
		negative: lexeme.negative,
		digits:   digits,
		exponent: exponent,
	}, true
}

func parseBoundedJSONExponent(value string) (int, bool) {
	if value == "" {
		return 0, true
	}
	negative := false
	index := 0
	if value[index] == '+' || value[index] == '-' {
		negative = value[index] == '-'
		index++
		if index == len(value) {
			return 0, false
		}
	}

	exponent := 0
	for ; index < len(value); index++ {
		if !isJSONDigit(value[index]) {
			return 0, false
		}
		digit := int(value[index] - '0')
		if exponent > (maxJSONNumberConversionExponent-digit)/10 {
			return 0, false
		}
		exponent = exponent*10 + digit
	}
	if negative {
		exponent = -exponent
	}
	return exponent, true
}

// parseIntegralJSONNumber returns the exact mathematical integer represented by
// number. It rejects non-integral values, malformed lexemes, and values outside
// the conversion resource limits.
func parseIntegralJSONNumber(number json.Number) (*big.Int, bool) {
	normalized, ok := normalizeBoundedJSONNumber(number)
	if !ok || normalized.exponent < 0 {
		return nil, false
	}
	if normalized.digits == "0" {
		return new(big.Int), true
	}
	if len(normalized.digits) > maxJSONNumberConversionDigits-normalized.exponent {
		return nil, false
	}

	magnitude := normalized.digits + strings.Repeat("0", normalized.exponent)
	integer, ok := new(big.Int).SetString(magnitude, 10)
	if !ok {
		return nil, false
	}
	if normalized.negative {
		integer.Neg(integer)
	}
	return integer, true
}

// parseJSONNumberInt64 accepts every resource-bounded JSON number whose
// mathematical value is integral and representable as an int64, including
// decimal and exponent spellings.
func parseJSONNumberInt64(number json.Number) (int64, bool) {
	normalized, ok := normalizeBoundedJSONNumber(number)
	if !ok || normalized.exponent < 0 || normalized.exponent > 19 ||
		len(normalized.digits) > 19-normalized.exponent {
		return 0, false
	}
	if normalized.digits == "0" {
		return 0, true
	}

	value := normalized.digits + strings.Repeat("0", normalized.exponent)
	if normalized.negative {
		value = "-" + value
	}
	integer, err := strconv.ParseInt(value, 10, 64)
	return integer, err == nil
}

// parseBoundedJSONRational returns the exact rational value when conversion is
// within the resource limits and the value lies in the inclusive bounds. A nil
// bound leaves that side open. Reversed bounds are rejected.
func parseBoundedJSONRational(number json.Number, minimum, maximum *big.Rat) (*big.Rat, bool) {
	if minimum != nil && maximum != nil && minimum.Cmp(maximum) > 0 {
		return nil, false
	}
	normalized, ok := normalizeBoundedJSONNumber(number)
	if !ok {
		return nil, false
	}

	numeratorText := normalized.digits
	denominatorText := "1"
	if normalized.exponent >= 0 {
		if len(numeratorText) > maxJSONNumberConversionDigits-normalized.exponent {
			return nil, false
		}
		numeratorText += strings.Repeat("0", normalized.exponent)
	} else {
		denominatorZeros := -normalized.exponent
		if denominatorZeros > maxJSONNumberConversionDigits-1 {
			return nil, false
		}
		denominatorText += strings.Repeat("0", denominatorZeros)
	}

	numerator, ok := new(big.Int).SetString(numeratorText, 10)
	if !ok {
		return nil, false
	}
	if normalized.negative {
		numerator.Neg(numerator)
	}
	denominator, ok := new(big.Int).SetString(denominatorText, 10)
	if !ok {
		return nil, false
	}
	rational := new(big.Rat).SetFrac(numerator, denominator)
	if minimum != nil && rational.Cmp(minimum) < 0 {
		return nil, false
	}
	if maximum != nil && rational.Cmp(maximum) > 0 {
		return nil, false
	}
	return rational, true
}

func appendCanonicalJSON(out *bytes.Buffer, value any) error {
	switch item := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if item {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		out.Write(encoded)
	case json.Number:
		number, ok := normalizeJSONNumber(item.String())
		if !ok {
			return errors.New("canonical JSON contains an invalid number")
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
		for index, child := range item {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSON(out, child); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(item))
		for key := range item {
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
			if err := appendCanonicalJSON(out, item[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return errors.New("canonical JSON value is unsupported")
	}
	return nil
}

func marshalNativeJSON(value any) (json.RawMessage, error) {
	if err := validateNativeJSONValue(value, 0); err != nil {
		return nil, err
	}
	return canonicalJSONValue(value)
}

func validateNativeJSONValue(value any, depth int) error {
	if depth > maxWireJSONDepth {
		return errors.New("JSON value nesting exceeds the wire limit")
	}
	switch item := value.(type) {
	case nil, bool:
		return nil
	case json.Number:
		if !validJSONNumberLexeme(item.String()) {
			return errors.New("JSON number has an invalid lexeme")
		}
		if _, ok := normalizeBoundedJSONNumber(item); !ok {
			return errors.New("JSON number exceeds the conversion resource limit")
		}
		return nil
	case string:
		if !utf8.ValidString(item) {
			return errors.New("JSON string contains invalid UTF-8")
		}
		return nil
	case []any:
		for _, child := range item {
			if err := validateNativeJSONValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, child := range item {
			if !utf8.ValidString(key) {
				return errors.New("JSON object key contains invalid UTF-8")
			}
			if err := validateNativeJSONValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("value is not in the native exact-JSON data model")
	}
}
