package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationRegistryAcceptsOpenAIStrictNullableOptionalInput(t *testing.T) {
	inputSchema := strictNullableInputSchema()
	strictInput := strictNullableInput()

	strictDocument, err := strictOpenAIParameters(inputSchema)
	if err != nil {
		t.Fatal(err)
	}
	strictSchemaRaw, err := json.Marshal(strictDocument)
	if err != nil {
		t.Fatal(err)
	}
	strictSchema, err := compileOperationSchema("test-strict-input", strictSchemaRaw)
	if err != nil {
		t.Fatal(err)
	}
	var strictValue any
	if err := json.Unmarshal(strictInput, &strictValue); err != nil {
		t.Fatal(err)
	}
	declaredSchema, err := compileOperationSchema("test-declared-input", inputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := declaredSchema.Validate(strictValue); err == nil {
		t.Fatal("strict input unexpectedly matches the declared operation schema")
	}
	if err := strictSchema.Validate(strictValue); err != nil {
		t.Fatalf("OpenAI strict input does not match the generated strict schema: %v", err)
	}

	registry := NewOperationRegistry()
	if err := registry.Register(Operation{
		Name:         "search",
		Description:  "search",
		InputSchema:  inputSchema,
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Effect:       OperationEffectRead,
		Confirmation: ConfirmationSpec{Mode: ConfirmationNone},
	}); err != nil {
		t.Fatal(err)
	}
	registered, ok := registry.Get("search")
	if !ok {
		t.Fatal("registered operation not found")
	}
	if string(registered.InputSchema) != string(inputSchema) {
		t.Fatalf("published input schema changed:\n%s", registered.InputSchema)
	}
	decoded, err := registry.DecodeInput("search", strictInput)
	if err != nil {
		t.Fatalf("strict input failed local operation validation: %v", err)
	}
	arguments, ok := decoded.(map[string]any)
	if !ok || arguments["limit"] != nil || arguments["mode"] != nil {
		t.Fatalf("decoded strict arguments=%#v", decoded)
	}
	if _, err := registry.DecodeInput("search", json.RawMessage(`{
		"query":"schema",
		"context":null,
		"changes":[{"kind":"create"}]
	}`)); err != nil {
		t.Fatalf("omitted optional input failed local operation validation: %v", err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"context":null,"limit":null,"mode":null,"changes":[]}`),
		json.RawMessage(`{"query":null,"context":null,"limit":null,"mode":null,"changes":[]}`),
		json.RawMessage(`{"query":"schema","context":null,"limit":"many","mode":null,"changes":[]}`),
		json.RawMessage(`{"query":"schema","context":null,"limit":0,"mode":null,"changes":[]}`),
		json.RawMessage(`{"query":"schema","context":null,"limit":null,"mode":null,"changes":[],"extra":true}`),
		json.RawMessage(`{"query":"schema","context":null,"changes":[{"kind":"create","title":42,"note":null}]}`),
	} {
		if _, err := registry.DecodeInput("search", invalid); err == nil {
			t.Fatalf("invalid input unexpectedly succeeded: %s", invalid)
		}
	}
}

func strictNullableInputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"context":{"type":["object","null"],"properties":{
				"label":{"type":"string"}
			},"required":["label"],"additionalProperties":false},
			"limit":{"type":"integer","minimum":1},
			"mode":{"anyOf":[{"type":"string"},{"type":"integer"}]},
			"changes":{"type":"array","items":{"anyOf":[
				{"$ref":"#/$defs/create_change"},
				{"type":"object","properties":{
					"kind":{"type":"string","enum":["delete"]},
					"id":{"type":"string"}
				},"required":["kind","id"],"additionalProperties":false}
			]}}
		},
		"$defs":{"create_change":{"type":"object","properties":{
			"kind":{"type":"string","enum":["create"]},
			"title":{"type":"string"},
			"note":{"type":["string","null"]}
		},"required":["kind"],"additionalProperties":false}},
		"required":["query","context","changes"],
		"additionalProperties":false
	}`)
}

func strictNullableInput() json.RawMessage {
	return json.RawMessage(`{
		"query":"schema",
		"context":null,
		"limit":null,
		"mode":null,
		"changes":[{"kind":"create","title":null,"note":null}]
	}`)
}

func TestOperationRegistryValidatesTerminalBatchLimit(t *testing.T) {
	newOperation := func(effect OperationEffect, terminal bool, limit int) Operation {
		confirmation := ConfirmationSpec{Mode: ConfirmationNone}
		var preview func(any) (json.RawMessage, error)
		if effect == OperationEffectWrite {
			confirmation = ConfirmationSpec{Mode: ConfirmationRequired, Description: "Confirm the result."}
			preview = func(any) (json.RawMessage, error) {
				return json.RawMessage(`{"safe":true}`), nil
			}
		}
		return Operation{
			Name: "proposal", ContractVersion: "test-v1", Description: "proposal", Effect: effect,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Confirmation: confirmation, ApprovalPreview: preview,
			ProjectTerminalSession: func(any) ([]TerminalSessionProjection, error) { return nil, nil },
			Terminal:               terminal, TerminalBatchLimit: limit,
		}
	}

	registry := NewOperationRegistry()
	if err := registry.Register(newOperation(OperationEffectWrite, true, 4)); err != nil {
		t.Fatalf("Register valid batched terminal operation: %v", err)
	}
	registered, ok := registry.Get("proposal")
	if !ok || registered.TerminalBatchLimit != 4 {
		t.Fatalf("registered operation=%+v ok=%t", registered, ok)
	}
	summaries := registry.Summaries()
	if len(summaries) != 1 || summaries[0].TerminalBatchLimit != 4 {
		t.Fatalf("operation summaries=%+v", summaries)
	}

	for name, operation := range map[string]Operation{
		"non-terminal": newOperation(OperationEffectWrite, false, 2),
		"read":         newOperation(OperationEffectRead, true, 2),
		"too-small":    newOperation(OperationEffectWrite, true, 1),
		"too-large":    newOperation(OperationEffectWrite, true, MaxTerminalBatchLimit+1),
	} {
		operation.Name = name
		if err := NewOperationRegistry().Register(operation); err == nil {
			t.Fatalf("Register %s unexpectedly succeeded", name)
		}
	}
}

func TestOperationRegistryRequiresTerminalWriteProjection(t *testing.T) {
	operation := operation("finish", OperationEffectWrite)
	operation.Terminal = true
	operation.ProjectTerminalSession = nil
	if err := NewOperationRegistry().Register(operation); err == nil || !strings.Contains(err.Error(), "requires a session projection") {
		t.Fatalf("Register error=%v, want explicit terminal projection requirement", err)
	}
}

func TestOperationSummaryPublishesTheDescriptionUsedByContractIdentity(t *testing.T) {
	register := func(t *testing.T, description string) OperationSummary {
		t.Helper()
		registry := NewOperationRegistry()
		op := operation("read_context", OperationEffectRead)
		op.Description = description
		if err := registry.Register(op); err != nil {
			t.Fatalf("Register: %v", err)
		}
		summaries := registry.Summaries()
		if len(summaries) != 1 {
			t.Fatalf("summaries=%+v", summaries)
		}
		return summaries[0]
	}

	canonical := register(t, "Read current state")
	padded := register(t, "  Read current state\t")
	if padded.Description != canonical.Description || padded.Description != "Read current state" {
		t.Fatalf("published descriptions differ: canonical=%q padded=%q", canonical.Description, padded.Description)
	}
	if padded.ContractID != canonical.ContractID {
		t.Fatalf("equivalent published contracts have different ids: %q != %q", padded.ContractID, canonical.ContractID)
	}
	changed := register(t, "Read historical state")
	if changed.ContractID == canonical.ContractID {
		t.Fatalf("substantive description change retained contract id %q", canonical.ContractID)
	}
}
