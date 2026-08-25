package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ly95/agentruntime"
)

func TestMathOperation(t *testing.T) {
	registry, err := mathOperations()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := registry.DecodeInput(addOperation, json.RawMessage(`{"left":19,"right":23}`))
	if err != nil {
		t.Fatal(err)
	}
	operation, ok := registry.Get(addOperation)
	if !ok {
		t.Fatal("math_add operation was not registered")
	}
	result, err := executeMath(context.Background(), agentruntime.OperationRequest{
		Operation: registry.Summaries()[0],
		Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Sum int64 `json:"sum"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatal(err)
	}
	if output.Sum != 42 || operation.Effect != agentruntime.OperationEffectRead {
		t.Fatalf("output=%+v operation=%+v", output, operation)
	}
}
