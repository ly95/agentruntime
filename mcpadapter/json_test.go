package mcpadapter

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJSONNumberEncodingRejectsMalformedLexemes(t *testing.T) {
	malformed := []string{
		"",
		"-",
		"+1",
		"01",
		"-01",
		"00",
		"-00",
		".",
		".1",
		"-.1",
		"1.",
		"1.e2",
		"1..0",
		"1.2.3",
		"e1",
		"-e1",
		"1e",
		"1E",
		"1e+",
		"1e-",
		"1e++1",
		"1e--1",
		"1e+-1",
		"1e-+1",
		"1ee1",
		"1e1.0",
		"1e1e2",
		"--1",
		"-+1",
		"NaN",
		"Infinity",
		"-Infinity",
		"0x1",
		"1_0",
		" 1",
		"1 ",
		"1\n",
	}

	for _, lexeme := range malformed {
		t.Run(strconv.Quote(lexeme), func(t *testing.T) {
			if validJSONNumberLexeme(lexeme) {
				t.Fatal("malformed lexeme reported valid")
			}
			if encoded, err := canonicalJSONValue(json.Number(lexeme)); err == nil {
				t.Fatalf("canonicalJSONValue encoded malformed number as %q", encoded)
			}
			value := map[string]any{"nested": []any{json.Number(lexeme)}}
			if encoded, err := marshalNativeJSON(value); err == nil {
				t.Fatalf("marshalNativeJSON encoded malformed number as %q", encoded)
			}
		})
	}
}

func TestJSONNumberEncodingPreservesCanonicalForms(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "0", want: "0"},
		{input: "-0", want: "0"},
		{input: "0.000e+999999999", want: "0"},
		{input: "1", want: "1"},
		{input: "1.0", want: "1"},
		{input: "1.2300", want: "123e-2"},
		{input: "1000", want: "1e3"},
		{input: "0.00000100", want: "1e-6"},
		{input: "0.0012300E+4", want: "123e-1"},
		{input: "-120.00e-1", want: "-12"},
		{input: "1e+0002", want: "1e2"},
		{input: "123.4500e-0002", want: "12345e-4"},
		{input: "1e1000000000", want: "1e1000000000"},
		{input: "10e999999999", want: "1e1000000000"},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			if !validJSONNumberLexeme(test.input) {
				t.Fatal("valid lexeme reported invalid")
			}
			canonical, err := canonicalJSONValue(json.Number(test.input))
			if err != nil {
				t.Fatalf("canonicalJSONValue: %v", err)
			}
			if got := string(canonical); got != test.want {
				t.Fatalf("canonicalJSONValue=%q, want %q", got, test.want)
			}
			native, err := marshalNativeJSON(json.Number(test.input))
			if _, bounded := normalizeBoundedJSONNumber(json.Number(test.input)); !bounded {
				if err == nil {
					t.Fatalf("marshalNativeJSON accepted conversion-unbounded number as %q", native)
				}
				return
			}
			if err != nil {
				t.Fatalf("marshalNativeJSON: %v", err)
			}
			if got := string(native); got != test.want {
				t.Fatalf("marshalNativeJSON=%q, want %q", got, test.want)
			}
		})
	}
}

func TestParseJSONNumberInt64BoundariesAndExponentForms(t *testing.T) {
	valid := []struct {
		input string
		want  int64
	}{
		{input: "0", want: 0},
		{input: "-0e-4095", want: 0},
		{input: "1.000e3", want: 1000},
		{input: "1000e-3", want: 1},
		{input: "120e-1", want: 12},
		{input: "9223372036854775807", want: int64(9223372036854775807)},
		{input: "9.223372036854775807e18", want: int64(9223372036854775807)},
		{input: "922337203685477580700e-2", want: int64(9223372036854775807)},
		{input: "-9223372036854775808", want: int64(-9223372036854775807 - 1)},
		{input: "-9.223372036854775808e18", want: int64(-9223372036854775807 - 1)},
	}
	for _, test := range valid {
		t.Run("valid_"+test.input, func(t *testing.T) {
			got, ok := parseJSONNumberInt64(json.Number(test.input))
			if !ok || got != test.want {
				t.Fatalf("parseJSONNumberInt64=(%d, %t), want (%d, true)", got, ok, test.want)
			}
		})
	}

	invalid := []string{
		"9223372036854775808",
		"9.223372036854775808e18",
		"-9223372036854775809",
		"-9.223372036854775809e18",
		"1e19",
		"1e-1",
		"12e-1",
		"1e4096",
		"01",
	}
	for _, input := range invalid {
		t.Run("invalid_"+input, func(t *testing.T) {
			if got, ok := parseJSONNumberInt64(json.Number(input)); ok {
				t.Fatalf("parseJSONNumberInt64=(%d, true), want rejection", got)
			}
		})
	}
}

