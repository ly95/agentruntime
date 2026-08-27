package mcpadapter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxMCPHeaderInteger = int64(9_007_199_254_740_991)

type headerValueKind uint8

const (
	headerString headerValueKind = iota + 1
	headerInteger
	headerBoolean
)

type headerBinding struct {
	path []string
	name string
	kind headerValueKind
}

type schemaBudget struct {
	limits Limits
	nodes  int
}

type schemaDialect uint8

const (
	schemaDialectDraft7 schemaDialect = iota + 1
	schemaDialectDraft2020
)

type operationSchemaRole uint8

const (
	operationInputSchema operationSchemaRole = iota + 1
	operationOutputSchema
)

func validateSchemaNativeValue(value map[string]any) error {
	if value == nil {
		return errors.New("mcpadapter: schema must be a JSON object")
	}
	if err := validateNativeJSONValue(value, 0); err != nil {
		return errors.New("mcpadapter: schema contains a number outside the conversion resource limit")
	}
	return nil
}

func validateSchemaDocument(value map[string]any, raw json.RawMessage, limits Limits) error {
	if err := validateSchemaNativeValue(value); err != nil {
		return err
	}
	if len(raw) > limits.MaxSchemaBytes {
		return errors.New("mcpadapter: schema exceeds the configured byte limit")
	}
	budget := schemaBudget{limits: limits}
	if err := budget.walk(value, 1); err != nil {
		return err
	}
	if err := validateSchemaObject(value); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:mcpadapter:schema", value); err != nil {
		return errors.New("mcpadapter: schema is invalid")
	}
	if _, err := compiler.Compile("urn:mcpadapter:schema"); err != nil {
		return errors.New("mcpadapter: schema is invalid")
	}
	return nil
}

