package mcpadapter

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	errInvalidStrictNullOmissionSchema = errors.New("mcpadapter: strict-null omission normalizer schema is invalid")
	errStrictNullOmissionInput         = errors.New("mcpadapter: operation arguments cannot be normalized to the declared input schema")
)

// strictNullOmissionNormalizer owns an immutable compiled copy of one frozen,
// sanitized MCP input schema. A method value of normalize has the signature
// required by agentruntime.Operation.NormalizeInput.
type strictNullOmissionNormalizer struct {
	declared *jsonschema.Schema
}

func newStrictNullOmissionNormalizer(inputSchema json.RawMessage) (*strictNullOmissionNormalizer, error) {
	value, err := decodeExactJSON(inputSchema)
	if err != nil {
		return nil, errInvalidStrictNullOmissionSchema
	}
	document, ok := value.(map[string]any)
	if !ok || document == nil || document["type"] != "object" {
		return nil, errInvalidStrictNullOmissionSchema
	}
	if err := validateSchemaObject(document); err != nil {
		return nil, errInvalidStrictNullOmissionSchema
	}

	compiler := jsonschema.NewCompiler()
	const location = "urn:mcpadapter:strict-null-omission-input"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, errInvalidStrictNullOmissionSchema
	}
	declared, err := compiler.Compile(location)
	if err != nil {
		return nil, errInvalidStrictNullOmissionSchema
	}
	return &strictNullOmissionNormalizer{declared: declared}, nil
}

func (normalizer *strictNullOmissionNormalizer) normalize(arguments any) (any, error) {
	if normalizer == nil || normalizer.declared == nil {
		return nil, errStrictNullOmissionInput
	}
	if err := validateNativeJSONValue(arguments, 0); err != nil {
		return nil, errStrictNullOmissionInput
	}

	// Declared-valid nulls are intentional. Preserve the complete caller value,
	// including its container identities, before considering omission sentinels.
	if err := normalizer.declared.Validate(arguments); err == nil {
		return arguments, nil
	}

	candidate := cloneJSONValue(arguments)
	candidate, _, err := normalizeStrictNullValue(candidate, normalizer.declared, 0, make(map[*jsonschema.Schema]struct{}))
	if err != nil {
		return nil, errStrictNullOmissionInput
	}
	if err := normalizer.declared.Validate(candidate); err != nil {
		return nil, errStrictNullOmissionInput
	}
	return candidate, nil
}

