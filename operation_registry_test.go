package agentruntime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestOperationRegistryRegisterAll(t *testing.T) {
	registry := NewOperationRegistry()
	first := registryTestOperation(" first ")
	first.Capabilities = []string{" beta ", "alpha", "alpha"}
	first.InputSchema = json.RawMessage(`{
		"type":"object",
		"properties":{"value":{"type":"string"}},
		"required":["value"],
		"additionalProperties":false
	}`)
	first.OutputSchema = json.RawMessage(`{
		"type":"object",
		"properties":{"ok":{"type":"boolean"}},
		"required":["ok"],
		"additionalProperties":false
	}`)

	if err := registry.RegisterAll([]Operation{first, registryTestOperation("second")}); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	registered, ok := registry.Get("first")
	if !ok {
		t.Fatal("first operation was not registered")
	}
	if !reflect.DeepEqual(registered.Capabilities, []string{"alpha", "beta"}) {
		t.Fatalf("first capabilities=%v", registered.Capabilities)
	}
	if _, ok := registry.Get("second"); !ok {
		t.Fatal("second operation was not registered")
	}
	assertRegistryOperationNames(t, registry, []string{"first", "second"})
	for _, provided := range []string{"first", "second", "alpha", "beta"} {
		if !registry.Provides(provided) {
			t.Errorf("registry does not provide %q", provided)
		}
	}
	if err := registry.ValidateInput("first", json.RawMessage(`{"value":"ready"}`)); err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	if err := registry.ValidateOutput("first", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
}

func TestOperationRegistryRegisterAllRejectsBatchDuplicate(t *testing.T) {
	registry := NewOperationRegistry()
	first := registryTestOperation("duplicate")
	first.Capabilities = []string{"batch-capability"}

	err := registry.RegisterAll([]Operation{first, registryTestOperation(" duplicate ")})
	if err == nil || !strings.Contains(err.Error(), "operation already registered: duplicate") {
		t.Fatalf("RegisterAll error=%v, want duplicate operation error", err)
	}
	assertRegistryOperationNames(t, registry, nil)
	if registry.Provides("duplicate") || registry.Provides("batch-capability") {
		t.Fatal("duplicate batch partially mutated registry provisions")
	}
}

func TestOperationRegistryRegisterAllRejectsExistingCollision(t *testing.T) {
	registry := NewOperationRegistry()
	if err := registry.Register(registryTestOperation("existing")); err != nil {
		t.Fatalf("Register existing operation: %v", err)
	}
	newOperation := registryTestOperation("new")
	newOperation.Capabilities = []string{"new-capability"}

	err := registry.RegisterAll([]Operation{newOperation, registryTestOperation(" existing ")})
	if err == nil || !strings.Contains(err.Error(), "operation already registered: existing") {
		t.Fatalf("RegisterAll error=%v, want existing collision error", err)
	}
	assertRegistryOperationNames(t, registry, []string{"existing"})
	if registry.Provides("new") || registry.Provides("new-capability") {
		t.Fatal("colliding batch partially mutated registry provisions")
	}
}

func TestOperationRegistryRegisterAllRejectsInvalidSchema(t *testing.T) {
	registry := NewOperationRegistry()
	invalid := registryTestOperation("invalid_schema")
	invalid.OutputSchema = json.RawMessage(`{"$ref":"#/$defs/missing"}`)

	err := registry.RegisterAll([]Operation{invalid})
	if err == nil || !strings.Contains(err.Error(), "compile operation output schema") {
		t.Fatalf("RegisterAll error=%v, want output schema compilation error", err)
	}
	assertRegistryOperationNames(t, registry, nil)
}

func TestOperationRegistryRegisterAllRejectsFrozenRegistry(t *testing.T) {
	registry := NewOperationRegistry()
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}

	err := registry.RegisterAll([]Operation{
		registryTestOperation("first"),
		registryTestOperation("second"),
	})
	if err == nil || !strings.Contains(err.Error(), "operation registry is frozen") {
		t.Fatalf("RegisterAll error=%v, want frozen registry error", err)
	}
	assertRegistryOperationNames(t, registry, nil)
}

func TestOperationRegistryRegisterAllIsAllOrNothing(t *testing.T) {
	registry := NewOperationRegistry()
	first := registryTestOperation("first")
	first.Capabilities = []string{"first-capability"}
	second := registryTestOperation("second")
	invalid := registryTestOperation("invalid")
	invalid.Effect = OperationEffect("invalid")

	err := registry.RegisterAll([]Operation{first, second, invalid})
	if err == nil || !strings.Contains(err.Error(), "effect must be read or write") {
		t.Fatalf("RegisterAll error=%v, want invalid operation configuration error", err)
	}
	assertRegistryOperationNames(t, registry, nil)
	for _, provided := range []string{"first", "second", "first-capability"} {
		if registry.Provides(provided) {
			t.Errorf("failed batch unexpectedly provides %q", provided)
		}
	}
}

func registryTestOperation(name string) Operation {
	return Operation{
		Name:         name,
		Description:  name,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Effect:       OperationEffectRead,
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}
}

func assertRegistryOperationNames(t *testing.T, registry *OperationRegistry, want []string) {
	t.Helper()
	summaries := registry.Summaries()
	got := make([]string, len(summaries))
	for i, summary := range summaries {
		got[i] = summary.Name
	}
	if len(got) != len(want) {
		t.Fatalf("registered operation names=%v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("registered operation names=%v, want %v", got, want)
		}
	}
}
