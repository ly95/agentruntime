package mcpadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestStrictNullOmissionNormalizerRemovesTopLevelAndNestedOptionalNulls(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		input  any
		want   any
	}{
		{
			name: "top level",
			schema: `{
				"type":"object",
				"properties":{
					"query":{"type":"string"},
					"limit":{"type":"integer"}
				},
				"required":["query"],
				"additionalProperties":false
			}`,
			input: map[string]any{"query": "schema", "limit": nil},
			want:  map[string]any{"query": "schema"},
		},
		{
			name: "nested and multiple",
			schema: `{
				"type":"object",
				"properties":{
					"filter":{
						"type":"object",
						"properties":{
							"query":{"type":"string"},
							"limit":{"type":"integer"},
							"order":{"type":"string"}
						},
						"required":["query"],
						"additionalProperties":false
					}
				},
				"required":["filter"],
				"additionalProperties":false
			}`,
			input: map[string]any{
				"filter": map[string]any{"query": "schema", "limit": nil, "order": nil},
			},
			want: map[string]any{
				"filter": map[string]any{"query": "schema"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalizer := mustStrictNullOmissionNormalizer(t, test.schema)
			before := cloneJSONValue(test.input)
			got, err := normalizer.normalize(test.input)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalized=%#v, want %#v", got, test.want)
			}
			if !reflect.DeepEqual(test.input, before) {
				t.Fatalf("caller input mutated: got %#v, want %#v", test.input, before)
			}
		})
	}
}