// normalizeStrictNullValue removes only nil object members that correspond to
// optional properties whose declared property schema rejects null. It never
// removes an array element. schemaPath bounds same-instance reference cycles;
// descending into a JSON child starts a new path and is bounded by depth.
func normalizeStrictNullValue(
	value any,
	schema *jsonschema.Schema,
	depth int,
	schemaPath map[*jsonschema.Schema]struct{},
) (any, int, error) {
	if depth > maxWireJSONDepth {
		return nil, 0, errStrictNullOmissionInput
	}
	if value == nil || schema == nil {
		return value, 0, nil
	}
	if err := schema.Validate(value); err == nil {
		return value, 0, nil
	}
	if _, cycle := schemaPath[schema]; cycle {
		return value, 0, nil
	}
	schemaPath[schema] = struct{}{}
	defer delete(schemaPath, schema)

	changes := 0
	if schema.Ref != nil {
		normalized, count, err := normalizeStrictNullValue(value, schema.Ref, depth, schemaPath)
		if err != nil {
			return nil, 0, err
		}
		value = normalized
		changes += count
		// Draft-07 treats siblings of $ref as ignored.
		if schema.DraftVersion < 2019 {
			return value, changes, nil
		}
	}

	switch item := value.(type) {
	case map[string]any:
		for name, propertySchema := range schema.Properties {
			property, exists := item[name]
			if !exists {
				continue
			}
			if property == nil {
				if propertySchema.Bool == nil &&
					!strictNullPropertyRequired(schema.Required, name) &&
					propertySchema.Validate(nil) != nil {
					delete(item, name)
					changes++
				}
				continue
			}
			normalized, count, err := normalizeStrictNullValue(
				property,
				propertySchema,
				depth+1,
				make(map[*jsonschema.Schema]struct{}),
			)
			if err != nil {
				return nil, 0, err
			}
			item[name] = normalized
			changes += count
		}
	case []any:
		var itemSchema *jsonschema.Schema
		start := 0
		if schema.DraftVersion >= 2020 {
			itemSchema = schema.Items2020
			start = len(schema.PrefixItems)
			if start > len(item) {
				start = len(item)
			}
		} else {
			itemSchema, _ = schema.Items.(*jsonschema.Schema)
		}
		if itemSchema != nil {
			for index := start; index < len(item); index++ {
				// A null array element is data, never an object-property omission.
				if item[index] == nil {
					continue
				}
				normalized, count, err := normalizeStrictNullValue(
					item[index],
					itemSchema,
					depth+1,
					make(map[*jsonschema.Schema]struct{}),
				)
				if err != nil {
					return nil, 0, err
				}
				item[index] = normalized
				changes += count
			}
		}
	}

	for _, child := range schema.AllOf {
		normalized, count, err := normalizeStrictNullValue(value, child, depth, schemaPath)
		if err != nil {
			return nil, 0, err
		}
		value = normalized
		changes += count
	}

	if schema.RecursiveRef != nil {
		normalized, count, err := normalizeStrictNullValue(value, schema.RecursiveRef, depth, schemaPath)
		if err != nil {
			return nil, 0, err
		}
		value = normalized
		changes += count
	}
	if schema.DynamicRef != nil && schema.DynamicRef.Ref != nil {
		normalized, count, err := normalizeStrictNullValue(value, schema.DynamicRef.Ref, depth, schemaPath)
		if err != nil {
			return nil, 0, err
		}
		value = normalized
		changes += count
	}

	normalized, count, err := normalizeStrictNullAlternatives(value, schema.AnyOf, false, depth, schemaPath)
	if err != nil {
		return nil, 0, err
	}
	value = normalized
	changes += count

	normalized, count, err = normalizeStrictNullAlternatives(value, schema.OneOf, true, depth, schemaPath)
	if err != nil {
		return nil, 0, err
	}
	value = normalized
	changes += count

	return value, changes, nil
}

func strictNullPropertyRequired(required []string, name string) bool {
	for _, candidate := range required {
		if candidate == name {
			return true
		}
	}
	return false
}

// normalizeStrictNullAlternatives considers one deterministic candidate per
// branch and never searches combinations of property deletions. It prefers the
// valid candidate with the fewest deletions. Equally small but different
// candidates are ambiguous and are conservatively ignored.
func normalizeStrictNullAlternatives(
	value any,
	alternatives []*jsonschema.Schema,
	exactlyOne bool,
	depth int,
	schemaPath map[*jsonschema.Schema]struct{},
) (any, int, error) {
	if len(alternatives) == 0 || strictNullAlternativesValidate(value, alternatives, exactlyOne) {
		return value, 0, nil
	}

	var best any
	bestChanges := 0
	hasBest := false
	ambiguous := false
	for _, alternative := range alternatives {
		candidate := cloneJSONValue(value)
		normalized, changes, err := normalizeStrictNullValue(candidate, alternative, depth, schemaPath)
		if err != nil {
			return nil, 0, err
		}
		if changes == 0 || alternative.Validate(normalized) != nil ||
			!strictNullAlternativesValidate(normalized, alternatives, exactlyOne) {
			continue
		}
		switch {
		case !hasBest || changes < bestChanges:
			best = normalized
			bestChanges = changes
			hasBest = true
			ambiguous = false
		case changes == bestChanges && !reflect.DeepEqual(best, normalized):
			ambiguous = true
		}
	}
	if !hasBest || ambiguous {
		return value, 0, nil
	}
	return best, bestChanges, nil
}

func strictNullAlternativesValidate(value any, alternatives []*jsonschema.Schema, exactlyOne bool) bool {
	matches := 0
	for _, alternative := range alternatives {
		if alternative.Validate(value) == nil {
			matches++
			if !exactlyOne {
				return true
			}
			if matches > 1 {
				return false
			}
		}
	}
	if exactlyOne {
		return matches == 1
	}
	return false
}
