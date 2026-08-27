package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/ly95/agentruntime"
)

type policyDenyModel struct {
	mu            sync.Mutex
	operationName string
	calls         int
}

func (model *policyDenyModel) Complete(_ context.Context, _ agentruntime.ModelRequest) (*agentruntime.ModelResponse, error) {
	model.mu.Lock()
	model.calls++
	model.mu.Unlock()

	call := agentruntime.ToolCall{
		ID:    "call-policy-denied",
		Name:  model.operationName,
		Input: json.RawMessage(`{"query":"status"}`),
	}
	raw, err := json.Marshal(map[string]any{
		"id":        "item-policy-denied",
		"type":      "function_call",
		"status":    "completed",
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": string(call.Input),
	})
	if err != nil {
		return nil, err
	}
	return &agentruntime.ModelResponse{
		ID: "response-policy-denied",
		Items: []agentruntime.ModelOutputItem{
			{
				ID:   "item-policy-denied",
				Type: agentruntime.ModelOutputFunctionCall,
				Call: &call,
				Raw:  raw,
			},
		},
	}, nil
}

func (model *policyDenyModel) callCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

func TestRuntimePolicyDenyPreventsMCPToolDispatch(t *testing.T) {
	snapshot, transport := testDiscoverSnapshot(t)
	registry := agentruntime.NewOperationRegistry()
	if err := snapshot.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	model := &policyDenyModel{operationName: testOperationName}
	policyCalls := 0
	var policyMu sync.Mutex
	runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model:      model,
		Operations: registry,
		Policy: agentruntime.OperationPolicyFunc(func(context.Context, agentruntime.OperationRequest) (agentruntime.PolicyDecision, error) {
			policyMu.Lock()
			policyCalls++
			policyMu.Unlock()
			return agentruntime.PolicyDecision{
				Action: agentruntime.PolicyDeny,
				Reason: "host policy blocked remote read",
			}, nil
		}),
		Executor: snapshot,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	_, err = runtime.Run(t.Context(), agentruntime.Input{User: "inspect status"})
	if !errors.Is(err, agentruntime.ErrOperationDenied) {
		t.Fatalf("Run error=%v, want errors.Is ErrOperationDenied", err)
	}
	if got := transport.requestCount("tools/call"); got != 0 {
		t.Fatalf("tools/call transport requests=%d, want 0 after PolicyDeny", got)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls=%d, want 1", got)
	}
	policyMu.Lock()
	gotPolicyCalls := policyCalls
	policyMu.Unlock()
	if gotPolicyCalls != 1 {
		t.Fatalf("policy calls=%d, want 1", gotPolicyCalls)
	}
}