func TestStrictNullOmissionNormalizerPreservesRequiredAndIntentionalNullableProperties(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{
		"type":"object",
		"properties":{
			"context":{"type":["object","null"]},
			"note":{"type":["string","null"]},
			"limit":{"type":"integer"}
		},
		"required":["context"],
		"additionalProperties":false
	}`)

	input := map[string]any{"context": nil, "note": nil, "limit": nil}
	got, err := normalizer.normalize(input)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := map[string]any{"context": nil, "note": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized=%#v, want %#v", got, want)
	}
	if _, exists := input["limit"]; !exists {
		t.Fatalf("caller input was mutated: %#v", input)
	}

	alreadyValid := map[string]any{"context": nil, "note": nil}
	preserved, err := normalizer.normalize(alreadyValid)
	if err != nil {
		t.Fatalf("normalize declared-valid input: %v", err)
	}
	preservedObject := preserved.(map[string]any)
	preservedObject["identity_probe"] = true
	if alreadyValid["identity_probe"] != true {
		t.Fatal("declared-valid arguments were copied instead of preserved unchanged")
	}
	delete(alreadyValid, "identity_probe")
}

func TestStrictNullOmissionNormalizerNormalizesObjectsInArraysAndPreservesArrayNulls(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{
		"type":"object",
		"properties":{
			"entries":{
				"type":"array",
				"items":{
					"type":["object","null"],
					"properties":{
						"title":{"type":"string"},
						"note":{"type":["string","null"]},
						"metadata":{
							"type":"object",
							"properties":{"tag":{"type":"string"}},
							"additionalProperties":false
						}
					},
					"additionalProperties":false
				}
			}
		},
		"required":["entries"],
		"additionalProperties":false
	}`)

	input := map[string]any{
		"entries": []any{
			map[string]any{
				"title":    nil,
				"note":     nil,
				"metadata": map[string]any{"tag": nil},
			},
			nil,
			map[string]any{"title": "kept"},
		},
	}
	before := cloneJSONValue(input)
	got, err := normalizer.normalize(input)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := map[string]any{
		"entries": []any{
			map[string]any{
				"note":     nil,
				"metadata": map[string]any{},
			},
			nil,
			map[string]any{"title": "kept"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized=%#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("caller input mutated: got %#v, want %#v", input, before)
	}

	gotEntries := got.(map[string]any)["entries"].([]any)
	gotEntries[0].(map[string]any)["result_only"] = true
	originalEntry := input["entries"].([]any)[0].(map[string]any)
	if _, aliased := originalEntry["result_only"]; aliased {
		t.Fatal("normalized result aliases a caller-owned nested object")
	}
}

func TestStrictNullOmissionNormalizerSupportsLocalReferences(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{
			name: "draft 2020 $defs",
			schema: `{
				"type":"object",
				"properties":{"entry":{"$ref":"#/$defs/entry"}},
				"required":["entry"],
				"additionalProperties":false,
				"$defs":{"entry":{
					"type":"object",
					"properties":{"kind":{"const":"create"},"title":{"type":"string"}},
					"required":["kind"],
					"additionalProperties":false
				}}
			}`,
		},
		{
			name: "draft 7 definitions",
			schema: `{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"type":"object",
				"properties":{"entry":{"$ref":"#/definitions/entry"}},
				"required":["entry"],
				"additionalProperties":false,
				"definitions":{"entry":{
					"type":"object",
					"properties":{"kind":{"const":"create"},"title":{"type":"string"}},
					"required":["kind"],
					"additionalProperties":false
				}}
			}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalizer := mustStrictNullOmissionNormalizer(t, test.schema)
			input := map[string]any{
				"entry": map[string]any{"kind": "create", "title": nil},
			}
			got, err := normalizer.normalize(input)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			want := map[string]any{"entry": map[string]any{"kind": "create"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized=%#v, want %#v", got, want)
			}
		})
	}
}

func TestStrictNullOmissionNormalizerSupportsCommonCompositions(t *testing.T) {
	t.Run("allOf", func(t *testing.T) {
		normalizer := mustStrictNullOmissionNormalizer(t, `{
			"type":"object",
			"properties":{"payload":{"allOf":[
				{
					"type":"object",
					"properties":{"kind":{"const":"a"},"note":{"type":"string"}},
					"required":["kind"]
				},
				{"type":"object","properties":{"kind":{"type":"string"}}}
			]}},
			"required":["payload"],
			"additionalProperties":false
		}`)
		input := map[string]any{"payload": map[string]any{"kind": "a", "note": nil}}
		got, err := normalizer.normalize(input)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		want := map[string]any{"payload": map[string]any{"kind": "a"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("normalized=%#v, want %#v", got, want)
		}
	})

	for _, keyword := range []string{"anyOf", "oneOf"} {
		t.Run(keyword, func(t *testing.T) {
			schema := fmt.Sprintf(`{
				"type":"object",
				"properties":{"choice":{"%s":[
					{
						"type":"object",
						"properties":{"kind":{"const":"a"},"value":{"type":"string"}},
						"required":["kind"],
						"additionalProperties":false
					},
					{
						"type":"object",
						"properties":{"kind":{"const":"b"},"count":{"type":"integer"}},
						"required":["kind"],
						"additionalProperties":false
					}
				]}},
				"required":["choice"],
				"additionalProperties":false
			}`, keyword)
			normalizer := mustStrictNullOmissionNormalizer(t, schema)
			input := map[string]any{"choice": map[string]any{"kind": "a", "value": nil}}
			got, err := normalizer.normalize(input)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			want := map[string]any{"choice": map[string]any{"kind": "a"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized=%#v, want %#v", got, want)
			}
		})
	}
}

func TestStrictNullOmissionNormalizerRejectsAmbiguousComposition(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{
		"type":"object",
		"properties":{"choice":{"anyOf":[
			{
				"type":"object",
				"properties":{"x":{"type":["string","null"]},"y":{"type":"string"}},
				"additionalProperties":false
			},
			{
				"type":"object",
				"properties":{"x":{"type":"string"},"y":{"type":["string","null"]}},
				"additionalProperties":false
			}
		]}},
		"required":["choice"],
		"additionalProperties":false
	}`)
	input := map[string]any{"choice": map[string]any{"x": nil, "y": nil}}
	before := cloneJSONValue(input)
	got, err := normalizer.normalize(input)
	if got != nil || !errors.Is(err, errStrictNullOmissionInput) {
		t.Fatalf("normalize result=%#v error=%v, want stable ambiguity error", got, err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("caller input mutated on error: got %#v, want %#v", input, before)
	}
}

func TestStrictNullOmissionNormalizerRejectsInvalidNonNullInput(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{
		"type":"object",
		"properties":{"query":{"type":"string"},"limit":{"type":"integer"}},
		"required":["query"],
		"additionalProperties":false
	}`)
	input := map[string]any{"query": false, "limit": nil}
	before := cloneJSONValue(input)
	for attempt := 0; attempt < 2; attempt++ {
		got, err := normalizer.normalize(input)
		if got != nil || !errors.Is(err, errStrictNullOmissionInput) {
			t.Fatalf("attempt %d result=%#v error=%v, want stable schema error", attempt, got, err)
		}
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("caller input mutated on error: got %#v, want %#v", input, before)
	}
}

func TestStrictNullOmissionNormalizerCompilesSchemaImmutably(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{"query":{"type":"string"},"limit":{"type":"integer"}},
		"required":["query"],
		"additionalProperties":false
	}`)
	normalizer, err := newStrictNullOmissionNormalizer(raw)
	if err != nil {
		t.Fatalf("new normalizer: %v", err)
	}
	for index := range raw {
		raw[index] = ' '
	}

	got, err := normalizer.normalize(map[string]any{"query": "schema", "limit": nil})
	if err != nil {
		t.Fatalf("normalize after caller schema mutation: %v", err)
	}
	want := map[string]any{"query": "schema"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized=%#v, want %#v", got, want)
	}
}

func TestStrictNullOmissionNormalizerIsConcurrencySafe(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer"},
			"nested":{
				"type":"object",
				"properties":{"tag":{"type":"string"}},
				"additionalProperties":false
			}
		},
		"required":["query","nested"],
		"additionalProperties":false
	}`)
	input := map[string]any{
		"query":  "schema",
		"limit":  nil,
		"nested": map[string]any{"tag": nil},
	}
	before := cloneJSONValue(input)
	want := map[string]any{"query": "schema", "nested": map[string]any{}}

	const workers = 64
	start := make(chan struct{})
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wait.Done()
			<-start
			got, err := normalizer.normalize(input)
			if err != nil {
				errorsFound <- fmt.Errorf("worker %d: %w", worker, err)
				return
			}
			if !reflect.DeepEqual(got, want) {
				errorsFound <- fmt.Errorf("worker %d: normalized=%#v", worker, got)
				return
			}
			got.(map[string]any)["worker_local"] = worker
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("concurrent normalization mutated caller input: got %#v, want %#v", input, before)
	}
}

func TestStrictNullOmissionNormalizerBoundsInputDepth(t *testing.T) {
	normalizer := mustStrictNullOmissionNormalizer(t, `{"type":"object"}`)
	var input any = map[string]any{}
	for depth := 0; depth <= maxWireJSONDepth; depth++ {
		input = map[string]any{"nested": input}
	}
	got, err := normalizer.normalize(input)
	if got != nil || !errors.Is(err, errStrictNullOmissionInput) {
		t.Fatalf("deep input result=%#v error=%v, want stable depth error", got, err)
	}
}

func TestNewStrictNullOmissionNormalizerRejectsInvalidSchemas(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"empty":        nil,
		"non-object":   json.RawMessage(`true`),
		"non-input":    json.RawMessage(`{"type":"string"}`),
		"invalid JSON": json.RawMessage(`{"type":"object"`),
		"external ref": json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"https://example.com/schema"}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			normalizer, err := newStrictNullOmissionNormalizer(schema)
			if normalizer != nil || !errors.Is(err, errInvalidStrictNullOmissionSchema) {
				t.Fatalf("new normalizer=%#v error=%v, want stable schema error", normalizer, err)
			}
		})
	}
}

func mustStrictNullOmissionNormalizer(t *testing.T, schema string) *strictNullOmissionNormalizer {
	t.Helper()
	normalizer, err := newStrictNullOmissionNormalizer(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("new strict-null omission normalizer: %v", err)
	}
	return normalizer
}