func TestParseIntegralJSONNumberExponentAndResourceBoundaries(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "0.000e4095", want: "0"},
		{input: "100e-2", want: "1"},
		{input: "1.25e2", want: "125"},
		{input: "-12.5e1", want: "-125"},
		{input: "1e100", want: "1" + strings.Repeat("0", 100)},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			integer, ok := parseIntegralJSONNumber(json.Number(test.input))
			if !ok || integer.String() != test.want {
				t.Fatalf("parseIntegralJSONNumber=(%v, %t), want (%s, true)", integer, ok, test.want)
			}
		})
	}

	boundaryInput := "1e" + strconv.Itoa(maxJSONNumberConversionExponent)
	boundary, ok := parseIntegralJSONNumber(json.Number(boundaryInput))
	if !ok {
		t.Fatalf("parseIntegralJSONNumber(%q) rejected resource boundary", boundaryInput)
	}
	if got := boundary.String(); len(got) != maxJSONNumberConversionDigits || got[0] != '1' || got[len(got)-1] != '0' {
		t.Fatalf("resource-boundary integer has unexpected shape: digits=%d", len(got))
	}

	tooLargeExponent := "1e" + strconv.Itoa(maxJSONNumberConversionExponent+1)
	tooManyDigits := strings.Repeat("1", maxJSONNumberConversionDigits+1)
	for _, input := range []string{"1.25e1", "1e-1", tooLargeExponent, tooManyDigits, "1."} {
		if integer, ok := parseIntegralJSONNumber(json.Number(input)); ok {
			t.Fatalf("parseIntegralJSONNumber(%q)=(%v, true), want rejection", input, integer)
		}
	}
}

func TestParseBoundedJSONRationalExactBoundsAndResourceBoundaries(t *testing.T) {
	zero := new(big.Rat)
	one := big.NewRat(1, 1)

	rational, ok := parseBoundedJSONRational(json.Number("1.25e-2"), zero, one)
	if !ok || rational.Cmp(big.NewRat(1, 80)) != 0 {
		t.Fatalf("parseBoundedJSONRational=(%v, %t), want (1/80, true)", rational, ok)
	}
	for _, input := range []string{"0", "1", "1.000e0"} {
		if _, ok := parseBoundedJSONRational(json.Number(input), zero, one); !ok {
			t.Fatalf("inclusive bound rejected %q", input)
		}
	}
	for _, input := range []string{"-1e-1", "1.0001", "01"} {
		if rational, ok := parseBoundedJSONRational(json.Number(input), zero, one); ok {
			t.Fatalf("parseBoundedJSONRational(%q)=(%v, true), want rejection", input, rational)
		}
	}
	if rational, ok := parseBoundedJSONRational(json.Number("0.5"), one, zero); ok {
		t.Fatalf("reversed bounds returned (%v, true)", rational)
	}

	boundaryInput := "1e-" + strconv.Itoa(maxJSONNumberConversionExponent)
	boundary, ok := parseBoundedJSONRational(json.Number(boundaryInput), nil, nil)
	if !ok {
		t.Fatalf("parseBoundedJSONRational(%q) rejected resource boundary", boundaryInput)
	}
	if got := boundary.Denom().String(); len(got) != maxJSONNumberConversionDigits || got[0] != '1' || got[len(got)-1] != '0' {
		t.Fatalf("resource-boundary denominator has unexpected shape: digits=%d", len(got))
	}

	tooLargeExponent := "1e-" + strconv.Itoa(maxJSONNumberConversionExponent+1)
	if rational, ok := parseBoundedJSONRational(json.Number(tooLargeExponent), nil, nil); ok {
		t.Fatalf("parseBoundedJSONRational(%q)=(%v, true), want rejection", tooLargeExponent, rational)
	}
}

func TestJSONNumberParsersRejectCompactExponentBombsQuickly(t *testing.T) {
	bombs := []json.Number{
		"1e1000000000",
		"1e-1000000000",
		"1e999999999999999999999999999999",
	}
	started := time.Now()
	for _, bomb := range bombs {
		if !validJSONNumberLexeme(bomb.String()) {
			t.Fatalf("bomb lexeme %q is syntactically valid JSON", bomb)
		}
		if integer, ok := parseIntegralJSONNumber(bomb); ok {
			t.Fatalf("parseIntegralJSONNumber(%q)=(%v, true), want resource rejection", bomb, integer)
		}
		if integer, ok := parseJSONNumberInt64(bomb); ok {
			t.Fatalf("parseJSONNumberInt64(%q)=(%d, true), want resource rejection", bomb, integer)
		}
		if rational, ok := parseBoundedJSONRational(bomb, nil, nil); ok {
			t.Fatalf("parseBoundedJSONRational(%q)=(%v, true), want resource rejection", bomb, rational)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("compact exponent rejection took %v, want at most 1s", elapsed)
	}
}