func (budget *schemaBudget) walk(value any, depth int) error {
	if depth > budget.limits.MaxSchemaDepth {
		return errors.New("mcpadapter: schema exceeds the configured depth limit")
	}
	budget.nodes++
	if budget.nodes > budget.limits.MaxSchemaNodes {
		return errors.New("mcpadapter: schema exceeds the configured node limit")
	}
	switch item := value.(type) {
	case map[string]any:
		for _, child := range item {
			if err := budget.walk(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := budget.walk(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSchemaObject(schema map[string]any) error {
	for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		if value, exists := schema[keyword]; exists {
			reference, ok := value.(string)
			if !ok || !supportedLocalSchemaReference(reference) ||
				(keyword == "$recursiveRef" && reference != "#") {
				return errors.New("mcpadapter: schema contains a non-local reference")
			}
		}
	}
	if value, exists := schema["$schema"]; exists {
		dialect, ok := value.(string)
		if !ok || !supportedSchemaDialect(dialect) {
			return errors.New("mcpadapter: schema dialect is not supported")
		}
	}
	if _, exists := schema["$id"]; exists {
		return errors.New("mcpadapter: schema identifiers are not supported")
	}
	if _, exists := schema["$vocabulary"]; exists {
		return errors.New("mcpadapter: custom schema vocabularies are not supported")
	}
	return visitChildSchemas(schema, func(child map[string]any) error {
		return validateSchemaObject(child)
	})
}

func supportedLocalSchemaReference(reference string) bool {
	if reference == "#" {
		return true
	}
	var name string
	switch {
	case strings.HasPrefix(reference, "#/$defs/"):
		name = strings.TrimPrefix(reference, "#/$defs/")
	case strings.HasPrefix(reference, "#/definitions/"):
		name = strings.TrimPrefix(reference, "#/definitions/")
	default:
		return false
	}
	if name == "" || strings.ContainsAny(name, "/%") {
		return false
	}
	for index := 0; index < len(name); index++ {
		if name[index] != '~' {
			continue
		}
		if index+1 >= len(name) || (name[index+1] != '0' && name[index+1] != '1') {
			return false
		}
		index++
	}
	return true
}

func supportedSchemaDialect(value string) bool {
	_, ok := parseSchemaDialect(value)
	return ok
}

func parseSchemaDialect(value string) (schemaDialect, bool) {
	switch value {
	case "https://json-schema.org/draft/2020-12/schema",
		"https://json-schema.org/draft/2020-12/schema#":
		return schemaDialectDraft2020, true
	case "http://json-schema.org/draft-07/schema",
		"http://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft-07/schema",
		"https://json-schema.org/draft-07/schema#":
		return schemaDialectDraft7, true
	default:
		return 0, false
	}
}

func visitChildSchemas(schema map[string]any, visit func(map[string]any) error) error {
	for _, keyword := range []string{
		"additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems",
		"propertyNames", "contains", "items", "not", "if", "then", "else", "contentSchema",
	} {
		if err := visitSchemaValue(schema[keyword], visit); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		values, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, value := range values {
			if err := visitSchemaValue(value, visit); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{
		"properties", "patternProperties", "dependentSchemas", "$defs", "definitions",
	} {
		values, ok := schema[keyword].(map[string]any)
		if !ok {
			continue
		}
		for _, value := range values {
			if err := visitSchemaValue(value, visit); err != nil {
				return err
			}
		}
	}
	if dependencies, ok := schema["dependencies"].(map[string]any); ok {
		for _, value := range dependencies {
			if _, isPropertyList := value.([]any); isPropertyList {
				continue
			}
			if err := visitSchemaValue(value, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func visitSchemaValue(value any, visit func(map[string]any) error) error {
	switch child := value.(type) {
	case nil, bool:
		return nil
	case map[string]any:
		return visit(child)
	case []any:
		for _, item := range child {
			if err := visitSchemaValue(item, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func sanitizeOperationSchema(source map[string]any, role operationSchemaRole) (map[string]any, json.RawMessage, error) {
	dialect, err := selectedRootSchemaDialect(source)
	if err != nil {
		return nil, nil, err
	}
	if role == operationInputSchema && dialect == schemaDialectDraft7 {
		if _, exists := source["$ref"]; exists {
			return nil, nil, errors.New("mcpadapter: selected draft-07 input schema cannot use a root $ref")
		}
	}
	value, err := sanitizeSchemaValue(source, dialect, true)
	if err != nil {
		return nil, nil, err
	}
	document, ok := value.(map[string]any)
	if !ok || document == nil {
		return nil, nil, errors.New("mcpadapter: selected schema must be a JSON object")
	}
	if role == operationInputSchema {
		if err := validateOperationInputSchemaProfile(document); err != nil {
			return nil, nil, err
		}
	}
	raw, err := canonicalJSONValue(document)
	if err != nil {
		return nil, nil, errors.New("mcpadapter: encode selected schema")
	}
	return document, raw, nil
}

func selectedRootSchemaDialect(schema map[string]any) (schemaDialect, error) {
	value, exists := schema["$schema"]
	if !exists {
		return schemaDialectDraft2020, nil
	}
	name, ok := value.(string)
	if !ok {
		return 0, errors.New("mcpadapter: schema dialect is not supported")
	}
	dialect, ok := parseSchemaDialect(name)
	if !ok {
		return 0, errors.New("mcpadapter: schema dialect is not supported")
	}
	return dialect, nil
}

func sanitizeSchemaValue(value any, inheritedDialect schemaDialect, root bool) (any, error) {
	switch schema := value.(type) {
	case bool:
		return schema, nil
	case map[string]any:
		dialect := inheritedDialect
		if value, exists := schema["$schema"]; exists {
			name, ok := value.(string)
			if !ok {
				return nil, errors.New("mcpadapter: schema dialect is not supported")
			}
			declared, ok := parseSchemaDialect(name)
			if !ok {
				return nil, errors.New("mcpadapter: schema dialect is not supported")
			}
			if !root && declared != inheritedDialect {
				return nil, errors.New("mcpadapter: selected schema cannot change dialect in a nested schema")
			}
			dialect = declared
		}
		out := make(map[string]any)
		for key, child := range schema {
			switch {
			case dialect == schemaDialectDraft7 && unsupportedDraft7SchemaKeyword(key):
				return nil, errors.New("mcpadapter: selected draft-07 schema contains unsupported keyword " + key)
			case dialect == schemaDialectDraft2020 && unsupportedDraft2020SchemaKeyword(key):
				return nil, errors.New("mcpadapter: selected 2020-12 schema contains unsupported keyword " + key)
			case schemaAnnotationKeyword(key) || key == "x-mcp-header":
				continue
			case key == "$id" || key == "$vocabulary":
				return nil, errors.New("mcpadapter: selected schema uses unsupported identity or vocabulary")
			case schemaMapKeyword(key):
				values, ok := child.(map[string]any)
				if !ok {
					return nil, errors.New("mcpadapter: selected schema has an invalid schema map")
				}
				cloned := make(map[string]any, len(values))
				for name, item := range values {
					normalized, err := sanitizeSchemaValue(item, dialect, false)
					if err != nil {
						return nil, err
					}
					cloned[name] = normalized
				}
				out[key] = cloned
			case schemaArrayKeyword(key):
				values, ok := child.([]any)
				if !ok {
					return nil, errors.New("mcpadapter: selected schema has an invalid schema array")
				}
				cloned := make([]any, len(values))
				for index, item := range values {
					normalized, err := sanitizeSchemaValue(item, dialect, false)
					if err != nil {
						return nil, err
					}
					cloned[index] = normalized
				}
				out[key] = cloned
			case schemaSingleKeyword(key):
				if values, ok := child.([]any); ok {
					cloned := make([]any, len(values))
					for index, item := range values {
						normalized, err := sanitizeSchemaValue(item, dialect, false)
						if err != nil {
							return nil, err
						}
						cloned[index] = normalized
					}
					out[key] = cloned
					continue
				}
				normalized, err := sanitizeSchemaValue(child, dialect, false)
				if err != nil {
					return nil, err
				}
				out[key] = normalized
			case key == "dependencies":
				values, ok := child.(map[string]any)
				if !ok {
					return nil, errors.New("mcpadapter: selected schema dependencies are invalid")
				}
				cloned := make(map[string]any, len(values))
				for name, item := range values {
					if list, ok := item.([]any); ok {
						cloned[name] = cloneJSONValue(list)
						continue
					}
					normalized, err := sanitizeSchemaValue(item, dialect, false)
					if err != nil {
						return nil, err
					}
					cloned[name] = normalized
				}
				out[key] = cloned
			case key == "format":
				format, ok := child.(string)
				if !ok || !supportedSchemaFormat(format) {
					return nil, errors.New("mcpadapter: selected schema format is not supported")
				}
				out[key] = format
			case schemaValueKeyword(key):
				out[key] = cloneJSONValue(child)
			default:
				return nil, errors.New("mcpadapter: selected schema contains an unsupported keyword")
			}
		}
		return out, nil
	default:
		return nil, errors.New("mcpadapter: selected schema child is invalid")
	}
}

func unsupportedDraft7SchemaKeyword(key string) bool {
	switch key {
	case "$defs", "$dynamicRef", "$recursiveRef", "$anchor", "$dynamicAnchor", "$recursiveAnchor",
		"dependentSchemas", "dependentRequired", "prefixItems", "unevaluatedProperties", "unevaluatedItems",
		"minContains", "maxContains", "contentSchema":
		return true
	default:
		return false
	}
}

func unsupportedDraft2020SchemaKeyword(key string) bool {
	switch key {
	case "definitions", "dependencies", "additionalItems", "$recursiveRef", "$recursiveAnchor":
		return true
	default:
		return false
	}
}

func validateOperationInputSchemaProfile(schema map[string]any) error {
	if _, exists := schema["prefixItems"]; exists {
		return errors.New("mcpadapter: selected input schema tuple arrays are not supported")
	}
	if _, tuple := schema["items"].([]any); tuple {
		return errors.New("mcpadapter: selected input schema tuple arrays are not supported")
	}
	return visitChildSchemas(schema, validateOperationInputSchemaProfile)
}

func schemaAnnotationKeyword(key string) bool {
	switch key {
	case "title", "description", "$comment", "default", "examples", "deprecated", "readOnly", "writeOnly",
		"contentEncoding", "contentMediaType", "contentSchema":
		return true
	default:
		return false
	}
}

func schemaMapKeyword(key string) bool {
	switch key {
	case "properties", "patternProperties", "dependentSchemas", "$defs", "definitions":
		return true
	default:
		return false
	}
}

func schemaArrayKeyword(key string) bool {
	switch key {
	case "allOf", "anyOf", "oneOf", "prefixItems":
		return true
	default:
		return false
	}
}

func schemaSingleKeyword(key string) bool {
	switch key {
	case "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems",
		"propertyNames", "contains", "items", "not", "if", "then", "else":
		return true
	default:
		return false
	}
}

func supportedSchemaFormat(format string) bool {
	switch format {
	case "date-time", "time", "date", "duration", "email", "idn-email", "hostname", "idn-hostname",
		"ipv4", "ipv6", "uuid", "uri", "uri-reference", "iri", "iri-reference", "uri-template",
		"json-pointer", "relative-json-pointer", "regex":
		return true
	default:
		return false
	}
}

func schemaValueKeyword(key string) bool {
	switch key {
	case "$schema", "$ref", "$dynamicRef", "$recursiveRef", "$anchor", "$dynamicAnchor", "$recursiveAnchor",
		"type", "enum", "const", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum",
		"maxLength", "minLength", "pattern", "maxItems", "minItems", "uniqueItems", "maxContains", "minContains",
		"maxProperties", "minProperties", "required", "dependentRequired":
		return true
	default:
		return false
	}
}

func cloneJSONValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			out[key] = cloneJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for index, child := range item {
			out[index] = cloneJSONValue(child)
		}
		return out
	default:
		return item
	}
}

func collectHeaderBindings(schema map[string]any) ([]headerBinding, error) {
	dialect, err := selectedRootSchemaDialect(schema)
	if err != nil {
		return nil, err
	}
	collector := headerCollector{dialect: dialect, names: make(map[string]struct{})}
	if err := collector.walk(schema, nil, false, true); err != nil {
		return nil, err
	}
	sort.Slice(collector.bindings, func(i, j int) bool {
		left := strings.ToLower(collector.bindings[i].name) + "\x00" + strings.Join(collector.bindings[i].path, "\x00")
		right := strings.ToLower(collector.bindings[j].name) + "\x00" + strings.Join(collector.bindings[j].path, "\x00")
		return left < right
	})
	return collector.bindings, nil
}

type headerCollector struct {
	dialect  schemaDialect
	names    map[string]struct{}
	bindings []headerBinding
}

func (collector *headerCollector) walk(schema map[string]any, path []string, reachable, propertiesReachable bool) error {
	if annotation, exists := schema["x-mcp-header"]; exists {
		if !reachable || len(path) == 0 {
			return errors.New("mcpadapter: x-mcp-header is not statically reachable through properties")
		}
		if collector.dialect == schemaDialectDraft7 {
			if _, hasRef := schema["$ref"]; hasRef {
				return errors.New("mcpadapter: draft-07 x-mcp-header parameter cannot use $ref")
			}
		}
		name, ok := annotation.(string)
		if !ok || !validHTTPToken(name) {
			return errors.New("mcpadapter: x-mcp-header name is invalid")
		}
		folded := strings.ToLower(name)
		if _, duplicate := collector.names[folded]; duplicate {
			return errors.New("mcpadapter: x-mcp-header names must be case-insensitively unique")
		}
		kind, err := annotatedPrimitiveKind(schema)
		if err != nil {
			return err
		}
		collector.names[folded] = struct{}{}
		collector.bindings = append(collector.bindings, headerBinding{
			path: append([]string(nil), path...),
			name: name,
			kind: kind,
		})
	}

	if properties, ok := schema["properties"].(map[string]any); ok {
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, ok := properties[key].(map[string]any)
			if !ok {
				continue
			}
			var childPath []string
			if propertiesReachable {
				childPath = append(append([]string(nil), path...), key)
			}
			if err := collector.walk(child, childPath, propertiesReachable, propertiesReachable); err != nil {
				return err
			}
		}
	}

	return visitChildSchemasExcludingProperties(schema, func(child map[string]any) error {
		return collector.walk(child, nil, false, false)
	})
}

func visitChildSchemasExcludingProperties(schema map[string]any, visit func(map[string]any) error) error {
	cloned := make(map[string]any, len(schema))
	for key, value := range schema {
		if key != "properties" {
			cloned[key] = value
		}
	}
	return visitChildSchemas(cloned, visit)
}

func annotatedPrimitiveKind(schema map[string]any) (headerValueKind, error) {
	types, ok := schema["type"]
	if !ok {
		return 0, errors.New("mcpadapter: x-mcp-header parameter must declare a primitive type")
	}
	var primitive string
	switch value := types.(type) {
	case string:
		primitive = value
	case []any:
		for _, item := range value {
			name, ok := item.(string)
			if !ok {
				return 0, errors.New("mcpadapter: x-mcp-header parameter type is invalid")
			}
			if name == "null" {
				continue
			}
			if primitive != "" {
				return 0, errors.New("mcpadapter: x-mcp-header parameter must have one primitive type")
			}
			primitive = name
		}
	default:
		return 0, errors.New("mcpadapter: x-mcp-header parameter type is invalid")
	}
	switch primitive {
	case "string":
		return headerString, nil
	case "integer":
		return headerInteger, nil
	case "boolean":
		return headerBoolean, nil
	default:
		return 0, errors.New("mcpadapter: x-mcp-header supports only string, integer, or boolean parameters")
	}
}

func constrainHeaderIntegers(schema map[string]any, bindings []headerBinding) error {
	for _, binding := range bindings {
		if binding.kind != headerInteger {
			continue
		}
		current := schema
		for _, component := range binding.path {
			properties, ok := current["properties"].(map[string]any)
			if !ok {
				return errors.New("mcpadapter: x-mcp-header path is missing from the operation schema")
			}
			next, ok := properties[component].(map[string]any)
			if !ok {
				return errors.New("mcpadapter: x-mcp-header path is missing from the operation schema")
			}
			current = next
		}
		if err := narrowIntegerBound(current, "minimum", -maxMCPHeaderInteger, true); err != nil {
			return err
		}
		if err := narrowIntegerBound(current, "maximum", maxMCPHeaderInteger, false); err != nil {
			return err
		}
	}
	return nil
}

func narrowIntegerBound(schema map[string]any, keyword string, bound int64, minimum bool) error {
	value, exists := schema[keyword]
	if !exists {
		schema[keyword] = json.Number(strconv.FormatInt(bound, 10))
		return nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return errors.New("mcpadapter: x-mcp-header integer bound is invalid")
	}
	rational, ok := parseBoundedJSONRational(number, nil, nil)
	if !ok {
		return errors.New("mcpadapter: x-mcp-header integer bound is invalid")
	}
	limit := new(big.Rat).SetInt64(bound)
	if (minimum && rational.Cmp(limit) < 0) || (!minimum && rational.Cmp(limit) > 0) {
		schema[keyword] = json.Number(strconv.FormatInt(bound, 10))
	}
	return nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func extractParameterHeaders(arguments any, bindings []headerBinding) (map[string]string, error) {
	root, ok := arguments.(map[string]any)
	if !ok || root == nil {
		return nil, errors.New("mcpadapter: operation arguments must be a JSON object")
	}
	headers := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		var current any = root
		present := true
		for _, component := range binding.path {
			object, ok := current.(map[string]any)
			if !ok {
				present = false
				break
			}
			current, present = object[component]
			if !present {
				break
			}
		}
		if !present || current == nil {
			continue
		}
		value, err := parameterHeaderValue(current, binding.kind)
		if err != nil {
			return nil, err
		}
		headers["Mcp-Param-"+binding.name] = encodeHeaderValue(value)
	}
	return headers, nil
}

func parameterHeaderValue(value any, kind headerValueKind) (string, error) {
	switch kind {
	case headerString:
		text, ok := value.(string)
		if !ok {
			return "", errors.New("mcpadapter: x-mcp-header argument is not a string")
		}
		return text, nil
	case headerBoolean:
		boolean, ok := value.(bool)
		if !ok {
			return "", errors.New("mcpadapter: x-mcp-header argument is not a boolean")
		}
		return strconv.FormatBool(boolean), nil
	case headerInteger:
		number, ok := value.(json.Number)
		if !ok {
			return "", errors.New("mcpadapter: x-mcp-header argument is not an integer")
		}
		integer, ok := parseJSONNumberInt64(number)
		if !ok {
			return "", errors.New("mcpadapter: x-mcp-header argument is not an integer")
		}
		if integer < -maxMCPHeaderInteger || integer > maxMCPHeaderInteger {
			return "", errors.New("mcpadapter: x-mcp-header integer exceeds the safe range")
		}
		return strconv.FormatInt(integer, 10), nil
	default:
		return "", errors.New("mcpadapter: x-mcp-header type is unsupported")
	}
}

func encodeHeaderValue(value string) string {
	if plainHeaderValue(value) {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func plainHeaderValue(value string) bool {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		return false
	}
	if len(value) > 0 && (value[0] == ' ' || value[0] == '\t' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' || (character >= 0x20 && character <= 0x7e) {
			continue
		}
		return false
	}
	return true
}
